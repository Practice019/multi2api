// Package admin 管理台后端：账号增删、模型目录、日志与统计。
//
// # Task 3c 之后的职责边界
//
// 本包只剩**上游无关**的通用端点：
//
//	/admin/accounts*   /admin/login/*   /admin/logs*   /admin/settings
//	/admin/stats       /admin/models/*  /healthz       /ui
//
// 平台特殊端点（workbuddy 的签到/成长/旅行/本机登录 22 条）**不在本包**：
// 它们由上游通过 gateway.AdminExt.AdminRoutes() 自注册，
// 本包在 New 里遍历已注册 Provider 挂载（见 mountUpstreamRoutes）。
// **加新上游时本文件零改动。**
//
// 安全模型（方案①）：整个 /admin/* 子树只接受 loopback 直连，非本机一律 403。
// 理由：这些接口能改账号池、能触发上游请求、能读到账号昵称与积分，
// 而浏览器从别的设备打开时无法自动携带 Bearer，与其做半吊子鉴权，不如直接关在门外。
// 需要远程操作时请走 SSH 隧道。
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/oauth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// Config 管理台依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	OAuth    *oauth.Client
	Log      *checkinlog.Log
	Ring     *logbuf.Ring
	AuthDir  string
	// AuthsBase 各上游凭证目录的**父目录**（= 配置里 auth_dir 的原值）。
	//
	// 与 AuthDir 的关系：`AuthDir = AuthsBase/<默认上游>`。
	// 迁移期凭证可能还在 AuthsBase 根下，兼容扫描（LoadDirCompat）
	// 需要它才能两处都看。
	//
	// 空串时回落 AuthDir（单上游旧形态，行为不变）。
	AuthsBase string

	// Registry 已注册的上游。本包遍历它，把每个上游通过 AdminExt
	// 声明的管理端点挂上来，并把通过 SettingsExt 声明的设置项合并进设置页。
	// nil = 不挂任何上游端点（测试路径）。
	//
	// 用 *gateway.Registry 而不是 []gateway.Provider：注册表是所有核心包
	// 共用的同一份事实来源，传列表会让"谁注册了什么"出现第二个来源。
	Registry *gateway.Registry

	// SettingsExts 上游设置扩展的**适配器**（由 cmd/server 注入）。
	//
	// # 为什么不直接从 Registry 里断言 SettingsExt
	//
	// 因为 workbuddy 的设置项用的是它自己的 SettingField 类型
	// （上游包不得 import admin，所以类型必须各声明一份），
	// 而 Go 的方法集是精确匹配的：[]workbuddy.SettingField ≠ []admin.SettingField，
	// 断言必然失败 —— 而且是**静默**失败（设置页少几个键，没有任何报错）。
	//
	// 所以这条线显式走适配器：cmd/server 把上游的 Fields() 逐字段转换后传进来。
	// AdminExt / JobExt / LoginFlow 不需要这一层，因为它们的接口全部定义在
	// gateway 里（只有一种类型，上游直接实现即可）。
	SettingsExts []SettingsExt

	// ResetModelsCache 清空模型目录缓存（由 server 包注入，避免 admin 反向依赖 server）。
	ResetModelsCache func()
	// ModelCatalog 取模型目录快照（成本系数来源）。nil = 未接线，统计里报 unavailable。
	//
	// 由 cmd/server 注入 server.(*Handler).ModelCatalog —— 它内部按 1h TTL / 5min 失败
	// 负缓存惰性回源上游。这里只当「取数函数」用，缓存与重试策略全部留在 server 包。
	ModelCatalog func() *upstream.ModelCatalog
	// ModelCatalogState 只读的目录缓存状态（ok/stale/unavailable）。nil = 未接线。
	// 与 ModelCatalog 分开注入是刻意的：状态查询**绝不能**触发上游请求，
	// 而取目录会。把两者混成一个函数，就没法在 /admin/stats 里安全地只要状态。
	ModelCatalogState func() ModelCatalogState
	// Settings 设置页的读写契约（由 cmd/server 实现并注入；nil = 关闭设置页）。
	Settings SettingsStore
	// BuildTime 进程启动时间，UI 用来算运行时长。
	StartedAt time.Time

	// Scheduler 核心调度器的只读视图，供 /admin/schedule 使用。
	//
	// # 为什么这两条端点属于核心（而不是某个上游）
	//
	// `/admin/schedule`（签到/保活的启停、时点、下次唤醒）与
	// `/admin/task`（全量任务槽状态）**不表达任何上游身份** ——
	// 它们读的是核心自己排的班。
	//
	// Task 3c 曾把它们随其它 20 条一起放进 workbuddy（理由：写方
	// checkin/keepalive 在那，读写不宜分家）。阶段 0 评审指出这会**真出问题**：
	// 第二个上游若不声明 CapCheckin，这两条路由就**没人服务**，
	// 而它们本是全上游通用的。所以移回核心。
	//
	// nil = 未接线（测试路径），返回"未接线"降级。
	Scheduler SchedulerView
	// TaskSlot 全量任务槽，供 /admin/task 使用。nil = 返回空快照。
	TaskSlot TaskSlot

	// DefaultProvider 缺省上游标识（裸模型名走它）。
	//
	// 用于 /admin/providers 标出哪个是默认 —— 前端据此把它的模型
	// 以**裸名**展示（其余上游带前缀）。空串 = 没有默认。
	DefaultProvider string

	// ServiceName 网关身份标识，经 /admin/ui/manifest 下发给控制台。
	//
	// # 为什么要注入而不是在这里定义
	//
	// 真实来源是 server.ServiceName（/healthz 的 service 字段与 X-Service 头
	// 用的同一个常量）。admin 不得 import server（架构约束：server 依赖 admin，
	// 反向依赖会成环），所以由 cmd/server 把值传进来。
	//
	// 空串时 manifest 里 service 为空，前端退回显示 "gateway" ——
	// **不在这里兜默认值**：那样会让"忘了注入"这个装配错误永远无法被发现。
	ServiceName string

	// ReloadProvider `AuthDir` 里的凭证**属于哪个上游**。
	//
	// # 为什么必须显式配置（评审 F3）
	//
	// 早先 reload 调裸 `SyncToDir`，它归一成"当前默认上游"。那在本部署里
	// 碰巧正确 —— AuthDir 只装 workbuddy 凭证，而 workbuddy 恰好是第一个注册的。
	// 评审证明：一旦默认上游不是 workbuddy，同一个 reload 会
	// **扫描 workbuddy 文件却按别的域剔除**，把那个上游的账号全删掉并落盘。
	//
	// 所以域由配置给出。空串时回落 DefaultProvider（兼容旧装配），但会记日志 ——
	// **静默回落正是这个 bug 当初能藏住的原因**。
	ReloadProvider string
}

// SchedulerView 核心调度器在本包看来是什么样（只保留 /admin/schedule 读的字段）。
type SchedulerView interface {
	// NextWake 下一次唤醒时刻与将要执行的任务名。
	NextWake() (time.Time, []string)
	// Hours 签到时点与保活时点。
	Hours() (checkinHours, keepaliveHours []int)
	// CheckinEnabled 签到是否启用。
	CheckinEnabled() bool
	// KeepaliveEnabled 保活是否启用。
	KeepaliveEnabled() bool
}

// JobStatusView 调度器的**可选**扩展：暴露已注册任务的运行状态。
//
// # 为什么是可选接口而不是塞进 SchedulerView
//
// SchedulerView 是 /admin/schedule 的契约，那边只读"排的什么班"。
// 把任务状态塞进去会让所有实现 SchedulerView 的测试替身都必须实现它，
// 而绝大多数测试根本不关心任务状态。
//
// 可选接口 + 类型断言是**本仓库既有的模式**（见 gateway.ExtOf 的注释）：
// 核心按需发现能力，不强迫每个实现都写空方法。
type JobStatusView interface {
	// JobStatuses 各已注册任务的运行状态（按任务名稳定排序）。
	JobStatuses() []scheduler.Status
}

// TaskSlot 全量任务槽：同一时刻只允许一个全量任务。
//
// 与改造前 h.task 的行为逐字一致 —— 只是改由 cmd/server 注入，
// 因为任务槽的生命周期归属核心调度器，不属于某个上游。
type TaskSlot interface {
	// Snapshot 当前任务槽状态（与改造前 /admin/task 的响应体一致）。
	Snapshot() map[string]any
}

// Handler 管理台路由（/admin/ 子树）。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	// patterns 已注册的通用端点 pattern（"METHOD /path" 形式）。
	//
	// 为什么必须单独记一份而不是问 mux：Go 1.22 的 ServeMux 没有"列出已注册
	// pattern"的接口，也没有注册失败回滚。上游端点挂载前要判断冲突，
	// 就得有一个可枚举的来源 —— 而它必须由 register 统一维护，
	// 漏记一条就等于那条可以被上游静默覆盖。
	patterns []string

	// 统计聚合的短缓存：聚合要扫整个落盘文件，而前端按轮询节奏调用，
	// 缓存让「扫盘频率」与「轮询频率」解耦（见 statsCacheTTL）。
	statsMu     sync.Mutex
	statsCache  []logbuf.Entry
	statsSource string
	statsErr    error
	statsAt     time.Time
}

// New 构建管理台 handler。
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}

	h.register("GET /admin/accounts", h.accounts)
	h.register("POST /admin/accounts/reload", h.accountsReload)
	h.register("POST /admin/accounts/{uid}/enable", h.accountEnable)
	h.register("POST /admin/accounts/{uid}/disable", h.accountDisable)
	h.register("POST /admin/accounts/{uid}/cooldown/clear", h.accountClearCooldown)
	h.register("DELETE /admin/accounts/{uid}", h.accountDelete)

	h.register("POST /admin/login/start", h.loginStart)
	h.register("POST /admin/login/poll", h.loginPoll)

	h.register("POST /admin/models/refresh", h.modelsRefresh)
	h.register("GET /admin/models/preview", h.modelsPreview)

	h.register("GET /admin/logs", h.logs)
	h.register("GET /admin/logs/history", h.logsHistory)
	h.register("GET /admin/stats", h.stats)

	h.register("GET /admin/settings", h.settings)
	h.register("PUT /admin/settings", h.settingsUpdate)

	// 核心调度相关的两条只读端点。
	//
	// # 为什么它们在通用段（而不是由上游自注册）
	//
	// 它们读的是**核心自己排的班**（签到/保活时点、下次唤醒、全量任务槽），
	// 不表达任何上游身份 —— 任何上游都该有这两条。
	//
	// Task 3c 曾把它们随另外 20 条搬进 workbuddy；阶段 0 评审指出
	// 第二个上游若不声明 CapCheckin 就**没人服务**这两条路由，
	// 而它们本该对所有上游可见。移回核心。
	//
	// 放在 mountUpstreamRoutes 之前是刻意的：通用端点优先，
	// 上游即便声明同名路由也无法覆盖（见 mountUpstreamRoutes 的冲突规则）。
	h.register("GET /admin/schedule", h.schedule)
	h.register("GET /admin/task", h.taskStatus)

	// 已注册上游清单 + 能力位（Task 8）。
	//
	// 前端按它渲染：账号池按上游分组、入口按**能力位**显隐。
	// 能力位由后端下发而不是前端硬编码 —— 否则加第三个上游还要改前端，
	// 那正是判据 1（加新上游核心零改动）要避免的。
	h.register("GET /admin/providers", h.providers)

	// 控制台渲染契约（Task 1）。
	//
	// 它把「有哪些上游、各自有什么能力、各自声明了哪些管理端点、有哪些定时任务」
	// 一次下发 —— 前端据此渲染导航与面板，**不认识任何上游名字**。
	// 放在 mountUpstreamRoutes 之前：通用端点优先，上游不得覆盖它。
	h.register("GET /admin/ui/manifest", h.uiManifest)

	// 上游自注册的管理端点。放在最后：它**不得**覆盖上面的通用路由，
	// 所以冲突时以先注册的为准（见 mountUpstreamRoutes）。
	h.mountUpstreamRoutes()

	return h
}

// mountUpstreamRoutes 遍历已注册上游，挂载它们声明的管理端点。
//
// # 为什么冲突时保留**先注册的**（通用端点）
//
// 通用端点在前，上游端点在后。若某个上游声明了一条与通用端点同名的路由，
// 保留通用端点并记日志 —— 静默覆盖会让"某个面板突然变成另一个上游的实现"，
// 那是最难查的一类故障。宁可少挂一条并让日志说话。
//
// 实现上必须**先探测再注册**：Go 的 ServeMux 在重复注册时会 panic，
// 而 panic 发生在注册过程中，届时已经注册的 pattern 无法回滚。
// 探测的代价是注册两次同一 pattern —— 由 muxConflict 里的 recover 兜住，
// 它自己不会真的占用路由（用 HandleFunc 注册后再注册会 panic，
// 所以探测走的是一个**独立的一次性 mux**，见下）。
func (h *Handler) mountUpstreamRoutes() {
	if h.cfg.Registry == nil {
		return
	}
	// taken 记录已占用的 pattern（含探测失败的）。
	// 用独立集合判断而不是靠 mux 的 panic 回滚：mux 是 append-only 的。
	taken := map[string]bool{}
	for _, p := range h.cfg.Registry.All() {
		ext, ok := gateway.ExtOf[gateway.AdminExt](p)
		if !ok {
			continue
		}
		for _, r := range ext.AdminRoutes() {
			if r.Path == "" || r.Handler == nil {
				log.Printf("admin: 上游 %s 声明了一条不完整的管理端点（path=%q method=%q），已跳过",
					p.ID(), r.Path, r.Method)
				continue
			}
			pattern := r.Path
			if r.Method != "" {
				pattern = r.Method + " " + r.Path
			}
			if taken[pattern] || h.routeConflict(pattern) {
				log.Printf("admin: 上游 %s 的管理端点 %s 与已有端点冲突，保留已有端点", p.ID(), pattern)
				continue
			}
			// 先试注册：若与**已有** pattern 冲突，ServeMux 会 panic。
			// 这里用一个独立的一次性 mux 做同样的注册来判定，
			// 避免把半注册的状态留在真正的 mux 上。
			h.mux.HandleFunc(pattern, r.Handler)
			taken[pattern] = true
		}
	}
}

// routeConflict 报告 pattern 是否与已注册的某条路由冲突。
//
// 手法：在一个独立的一次性 ServeMux 上同时注册**全部既有 pattern** 与该
// pattern。既有 pattern 不可能互相冲突（它们已经成功注册过），
// 所以唯一的 panic 来源就是新来的这一条 —— 正是我们要判定的东西。
func (h *Handler) routeConflict(pattern string) (conflict bool) {
	defer func() {
		if rec := recover(); rec != nil {
			conflict = true
		}
	}()
	probe := http.NewServeMux()
	for _, existing := range h.patterns {
		probe.HandleFunc(existing, h.conflictProbe)
	}
	probe.HandleFunc(pattern, h.conflictProbe)
	return false
}

// conflictProbe 只在冲突探测里被临时注册，永远不会被真正调用。
func (h *Handler) conflictProbe(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "占位路由")
}

// register 注册一条通用端点，同时记录 pattern 供冲突探测使用。
//
// 所有通用端点都经它注册 —— 少记一条就会让那条被上游静默覆盖。
func (h *Handler) register(pattern string, fn http.HandlerFunc) {
	h.mux.HandleFunc(pattern, fn)
	h.patterns = append(h.patterns, pattern)
}

// ServeHTTP 先做本机校验，再进路由。
//
// 这里统一把请求体读完再回应：Go 的 http server 在 body 未被耗尽时会直接关闭连接
// 而非复用，客户端可能观察到 ECONNRESET（表现为「服务端日志成功、浏览器/脚本报连接重置」）。
// 不要求每个 handler 自己记得 drain，放在唯一入口一次做掉。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
	}()
	if !isLoopback(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "管理接口仅允许本机直连（RemoteAddr=" + r.RemoteAddr + "）。需要远程访问请使用 SSH 隧道。",
		})
		return
	}
	h.mux.ServeHTTP(w, r)
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// 账号视图
// ---------------------------------------------------------------------------

// AccountView 是 pool.Status 的管理台增强视图（补 token 有效期、凭证文件、今日签到）。
// ModelCatalogState 模型目录缓存的只读状态视图（由 server 包注入的实现填充）。
//
// 三态语义（ok / stale / unavailable）刻意不做成 bool：
// 「有目录但已过 TTL」和「压根没拿到目录」对读报表的人是两件事 ——
// 前者可以继续用（数值可能不准），后者必须显示成缺数据。
type ModelCatalogState struct {
	State     string `json:"state"`      // ok | stale | unavailable
	Models    int    `json:"models"`     // 目录内模型数
	Stale     bool   `json:"stale"`      // 数据存在但已超出 TTL
	Cooldown  bool   `json:"cooldown"`   // 正处于失败负缓存冷却期
	FetchedAt string `json:"fetched_at"` // 最近一次成功拉取时间（RFC3339；无则空）
}

// ModelMultiplier 单模型的成本系数视图（credit 的放大倍数，来自 /v3/config）。
type ModelMultiplier struct {
	Model      string  `json:"model"`
	Multiplier float64 `json:"multiplier"`
	// Calls 该模型在本次聚合窗口内的调用次数（来自 by_model），
	// 让前端能在同一条 chip 上同时说清「调了多少次」和「每次贵多少倍」。
	Calls int `json:"calls"`
}

// AccountView 是 pool.Status 的管理台增强视图（补 token 有效期、凭证文件、今日签到）。
type AccountView struct {
	pool.Status
	HasToken       bool   `json:"has_token"`
	TokenExpireAt  int64  `json:"token_expire_at,omitempty"`
	TokenExpireSec int64  `json:"token_expire_sec,omitempty"`
	File           string `json:"file,omitempty"`
	TodayCheckin   string `json:"today_checkin,omitempty"`
	TodayCheckinAt int64  `json:"today_checkin_at,omitempty"`
}

func (h *Handler) accountViews() []AccountView {
	list := h.cfg.Pool.List()
	today := checkinlog.TodayStart()
	out := make([]AccountView, 0, len(list))
	for _, st := range list {
		v := AccountView{Status: st}
		if a := h.cfg.Pool.AuthByUID(st.UID); a != nil {
			v.HasToken = a.AccessToken != ""
			v.TokenExpireAt = a.ExpiresAt
			if a.ExpiresAt > 0 {
				v.TokenExpireSec = a.ExpiresAt - time.Now().Unix()
			}
			if a.FilePath != "" {
				v.File = filepath.Base(a.FilePath)
			}
		}
		// 今日签到结果：从历史里取当天该 uid 的最近一条 checkin。
		if h.cfg.Log != nil {
			for _, rec := range h.cfg.Log.Since(today) {
				if rec.UID == st.UID && rec.Kind == checkinlog.KindCheckin {
					v.TodayCheckin = rec.Status
					v.TodayCheckinAt = rec.At.UnixMilli()
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.accountViews(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		"auth_dir":       h.cfg.AuthDir,
	})
}

// reloadProvider `AuthDir` 所属的上游标识。
//
// 配置了就用配置的；没配置回落默认上游，但**记一条日志** ——
// 静默回落正是"reload 按错误域剔除账号"这个 bug 当初能藏住的原因。
func (h *Handler) reloadProvider() string {
	if h.cfg.ReloadProvider != "" {
		return h.cfg.ReloadProvider
	}
	log.Printf("admin: 未配置 ReloadProvider，回落到默认上游 %q —— "+
		"多上游部署下应显式配置，否则可能按错误的域剔除账号", h.cfg.DefaultProvider)
	return h.cfg.DefaultProvider
}

// accountsReload 重新扫描 auths 目录并对齐池（手工拷入凭证后无需重启网关）。
//
// # ⚠ 必须显式指定**这个目录属于哪个上游**
//
// 早先这里调的是裸 `SyncToDir(auths)`，它归一成"**当前默认上游**"。
// 那个语义在本部署里"碰巧"正确 —— 因为 `h.cfg.AuthDir` 只装 workbuddy 凭证，
// 而 workbuddy 恰好是第一个注册的（于是也是默认）。
//
// 评审证明了这是个**潜在 bug**：一旦默认上游变成 codearts（改注册顺序、
// 或将来有第三个上游先注册），同一个 reload 调用会：
//
//	扫描的是 workbuddy 文件（`auth.LoadDir` 只 glob workbuddy*.json）
//	却按 codearts 的域去剔除 → **把 codearts 账号全部删掉并落盘**。
//
// 所以域必须由配置显式给出，不能依赖"谁是默认"。
// 缺省回落到 DefaultProvider 只为兼容旧装配（并会在日志里留痕）。
//
// # 请求体里的 provider：这条按钮终于有了它本该有的语义
//
// 上面那段只解决了"域不能被默认上游带跑偏"，但**域本身仍是配置里的
// 一个固定值**（`ReloadProvider` = workbuddy）。于是账号池页面上
// codearts 那行的「重载 auths」按钮**扫的一直是 workbuddy 目录**，
// 而 toast 却说「对齐 codearts」。
//
// 这不是"少个功能"，而是**这条按钮的存在意义被架空了** ——
// 它的用途正是"往 `auths/<provider>/` 手工拷入凭证后不用重启网关"
// （见 auth.UpstreamDir 的注释），而对 codearts 来说它**根本不工作**，
// 界面还说成功了。用户实测的回执是三条一模一样的 `{"provider":"workbuddy"}`，
// 连不存在的上游 `ghost` 都被默默接受。
//
// 所以要害不只是"读请求体"，而是**读不到时必须失败**（见下面的 404 分支）。
func (h *Handler) accountsReload(w http.ResponseWriter, r *http.Request) {
	// 宽容解析：空体 / 坏体都**不算错**。
	//
	// 老客户端（以及缓存里的旧页面）发的是 `{}` 或空体 ——
	// 解析失败不该让这条端点挂掉，那会把"重载"整个功能打没。
	// 与 accountDisable / loginPoll 同一范式。
	var body struct {
		Provider string `json:"provider"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// 没带 provider = 老调用方：行为**逐字不变**（配置回落）。
	// 这是向后兼容，不是重复实现 —— `reloadProvider()` 的语义一个字没动。
	provider := body.Provider
	if provider == "" {
		provider = h.reloadProvider()
	} else if _, ok := h.providerByID(provider); !ok {
		// ⚠ 显式传了 provider 却查不到 → **404**，绝不回落默认目录。
		//
		// 回落正是要修的那个 bug 形态：用户点 `ghost` 那行、
		// 或前端传了个拼错的上游名，界面会收到 200 + 一个别的上游的
		// scanned —— 看起来"重载成功"，实际动的是别人的池子。
		// **宁可失败得刺眼，也不要成功得可疑。**
		//
		// 404 而不是 400：这是"这个上游不存在"（资源问题），
		// 不是"provider 字段格式不对"（参数问题）。
		writeError(w, http.StatusNotFound, "上游不存在: "+provider)
		return
	}

	// 兼容读的根：`h.cfg.AuthDir` 现在是默认上游的子目录
	//（`auths/workbuddy/`），但迁移期凭证可能还在 `auths/` 根。
	base := h.cfg.AuthsBase
	if base == "" {
		base = h.cfg.AuthDir
	}

	// 目录：**上游自报优先**（与 pollViaFlow 的落盘目录同一条判据）。
	//
	// 目录是**上游的事实**（它知道自己从哪读凭证），核心不该猜。
	// 问的是 `p`（同一个上游的 Provider 面），不是 LoginFlow ——
	// `ExtOf` 只接受 Provider，而且"手工拷凭证再重载"这条路径
	// **根本不需要登录流程**（见 gateway.AuthDirExt 的注释）。
	//
	// 上游返回空串 = "我没有独立目录，用核心默认的"（单上游部署的旧形态）。
	dir := ""
	if p, ok := h.providerByID(provider); ok {
		if ext, ok2 := gateway.ExtOf[gateway.AuthDirExt](p); ok2 {
			dir = ext.AuthDir()
		}
	}
	if dir == "" {
		// 回落也走 auth.UpstreamDir —— **不自己拼 filepath.Join**。
		// "auths/<provider>/ 长什么样"这个判据只该写一份，
		// 抄第二份就会在将来改命名规则时漏掉一处。
		dir = auth.UpstreamDir(base, provider)
	}

	// ⚠ 扫描目录与"上游自报目录"的关系 —— 这里刻意做了取舍，写清楚：
	//
	// `LoadDirCompat(base, provider)` 内部**自己**算 `UpstreamDir(base, provider)`，
	// 也就是说它只认"`base/<provider>` + `base` 根"这两个位置。
	// 而上面从 `AuthDirExt` 拿到的 `dir` 是**上游自己说的**，
	// 多上游部署里它可能与 `base/<provider>` 不同（例如各上游配了独立的
	// auth_dir，或上游把凭证放在别处）。
	//
	// 两者打架时**以扫描结果的实际位置为准**，所以这里按 `dir` 反推 base：
	//
	//   dir == UpstreamDir(base, provider)  → 传 base，两个位置都扫（迁移期正确）
	//   dir != UpstreamDir(base, provider)  → 传 dir 自己的父目录语义：
	//                                        直接把 dir 当 base 传进去，
	//                                        LoadDirCompat 会算 dir/<provider>
	//                                        （通常不存在，于是只扫 dir 这一层）
	//
	// 选这个做法的理由：**优先保证"扫的就是上游说它会读的那个目录"**。
	// 迁移期兼容（根目录也扫）只在目录布局确实是 `base/<provider>` 时才成立 ——
	// 那正是它被设计出来的场景。若上游另立目录，再扫 `base` 根只会
	// **把别的上游的凭证也捞进来**（LoadDir 靠 `workbuddy*.json` 前缀过滤，
	// 但同一个上游的旧文件可能同时躺在两处 → 按 uid 去重后条数对不上，
	// 排障时看不出是"多扫了根目录"）。
	scanBase := base
	if sub := auth.UpstreamDir(base, provider); dir != sub {
		scanBase = dir
	}
	auths, err := auth.LoadDirCompat(scanBase, provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取 auths 目录失败: "+err.Error())
		return
	}

	// ⚠ 池子可能为 nil（本包其它地方都判了空，见 pollViaFlow 的注释）。
	// 漏判的后果是 **nil pointer panic（进程级）**。
	var before, after int
	if h.cfg.Pool != nil {
		before = len(h.cfg.Pool.List())
		h.cfg.Pool.SyncToDirFor(provider, auths)
		after = len(h.cfg.Pool.List())
	} else {
		log.Printf("admin: reload 扫描到 %d 个凭证，但没有账号池可同步", len(auths))
	}
	// 日志打 `dir=`（真正扫的目录）而不是 `h.cfg.AuthDir` ——
	// 按上游重载之后两者不再是同一个值，打配置值等于没打。
	log.Printf("admin: reload auths dir=%s provider=%s scanned=%d pool %d -> %d",
		dir, provider, len(auths), before, after)
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned": len(auths), "before": before, "after": after, "provider": provider,
		// dir 回执：排障时"我点的那个上游到底扫了哪个目录"一眼可见。
		// 前端不依赖它做逻辑，但它是这条端点最容易被问的问题的答案。
		"dir": dir,
	})
}

func (h *Handler) accountEnable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !h.cfg.Pool.Enable(uid) {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	log.Printf("admin: enable %s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": false})
}

func (h *Handler) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Reason == "" {
		body.Reason = "manual disable"
	}
	if _, ok := h.cfg.Pool.Status(uid); !ok {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	h.cfg.Pool.Disable(uid, body.Reason)
	log.Printf("admin: disable %s reason=%s", uid, body.Reason)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": true, "reason": body.Reason})
}

func (h *Handler) accountClearCooldown(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !h.cfg.Pool.ClearCooldown(uid) {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	log.Printf("admin: clear cooldown %s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "cleared": true})
}

// accountDelete 把账号移出池。默认只出池、保留 auths 文件；purge_file=1 才连文件一起删。
func (h *Handler) accountDelete(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	purge := r.URL.Query().Get("purge_file") == "1"

	var filePath string
	if a := h.cfg.Pool.AuthByUID(uid); a != nil {
		filePath = a.FilePath
	}
	if _, ok := h.cfg.Pool.Status(uid); !ok {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	h.cfg.Pool.Remove(uid)

	deleted := ""
	if purge && filePath != "" {
		// 只允许删 auths 目录内的文件：防止 FilePath 被构造成任意路径删除。
		absDir, _ := filepath.Abs(h.cfg.AuthDir)
		absFile, _ := filepath.Abs(filePath)
		if strings.HasPrefix(absFile, absDir+string(os.PathSeparator)) {
			if err := os.Remove(absFile); err != nil {
				log.Printf("admin: delete %s pool 移除成功但删文件失败: %v", uid, err)
			} else {
				deleted = absFile
			}
		} else {
			log.Printf("admin: delete %s 拒绝删目录外文件 %s", uid, absFile)
		}
	}
	log.Printf("admin: remove %s (purge_file=%v deleted=%q)", uid, purge, deleted)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "removed": true, "purged_file": deleted, "file": filePath,
	})
}

// ---------------------------------------------------------------------------
// 添加账号（OAuth 设备授权）
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 添加账号（按上游分派）
//
// # 为什么要按上游分派
//
// 添加账号是**上游专属**动作：workbuddy 是 OAuth 设备码，
// codearts 是 OAuth + DPoP（还要生成密钥对、签名请求、~30 分钟凭证）——
// 交互步骤数都不同（见 gateway.LoginFlow 的注释）。
// 前端"账号池"的每个上游分组行都有自己的「＋ 添加账号」，
// 点哪个上游就该走哪个上游的流程。
//
// # 向后兼容
//
// 请求体不带 `provider` 时，**完全走原路径**（`h.cfg.OAuth`，
// 也就是装配时注入的那个客户端）。现有前端与脚本不受影响。
//
// # 找不到该上游的 LoginFlow 时返回 501
//
// 不是 500 也不是静默回落 —— "这个上游不支持页内添加"是**能力问题**，
// 501 Not Implemented 是准确的语义。静默回落到默认上游更糟：
// 用户在 codearts 那行点「添加账号」，账号会加进 workbuddy。
// ---------------------------------------------------------------------------

func (h *Handler) loginStart(w http.ResponseWriter, r *http.Request) {
	// provider 可选：不带就走旧的硬接线路径（向后兼容）。
	var body struct {
		Provider string `json:"provider"`
	}
	// 解析失败不算错误 —— 老客户端发的可能是 `{}` 或空体。
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.Provider != "" {
		// 指定了 provider → 必须有它自己的 LoginFlow，否则明确 501。
		flow, ok, _ := h.wantsFlowDispatch(body.Provider)
		if !ok {
			writeError(w, http.StatusNotImplemented,
				"上游 "+body.Provider+" 不支持在页面内添加账号（未实现登录流程）")
			return
		}
		state, authURL, err := flow.Start()
		if err != nil {
			writeError(w, http.StatusBadGateway, "向上游申请授权链接失败: "+err.Error())
			return
		}
		log.Printf("admin: oauth start provider=%s state=%s", body.Provider, shortState(state))
		writeJSON(w, http.StatusOK, map[string]any{
			"state":    state,
			"auth_url": authURL,
			"provider": body.Provider,
			// 各上游的 state 有效期可能不同；LoginFlow 接口没暴露它，
			// 这里沿用核心的 oauth.StateTTL（前端只用它做倒计时提示）。
			"expires_in_sec": int64(oauth.StateTTL().Seconds()),
		})
		return
	}

	// 不带 provider：旧路径（向后兼容）。
	//
	// ⚠ 若没配 OAuth 客户端就明确报 503，**不能 panic** ——
	// 老客户端发的就是不带 provider 的请求，崩了整片功能都没了。
	if h.cfg.OAuth == nil {
		writeError(w, http.StatusServiceUnavailable,
			"未配置默认 OAuth 客户端；请指定 provider（该上游需实现登录流程）")
		return
	}
	state, authURL, err := h.cfg.OAuth.Start()
	if err != nil {
		writeError(w, http.StatusBadGateway, "向上游申请授权链接失败: "+err.Error())
		return
	}
	log.Printf("admin: oauth start state=%s", shortState(state))
	writeJSON(w, http.StatusOK, map[string]any{
		"state":          state,
		"auth_url":       authURL,
		"expires_in_sec": int64(oauth.StateTTL().Seconds()),
	})
}

// wantsFlowDispatch 判断这次请求是否应该走「上游自己的 LoginFlow」。
//
// # 这里曾经写错过（测试抓到的）
//
// 第一版是 `body.Provider != h.providerOfDefaultOAuth()`，而
// `providerOfDefaultOAuth()` 的两个分支**返回同一个值** —— 于是
// "provider == DefaultProvider" 时条件恒假，请求掉进旧路径，
// 而测试里没配 OAuth 客户端 → **panic**。
//
// 更要紧的是语义：**指定的上游实现了 LoginFlow 就该走它**，
// 与"它是不是默认上游"无关。默认上游同样可能（且应该）用自己的 flow。
// `h.cfg.OAuth` 只是**过渡期**的旧客户端，不该抢在 flow 前面。
//
// 所以判据只有一条：**该上游有没有实现 LoginFlow**。
//
//   - 指定了 provider 且它有 LoginFlow → 走 flow
//   - 指定了 provider 但它没有      → 501（明确失败，不回落）
//   - 没指定 provider               → 旧路径（向后兼容）
func (h *Handler) wantsFlowDispatch(providerID string) (gateway.LoginFlow, bool, bool) {
	// 返回 (flow, 有flow, 该上游存在)
	if providerID == "" {
		return nil, false, false
	}
	if h.cfg.Registry == nil {
		return nil, false, false
	}
	for _, p := range h.cfg.Registry.All() {
		if p.ID() != providerID {
			continue
		}
		// ⚠ 两道判据缺一不可：
		//   1. `ExtOf` —— 该上游把 Start/Poll 挂上了（类型层面的能力）
		//   2. `Configured` —— 这次部署**真的配了**登录客户端
		//
		// 只看第 1 条会让"没配 OAuth 的部署"也通过，前端显示按钮、
		// 点下去才报错（假按钮）。只实现了 Start/Poll 却返回
		// Configured=false 的上游应当被当作**不支持** ——
		// 与 schedule.go / uimanifest.go 里那两处判据逐字一致。
		fl, ok := gateway.ExtOf[gateway.LoginFlow](p)
		if !ok || !fl.Configured() {
			return nil, false, true
		}
		return fl, true, true
	}
	// 上游不存在：既没 flow 也没这个上游
	return nil, false, false
}

// providerByID 按上游标识在注册表里找一个上游。
//
// # 为什么不能只拿 LoginFlow（实测的编译期约束）
//
// `gateway.ExtOf[T]` 的签名是 `ExtOf[T any](p Provider)` —— 它吃的是
// **Provider**，不是任意接口值。而 `pollViaFlow` 过去只持有
// `flow gateway.LoginFlow`，于是想在那里问 `AuthDirExt` 时会直接编译失败：
//
//	cannot use flow (variable of interface type gateway.LoginFlow) as
//	gateway.Provider value in argument to ExtOf[AuthDirExt]:
//	gateway.LoginFlow does not implement gateway.Provider (missing method Caps)
//
// 这不是需要绕开的麻烦 —— 它恰好说明**问错对象了**：
// "我的凭证目录在哪"是 **Provider 这个实体**自报的事实，
// 而 `LoginFlow` 只是它身上挂着的一个交互能力。
// 把 `*Provider` 传下去才是对的（见 pollViaFlow 的注释）。
func (h *Handler) providerByID(providerID string) (gateway.Provider, bool) {
	if providerID == "" || h.cfg.Registry == nil {
		return nil, false
	}
	for _, p := range h.cfg.Registry.All() {
		if p.ID() == providerID {
			return p, true
		}
	}
	return nil, false
}

// shortState 日志里只打前 8 位 —— 完整的 state 是凭据的一部分，
// 不该整条进日志（本项目清理过一次真实密钥泄漏）。
func shortState(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func (h *Handler) loginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State    string `json:"state"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.State == "" {
		writeError(w, http.StatusBadRequest, "缺少 state")
		return
	}
	// provider 指定了且不是默认 → 走该上游自己的 LoginFlow。
	// 与 loginStart 同一条分派规则，避免"开始走 A、轮询走 B"。
	if body.Provider != "" {
		// 与 loginStart **同一条**判据：指定了上游就必须有它自己的 flow。
		// 两者不一致会产生"用 A 的 state 去问 B"的诡异现象。
		flow, ok, _ := h.wantsFlowDispatch(body.Provider)
		if !ok {
			writeError(w, http.StatusNotImplemented,
				"上游 "+body.Provider+" 不支持在页面内添加账号（未实现登录流程）")
			return
		}
		// 同一个上游的 Provider 面：落盘目录要问它（见 pollViaFlow 的注释）。
		// 这里**必定**找得到 —— 上一行已经从注册表里认出了该上游。
		p, pok := h.providerByID(body.Provider)
		if !pok {
			writeError(w, http.StatusNotFound, "上游不存在: "+body.Provider)
			return
		}
		if err := h.pollViaFlow(w, p, flow, body.State); err != nil {
			return
		}
		return
	}
	// 不带 provider：旧路径（向后兼容）。没配客户端时明确报错，不 panic。
	if h.cfg.OAuth == nil {
		writeError(w, http.StatusServiceUnavailable,
			"未配置默认 OAuth 客户端；请指定 provider（该上游需实现登录流程）")
		return
	}
	cred, err := h.cfg.OAuth.Poll(body.State)
	switch {
	case errors.Is(err, oauth.ErrPending):
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
		return
	case errors.Is(err, oauth.ErrStateUnknown):
		writeError(w, http.StatusGone, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "轮询失败: "+err.Error())
		return
	}

	path, err := cred.SaveToDir(h.cfg.AuthDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "凭证落盘失败: "+err.Error())
		return
	}
	// 落盘后重扫目录对齐池：新账号立即参与选号，无需重启。
	//
	// ⚠ 与 accountsReload 同一个坑：必须显式指定域。
	// 这里落盘的凭证进的是 `h.cfg.AuthDir`，那属于 ReloadProvider ——
	// 用裸 SyncToDir（=当前默认上游）在默认上游不是它时会**误删**。
	//
	// 兼容读：迁移期凭证可能还在 `auths/` 根，只读子目录会看不到。
	base := h.cfg.AuthsBase
	if base == "" {
		base = h.cfg.AuthDir
	}
	auths, lerr := auth.LoadDirCompat(base, h.reloadProvider())
	if lerr != nil {
		writeError(w, http.StatusInternalServerError, "凭证已写入但重扫目录失败: "+lerr.Error())
		return
	}
	// ⚠ 池子可能为 nil（同 pollViaFlow / accountsReload 的注释）。
	// 漏判的后果是 nil pointer panic。
	if h.cfg.Pool != nil {
		h.cfg.Pool.SyncToDirFor(h.reloadProvider(), auths)
	} else {
		log.Printf("admin: oauth 凭证已落盘，但没有账号池可同步（uid=%s）", cred.UID)
	}
	log.Printf("admin: oauth 成功 uid=%s nick=%s file=%s", cred.UID, cred.Nickname, filepath.Base(path))
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"uid":           cred.UID,
		"nickname":      cred.Nickname,
		"enterprise_id": cred.EnterpriseID,
		"domain":        cred.Domain,
		"expires_in":    cred.ExpiresIn,
		"file":          filepath.Base(path),
	})
}

// pollViaFlow 用某个上游自己的 LoginFlow 轮询，并把拿到的凭证落盘、对齐池。
//
// # 为什么不能沿用旧路径的收尾
//
// 旧路径直接 `cred.SaveToDir(...)`（`*oauth.Credential` 的方法），
// 它自带宽 workbuddy auth 文件格式的知识（`MarshalAuthFile` 决定文件名与内容）。
// 而 `gateway.LoginFlow.Poll` 返回**通用** `gateway.Credential`，
// 它的 `Secret` 是 `any` —— 各上游结构完全不同（见该类型注释）。
//
// # 本函数要求 Secret 实现 authFileWriter
//
// 落盘这件事只有上游自己知道怎么做（文件名规则、字段形状、
// codearts 还要写 DPoP 私钥）。所以定义一个**窄接口**：
//
//	type authFileWriter interface {
//	    MarshalAuthFile() (name string, raw []byte, err error)
//	}
//
// 与 `*oauth.Credential` 已有的方法**同名同签名** —— 所以 workbuddy
// 的适配器只要把 `*oauth.Credential` 放进来就自动满足，零转换代码。
//
// Secret 不满足时**501 并说明原因**，而不是猜字段映射：
// 把未知结构"尽力映射"会产生看起来成功、实际字段错位的凭证文件 ——
// 那比明确失败糟得多（本项目反复的教训）。
//
// # 为什么要多收一个 p（Provider）
//
// 落盘目录必须问 `AuthDirExt`，而 `gateway.ExtOf` 只接受 `Provider`
// （见 providerByID 的注释）。`flow` 与 `p` 是**同一个上游**的两个面：
// `flow` 负责"怎么拿到凭证"，`p` 负责"凭证该落在哪"。
// 由调用方（loginPoll，从注册表里遍历出来的那个 p）一并传入，
// 而不是在这里反查 —— 反查要多一次遍历，且容易传进不一致的两个上游。
func (h *Handler) pollViaFlow(w http.ResponseWriter, p gateway.Provider, flow gateway.LoginFlow, state string) error {
	cred, err := flow.Poll(state)
	switch {
	case errors.Is(err, gateway.ErrLoginPending):
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
		return errHandled
	case err != nil:
		// LoginFlow 的契约只有 ErrLoginPending 一个哨兵，
		// 无法区分"state 未知"与"轮询失败"，统一 502 并带上游原文。
		writeError(w, http.StatusBadGateway, "轮询失败: "+err.Error())
		return errHandled
	}

	mw, ok := cred.Secret.(authFileWriter)
	if !ok {
		writeError(w, http.StatusNotImplemented,
			"该上游的凭证结构尚未接入落盘"+
				"（LoginFlow 的 Secret 需要实现 MarshalAuthFile）")
		return errHandled
	}
	name, raw, merr := mw.MarshalAuthFile()
	if merr != nil {
		writeError(w, http.StatusInternalServerError, "凭证序列化失败: "+merr.Error())
		return errHandled
	}
	// ⚠ 落盘目录用**上游自报的**，不是 h.cfg.AuthDir。
	//
	// 我第一版用的是 h.cfg.AuthDir —— 那是**默认上游（workbuddy）的目录**。
	// 于是 codearts 授权成功后凭证被往 workbuddy 的 `./auths` 写。
	//
	// 按上游分子目录之后（`auths/workbuddy/`、`auths/codearts/`），
	// 目录是**上游的事实**（它知道自己从哪读凭证），核心不该猜。
	//
	// ⚠ 目录从 `AuthDirExt` 问，**不是**从 `flow` 问 ——
	// 它原来挂在 `LoginFlow` 上，那等于"不实现登录流程的上游连
	// 自己的凭证目录都答不出来"，而按上游重载 auths **不需要登录流程**
	//（手工拷凭证是常见路径）。见 gateway.AuthDirExt 的注释。
	//
	// 问的是 p（同一个上游的 Provider 面）：`ExtOf` 只接受 Provider。
	// 上游返回空串 = "用核心默认目录"（单上游部署的旧形态）。
	dir := ""
	if ext, ok := gateway.ExtOf[gateway.AuthDirExt](p); ok {
		dir = ext.AuthDir()
	}
	if dir == "" {
		dir = h.cfg.AuthDir
	}
	path, werr := writeAuthFile(dir, name, raw)
	if werr != nil {
		writeError(w, http.StatusInternalServerError, "凭证落盘失败: "+werr.Error())
		return errHandled
	}
	// 重扫**该上游的**目录。
	//
	// ⚠ 与落盘目录必须一致 —— 扫错目录会得到"写成功但池子里没有"
	//（用户看到账号没进池，而日志说成功）。
	//
	// 用 LoadDirCompat：迁移期凭证可能还在旧的根目录位置，
	// 只读子目录会看到"账号池突然空了"。
	auths, lerr := auth.LoadDirCompat(h.cfg.AuthDir, cred.Provider)
	if lerr != nil {
		writeError(w, http.StatusInternalServerError, "凭证已写入但重扫目录失败: "+lerr.Error())
		return errHandled
	}
	// ⚠ 池子可能为 nil —— 本包其它地方（schedule.go / uimanifest.go）
	// 都判了空，说明"Pool 可缺省"是**本包自己的设计假设**；
	// 这条落盘路径原来漏判了，后果是 **nil pointer panic（进程级）**。
	//
	// 实测抓到：我为此写的测试在 Pool=nil 时直接 panic 在
	// `pool.(*Pool).SyncToDirFor` 里。
	//
	// 凭证**已经落盘**（那一步不依赖池子），所以这里只是"没能即时进池"——
	// 告诉调用方重启即可，而不是 500 让它以为凭证写失败了。
	if h.cfg.Pool != nil {
		h.cfg.Pool.SyncToDirFor(cred.Provider, auths)
	} else {
		log.Printf("admin: oauth(flow) 凭证已落盘，但没有账号池可同步（provider=%s uid=%s）",
			cred.Provider, cred.UID)
	}
	log.Printf("admin: oauth(flow) 成功 provider=%s uid=%s file=%s dir=%s",
		cred.Provider, cred.UID, filepath.Base(path), dir)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"uid":      cred.UID,
		"nickname": cred.Nickname,
		"provider": cred.Provider,
		"file":     filepath.Base(path),
	})
	return errHandled
}

// authFileWriter 上游凭证"能自己序列化成 auth 文件"的窄接口。
//
// 与 `*oauth.Credential` 已有的 `MarshalAuthFile` 同名同签名 ——
// 所以那个类型天然满足，不需要包装。
type authFileWriter interface {
	MarshalAuthFile() (name string, raw []byte, err error)
}

// writeAuthFile 原子写一个凭证文件。
//
// 单独抽出来是为了**与旧路径用同一套写盘规则**（0600 权限、先写 .tmp
// 再 rename）。旧路径在 `oauth.Credential.SaveToDir` 里直接把这几行写在
// 自己内部；这里复刻同样语义，保证两条路径产出的文件权限与原子性一致。
//
// 不共用那个方法的原因：它在 oauth 包，签名绑着 *Credential；
// 为了共用而让 admin 依赖 oauth 的具体类型，会把"通用分派"又焊回单一上游。
func writeAuthFile(dir, name string, raw []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// errHandled 表示"响应已经写过了" —— 调用方据此直接 return，
// 不要再写第二次（重复 WriteHeader 会打日志且状态码以第一次为准）。
var errHandled = errors.New("响应已写")

// ---------------------------------------------------------------------------
// 模型目录 / 日志 / 统计
// ---------------------------------------------------------------------------

func (h *Handler) modelsRefresh(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ResetModelsCache == nil {
		writeError(w, http.StatusNotImplemented, "模型缓存刷新未接线")
		return
	}
	h.cfg.ResetModelsCache()
	log.Printf("admin: 模型目录缓存已清空")
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// modelsPreview 返回**全部目录模型**的 id 与成本倍率，供前端在模型名称旁标注倍率。
//
// 为什么不把倍率塞进 /v1/models：
//   - 那是**对外**的 OpenAI 兼容端点。客户端按规范解析 `data[]`，
//     塞自定义字段属于污染公共契约（部分客户端会对未知字段告警，甚至有严格模式直接报错）。
//   - 倍率是网关自己的观测信息，只对本地管理界面有意义 —— 该走 admin 面。
//
// 与 /admin/stats 里的 model_multipliers 的分工（两者都保留）：
//   - stats 那份只含**窗口内被调用过**的模型 —— 报表口径，回答"钱花在哪"
//   - 这份是**全部目录模型** —— 选择口径，回答"这个模型多贵"，下拉框需要给所有选项标注
//
// 降级：未接线或目录拿不到时返回**空数组**而不是报错 ——
// 前端拿不到倍率就只显示模型名，不该让整个模型列表渲染失败。
func (h *Handler) modelsPreview(w http.ResponseWriter, r *http.Request) {
	out := []ModelMultiplier{}
	if h.cfg.ModelCatalog != nil {
		if cat := h.cfg.ModelCatalog(); cat != nil {
			for _, m := range cat.Models {
				if m.ID == "" {
					continue
				}
				// 与 stats 那份不同：这里**保留 0 倍率**。
				// x0.00 是"免费"这个有意义的事实，不是"没有数据"。
				out = append(out, ModelMultiplier{Model: m.ID, Multiplier: m.Multiplier})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "cursor": 0})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	items, cursor := h.cfg.Ring.Snapshot(since)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    items,
		"cursor":   cursor,
		"capacity": h.cfg.Ring.Cap(),
		"held":     h.cfg.Ring.Len(),
	})
}

// logsHistory 从落盘文件读历史请求日志（进程重启后仍可回溯）。
//
// 支持 offset/limit 分页，契约与 /admin/checkin/history 一致：
// 时间倒序（最新在前），返回过滤后总数，前端用同一个分页组件接两个数据源。
func (h *Handler) logsHistory(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil || h.cfg.Ring.Sink() == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "enabled": false})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = logbuf.DefaultPageSize
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	items, total, err := h.cfg.Ring.Sink().Page(offset, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取日志文件失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   items,
		"total":   total,
		"offset":  offset,
		"limit":   limit,
		"enabled": true,
		"file":    h.requestLogStats(),
	})
}

// statsCacheTTL 统计聚合结果的缓存时长。
// 聚合要扫整个落盘文件，而前端是轮询调用；缓存让「扫描频率」与「轮询频率」解耦。
const statsCacheTTL = 5 * time.Second

// stats 汇总请求日志，给出「这个网关到底跑了多少、跑得怎么样」的只读视图。
//
// 数据源优先级：**落盘日志** > 内存环形缓冲。
// 历史实现只读内存缓冲（上限 2000 条），于是重启后立刻显示「暂无请求」，
// 且窗口被限制在「进程启动至今」——那既不是全部历史，也不是用户以为的统计范围。
// 现在改为聚合落盘文件，重启后依然覆盖全部保留期内的历史；
// 落盘不可用（未启用/读失败）时回落到内存缓冲，并在响应里说明当前范围。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil {
		writeJSON(w, http.StatusOK, map[string]any{"total": 0, "source": "none"})
		return
	}

	items, source, err := h.statsItems()
	resp := aggregateChatLog(items)
	resp["source"] = source
	resp["capacity"] = h.cfg.Ring.Cap()
	resp["held"] = h.cfg.Ring.Len()
	// aggregated 显式说明「本次聚合了多少条」，与 total 语义不同：
	//   total      窗口内命中某种条件的条数（aggregateChatLog 里就是 len(items)）
	//   aggregated 实际参与聚合的条数
	// 正常情况两者相等。分开暴露是为了让「显示的窗口 ≠ 实际聚合窗口」这类
	// 静默偏差可见 —— 曾经 statsItems 用 Page(0,1<<20) 想取全量却被分页上界
	// 夹到 300，界面仍显示「全部落盘历史，共 300 条」，看不出是截断。
	resp["aggregated"] = len(items)
	if err != nil {
		// 读盘失败时把原因带上，避免用户对着「总数变小」猜原因。
		resp["error"] = "读取落盘日志失败，已回落到内存缓冲: " + err.Error()
	}
	if f := h.requestLogStats(); f != nil {
		resp["file"] = f
		// 自检：聚合条数与文件总条数不一致时明确标注，不静默。
		if n, ok := f["count"].(int); ok && source == "file" && n != len(items) {
			resp["truncated"] = true
			resp["error"] = fmt.Sprintf(
				"聚合窗口不完整：文件 %d 条，本次只聚合了 %d 条", n, len(items))
		}
	}
	// 成本系数与目录状态（B1.2）：放在同一个响应里而不是新开端点。
	//
	// 为什么不新开 /admin/models/catalog：
	//   - 前端渲染「调用统计」时**同时**需要 by_model（调用次数）与 multiplier（每次多贵），
	//     拆成两个端点会让同一次渲染出现「系数到了但次数还没到」的中间态，
	//     还会多一次轮询往返（仪表盘本来就是 5s 轮询）；
	//   - 两个数据源的生命周期不同（日志聚合 5s 缓存 vs 目录 1h TTL），
	//     但**读口径统一**：都是「展示现状」，没有写语义，合并不引入权限/一致性问题；
	//   - 失败降级是局部的：目录拿不到只让 model_catalog.state=unavailable，
	//     其余统计字段照常返回（不会被 5xx 连坐）。
	// 真正需要独立端点的是「刷新目录」这种**写**动作 —— 那已经由 /admin/models/refresh 承担。
	// 顺序很重要：**先**取倍率（可能触发一次惰性回源），**再**读状态。
	// 反过来会让首次响应自相矛盾 —— 倍率已经拿到了，state 却还报 unavailable。
	//
	// ⚠ 已知代价（评审指出，见代码注释中的 HIGH-2）：ModelCatalog() 可能同步阻塞
	// 至多 MaxRotate 次上游请求。若不接受，正确做法是把回源挪到后台 goroutine、
	// 本接口只读缓存 —— 那会改变「首次点击需要等一次拉取」的现行行为，
	// 是产品取舍而非 bug 修复，故留待专门决定（详见 tasks/board.md）。
	resp["model_multipliers"] = h.modelMultipliers(resp)
	resp["model_catalog"] = h.modelCatalogState()
	writeJSON(w, http.StatusOK, resp)
}

// modelCatalogState 取目录缓存状态；未接线或实现返回零值时统一归到 unavailable。
//
// 与 modelMultipliers 分开是因为调用时机不同：状态每次统计都要报（哪怕系数表没取到），
// 而系数表要按 by_model 过滤，且允许整体缺失。
func (h *Handler) modelCatalogState() ModelCatalogState {
	if h.cfg.ModelCatalogState == nil {
		return ModelCatalogState{State: "unavailable"}
	}
	st := h.cfg.ModelCatalogState()
	if st.State == "" {
		// 实现方漏填状态：按最保守的语义处理，不假装有数据。
		st.State = "unavailable"
	}
	return st
}

// modelMultipliers 返回**在本次统计窗口里真的被调用过**的模型的成本系数。
//
// 为什么要按 by_model 过滤，而不是把整个目录倒出来：
// 目录有 30 个模型，而一个网关上通常只跑其中几个；把没调用过的模型也塞进响应，
// 前端就得自己判断「哪些 chip 该显示」——而那正是这份数据存在的理由。
// 过滤后「有 chip = 有调用」，前端不需要第二套判断。
//
// 只输出**窗口里出现过**且**目录里有系数**的模型：报表要回答的是"钱花在哪"，
// 不是"目录里有什么"。
//
// 触发上游的边界（三条），都在 ModelCatalog 内部自限：
//
//  1. 窗口里没有任何 by_model 时直接跳过 —— 没调过模型就没什么可归因的；
//  2. 单测/未接线（nil）时直接返回空切片；
//  3. 其余情况交给 ModelCatalog()，它自带 1h TTL + 5min 失败负缓存，
//     自己决定要不要真的回源。
//
// ⚠ **不要**在这里按 state.State == "unavailable" 提前返回。
// 曾经这么写过，导致功能无法自举 —— 自锁死循环：
//
//	state 在缓存为空时返回 "unavailable"
//	→ 这里提前返回，从不调用 ModelCatalog()
//	→ 缓存永远为空 → state 永远 unavailable
//
// 冷启动时目录**永远不会**被拉取。而当时的测试全把 state 桩成 "ok"，
// 唯一直接调 ModelCatalog() 的 e2e 又绕开了 /admin/stats，所以全绿但功能是死的
// （由独立评审发现，回归测试见 catalog_bootstrap_test.go）。
//
// 正确做法：把"要不要回源"的决策权交给 ModelCatalog 自己 ——
// 它才知道自己是不是在冷却期、缓存是否过期。状态只用于**展示**，不用于决策。
func (h *Handler) modelMultipliers(resp map[string]any) []ModelMultiplier {
	out := []ModelMultiplier{}
	byModel, ok := resp["by_model"].(map[string]int)
	if !ok || len(byModel) == 0 || h.cfg.ModelCatalog == nil {
		return out
	}
	cat := h.cfg.ModelCatalog()
	if cat == nil {
		return out
	}
	// 按模型名排序输出：map 遍历顺序随机，固定顺序让响应可 diff（与目录排序同一动机）。
	names := make([]string, 0, len(byModel))
	for id := range byModel {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		m, ok := cat.Multiplier(id)
		if !ok {
			// 目录里没有这个模型（或系数为 0）：不输出条目，而不是输出 0 ——
			// 0 系数在前端会显示成「免费」，而真相是「不知道」。
			continue
		}
		out = append(out, ModelMultiplier{Model: id, Multiplier: m, Calls: byModel[id]})
	}
	return out
}

// statsItems 取出用于聚合的条目及其来源标识。
// 带 TTL 缓存：聚合是 O(文件行数)，而调用方是轮询。
func (h *Handler) statsItems() ([]logbuf.Entry, string, error) {
	h.statsMu.Lock()
	if !h.statsAt.IsZero() && time.Since(h.statsAt) < statsCacheTTL {
		items, err := h.statsCache, h.statsErr
		h.statsMu.Unlock()
		if err != nil {
			return items, "memory", err
		}
		return items, h.statsSource, nil
	}
	h.statsMu.Unlock()

	var (
		items  []logbuf.Entry
		source = "memory"
		err    error
	)
	if sink := h.cfg.Ring.Sink(); sink != nil {
		// 用 LoadAll 而不是 Page(0, sinkCountAll)：
		// Page 会按 MaxPageSize 夹紧 limit，而它分页上界是给 HTTP 调用方设的。
		// 曾经这里传 1<<20 想取全量，被静默夹到 300 —— 一份 1855 行的日志
		// 只聚合了最近 300 条，界面上的成功率/平均 TTFB 全是那 300 条的。
		// 内部聚合走旁路，不受分页上界约束（见 logbuf.Sink.LoadAll 的注释）。
		if all, e := sink.LoadAll(); e != nil {
			err = e
		} else if len(all) > 0 {
			items, source = all, "file"
		} else {
			source = "file" // 文件存在但为空：如实说是文件来源，而不是内存
		}
	}
	if source == "memory" {
		items, _ = h.cfg.Ring.Snapshot(0)
	}

	h.statsMu.Lock()
	h.statsCache, h.statsSource, h.statsErr, h.statsAt = items, source, err, time.Now()
	h.statsMu.Unlock()
	return items, source, err
}

// cacheHitRate 缓存命中率 = 命中 / (命中 + 未命中)。
//
// 分母为 0 时返回 (0, false)：调用方据此决定「输出 0」还是「不输出该字段」。
// 之所以必须显式挡掉 0/0，是因为 IEEE-754 下 float64(0)/float64(0) = NaN，
// NaN 经 encoding/json 序列化成 JSON 字面量 `null`（Marshal 对 NaN/Inf 返回
// 不支持值错误，writeJSON 里 `raw, _ :=` 把错误吞掉后会写出一个空 body）——
// 前端拿到 null 再做算术就是 NaN，页面上会出现 "NaN%"。
// 宁可少一个字段，也不要让「没有缓存数据」伪装成一个数值。
func cacheHitRate(hit, miss int64) (float64, bool) {
	den := hit + miss
	if den <= 0 {
		return 0, false
	}
	return float64(hit) / float64(den), true
}

// aggregateChatLog 对条目做纯聚合（无 IO），便于单测直接覆盖算术。
//
// 新增的 usage 派生字段（Credit/ThinkTokens/CacheHitTokens/CacheMissTokens）
// 一律只在 >0 时累加：旧格式行没有这些 JSON 键，反序列化后是 Go 零值，
// 天然贡献 0；-0.0 / 负数这类病态值（理论上解析层已挡掉）也和 0 等价，
// 不会污染其它聚合量（它们各走各的累加器，唯一的交汇点是 cache_hit_rate 的分母，
// 而那里对 <=0 有显式兜底）。
// providerStat 单个上游在聚合窗口内的表现。
//
// # 为什么要按上游分组（Task 7）
//
// one-api 社区的实证教训："先把观测建好"。多上游之后，若统计不带 provider 维度，
// 两个上游的调用次数与消耗混在一个数里，面板**无法归因** ——
// 看不出成本花在哪个上游、哪个上游在报错。
//
// 字段刻意保持最小：调用次数 / 成功 / 失败 / 累计消耗。
// 各自的模型分布已经由 by_model 覆盖（模型名本身带前缀即可区分上游）。
type providerStat struct {
	Calls  int64   `json:"calls"`
	OK     int64   `json:"ok"`
	Fail   int64   `json:"fail"`
	Credit float64 `json:"credit"`
}

// unlabeledProvider 无 provider 标记的行（历史日志）归入这一桶。
//
// 为什么不丢弃它们：旧日志文件里的行没有 provider 键，反序列化得空串。
// 丢弃会让"历史调用了多少次"这个数**变小**，是数据失真；
// 归到一个明确的桶里既保住了总数，又如实说明"这些行不知道是哪个上游"。
const unlabeledProvider = "(未标注)"

// aggregateChatLog 把日志行聚合成 /admin/stats 的响应。
func aggregateChatLog(items []logbuf.Entry) map[string]any {
	byModel := map[string]int{}
	byStatus := map[string]int{}
	byUID := map[string]int{}
	byProvider := map[string]*providerStat{}
	var okN, failN, tokens int64
	var ttfbSum, totalSum int64
	var ttfbN int64
	var credit float64
	var thinkTokens, cacheHitTokens, cacheMissTokens int64
	var oldest, newest time.Time

	for _, e := range items {
		byModel[e.Model]++
		byStatus[strconv.Itoa(e.Status)]++
		if e.UID != "" {
			byUID[e.UID]++
		}

		// 按上游累计。空 provider = 历史行 → 归入"未标注"桶（不丢弃）。
		pk := e.Provider
		if pk == "" {
			pk = unlabeledProvider
		}
		ps, exists := byProvider[pk]
		if !exists {
			ps = &providerStat{}
			byProvider[pk] = ps
		}
		ps.Calls++

		switch {
		case e.Status >= 200 && e.Status < 300:
			okN++
			ps.OK++
		case e.Status >= 400:
			failN++
			ps.Fail++
		}
		if e.Tokens > 0 {
			tokens += int64(e.Tokens)
		}
		// Credit 用 >0 而不是 !=0：负数/负零属于上游异常数据，
		// 让它进入累加会把「累计消耗」变成负值，比丢弃它更难解释。
		if e.Credit > 0 {
			credit += e.Credit
			ps.Credit += e.Credit
		}
		if e.ThinkTokens > 0 {
			thinkTokens += int64(e.ThinkTokens)
		}
		if e.CacheHitTokens > 0 {
			cacheHitTokens += int64(e.CacheHitTokens)
		}
		if e.CacheMissTokens > 0 {
			cacheMissTokens += int64(e.CacheMissTokens)
		}
		totalSum += e.TotalMS
		if e.TTFBMS > 0 {
			ttfbSum += e.TTFBMS
			ttfbN++
		}
		if oldest.IsZero() || e.At.Before(oldest) {
			oldest = e.At
		}
		if e.At.After(newest) {
			newest = e.At
		}
	}

	resp := map[string]any{
		"total":             len(items),
		"ok":                okN,
		"fail":              failN,
		"by_model":          byModel,
		"by_status":         byStatus,
		"by_uid":            byUID,
		"tokens":            tokens,
		"credit_total":      credit,
		"think_tokens":      thinkTokens,
		"cache_hit_tokens":  cacheHitTokens,
		"cache_miss_tokens": cacheMissTokens,
	}
	// cache_hit_rate 只在分母 >0 时输出（空集、全为旧格式行、或只有 miss 也为 0 时都不输出）：
	// 前端据此显示「—」而不是一个 0%，避免把「没有数据」读成「命中率真的是 0」。
	if r, ok := cacheHitRate(cacheHitTokens, cacheMissTokens); ok {
		resp["cache_hit_rate"] = r
	}
	// by_provider 同样只在有数据时输出（与 cache_hit_rate 一致的约定）：
	// 空集时给一个空对象会让前端把"没有数据"渲染成"所有上游都是 0"。
	// 单上游部署下它仍会有一个桶 —— 那样前端不必分两种形态渲染。
	if len(byProvider) > 0 {
		out := make(map[string]providerStat, len(byProvider))
		for k, v := range byProvider {
			out[k] = *v
		}
		resp["by_provider"] = out
	}
	if len(items) > 0 {
		resp["avg_total_ms"] = totalSum / int64(len(items))
	}
	if ttfbN > 0 {
		resp["avg_ttfb_ms"] = ttfbSum / ttfbN
	}
	if !oldest.IsZero() {
		resp["window_from"] = oldest
		resp["window_to"] = newest
	}
	return resp
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
