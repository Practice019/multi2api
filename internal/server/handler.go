// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429 冷却，默认 60s
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// NextResetAt 返回"额度耗尽的账号下次可用的时刻"。
	//
	// # 为什么做成可注入的回调（而不是出口层自己算）
	//
	// 早先出口层直接调 pool.CooldownUntilTomorrow4AM —— 那是把
	// **workbuddy 的策略**（次日 04:00，等 09:00/21:00 的签到恢复）
	// 写死在核心。codearts 没有签到，次日 4 点对它毫无意义。
	//
	// 现在由上游提供：workbuddy 给"次日 04:00"，codearts 给"配额窗口重置时刻"。
	// nil 时回落到一个通用的保守值（1 小时），保证没有 Provider 也能跑。
	NextResetAt func() time.Time

	// OwnedBy 填进 /v1/models 的 `owned_by` 字段。
	//
	// 早先这里硬编码 "workbuddy" —— 上游名字写死在核心出口层。
	// 现在由 cmd/server 注入（单上游时就是该上游的 ID）。
	// 空值时回落到 "local"，**不假装知道**是哪个上游。
	//
	// 多上游之后它是**默认上游**的 owned_by；其余上游按各自的 Provider.ID()
	// 填（见 Provider 字段与 modelEntries）。
	OwnedBy string

	// Provider 多上游路由的接缝（可选；nil = 单上游模式，行为与改造前一致）。
	//
	// # 为什么做成接口而不是直接持有 gateway.Registry
	//
	// server 已经 import gateway（用 SplitModel），再持有 Registry 也不成环。
	// 但这里只声明**它实际需要的四个动作**，原因是：
	//   - 上游目录是**可能失败**的（无凭证/无账号），而 Registry 只有 Get，
	//     拿不到"这个上游现在能不能出目录"的答案；
	//   - 全部可缺省：nil 时整条路径退化成改造前的单上游行为（本字段是
	//     向后兼容的关键 —— 既有测试与既有部署都不注入它）。
	//
	// 装配层（cmd/server）用 gateway.Registry + 各 Provider 实现它。
	Provider ProviderRouter

	// DefaultProvider 默认上游标识：裸模型名（"auto"）与无前缀请求走它。
	//
	// 空串与 Provider 为 nil 两种情形下的语义都是"没有多上游概念"，
	// 此时 /v1/models 与选号都保持单上游行为。
	DefaultProvider string
	// Admin 管理台子树（挂在 /admin/，由 internal/admin 提供）。nil = 不注册该子树。
	Admin http.Handler
	// ModelCatalog 供管理台取模型目录快照（成本系数用）。nil = 本实例不提供该能力。
	//
	// 为什么是函数而不是把 *Handler 传过去：admin 已经在用 `ResetModelsCache func()`
	// 这种「由 server 注入闭包」的方向；反向传 *Handler 会让 admin → server 形成
	// 编译期依赖，而 server 反过来 import admin 是为了挂 /admin/ 子树 —— 一反向就是
	// import cycle。传函数既避开环，也让 admin 不必知道缓存住在哪个包。
	ModelCatalog func() *upstream.ModelCatalog
	// ModelCatalogState 只读地报告模型目录缓存状态（ok/stale/unavailable）。
	// 语义见包级 ModelCatalogState —— 它绝不触发上游请求。nil = 未接线。
	ModelCatalogState func() CatalogState
}

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	registerCatalogHost(h) // 让包级 ModelCatalog/ModelCatalogState 能找到本实例的缓存
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// 单数别名：部分客户端/探针按 /v1/model 探测，与 /v1/models 同源同响应。
	h.mux.HandleFunc("GET /v1/model", h.withAuth(h.models))
	// 内嵌本地控制台（同源，无需 CORS）。
	h.mux.HandleFunc("GET /ui", h.ui)
	h.mux.HandleFunc("GET /favicon.ico", h.favicon)
	h.mux.HandleFunc("GET /favicon.svg", h.favicon)
	h.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// 管理台子树：自身做本机校验与鉴权，这里只做挂载（不带 withAuth，否则浏览器拿不到）。
	if cfg.Admin != nil {
		h.mux.Handle("/admin/", cfg.Admin)
	}
	return h
}

// ResetModelsCache 清空动态模型缓存，让下一次 /v1/models 强制回源上游。
// 正缓存 1h / 负缓存 5min 都清掉——用户点「刷新模型」的意图是立刻看到最新目录。
//
// 模型目录（成本系数）缓存一并清空：两者是同一个上游账号在同一次「刷新」里的期望产物，
// 只清一个会出现「模型列表变了但系数还是旧的」这种半刷新状态。
func ResetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	modelCatalogCache.Lock()
	modelCatalogCache.cat = nil
	modelCatalogCache.fetched = time.Time{}
	modelCatalogCache.lastFail = time.Time{}
	modelCatalogCache.Unlock()
}

// ChatLogRing 返回请求日志环形缓冲（/admin/logs 的数据源）。
func ChatLogRing() *logbuf.Ring { return chatLogRing }

// chatLogRing 进程内请求日志缓冲：与 stdout 表格日志同源，容量 2000 条。
var chatLogRing = logbuf.New(logbuf.DefaultCapacity)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// validBearer 报告请求是否携带正确凭据；未配置 APIKey 时恒真。
// 抽成独立方法供 /ui 的非本机分支复用（那里要出 401 + 纯文本，而不是 OpenAI 错误信封）。
func (h *Handler) validBearer(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	return strings.HasPrefix(authz, "Bearer ") && strings.TrimPrefix(authz, "Bearer ") == h.cfg.APIKey
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.validBearer(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": ServiceName,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// staticModels 动态接口失败时的**回退模型表**。
//
// # 为什么留在这里而不是搬走
//
// 这张表是 workbuddy 的模型清单（`owned_by: "workbuddy"`），
// 严格说是上游数据。但它的作用是"上游拉不到时的兜底展示" ——
// 一个纯静态、无副作用的常量，且 /v1/models 是出口层职责。
//
// 多上游落地后这里要改成**按 provider 合并**（Task 6/8）：
// 每个 Provider 通过 Models() 给出自己的目录，出口层合并并加前缀。
// 本任务（阶段 0）只做解耦，不改行为 —— 所以暂时保留原表，
// 但把它与 owned_by 抽成可注入字段，避免核心继续**硬编码**上游名字。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
//
// # 多上游下的形状（Task 6）
//
// 每个上游的模型以 "provider/model" 列出，**默认上游的模型同时以裸名再列一次**。
// 于是同一个上游会贡献两组记录：
//
//	{"id":"glm-5.2",              "owned_by":"workbuddy"}   ← 裸名，向后兼容
//	{"id":"workbuddy/glm-5.2",    "owned_by":"workbuddy"}
//	{"id":"codearts/GLM-5.2",     "owned_by":"codearts"}
//
// # 为什么裸名必须保留（硬要求）
//
// 既有客户端把 "glm-5.2" 直接填进 model 就打 —— 它们是按改造前的
// /v1/models 响应写的。去掉裸名等于让所有既有客户端一夜之间全挂。
// 所以 DefaultProvider 的模型一律**双份**列出，其余上游只有带前缀的一份。
//
// # 未注入路由时（Provider == nil）
//
// 完全退化成改造前的单上游行为：**只有裸名，没有任何前缀记录**。
// 这是既有测试与既有部署的路径，字节级不变。
func (h *Handler) modelList() []map[string]any {
	// 单上游模式：与改造前逐字节一致。
	if h.cfg.Provider == nil {
		return h.singleProviderModelList(h.ownedBy())
	}

	def := h.defaultProvider()
	out := make([]map[string]any, 0, 32)

	// 1. 默认上游：裸名 + 带前缀，两份。
	//    裸名走既有路径（动态列表 + 静态回退），保证形状完全不变。
	for _, e := range h.singleProviderModelList(h.ownedByFor(def)) {
		out = append(out, e)
		if def != "" {
			out = append(out, prefixed(e, def))
		}
	}

	// 2. 其余上游：只有带前缀的一份。
	//    顺序上排在默认上游之后 —— 既有客户端取 data[0] 时拿到的仍是它熟悉的那个。
	for _, id := range h.otherProviders(def) {
		infos, ok := h.cfg.Provider.Models(context.Background(), id)
		if !ok || len(infos) == 0 {
			continue // 该上游暂时给不出目录：跳过，不拖垮整个 /v1/models
		}
		for _, mi := range infos {
			out = append(out, prefixed(h.entryOf(mi.ID, int64(mi.ContextWindow), int64(mi.MaxOutputTokens), h.ownedByFor(id)), id))
		}
	}
	return out
}

// singleProviderModelList 单上游路径：动态列表优先，失败回退静态表。
// owned 是填进 owned_by 的值。
func (h *Handler) singleProviderModelList(owned string) []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			out = append(out, h.entryOf(mi.ID, mi.ContextWindow, mi.MaxTokens, owned))
		}
		return out
	}
	// 静态回退表：补上 owned_by（表里不再硬编码上游名）
	out := make([]map[string]any, 0, len(staticModels))
	for _, m := range staticModels {
		e := make(map[string]any, len(m)+1)
		for k, v := range m {
			e[k] = v
		}
		e["owned_by"] = owned
		out = append(out, e)
	}
	return out
}

// entryOf 把模型元信息编成 OpenAI 形状的一条记录。
//
// 刻意收**三个标量**而不是收一个结构体：两个来源（upstream.ModelInfo 与
// gateway.ModelInfo）是两个包的具名类型，收结构体会逼本函数选边站，
// 从而让 server 依赖其中一个的具体类型。收标量则两边都能用。
//
// 窗口/token 参数是 int64 —— upstream.ModelInfo 用 int64、gateway.ModelInfo
// 用 int，int64 是两者的公共超集，调用处各自隐式提升即可。
func (h *Handler) entryOf(id string, ctxWindow, maxTokens int64, owned string) map[string]any {
	entry := map[string]any{
		"id":                id,
		"object":            "model",
		"created":           1753600000,
		"owned_by":          owned,
		"context_length":    ctxWindow,
		"max_output_tokens": maxTokens,
	}
	if ctxWindow == 0 {
		entry["context_length"] = 131072 // 兜底
	}
	return entry
}

// otherProviders 返回除默认上游外的已注册上游（按字典序，稳定输出）。
//
// 返回空切片时 modelList 退化成"只有默认上游" ——
// 这正是"codearts 未启用"时应当发生的事。
func (h *Handler) otherProviders(def string) []string {
	all := h.providerIDs()
	out := make([]string, 0, len(all))
	for _, id := range all {
		if id == def {
			continue
		}
		out = append(out, id)
	}
	return out
}

// providerIDs 返回已知的全部上游 ID。
//
// 优先问路由（它是唯一的权威）；路由没提供枚举能力时回落成"只有默认上游"，
// 因为一个不枚举的上游集合无法安全地猜测。
func (h *Handler) providerIDs() []string {
	if h.cfg.Provider == nil {
		return nil
	}
	if l, ok := h.cfg.Provider.(interface{ IDs() []string }); ok {
		return l.IDs()
	}
	return nil
}

// ownedBy 返回默认上游在 /v1/models 里的 owned_by 值。
//
// 未注入时给 "local" —— 一个**不假装知道**是哪个上游的中性值。
func (h *Handler) ownedBy() string {
	return h.ownedByFor(h.defaultProvider())
}

// ownedByFor 返回指定上游的 owned_by：用它自己的 ID ——
// 每条模型的 `owned_by` 反映**实际上游**，而不是全局一个值。
//
// 默认上游且装配层显式给了 OwnedBy 时以 OwnedBy 为准
// （那是改造前就存在的注入点，行为要保持）。
// 没有上游上下文时回落 "local"。
func (h *Handler) ownedByFor(id string) string {
	if id == "" {
		if h.cfg.OwnedBy != "" {
			return h.cfg.OwnedBy
		}
		return "local"
	}
	if id == h.defaultProvider() && h.cfg.OwnedBy != "" {
		return h.cfg.OwnedBy
	}
	return id
}

// fetchDynamicModels 从池中任一健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持全局负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// modelCatalogCache 模型目录（GET /v3/config，成本系数）缓存。
//
// 与 dynamicModelsCache 是**两份**缓存而不是合并成一份，理由与两个端点不能互相替代同源：
//   - 数据源不同：/console/enterprises/personal/models（模型可用性）vs /v3/config（成本系数）；
//   - 失败模式不同：系数拿不到只是少一个观测维度，不该把 /v1/models 的模型列表也拖下水。
//
// 生命周期策略刻意与 dynamicModelsCache 完全一致（1h 正缓存 + 5min 失败负缓存、
// 惰性拉取、不加 ticker/goroutine），这样「什么时候会打上游」在代码里只有一套心智模型。
// cat 存 *upstream.ModelCatalog：nil 表示尚无有效目录。
var modelCatalogCache struct {
	sync.RWMutex
	cat      *upstream.ModelCatalog
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

// ModelCatalog 返回缓存的模型目录；缓存失效时惰性回源一次，失败返回 nil。
//
// 这是 FetchModelCatalog 的生产调用点。三条硬约束体现在实现里：
//
//  1. **失败永不向上传播**：签名没有 error，拿不到就返回 nil，调用方（/admin/stats）
//     降级成「unavailable」而不是把统计接口打成 500。
//  2. **不加 ticker/goroutine**：与 fetchDynamicModels 一样纯惰性 —— 只有被问到时才可能拉，
//     没人查询就一次上游请求都不发。
//  3. **失败惩罚账号**：与动态模型列表一致走 Pool.NoteError + 全局 5min 负缓存，
//     否则一个坏号会被反复 Pick 到并反复打上游（这里不主动刷新 token，
//     与 fetchDynamicModels 的取舍保持一致：系数是锦上添花，不值得为它触发一次 refresh）。
//
// 取号策略：**失败换号**。fetchDynamicModels 每次只 Pick 一个号，多账号下
// 「pick 到坏号 → 整个实例 5min 没有目录」的概率不低；系数归因只读多花至多 MaxRotate 次
// 请求即可显著提高成功率，且总次数有硬上限，不会形成风暴。
//
// 为什么是包级函数而不是 (*Handler) 方法：admin 需要这两个能力，而
// `server.NewHandler(Config{Admin: admin.New(...)})` 是一层自引用 ——
// 在构造 h 时 h 还不存在，没法把 h.modelCatalog 作为方法值传进 Config。
// 缓存本身也是包级变量（与 dynamicModelsCache 同款），所以不持有 Handler 也不丢东西。
// ModelCatalog 与 ModelCatalogState 通过 Config 的两个同名字段注入，
// 测试可覆盖成桩函数，生产由 cmd/server 直接传这两个包级函数。
func ModelCatalog() *upstream.ModelCatalog {
	if h := catalogHost.Load(); h != nil {
		return h.modelCatalog()
	}
	return nil
}

// ModelCatalogState 报告模型目录缓存状态并返回快照。**只读，绝不触发拉取**：
// 它会被 /admin/stats 调用，而统计是轮询接口，让它触发上游请求会把「看报表」
// 变成「打上游」。真正的回源只发生在 modelCatalog() 里。
//
// 返回 CatalogState（本包类型）而不是上游类型：状态描述是「缓存这件事」的属性，
// 与上游响应结构无关，放在本包才不需要让 admin 认识 upstream 的字段。
func ModelCatalogState() CatalogState {
	if h := catalogHost.Load(); h != nil {
		return h.modelCatalogState()
	}
	return CatalogState{State: "unavailable"}
}

// catalogHost 保存「目录缓存归属的 Handler」。
//
// 存在的唯一理由：admin 需要在 handler 构造前就拿到这两个取值函数（见 ModelCatalog 的注释）。
// 用 atomic.Pointer 而不是普通变量，是因为它会被 HTTP 处理路径读取，
// 而写入发生在 NewHandler（可能与其他初始化并发）。
//
// ⚠ 这是一个**隐式全局**：后构造的 Handler 会覆盖先前的，因此"谁拥有目录缓存"
// 在进程内只能有一个答案。当前唯一的生产构造点是 cmd/server（单实例），
// 测试也各自串行，所以实际成立；但引入第二个并存的 Handler 时这里会静默错位。
// 之所以接受它：缓存本身（modelCatalogCache）已经是包级变量，语义上就只有一个，
// 这里只是补一个"用谁的 Upstream/Pool 去回源"的答案。若将来真的需要多实例并存，
// 正确做法是把 modelCatalogCache 一并收进 Handler，而不是在这里加锁。
var catalogHost atomic.Pointer[Handler]

// registerCatalogHost 由 NewHandler 调用，登记目录缓存归属。
//
// 抽成函数而不是在 NewHandler 里裸写一行，是为了让"写入这个全局"这件事
// 在 grep 时可见（避免将来有人以为它是只读的）。
func registerCatalogHost(h *Handler) { catalogHost.Store(h) }

// CatalogState 模型目录缓存状态（供管理台区分「有数据 / 数据已过期 / 拿不到」）。
//
// 三态而不是「有没有目录」两态：stale 时目录内容仍然可用（只是可能已过期），
// 前端必须能把它和 ok 区分开，否则用户会拿一份过期系数当实时值读。
type CatalogState struct {
	State     string    `json:"state"` // ok | stale | unavailable
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	Models    int       `json:"models"`
	Stale     bool      `json:"stale"`
	Cooldown  bool      `json:"cooldown,omitempty"` // 是否处于失败负缓存冷却期
}

func (h *Handler) modelCatalogState() CatalogState {
	modelCatalogCache.RLock()
	defer modelCatalogCache.RUnlock()
	st := CatalogState{State: "unavailable"}
	if cool := !modelCatalogCache.lastFail.IsZero() &&
		time.Since(modelCatalogCache.lastFail) < modelsFetchFailCooldown; cool {
		st.Cooldown = true
	}
	if modelCatalogCache.cat == nil {
		return st
	}
	st.Models = len(modelCatalogCache.cat.Models)
	st.FetchedAt = modelCatalogCache.fetched
	if !modelCatalogCache.fetched.IsZero() && time.Since(modelCatalogCache.fetched) >= dynamicModelsTTL {
		st.State, st.Stale = "stale", true
		return st
	}
	st.State = "ok"
	return st
}

// modelCatalog 是 (*Handler) 上的实现体，语义见包级 ModelCatalog。
func (h *Handler) modelCatalog() *upstream.ModelCatalog {
	if h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return nil
	}
	modelCatalogCache.RLock()
	if modelCatalogCache.cat != nil && time.Since(modelCatalogCache.fetched) < dynamicModelsTTL {
		cat := modelCatalogCache.cat
		modelCatalogCache.RUnlock()
		return cat
	}
	// 失败负缓存：冷却期内不再请求上游（与 dynamicModelsCache 同一套语义）。
	if !modelCatalogCache.lastFail.IsZero() && time.Since(modelCatalogCache.lastFail) < modelsFetchFailCooldown {
		modelCatalogCache.RUnlock()
		return nil
	}
	// 内有未过期目录但已超 TTL：先取出来，全部重取失败时回吐旧目录。
	stale := modelCatalogCache.cat
	modelCatalogCache.RUnlock()

	tried := map[string]bool{}
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		cat, err := h.cfg.Upstream.FetchModelCatalog(acct)
		if err != nil || cat == nil || len(cat.Models) == 0 {
			// 与 fetchDynamicModels 同口径：惩罚该账号，避免下次 Pick 又选中同一个反复失败的号。
			h.cfg.Pool.NoteError(acct.UID)
			continue
		}
		modelCatalogCache.Lock()
		modelCatalogCache.cat = cat
		modelCatalogCache.fetched = time.Now()
		modelCatalogCache.lastFail = time.Time{} // 成功则清空负缓存
		modelCatalogCache.Unlock()
		return cat
	}

	// 全部尝试失败：进负缓存。已有旧目录时**保留它并返回** ——
	// 成本系数是观测维度，过期的系数比没有系数有用得多（前端会标 stale 提示其不可信）。
	modelCatalogCache.Lock()
	modelCatalogCache.lastFail = time.Now()
	modelCatalogCache.Unlock()
	return stale
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 选号要按**请求的模型**取额度（见 pool.PickForModel），
	// 并按 **provider 前缀**限定上游（见 pool.PickFor）。
	//
	// 为什么在这里做而不是只在池内：只有出口层知道客户端要哪个模型。
	// 按模型额度的上游（codearts）里，一个账号可能对 gpt-5.5 零额度、
	// 对别的模型额度很高 —— 用标量选号会优先选中它，然后白跑一次。
	//
	// 剥掉 provider 前缀（"workbuddy/auto" → "auto"）：额度表按上游模型名建，
	// 前缀是网关的路由记号，不属于上游模型名。
	//
	// 未知前缀立刻回 400：那是客户端写错了，让它马上知道，
	// 而不是把 "codearts/GLM-5.2" 当模型名发给默认上游再收一个莫名的 400。
	reqProvider, reqModel, perr := h.providerFor(peek.Model)
	if perr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "unknown_provider", perr.Error())
		return
	}
	// 出站请求体里必须写回**剥掉前缀**的模型名：前缀是网关的路由记号，
	// 上游不认识 "workbuddy/auto"（实测：带着前缀发过去，上游按未知模型拒绝，
	// 表现为一次白跑的 503 + 换号）。这一条与"按前缀选号"是一体两面 ——
	// 选号看前缀，发出去看裸名。
	//
	// 只在模型名真的变了时才重新编码：不带前缀的请求（既有客户端）
	// 保持**原始字节原样转发**，行为与改造前逐字节一致。
	outBody := body
	if hasPrefixIn(peek.Model) && reqModel != peek.Model {
		if rewritten, ok := rewriteModel(body, reqModel); ok {
			outBody = rewritten
		}
		// 重写失败（应当不可能：body 刚被 JSON 解析过）时保留原始 body ——
		// 宁可让上游按原样拒绝并给出它的错误，也不要在这里造一个假错误。
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	st.provider = reqProvider
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUID 已校验 health + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUID(stickyUID)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满）→ 解绑，本次回落普通轮换。
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
		}
		if acct == nil {
			// 按 (上游, 模型) 选号：只在该上游的账号里挑，
			// 绝不让 codearts 的请求落到 workbuddy 的账号上。
			// reqProvider 为空（单上游模式 / 裸模型名）时池子按默认上游解释。
			acct = h.cfg.Pool.PickFor(reqProvider, reqModel, tried)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, outBody)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind)
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = upstream.Stream(w, stats)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.usage = stats.Usage()
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		st.usage = usageOf(resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 五条路径，各司其职：
//   - ErrHardCredit → CooldownUntilNextReset：即时硬冷却到**上游给出的**下次重置时刻
//     （workbuddy 是次日 04:00 等签到恢复；其它上游可能是配额窗口重置）。
//   - ErrSoftRate / ErrNotFound → Cooldown(CoolSoft)：即时软冷却（429/404）。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
// nextResetAt 问上游"额度耗尽的号什么时候能再用"。
//
// 没有配置 NextResetAt（单上游未注入、或测试）时回落到 now+1h。
// 选 1 小时而不是"次日某点"：那是个**通用**的保守值，
// 不假装知道任何上游的具体重置策略。配置了上游就以它为准。
func (h *Handler) nextResetAt() time.Time {
	if h.cfg.NextResetAt != nil {
		return h.cfg.NextResetAt()
	}
	return time.Now().Add(time.Hour)
}

func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 额度耗尽关键词：冷却到**上游给出的下次重置时刻**，立即换号。
		//
		// 早先这里直接调 pool.CooldownUntilTomorrow4AM —— 把 workbuddy 的
		// 「次日 04:00 等签到恢复」写进了出口层。现在改为向 Provider 要时刻：
		// 出口层只问"这个号什么时候能再用"，具体策略由上游定义
		// （见 Config.NextResetAt）。
		h.cfg.Pool.CooldownUntilNextReset(uid, h.nextResetAt(), "额度不足")
	case upstream.ErrSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
