// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	APIKey   string // 空 = 不鉴权
	// APIKeys 多 API key 管理（对标 new-api 令牌体系）：config.api_key 是
	// 管理钥匙（不限额），普通 key 走 apikey.Store 的额度/限速/用量。
	// nil = 不启用多 key 管理（旧行为）。
	APIKeys   *apikey.Store
	MaxRotate int // 单请求最多换号次数，默认 3

	// MaxBodyMB 出站前允许的最大请求体（MiB），<=0 回落 8。
	//
	// 见 cmd/server/config.go 的 Server.MaxBodyMB 注释：这是把
	// "静默截断"换成"明确 413"的开关。默认值与改造前硬编码的 8<<20 一致，
	// 因此既有部署的行为只在"超限"这一条路径上发生变化。
	MaxBodyMB int

	// PromptGate 内容拦截降级状态机（可为 nil）。
	//
	// # 为什么它必须与出站客户端**共用同一个实例**
	//
	// 出站循环在这里 Trigger()，出站客户端在每次发请求前读 Active()。
	// 两个不同的实例会让机制完全失效：handler 触发了降级，
	// 客户端却还在用正常提示词 → 每次重试都先撞一次 400。
	//
	// nil 表示本部署未接提示词体系：内容拦截退化为"只换号不罚号"
	// （保守但正确 —— 不会误伤账号，只是会多跑几次往返）。
	PromptGate *prompt.Gate
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429 冷却，默认 60s
	// RefreshSkew token 提前刷新窗口，默认 10m
	RefreshSkew time.Duration

	// ServiceName 本实例的身份标识；空 = 回落包级默认常量 ServiceName。
	//
	// # 为什么要可配置（T8 / 评审 F5）
	//
	// 原先它是编译期常量，于是控制台顶栏那个"服务名"是**前端硬编码**的
	// （3 处），后端改名前端不跟随 —— 那就是 B8。
	// 多实例部署时同名的另一个后果是宿主认不出"这是不是我管的那一个"。
	ServiceName string

	// NextResetAt 返回"额度耗尽的账号下次可用的时刻"，**按上游分派**。
	//
	// # 为什么做成可注入的回调（而不是出口层自己算）
	//
	// 早先出口层直接调 pool.CooldownUntilTomorrow4AM —— 那是把
	// **workbuddy 的策略**（次日 04:00，等 09:00/21:00 的签到恢复）
	// 写死在核心。codearts 没有签到，次日 4 点对它毫无意义。
	//
	// 现在由上游提供：workbuddy 给"次日 04:00"，codearts 给"配额窗口重置时刻"。
	// nil 或 ok=false 时回落到一个通用的保守值（1 小时），保证没有 Provider 也能跑。
	//
	// # ⚠ 为什么参数是 providerID 而不是没有参数（P2 修复）
	//
	// 旧签名是 `func() time.Time`。它**没有参数**，于是签名本身就排除了
	// "按上游分派"的可能 —— 无论注释怎么写，实现永远只能返回**同一个**上游的答案。
	// 装配层注入的是 `wb.NextResetAt`（workbuddy 的次日 04:00），于是
	// **任何上游**的账号在 ErrHardCredit 时都被冷到 workbuddy 的次日 04:00：
	//
	//	codearts 明明已恢复却还冷到次日凌晨 → 白白闲置近 24h
	//	或按 04:00 解冻而实际未恢复        → 又撞一次硬错误
	//
	// providerID 由 applyErrorPolicy / nextResetAt 从账号池的归属反查得到
	// （见 Pool.ProviderOf），与 Chat / Credential / RefreshCredential /
	// RefreshSkew 四条路径**同一形状**：出口层只按 ID 问，不做判断。
	//
	// # ok 的语义
	//
	//	ok=true   该上游上报了自己的恢复排程 → 用它的时刻
	//	ok=false  该上游**没有**这个信息     → 核心回落通用保守值
	//
	// 与 RefreshSkew 的 ok=false 语义逐字一致（见 ProviderRouter.RefreshSkew），
	// 也与 gateway.ResetPolicyExt 的返回值同形。
	NextResetAt func(providerID string) (until time.Time, ok bool)

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
const ServiceName = "multi2api"

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
	// apiKey 当前生效的管理钥匙（config api_key，支持运行时轮换）。
	// 与 cfg.APIKey 解耦：轮换写回 config 后调用 SetAPIKey 立即生效，无需重启。
	apiKeyMu sync.RWMutex
	apiKey   string
}

// SetAPIKey 运行时更新管理钥匙（管理台「轮换管理密钥」用）。
func (h *Handler) SetAPIKey(k string) {
	h.apiKeyMu.Lock()
	h.apiKey = k
	h.apiKeyMu.Unlock()
}

// currentAPIKey 返回当前生效的管理钥匙。
func (h *Handler) currentAPIKey() string {
	h.apiKeyMu.RLock()
	defer h.apiKeyMu.RUnlock()
	return h.apiKey
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
	if cfg.MaxBodyMB <= 0 {
		cfg.MaxBodyMB = defaultMaxBodyMB
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), apiKey: cfg.APIKey}
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

// apikeyCtxKey 请求上下文里存放"本次鉴权命中的 API key id"的键。
type apikeyCtxKeyType struct{}

var apikeyCtxKey apikeyCtxKeyType

// validBearer 报告请求是否携带正确凭据；未配置任何鉴权时恒真。
//
// 鉴权优先级（对标 new-api 的令牌体系，config.api_key 是管理钥匙）：
//
//	config.api_key 非空且匹配       → 管理 key，通过（不限额不限速）
//	apikey.Store 非空且 Bearer 命中 → 普通 key，通过（额度/限速校验见下）
//	两者都未配置                     → 不鉴权（旧行为）
//
// 普通 key 的错误映射：未知/禁用 → 401；额度用尽 → 402；超速 → 429。
// 抽成独立方法供 /ui 的非本机分支复用（那里要出 401 + 纯文本，而不是 OpenAI 错误信封）。
func (h *Handler) validBearer(r *http.Request) bool {
	bearer := ""
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		bearer = strings.TrimPrefix(authz, "Bearer ")
	}
	ak := h.currentAPIKey()
	if ak == "" && h.cfg.APIKeys == nil {
		return true
	}
	if ak != "" && bearer == ak {
		return true
	}
	if h.cfg.APIKeys != nil && bearer != "" {
		_, err := h.cfg.APIKeys.Validate(bearer)
		// 额度/限速错误由 withAuth 映射 402/429；这里只回答"凭证是否有效"。
		return err == nil
	}
	return false
}

// authResult 鉴权结果：命中的 apikey id（空 = 管理 key / 无多 key）与错误。
type authResult struct {
	apikeyID string
	err      error // nil = 通过；否则为 apikey 包的哨兵错误
}

// authorize 完整鉴权判定（含普通 key 的额度/限速）。
func (h *Handler) authorize(r *http.Request) authResult {
	bearer := ""
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		bearer = strings.TrimPrefix(authz, "Bearer ")
	}
	ak := h.currentAPIKey()
	// 未配置任何鉴权 → 恒通过（旧行为）。
	if ak == "" && h.cfg.APIKeys == nil {
		return authResult{}
	}
	// 管理 key 优先。
	if ak != "" && bearer == ak {
		return authResult{}
	}
	// 普通 key。
	if h.cfg.APIKeys != nil && bearer != "" {
		id, err := h.cfg.APIKeys.Validate(bearer)
		if err == nil {
			return authResult{apikeyID: id}
		}
		return authResult{err: err}
	}
	return authResult{err: apikey.ErrUnknown}
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ar := h.authorize(r)
		if ar.err != nil {
			switch {
			case errors.Is(ar.err, apikey.ErrQuota):
				writeOpenAIError(w, http.StatusPaymentRequired, "insufficient_quota", "API key 额度已用完")
			case errors.Is(ar.err, apikey.ErrRateLimit):
				writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "请求过于频繁（触发该 API key 的每分钟上限）")
			default:
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			}
			return
		}
		if ar.apikeyID != "" {
			r = r.WithContext(context.WithValue(r.Context(), apikeyCtxKey, ar.apikeyID))
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
	svc := h.serviceName()
	w.Header().Set("X-Service", svc)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": svc,
	})
}

// serviceName 返回本实例的身份标识。
//
// 优先用配置里显式给的值（见 cmd/server.Config.ServiceName），
// 空则回落编译期默认值 —— 老配置无需任何改动即可继续工作。
func (h *Handler) serviceName() string {
	if h.cfg.ServiceName != "" {
		return h.cfg.ServiceName
	}
	return ServiceName
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
// # 多上游下的形状（目录收窄：一个上游只列一条 provider/model）
//
// 每个上游的模型**只以 "provider/model" 列一条**，没有任何裸名记录：
//
//	{"id":"workbuddy/glm-5.2", "owned_by":"workbuddy"}
//	{"id":"codearts/GLM-5.2",  "owned_by":"codearts"}
//
// 收窄之前默认上游是"裸名 + 带前缀"两份（workbuddy 的 16 个模型列成 32 条），
// 同一个模型在下拉框里出现两次。用户明确要求"统一、不要重复"，
// 所以默认上游那一支现在**只 append 带前缀的那一份**。
//
// # 这次收窄的代价（如实记录，不是"没有影响"）
//
// 选路侧没变：providerFor（provider_router.go）对裸名仍按默认上游解析，
// 老客户端把 "glm-5.2" 填进 model 打进来照样能用。变的是**列目录**：
// 既有客户端若从 /v1/models 里取 id 再回填，收窄后会看不到裸名，
// 必须改用 "provider/model" 形式。
//
// 顺序：默认上游的条目仍排在最前（既有客户端取 data[0] 的习惯），
// 其后是其它上游（按字典序）。
//
// # def == "" 的边界（多上游但默认上游为空）
//
// 没有上游身份就拼不出 "provider/model"，硬拼只会得到 "/glm-5.2" ——
// 一个没人能解析的畸形 id。所以 def == "" 时保持**裸名**（每个模型仍只有
// 一条，只是不做前缀投影），且绝不产生以 "/" 开头的 id。
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
	out := make([]map[string]any, 0, 16)

	// 1. 默认上游：只列带前缀的一份 —— 收窄后不再同时列裸名，
	//    否则同一个模型会在目录里出现两次（用户报的就是这个重复）。
	//    def == "" 是边界：拼前缀只会得到 "/glm-5.2"，
	//    所以此时保持裸名 —— 每个模型仍然只有一条，只是没有上游前缀。
	for _, e := range h.singleProviderModelList(h.ownedByFor(def)) {
		if def == "" {
			out = append(out, e)
			continue
		}
		out = append(out, prefixed(e, def))
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

	// ⚠ P1（本次修复）：选号必须带 **provider 维度**。
	//
	// 原来这里是 `h.cfg.Pool.Pick()` —— **不带 provider 过滤**，于是在多上游
	// 部署里可能抽到 codearts 的账号，然后拿它去调 workbuddy 的 FetchModels。
	// 失败之后下面那句 `Pool.NoteError(acct.UID)` 会把**一个无辜的 codearts
	// 账号**喂进熔断计数（err_total +1，连续 3 次就熔断），而真正的原因
	// （用错了上游的客户端）没有任何痕迹。
	//
	// 这与本次修的 chat 路径是同一个 bug 的两种形态：**选号按 provider 分域了，
	// 出站调用没有**。chat 路径换成按 Provider 分派；这里没有"多上游目录"的
	// 需求（/v1/models 的动态列表只属于默认上游），所以最小且正确的修法是
	// 取号时限定在默认上游。
	//
	// ⚠ 传 **""（不是 h.defaultProvider()）**：本函数用的客户端恒是
	// h.cfg.Upstream（默认上游的），所以候选集必须是**池子眼里的默认上游**。
	//
	// 池子的默认上游由 `SetDefaultProvider` 在启动时注入（cmd/server），
	// 而 `h.cfg.DefaultProvider` 是出口层的配置值 —— 两者**可能不一致**
	// （测试里就是：池子没注入、出口层配了 "workbuddy"）。传后者会让
	// `normalizeProvider` 选出一个池子里根本不存在的域，候选集为空、
	// 动态目录静默消失（实测：32 条基线掉到 27 条 —— 静默退回了静态表）。
	//
	// "" 的语义正是"没有上游上下文，走池子的默认"（见 normalizeProvider），
	// 与改造前 `Pick()` 的候选集**完全一致**，只是把"不带过滤"换成了
	// "按默认上游过滤"。两者在单上游部署里是同一个集合，因此既有部署零变化。
	acct := h.cfg.Pool.PickForExcluding("", nil)
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
		// ⚠ P1（本次修复）：与 fetchDynamicModels 同一条理由 ——
		// PickExcluding 不带 provider 过滤，混池部署里会抽到别的上游的账号，
		// 用默认上游的客户端去拉系数必然失败，然后**惩罚那个无辜的账号**。
		// 这里的缓存（modelCatalogCache）同样只有一份、只属于默认上游。
		//
		// 传 ""（不是 h.defaultProvider()）：理由见 fetchDynamicModels 里的长注释
		// —— 本处用的客户端同样是 h.cfg.Upstream（池子眼里的默认上游），
		// 用出口层的配置值会选出一个池子里不存在的域。
		acct := h.cfg.Pool.PickForExcluding("", tried)
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

// defaultMaxBodyMB 请求体上限的兜底值（MiB）。
//
// 与改造前那句 `io.LimitReader(r.Body, 8<<20)` 的 8 MiB **数值相同** ——
// 本改动的意图不是改变"能收多大"，而是改变"收不下时说什么"：
// 从"无声截断 → 上游 11101 → 客户端以为自己的 JSON 写错了"，
// 变成"读的时候就报 413 → 客户端知道是大小问题"。
const defaultMaxBodyMB = 8

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader 而不是 LimitReader：超限时 Read 返回 *http.MaxBytesError，
	// 我们据此回 413；LimitReader 只会静默截断，把"太大"伪装成"JSON 畸形"。
	//
	// 传 w 是为了让 net/http 在超限时标记连接不可复用（避免残留未读字节
	// 污染同一 keep-alive 连接上的下一个请求）。
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(h.cfg.MaxBodyMB)<<20))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			// 报错文案必须给出**可执行的两条出路**：缩小请求 / 调大配置。
			// 只说"too large"会让用户去猜上限是多少、在哪改。
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("请求体超过上限 %d MiB（可在 config.json 的 server.max_body_mb 调整）；"+
					"多图/长上下文会话请缩小 messages 或提高该上限", h.cfg.MaxBodyMB))
			return
		}
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
	// API key 用量记录：命中普通 key 时在出口把 token/成败记给该 key。
	if kid, ok := r.Context().Value(apikeyCtxKey).(string); ok && kid != "" && h.cfg.APIKeys != nil {
		st.apikeyID = kid
		st.bump = func(toks int, ok bool) { h.cfg.APIKeys.BumpUsage(kid, int64(toks), ok) }
	}
	defer st.done()

	// ctx 供出站调用使用（Provider.Chat / RefreshCredential 都收 ctx）。
	//
	// # 为什么挂在 r.Context() 上
	//
	// 客户端断开时 r.Context() 被取消，Provider 的实现据此**立刻返回**而不是
	// 继续跑完一次上游往返 —— 这是契约要求（见 gateway 的
	// verifyChatRespectsCancel），对 codearts 尤其重要：它每个账号只有 3 个
	// 并发会话槽，前端断开后上游调用仍在跑会白占槽位。
	//
	// ⚠ 会不会把"客户端断开"变成一次账号失败的惩罚？不会 ——
	// 取消会走 Provider.Chat 返回的 error → chatVia 走 terr → 只换号
	// 不喂熔断（与网络抖动同一条路径）。
	//
	// # 客户端 IP 也走 ctx（而不是 Client 上的字段）
	//
	// 见 gateway/clientip.go：字段形态在并发下会串扰（A 请求写、B 请求读），
	// 表现为上游看到来源错乱的 IP，且只在并发下出现。
	// 这里**无条件**放入（即使 passthrough_ip 关闭）：放不放的成本是零，
	// 而"要不要用"由出站客户端读它自己的配置决定 ——
	// 判断留在唯一知道该配置的那一层。
	ctx := gateway.WithClientIP(r.Context(), gateway.ExtractClientIP(r))

	tried := map[string]bool{}
	var lastErr error

	// degradedTried 本请求是否已经用过降级提示词重试。
	//
	// 初值取"进入本请求时降级是否已激活"：若上一批请求已经把 Gate 触发过，
	// 那么客户端**本次发的就是 Degraded 提示词** —— 再拦就说明问题在用户内容，
	// 没有第三次可试。这样"每个请求最多因内容拦截重试一次"是精确成立的，
	// 而不是靠一个恒为 false 的局部变量（那会让每次请求都白白多试一轮）。
	degradedTried := h.cfg.PromptGate.Active()

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
		// 选号：粘性号优先（已校验 **provider + health + 在途未满**），否则普通轮换。
		//
		// ⚠ 必须用 PickByUIDFor(reqProvider, ...) 而不是 PickByUID(...)：
		// 会话粘性跨上游时，粘性命中会把**别的上游的账号**塞进出站循环。
		// 改造后出站按"账号自己的上游"分派（见 chatVia），所以它**不会报错** ——
		// 只会让「请求 workbuddy 的模型却用了 codearts 的账号」这种路由错误
		// 静默发生，比一次失败难查得多。
		//
		// 不匹配时返回 nil → 走下面的解绑 + 回落普通轮换分支，
		// 按本次请求的 reqProvider 重新选号。粘性是优化不是契约，
		// 跨上游时放弃绑定是正确降级。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDFor(reqProvider, stickyUID)
			if acct == nil {
				// 粘性号当前不可用（别的上游/冷却/占满）→ 解绑，本次回落普通轮换。
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
			// 满载粘性号再浪费一次 PickByUIDFor 往返（语义与 fail()/PickByUIDFor-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// 该不该刷由**上游自己**回答（失败冷却换号）。
		//
		// ⚠ 这里刻意**不是** `acct.NeedsRefresh(h.cfg.RefreshSkew)`：
		// 核心的 10m 窗口对 codearts（STS 仅约 2h、自己的窗口 3m）是错的，
		// 且错的方向是"太早"—— 剩 8m 就被核心判为该刷，而 codearts 的
		// CredentialRefresher 内部没有 skew 检查，于是真的去消费那个
		// **一次性**的 refresh_token。判据必须与"怎么刷"同处一地，见
		// needsRefreshVia 的注释。
		if h.needsRefreshVia(reqProvider, acct) {
			if err := h.refreshCredential(ctx, reqProvider, acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 与出站路径同口径：刷新时的 session dead 也走计数门控。
					// 刷新失败常常正是**竞态**（另一个并发请求刚消费了 refresh token），
					// 一次就禁用是这个路径上最典型的误杀来源。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
		}

		// 出站：按**选中的账号所属上游**分派到它自己的实现。
		//
		// 这是本次修复的核心。改造前这里恒用 h.cfg.Upstream（= 默认上游的
		// upstream.Client），于是 codearts 的请求被 workbuddy 的客户端发出去 ——
		// 凭证格式不匹配，必然失败，而且失败得很晚（一次完整往返）。
		//
		// cfg.Provider == nil（单上游部署 / 既有测试）时逐字节回退到原行为，
		// 见 chatVia 的注释。
		cr, status, terr := h.chatVia(ctx, reqProvider, acct, outBody)
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
			// 非 2xx：上游的错误体在流里，读出来喂 Classify。
			//
			// ⚠ 上限 1MB：上游的错误体正常是几百字节，但一个坏的/恶意的响应
			// 可能是一整个 SSE 流。无上限 ReadAll 会把它整个灌进内存，
			// 而这里只是要一段能分类的文本。
			respBody, _ := io.ReadAll(io.LimitReader(cr, 1<<20))
			// 错误体读完即弃（response body 已到 EOF），Close 防连接泄漏 ——
			// 这与成功路径上的 rc.Close() 对称。
			_ = cr.Close()
			//
			// 分类按**选中账号所属上游**分派（见 classifyErr 的长注释）：
			// 改造前这里恒调 `upstream.Classify` —— 那是 workbuddy 的分类器，
			// 却作用在两条上游的响应体上，造成"codearts 额度漏判"与
			// "codearts 号被裸数字 12153 永久禁用"两条真危害。
			kind := h.classifyErr(reqProvider, status, respBody)
			lastErr = &upstream.Error{
				// 中立类型 → upstream.ErrKind 的翻译**仅用于拼错误消息**：
				// lastErr 只出现在循环出口的 503 文本里（见下方 msg），
				// 不参与任何策略判断。真正的策略走 applyErrorPolicy(kind)。
				Kind:   upstreamKindOf(kind),
				Status: status,
				Msg:    string(respBody),
			}
			// 内容策略拦截：**不罚账号**，改用降级提示词重试一次。
			//
			// 见 gateway.ErrKindContentBlocked 的长注释：内容问题换号没有意义
			// （每个账号背后是同一套策略），正确的动作是换提示词。
			// 因此这里刻意**不调** applyErrorPolicy —— 它会把账号冷却/熔断，
			// 而账号在这件事上没有任何过错。
			if kind == gateway.ErrKindContentBlocked {
				if h.retryWithDegradedPrompt(w, st, reqProvider, acct.UID, status, respBody, &degradedTried) {
					// 内容问题**不是账号问题**，所以这一轮的动作刻意与其它错误不同：
					//
					//	不调 applyErrorPolicy → 不冷却 / 不计错 / 不喂熔断
					//	不调 fail(uid)        → 不解绑会话粘性（会话本身没坏）
					//
					// 只释放在途租约，并把该账号从 tried 里**撤掉** ——
					// 让下一轮能重新选回同一个账号。
					//
					// # 为什么必须撤 tried（这是单账号部署的生死线）
					//
					// 不撤的话，PickFor 会因为 "已试过" 而跳过它：
					// 单账号部署下一轮**选不出任何号** → 直接 break →
					// 返回 503 "all accounts unavailable"。
					// 于是"降级重试"在最常见的部署形态下根本没发生过，
					// 用户看到的是"内容拦截 = 服务不可用"。
					//
					// 保留粘性绑定则让下一轮优先回到同一个账号（PickByUIDFor 先行），
					// 多账号部署下若粘性未命中，普通轮换选到别的号也无妨 ——
					// 内容策略是账号无关的，用哪个号都一样。
					releaseHeld()
					delete(tried, acct.UID)
					continue
				}
				releaseHeld()
				return
			}
			// 模型级限流收窄（借鉴 workbuddy2api-panel）：
			// 429 code=6004 明说"将在 … 重置"时，把冷却精确到那个墙钟，
			// 并**记录触发模型** —— 换模型请求时该账号视为可用。
			//
			// 必须在 applyErrorPolicy **之前**判：后者会给一个
			// "基数 × 指数退避"的账号级冷却，那正是本收窄要避免的形态
			// （它会让账号在只被单模型限流时被整体冷掉）。
			if kind == gateway.ErrKindSoftRate {
				if resetAt, ok := h.softRateReset(reqProvider, status, respBody); ok {
					h.cfg.Pool.CooldownSoftForModel(acct.UID, h.cfg.SoftCooldown,
						resetAt, reqModel, "429 模型级限流")
					fail(acct.UID)
					continue
				}
			}
			h.applyErrorPolicy(acct.UID, kind)
			fail(acct.UID)
			continue
		}
		rc := cr
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			//
			// ⚠ 这里**不能**再提前写死 `st.status = OK`：Stream 在
			// "首帧即流内错误"时一个字节都不会写，并把 *InBandError 交回来
			// （见 upstream.Stream 的返回值约定）。那种情形必须与下面的
			// status>=400 同路（分类 + 换号），否则客户端拿到的就是那个
			// `200 + 一帧没有 choices 的空壳 + [DONE]` —— 正是"外部调用 api
			// 只有 codearts 报错"的形态。
			stats := newChatStatsReaderSince(rc, st.start)
			serr := upstream.Stream(w, stats)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.usage = stats.Usage()
			rc.Close()

			var inband *upstream.InBandError
			if errors.As(serr, &inband) {
				if inband.Committed {
					// 内容已流出、状态码已提交，错误帧也已原样下发：
					// 既不能改响应也不再换号（客户端已经"消费"了这个回复）。
					// 只记录，保留可排查的上游原文。
					log.Printf("chat stream uid=%s provider=%s: 上游流内错误（状态码已提交）: %s",
						acct.UID, reqProvider, inband.Message)
					h.cfg.Pool.NoteSuccess(acct.UID)
					if sessKey != "" && h.cfg.Session != nil {
						h.cfg.Session.Bind(sessKey, acct.UID)
					}
					st.status = http.StatusOK
					return
				}
				// 状态码未提交 → 与 status>=400 完全同路。
				//
				// classifyErr 用的是**该上游自己的**分类器（status 传 200：
				// 这正是"错误藏在 2xx 里"的事实，分类器必须看得见它）。
				// 模型名/通道类错误会落到 ErrKindNone（只换号不罚），
				// 额度类错误会落到 ErrKindHardCredit（进硬冷却）——
				// 判据全部来自上游，这里不新增任何假设。
				kind := h.classifyErr(reqProvider, http.StatusOK, []byte(inband.Body))
				lastErr = &upstream.Error{
					Kind:   upstreamKindOf(kind),
					Status: http.StatusOK,
					Msg:    inband.Message,
				}
				h.applyErrorPolicy(acct.UID, kind)
				fail(acct.UID)
				continue
			}

			// 到这里才是真正的成功流（serr 非 nil 的情形只有"上游空流"，
			// 那一帧 error + [DONE] 已由 Stream 写出，状态码必须保持 200）。
			h.cfg.Pool.NoteSuccess(acct.UID)
			// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
			st.status = http.StatusOK
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()

		// 非流式同样要拦流内错误：Aggregate 命中错误信封时返回 *InBandError，
		// 而不是合成一个 content:"" 的成功响应（改造前它就是这么被吞掉的）。
		var inband *upstream.InBandError
		if errors.As(err, &inband) {
			kind := h.classifyErr(reqProvider, http.StatusOK, []byte(inband.Body))
			lastErr = &upstream.Error{
				Kind:   upstreamKindOf(kind),
				Status: http.StatusOK,
				Msg:    inband.Message,
			}
			h.applyErrorPolicy(acct.UID, kind)
			fail(acct.UID)
			continue
		}
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
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

// retryWithDegradedPrompt 处理一次**内容策略拦截**（借鉴 workbuddy2api-panel）。
//
// # 语义
//
//	返回 true  → 已触发降级，调用方应 continue 让下一轮用中性提示词重发
//	返回 false → 已向客户端写出终态错误，调用方应立即 return（不再重试）
//
// # 为什么"重试一次"就够了，不做更多
//
// 上游只给一句"blocked"，无法区分「system 指纹误报」与「用户内容真的违规」。
// 换中性提示词重试一次是**唯一**能区分两者的实验：
//
//	重试后通过 → 是①，降级有效，本请求正常返回
//	重试后仍被拦 → 是②，问题在用户内容里，再怎么换提示词/换账号都没用
//
// 继续重试只会把一次内容问题拖成 MaxRotate 次白跑往返，
// 最后返回"所有账号不可用" —— 一个把内容问题误报成账号池故障的错误结论。
// 因此第二次被拦时**立刻如实告诉调用方**，带上上游原文。
//
// # 为什么账号不罚
//
// 内容策略是**账号无关**的：同一套策略在所有账号后面。因此这条路径
// 绝不调用 applyErrorPolicy（不冷却、不计错、不喂熔断）。
// 调用方只做 fail(uid) —— 那是**释放租约 + 解绑粘性**的资源动作，不是惩罚。
//
// # status 兜底
//
// 传入的是上游的原始状态码（正常是 400）。若上游给的是 2xx（错误藏在流内），
// 直接把它转发给客户端会让客户端以为成功 —— 因此夹到 400。
// 这一点很重要：本函数是在"读流内错误"的分支上被复用的。
func (h *Handler) retryWithDegradedPrompt(
	w http.ResponseWriter, st *chatStat, providerID, uid string,
	status int, respBody []byte, degradedTried *bool,
) bool {
	msg := contentBlockMsg(respBody)
	if h.cfg.PromptGate != nil && !*degradedTried {
		h.cfg.PromptGate.Trigger()
		*degradedTried = true
		log.Printf("chat uid=%s provider=%s: 内容策略拦截，已切降级提示词重试一次（降级至 %s）: %s",
			uid, providerID, h.cfg.PromptGate.Until().Format(time.RFC3339), msg)
		return true
	}
	// 中性提示词仍被拦（或本部署没接提示词体系）→ 判定为用户内容触发审核。
	//
	// 注意 nil gate 时也走这里：没有降级能力还继续换号是纯浪费，
	// 如实返回上游错误比"所有账号不可用"更有信息量。
	log.Printf("chat uid=%s provider=%s: 内容策略拦截（%s），判定为请求内容本身触发：%s",
		uid, providerID, contentBlockExhaustedReason(h.cfg.PromptGate, *degradedTried), msg)
	if status < 400 || status > 599 {
		status = http.StatusBadRequest
	}
	writeOpenAIError(w, status, "content_blocked", "上游内容策略拦截："+msg)
	st.status = status
	return false
}

// contentBlockExhaustedReason 给日志一个能区分两种终态的措辞。
func contentBlockExhaustedReason(gate *prompt.Gate, degradedTried bool) string {
	switch {
	case gate == nil:
		return "未接提示词体系，无法降级"
	case degradedTried:
		return "降级提示词后仍被拦"
	default:
		return "降级重试次数已用尽"
	}
}

// contentBlockMsg 摘要上游错误体，供日志与返回给客户端的文案使用。
//
// 上限 300 字节并按**字符边界**截断：上游错误体是 UTF-8 中文，
// 按字节切会把一个汉字切成两半，产生 \ufffd 乱码
// （本仓库已踩过这个坑，见 CHANGELOG 的「UTF-8 截断」条目）。
func contentBlockMsg(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(上游未返回错误详情)"
	}
	// 压掉换行，让日志保持"一行一条"
	s = strings.Join(strings.Fields(s), " ")
	const maxRunes = 300
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// ⚠ 参数只有 uid：调用方（出站循环）只拿得到**出错的号**，拿不到"当前请求的
// 上游" —— 多上游下两者可能不同（粘性命中、轮换换号）。所以需要上游事实的
// 分支（ErrKindHardCredit）自己去池子反查归属：
//
//	Pool.ProviderOf(uid) → nextResetAt(uid) → cfg.Provider.ResetAt(id, cred)
//
// 这正是 P2 修复的形状：把"哪个上游"这个事实从 uid 反查出来，
// 而不是让一个无参回调替所有上游回答同一个时刻。
//
// kind 是唯一权威分类（来自**选中账号所属上游**的错误分类器，
// 经 gateway 层中立化后传进来），此处不再按原始 status 二次判断。
//
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// # 为什么参数类型从 upstream.ErrKind 换成 gateway.ErrorKind
//
// 改造前它吃 `upstream.ErrKind` —— 那是 **workbuddy 的类型**，
// 于是 codearts 的分类结果**在类型上就传不进来**，core 只能拿
// workbuddy 的判据去判所有上游的响应（P2 缺口的技术根因）。
//
// 现在它吃 gateway.ErrorKind（跨上游中立，core 与上游都合法依赖 gateway）。
// 各上游在自己的包里把自己的 ErrKind 翻译过来，core 只做策略决策 ——
// **策略本身一行都没改**，只换了接缝上的词汇。
//
// 七条路径，各司其职：
//   - ErrKindHardCredit → CooldownUntilNextReset：即时硬冷却到**上游给出的**
//     下次重置时刻（workbuddy 是次日 04:00 等签到恢复；codearts 是配额窗口重置）。
//   - ErrKindSoftRate / ErrKindNotFound → Cooldown(CoolSoft)：即时软冷却（429/404）。
//   - ErrKindSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//     ⚠ **只有 workbuddy 会产生这个分类** —— codearts 的凭证失效是
//     ErrKindAuth（可自动续期恢复），刻意不映射到这里。
//   - ErrKindServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrKindAuth / 其他（default：ErrKindClient/ErrKindNone）→ 只换号不罚
//     （防雪崩），不喂熔断。
//
// # ErrKindAuth 为什么走 default（只换号不罚）
//
// 它是 codearts 的凭证失效（401/403）。这类失效**可以由
// RefreshCredential 恢复**（STS 凭证仅约 2 小时，401 是常见路径而非异常，
// 见 codearts/client.go 的 MaxAuthRetry 与 credentialrefresher.go）。
// 把它升级成 Disable 会把"一次 401"变成"永久禁用" —— 那正是本次要修的
// 危害 ② 的另一半。保守方向是明确的：宁可多换一次号，也不能把可恢复的
// 账号永久打掉。续期由后台任务 + 请求路径的 needsRefreshVia 分支负责。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
// nextResetAt 问**该账号所属上游**"额度耗尽的号什么时候能再用"。
//
// # 四条路径（与 chatVia / refreshCredential / needsRefreshVia / classifyErr 同形状）
//
//	Provider == nil                    → 单上游：注入的回调就代表那个上游
//	取不到该 uid 的上游归属            → 接线问题（号不在池里），回落通用值
//	该上游没实现 ResetPolicyExt        → ok=false → 回落通用值
//	该上游上报了恢复排程               → 用它的时刻
//
// 通用值是 now+1h。选 1 小时而不是"次日某点"：那是个**通用**的保守值，
// 不假装知道任何上游的具体重置策略。上游上报了就以上游为准。
//
// # ⚠ 这里为什么先反查 provider（P2 修复的核心两行）
//
// 旧实现直接 `return h.cfg.NextResetAt()`，而旧回调**没有参数** ——
// 于是不管出错的号属于哪个上游，拿到的都是同一个（workbuddy 的次日 04:00）。
// codearts 的号因此被冷到一个对它毫无意义的时刻：它**没有签到恢复机制**，
// 04:00 既不是它的配额窗口边界，也不是它的任何事实。
//
// 归属由账号池回答（Pool.ProviderOf，只读 —— 池子按 provider 分域，
// 标签与默认值都在它手里）。出口层只按 ID 问，与其它四条多上游路径同构：
//
//	Chat / Credential / RefreshCredential / RefreshSkew → Provider 路由
//	ResetAt                                            → Provider 路由（本条）
//
// # 为什么 ProviderOf 失败时**不猜**成默认上游
//
// 号不在池里是接线问题，不是"它属于默认上游"。拿默认上游的排程去回答
// 一个没有归属的号，正是本 bug 的形态（用 A 的事实回答 B 的问题）。
// 回落通用值的方向是保守的：now+1h 比"次日 04:00"短得多，
// 最坏情况只是多撞一次硬错误，而不是白闲置一天。
//
// # 为什么单上游模式（Provider == nil）不走 ProviderOf
//
// 单上游没有"别的上游"概念，注入的回调（或它缺省时的 nil）就是那个上游的
// 全部事实 —— 与改造前逐字节一致。这也是既有部署与既有测试的路径。
func (h *Handler) nextResetAt(uid string) time.Time {
	if h.cfg.Provider != nil {
		providerID, _ := h.cfg.Pool.ProviderOf(uid)
		if cred, ok := h.cfg.Provider.Credential(providerID, uid); ok {
			if until, has := h.cfg.Provider.ResetAt(providerID, cred); has {
				return until
			}
		}
		// 该上游没上报排程 → 通用保守值（不回落到注入的回调：
		// 那个回调是**单上游/默认上游**的事实，见上面的"不猜"）。
		return time.Now().Add(time.Hour)
	}
	if h.cfg.NextResetAt != nil {
		if until, ok := h.cfg.NextResetAt(""); ok {
			return until
		}
	}
	return time.Now().Add(time.Hour)
}

// classifyErr 按**选中账号所属上游**的错误分类器判定错误类别。
//
// # 三条路径（与 chatVia / refreshCredential / needsRefreshVia 同形状）
//
//	Provider == nil                   → upstream.Classify（单上游，逐字节不变）
//	Provider 在 && 上游实现了分类器   → 上游自己的分类（翻译成中立类型）
//	Provider 在 && 上游没实现分类器   → upstream.Classify（回落默认上游判据）
//
// # 后两条回落为什么是同一个分支，且为什么**必须**是 upstream.Classify
//
// 单上游模式没有"别的上游"概念，行为要与改造前一致 —— 这正是
// `upstream.Classify`。
//
// 而"该上游没实现 ErrorClassifier"的正确解释**不是**"用通用猜测"，
// 而是"用默认上游的判据"：默认上游就是 workbuddy，它的分类判据
// （hardMarkers / sessionDeadMarkers）就住在 upstream.Classify 里。
// 换句话说，回落它不是妥协，而是**默认上游的分类器本身**。
//
// ⚠ 刻意**不**回落到 `gateway.DefaultErrorKind`（只按状态码的通用兜底）：
// 那会丢掉 workbuddy 的两个正文判据 ——
//
//	`{"code":12153,...}` 在 200/401 上不再被判成 session dead
//	（TestChatSessionDeadDisables 会立刻变红）
//	`{"msg":"余额不足"}` 在 400 上不再触发硬冷却
//	（TestChatRotatesOnHardCredit 会立刻变红）
//
// 也就是说：通用兜底会**静默削弱已有判据**。它不是回落目标，
// 只是 gateway 为"将来某个全新的、既没实现分类器又不想沿用任何默认判据
// 的上游"准备的显式选项 —— 当前没有任何上游走它。
func (h *Handler) classifyErr(providerID string, status int, body []byte) gateway.ErrorKind {
	if h.cfg.Provider != nil {
		if kind, ok := h.cfg.Provider.Classify(providerID, status, string(body)); ok {
			return kind
		}
	}
	// 回落：默认上游（workbuddy）的判据 —— 也正是改造前的行为。
	//
	// ⚠ 这里**只**翻译类型（upstream.ErrKind → gateway.ErrorKind），
	// 不改变任何判据：`upstream.Classify` 的返回值与改造前逐字相同。
	return upstreamToGateway(upstream.Classify(status, string(body)))
}

// upstreamErrKindMirror 是 upstream.ErrKind → gateway.ErrorKind 的逐项镜像。
//
// # 为什么 core 也需要这张表（而不是只让上游翻译）
//
// 回落分支拿到的仍然是 `upstream.ErrKind`（那是 upstream.Classify 的签名，
// 不归本次改动管 —— 改它会把 workbuddy 的客户端契约也一起动）。
// 要把它的结果变成 applyErrorPolicy 能吃的中立类型，就必须翻译一次。
//
// ⚠ 这张表与 `internal/workbuddy` 的 `toGatewayKind` **语义相同但方向相反**
// （一个翻译 ErrKind→中立，一个中立→ErrKind）。两份都必须存在，因为
// core **不得** import 任何具体上游（架构判据 3，arch_test.go 强制）——
// 它不可能是"复用 workbuddy 的那一份"。
//
// 风险（枚举漂移）由两侧的测试共同钉住：
//
//	internal/server  的 TestUpstreamKindMirrorCoversEveryKind
//	internal/workbuddy 的 TestToGatewayKindCoversEveryUpstreamKind
func upstreamToGateway(k upstream.ErrKind) gateway.ErrorKind {
	switch k {
	case upstream.ErrHardCredit:
		return gateway.ErrKindHardCredit
	case upstream.ErrSoftRate:
		return gateway.ErrKindSoftRate
	case upstream.ErrSessionDead:
		return gateway.ErrKindSessionDead
	case upstream.ErrNotFound:
		return gateway.ErrKindNotFound
	case upstream.ErrServer:
		return gateway.ErrKindServer
	case upstream.ErrClient:
		return gateway.ErrKindClient
	case upstream.ErrContentBlocked:
		// ⚠ 这一条**必须**在回落分支里存在（单上游部署走的正是这里）。
		//
		// 漏掉它的后果不是"降级不生效"那么轻：内容拦截会被翻成
		// gateway.ErrKindNone，而 None 在 core 侧是"只换号不罚" ——
		// 于是单上游部署下，一次内容拦截会白烧 MaxRotate 次往返，
		// 最后把"内容被拦"误报成"所有账号不可用"。
		//
		// ⚠ 这个文件里**有两张** upstream.ErrKind → gateway.ErrorKind 的表：
		// 本函数（参与策略判断）与 upstreamKindOf（只用于日志）。
		// 新增 upstream 常量时**两张都要改**，只改一张不会编译失败，
		// 也不会有测试红 —— 除非 upstream_kind_mirror_test.go 的两条 guard 在。
		// 我第一版正是只改了 upstreamKindOf，被那两个 guard 与
		// degrade_test.go 的端到端用例一起抓出来。
		return gateway.ErrKindContentBlocked
	default:
		return gateway.ErrKindNone
	}
}

// upstreamKindOf 把中立类型翻回 upstream.ErrKind。
//
// # 唯一的用途：拼错误消息
//
// 出站循环的 lastErr 是 `*upstream.Error`，它的 Kind 会被 `Error()`
// 打成 `"upstream session_dead (http 401): ..."` 这样的文本，
// 出现在最终 503 的 body 里。那个文本**不参与任何策略判断**。
//
// 为什么要保留它而不是把 lastErr 整个换掉：`*upstream.Error` 还被
// **续期分支**使用（`errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead`
// → Pool.Disable）—— 那是刷新路径上的独立判据，不在本次改动范围内，
// 必须原样保留。
//
// ⚠ 未知值回落到 `upstream.ErrNone`（打出来是 "none"）而不是某个惩罚性分类：
// 它只影响日志文本。
func upstreamKindOf(k gateway.ErrorKind) upstream.ErrKind {
	switch k {
	case gateway.ErrKindHardCredit:
		return upstream.ErrHardCredit
	case gateway.ErrKindSoftRate:
		return upstream.ErrSoftRate
	case gateway.ErrKindSessionDead:
		return upstream.ErrSessionDead
	case gateway.ErrKindNotFound:
		return upstream.ErrNotFound
	case gateway.ErrKindServer:
		return upstream.ErrServer
	case gateway.ErrKindClient:
		return upstream.ErrClient
	case gateway.ErrKindContentBlocked:
		// 内容策略拦截有独立的 upstream 分类（workbuddy 会产出它）。
		// 日志里打成 "upstream content_blocked (http 400): blocked by security policy"，
		// 让"内容被拦"与"客户端参数写错"在日志里可区分 —— 两者的处置完全不同。
		return upstream.ErrContentBlocked
	default:
		// 含 gateway.ErrKindAuth 与 ErrKindNone。
		//
		// ⚠ ErrKindAuth 刻意落 `upstream.ErrNone`（"none"）而不是 ErrClient：
		// upstream.ErrKind 里**没有** auth 这一档，硬塞一个会让日志暗示
		// "codearts 的 401 是 client 错误"，那是错的归因。
		// 而它只影响文本 —— 策略早已由 applyErrorPolicy(kind) 决定。
		return upstream.ErrNone
	}
}

// softRateReset 问**该上游自己**"这次软限流有没有精确的重置时刻"。
//
// # 两条路径（与 classifyErr 同形状，但回落策略刻意不同）
//
//	Provider == nil（单上游）        → upstream.ParseSoftRateReset（逐字节旧行为 + 收窄）
//	Provider 在 && 上游实现了扩展点  → 该上游自己解析
//	Provider 在 && 上游没实现        → ok=false（保持账号级软冷却）
//
// # ⚠ 为什么第三条**刻意不**回落到默认上游的解析器
//
// 这与 `classifyErr` 的取舍**相反**，是刻意的：
//
//	分类器回落（classifyErr）→ 保守方向：把不认识的错误判成"只换号不罚"
//	本函数回落              → **激进**方向：判成"模型级 + 已知重置时刻"
//	                          → 账号被收窄冷却**并被豁免**
//
// 拿 workbuddy 的 6004 判据去解析 codearts 的错误体，最坏情况是把
// codearts 的一次普通限流误判成"模型级且已给出重置时刻"：
// 冷却被错误收窄 + 该模型被豁免 → 账号过早回到候选集继续撞限流。
//
// 那是 P2 那类"用 A 的事实回答 B 的问题"，且后果比漏判更重。
// 所以多上游场景下，**没实现就保持旧行为**（账号级软冷却，永不更差）。
func (h *Handler) softRateReset(providerID string, status int, body []byte) (time.Time, bool) {
	if h.cfg.Provider != nil {
		return h.cfg.Provider.SoftRateReset(providerID, status, string(body))
	}
	// 单上游：默认上游（workbuddy）的解析器就是"那个上游的事实"。
	// status 必须显式限定为 429 —— 与 workbuddy.SoftRateReset 的两道闸门同口径。
	if status != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	return upstream.ParseSoftRateReset(string(body))
}

// chatVia 按**账号所属上游**把一次对话发出去，返回 (响应流, 状态码, 传输错误)。
//
// # 为什么必须分派（这是 861dee5 引入、预先存在的那条真 bug）
//
// 「账号池支持多上游」那次改动只改了**选号**（PickFor 按 provider 过滤），
// 没有改**出站**——出站仍然恒用 `cfg.Upstream`。于是：
//
//	PickFor("codearts", ...) 选出 codearts 账号   ← 对
//	cfg.Upstream.ChatStream(codearts 账号, body)  ← 用 workbuddy 的客户端发
//
// codearts 的签名（SDK-HMAC-SHA256 + DPoP）、路径前缀（/api/v2）、
// 通道头（maas_type）全在它自己的 Client.ChatStream 里。用 workbuddy 的
// 客户端发等于把这些全跳过，必然失败。
//
// 出口层**无法**自己选对实现：它只认 ID（不认识任何具体上游，见包注释）。
// 「ID → 能发请求的那个实例」是装配层的事实，所以这条路走 cfg.Provider。
//
// # 三条返回路径的形状（与改造前的四元组对齐）
//
//	Provider == nil           → cfg.Upstream 的原始四元组（逐字节回退）
//	Provider 在 && 取到凭证   → gateway.ChatStream 的 {Status, Body}
//	Provider 在 && 取不到凭证 → 传输错误（换号，不喂熔断）
//
// 第三条刻意复用 terr 而不是新造一种失败：它的语义与"网络抖动"在调用方
// 看来完全一致 —— 这个号现在发不出请求，换下一个，**不要**惩罚它
// （取不到凭证是接线问题，不是这个账号的错；这一点与本次修的 bug 同源：
// 改造前正是"账号被无关的失败惩罚"）。
//
// 关于 status>=400 时 Body 的语义：gateway.ChatStream 把非 2xx 的
// 上游错误体**也放在 Body 里**（见 gateway.Provider.Chat 的注释），
// 所以调用方要在分支里 io.ReadAll 它才能喂 Classify。这与
// upstream.Client.ChatStream 返回的 []byte respBody 是同一份数据，
// 只是形态从 []byte 变成了流。
func (h *Handler) chatVia(ctx context.Context, providerID string, acct *auth.Auth, body []byte) (io.ReadCloser, int, error) {
	if h.cfg.Provider == nil {
		// 单上游模式：没有任何多上游概念，行为与改造前**逐字节一致**。
		//
		// upstream.Client.ChatStream 在非 2xx 时把错误体放在**第三个返回值**里、
		// rc 为 nil；而调用方那段 status>=400 的分支是按 gateway.ChatStream 的
		// 契约写的（错误体在 Body 流里）。这里必须把 respBody **原样**包成流，
		// 而不是丢掉它。
		//
		// ⚠ 这一条是**真踩过的**：第一版回退路径写成 `_` 丢掉 respBody、
		// 返回一个空流。后果是 `upstream.Classify(status, "")` 永远拿不到
		// 上游的错误正文 —— 而 session dead 的判据正是正文里的 **12153**
		// （见 upstream.Classify）。于是 TestChatSessionDeadDisables 变红：
		// 401 不再被分类成 ErrSessionDead，账号不再被禁用。
		//
		// 换句话说：回退路径的"逐字节一致"不是修辞，丢掉一个 []byte 就足以
		// 让一条既有契约静默失效。测试抓住了它（这正是那些测试存在的理由）。
		rc, status, respBody, err := h.cfg.Upstream.ChatStreamWithIP(acct, body, gateway.ClientIPFrom(ctx))
		if err == nil && status >= 400 && rc == nil {
			return io.NopCloser(bytes.NewReader(respBody)), status, nil
		}
		return rc, status, err
	}

	// 多上游：拿到这个用户 id 对应的、该上游能用的凭证。
	// 出口层**不读 Secret**（那是上游的事实），只把装配层组装好的整份传下去。
	cred, ok := h.cfg.Provider.Credential(providerID, acct.UID)
	if !ok {
		return nil, 0, errNoProviderCredential
	}
	cs, registered, err := h.cfg.Provider.Chat(ctx, providerID, cred, body)
	if !registered {
		// 该上游没接上（未注册 / 实例缺失）：与"取不到凭证"同类 —— 接线问题，
		// 走传输错误分支换号，**不惩罚这个账号**。
		return nil, 0, errNoProviderCredential
	}
	if err != nil {
		// Provider.Chat 的契约：只有**传输层**失败才返回 error；
		// 非 2xx 属于业务错误，走 (ChatStream, nil)。
		return nil, 0, err
	}
	if cs.Body == nil {
		// 既没有 error 又没有 body：Provider 违约。
		// 造一个**有状态码**的空流，让调用方按 status 走分类而不是 panic
		// （契约测试会挡住这种情况，但生产路径不能依赖测试跑过）。
		return io.NopCloser(strings.NewReader("")), cs.Status, nil
	}
	return cs.Body, cs.Status, nil
}

// refreshCredential 按**账号所属上游**续期凭证。
//
// # 修的是什么
//
// 改造前：`h.cfg.Upstream.RefreshToken(acct)` —— 恒用 workbuddy 的客户端。
// 选中 codearts 账号时它会读到一份它不认识的凭证 → "no refreshToken"
// → 上层把**这个无辜的 codearts 账号**标记失败并冷却 → 池子耗尽 → 503。
// 实测：一次请求让 codearts 账号的 err_total +3。
//
// # 分派链条
//
//	cfg.Provider.RefreshCredential(ctx, id, cred)
//	  → 装配层按 id 取 Provider 实例
//	  → gateway.ExtOf[CredentialRefresher](pv)  ← 上游自报"怎么刷新我的"
//	  → workbuddy / codearts 各自的实现
//
// 出口层不认识任何具体上游，也不认识 gateway 之外的续期协议。
//
// # 三种"不刷新"为什么要区分
//
//	Provider == nil        → 回落 cfg.Upstream（单上游，行为不变）
//	取不到凭证             → 报错换号（接线问题，不能假装成功）
//	ok=false 且 err==nil   → **跳过刷新**，继续用现有凭证发请求
//
// 第三种是「该上游没实现 CredentialRefresher」，语义是**这个上游的凭证
// 不需要刷新**（例如纯 API Key 的上游）。把它当成失败会让这类上游
// 每次请求都白换一次号 —— 那正是"合法实现被通用假设误伤"。
func (h *Handler) refreshCredential(ctx context.Context, providerID string, acct *auth.Auth) error {
	if h.cfg.Provider == nil {
		// 单上游模式：逐字节回退（含落盘与失败日志，与改造前一致）。
		if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
			h.noteRefreshFailure(acct.UID)
			return err
		}
		if err := acct.SaveAtomic(); err != nil {
			// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
			log.Printf("chat refresh uid=%s: save auth failed: %v", acct.UID, err)
		}
		return nil
	}

	cred, ok := h.cfg.Provider.Credential(providerID, acct.UID)
	if !ok {
		return errNoProviderCredential
	}
	refreshed, err := h.cfg.Provider.RefreshCredential(ctx, providerID, cred)
	if err != nil {
		h.noteRefreshFailure(acct.UID)
		return err
	}
	if !refreshed {
		// 该上游没有续期实现 = 它的凭证不需要刷新。落盘也归它自己管
		// （我们连它要不要落盘都不知道，不该替它决定）。
		return nil
	}
	return nil
}

// noteRefreshFailure 记录一次凭证续期失败；连续失败达上限则禁用（A2 移植）。
//
// # 为什么在刷新路径上计数
//
// refresh token 连续失效基本是"已死"（被踢/过期），反复重试只会每次请求
// 都多付一次失败往返。达到上限直接禁用，让对账/重新登录接管。
func (h *Handler) noteRefreshFailure(uid string) {
	if h.cfg.Pool == nil {
		return
	}
	if h.cfg.Pool.NoteRefreshFailure(uid) {
		log.Printf("chat refresh uid=%s: 凭证续期连续失败达上限，已禁用（需重新登录）", uid)
	}
}

// needsRefreshVia 问**上游自己**"这个号现在要不要刷"。
//
// # 为什么核心不能自己拿 h.cfg.RefreshSkew 判（这是出站修复的漏网）
//
// 改造后 `refreshCredential` 已经把"**怎么**刷"交给了上游，但"**要不要**刷"
// 还留在核心：出站循环先做 `acct.NeedsRefresh(h.cfg.RefreshSkew)`（默认 10m），
// 只有它为真才会走到上游的续期实现。而"多早算该刷"同样是**上游的事实**：
//
//	workbuddy → access token 寿命以小时计，10m 窗口合理
//	codearts  → STS 凭证只有约 2h 寿命，它自己的 refreshSkew 是 3m
//
// 一个上游的 skew 被当成所有上游的 skew，后果是**两个方向的错**：
//
//	token 剩 8m（codearts 视角）→ 核心的 10m 判 true → 进续期分支 →
//	  codearts 的 CredentialRefresher 里**没有**自己的 skew 检查（它只
//	  断言+转发到 Client.RefreshToken），于是真的去消费 refresh_token ——
//	  而它是**一次性**的（用一次即作废，见 codearts/jobs.go 的包注释）。
//	  为省一次 401 往返烧掉一个凭证，这正是"核心越俎代庖"最贵的形态。
//
//	token 剩 4m 且某上游的窗口比 10m 更宽 → 核心判 false → 永不续期 → 必然 401。
//
// 所以判据必须和"怎么刷"待在同一个地方：上游。核心只问，不做判断。
//
// # 三条回落路径
//
//	Provider == nil        → 单上游：核心的 skew 就是那个上游的事实
//	取不到凭证             → 接线问题（与选号无关），不刷，出站会立刻报错换号
//	上游未上报窗口         → 回落到核心的通用兜底（明确的保守值）
//
// # 与"取不到凭证"为什么返回 false 而不是报错
//
// 本函数只回答"要不要刷"，不负责报告接线问题。取不到凭证时不刷，
// 紧接着的 `chatVia` 会在同一次迭代里拿到同一个 errNoProviderCredential
// 并按传输错误换号（**不惩罚账号**），语义与改造前一致。
// 在这里提前报错反而会让两条路径的错误处理分叉。
func (h *Handler) needsRefreshVia(providerID string, acct *auth.Auth) bool {
	if h.cfg.Provider == nil {
		// 单上游：没有别的上游可问，核心的窗口就是这个上游的事实。
		return acct.NeedsRefresh(h.cfg.RefreshSkew)
	}
	cred, ok := h.cfg.Provider.Credential(providerID, acct.UID)
	if !ok {
		// 取不到凭证 = 接线问题，不是"该刷了"。出站会报错换号。
		return false
	}
	skew, has := h.cfg.Provider.RefreshSkew(providerID, cred)
	if !has {
		// 上游只说了"怎么刷"，没说"多早刷" → 用核心的通用兜底。
		// 这是**明确的**保守值，不是"核心假装知道上游的寿命"。
		return acct.NeedsRefresh(h.cfg.RefreshSkew)
	}
	if skew <= 0 {
		// 上游明确声明"不需要提前刷"（只在 401 后被动续期）。
		// ⚠ 必须尊重它，不能用通用窗口覆盖 —— 那正是本函数要修的那类错误。
		return false
	}
	return acct.NeedsRefresh(skew)
}

func (h *Handler) applyErrorPolicy(uid string, kind gateway.ErrorKind) {
	switch kind {
	case gateway.ErrKindHardCredit:
		// 402 + 额度耗尽关键词：冷却到**上游给出的下次重置时刻**，立即换号。
		//
		// 早先这里直接调 pool.CooldownUntilTomorrow4AM —— 把 workbuddy 的
		// 「次日 04:00 等签到恢复」写进了出口层。现在改为向 Provider 要时刻：
		// 出口层只问"这个号什么时候能再用"，具体策略由上游定义
		// （见 Config.NextResetAt）。
		//
		// ⚠ 传 uid 而不是"当前请求的上游"：这一行**只拿得到出错的号**，
		// 而"这个号属于哪个上游"是账号池的事实 —— 由 nextResetAt 反查
		// （Pool.ProviderOf）。早先它无从反查，于是所有上游共用一个时刻。
		h.cfg.Pool.CooldownUntilNextReset(uid, h.nextResetAt(uid), "额度不足")
	case gateway.ErrKindSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case gateway.ErrKindSessionDead:
		// ⚠ 只有 workbuddy 会产生这个分类（它的 401 + 12153）。
		// codearts 的凭证失效是 ErrKindAuth，走 default 的"只换号不罚"——
		// 见本函数上方的长注释（危害 ② 的另一半）。
		//
		// # 为什么不是一次就 Disable（借鉴 workbuddy2api-panel）
		//
		// 12153 的成因里有一大类是**瞬时抖动**（网络闪断、上游瞬时故障、
		// token 刷新竞态）。改造前一次就永久禁用，于是一次抖动
		// 就能让一个健康的号掉出池子，而界面上只显示"已禁用"，
		// 没有任何线索指向真因。
		//
		// 现在交给 Pool.NoteSessionDead 计数门控：连续 3 次才禁用，
		// 任何一次成功都清零。真正失效的 session（每次都会 12153）
		// 仍然会被停掉，行为与改造前一致。
		h.cfg.Pool.NoteSessionDead(uid)
	case gateway.ErrKindNotFound:
		// 404 短冷却（软冷却），防雪崩。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
	case gateway.ErrKindServer:
		// 5xx 上游故障：分类器已把 ≥500 判为 server，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	default:
		// 其余（ErrKindAuth/ErrKindClient/ErrKindNone）：只换号不罚（防雪崩），不喂熔断。
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
