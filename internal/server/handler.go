// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + 转发到上游。
//
// # 本包**不依赖任何具体上游**（架构判据 3）
//
// 它认识的是：
//
//	gateway.Provider      契约（经 cfg.Provider / ProviderRouter 注入）
//	DefaultUpstream       默认上游客户端（窄接口，见其注释）
//	DefaultClassifier     默认上游的错误分类器（函数值）
//	wire.*                中立线协议工具（SSE 读写、usage 抽取、限流解析）
//
// 它**不认识** workbuddy / codearts / … 中的任何一个包。
// 这条边界由 arch_test.go 的 TestCoreDoesNotDependOnUpstreams 强制。
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
	"workbuddy2api/internal/channelstore"

	"workbuddy2api/internal/account"
	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/wire"
)

// serverRetryExtra 上游 5xx（服务端故障）时额外允许的换号次数（MaxRotate 之上）。
//
// # 为什么需要它（用户实测报障）
//
// 讯飞 Loomy 网关抖动时短暂返 502，而默认 MaxRotate=3：一次用户请求试完
// 3 个号就回 503「all accounts unavailable」—— 池里 11 个号其实全是健康的。
// 用户看到的是"账号都不可用"，真相是"上游抖了几秒"（实测：同刻直连上游 200）。
//
// # 为什么是 2
//
// 抖动通常持续数秒到数十秒，而每次换号是一次完整往返；多试 2 个号足以跨过
// 短抖动，同时不会在一次请求里把整个池子的熔断计数都喂一遍（那会让
// "上游抖"演变成"全池冷却"，比一次失败更糟）。
const serverRetryExtra = 2

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream DefaultUpstream
	APIKey   string // 空 = 不鉴权
	// AdminToken 管理台门禁令牌（与 APIKey **完全独立**）。
	//
	// # 为什么必须独立（用户要求：门禁 token 不能和 api 一样）
	//
	// 改造前 /admin/* 的门禁凭据就是 APIKey，也就是对话接口那把 ——
	// 于是"给某个客户端配一把 key 调对话"同时把整个管理台交了出去。
	// 两件事的风险等级完全不同：对话 key 泄露 = 别人白用额度；
	// 管理令牌泄露 = 别人接管网关。详见 admin 包的 adminTokenMatches。
	//
	// 空 = 没有远程管理面（只有"本机免令牌"那条路，且要求监听回环）。
	AdminToken string
	// ListensLoopback 本进程是否**只**监听回环地址。
	//
	// 它是「本机免令牌」的前提之一（见 webui.go 的 localExempt）：
	// 绑 0.0.0.0 时"本机"没有意义（端口公开），豁免必须失效。
	ListensLoopback bool
	// DefaultClassifier 默认上游的错误分类器（装配层注入）。
	//
	// # 为什么由装配层注入而不是核心自己调
	//
	// 分类判据（`hardMarkers` / `sessionDeadMarkers` / `12153`）是
	// **workbuddy 的上游知识**。核心包不得认识任何具体上游
	//（arch_test 判据 3 强制），所以核心只收一个函数值。
	//
	// 单上游部署（`Provider == nil`）与"该上游没实现 ErrorClassifier"
	// 两种情形都回落到它 —— 语义上就是"用默认上游的判据"，
	// 与改造前逐字节一致。
	//
	// nil 时回落 `gateway.DefaultErrorKind`（只按状态码）—— 那是**明确的降级**。
	DefaultClassifier func(status int, body string) gateway.ErrorKind
	// APIKeys 多 API key 管理（对标 new-api 令牌体系）：config.api_key 是
	// 管理钥匙（不限额），普通 key 走 apikey.Store 的额度/限速/用量。
	// nil = 不启用多 key 管理（旧行为）。
	APIKeys *apikey.Store
	// ChannelProvider 由**组合根**（cmd/server）注入：给 (渠道id, BaseURL) 造一个 gateway.Provider。
	//
	// ⚠ 为什么是工厂而不是核心直接 import 具体上游：核心包不得依赖上游包
	// （架构守卫 TestCoreDoesNotDependOnUpstreams）。有了它，"再加一种 Key 型上游"
	// 只需在组合根换一个函数，核心一行都不用改。
	ChannelProvider func(id, baseURL string) gateway.Provider
	// Channels 渠道存储（中转站方向）。nil = 对外目录仍旧走上游目录（既有行为）。
	Channels  *channelstore.Store
	MaxRotate int // 单请求最多换号次数，默认 3

	// AccountAuth 控制台登录端点（注册/登录/登出/读会话），由装配层注入。
	//
	// 挂在 `/admin/auth/`，即管理台门禁**之外** —— 理由见 New() 里挂载处的注释。
	// nil = 不启用账号体系（旧行为：只有 admin_token 一条路）。
	AccountAuth http.Handler

	// Accounts / Sessions 控制台账号与会话（`/ui` 判断"这次请求是谁"）。
	//
	// # 为什么 server 包也要这两个（与 admin 包各持一份是**故意**的）
	//
	// 两处问的是同一个问题、答案也必须一致，但判据的实现只有**一份**：
	// account.Identify（见 internal/account/identify.go）。这里传的是数据源，
	// 不是判据 —— 判据若在两处各写一遍就会漂移，而漂移的形态是
	// "页面能打开但每个标签页 401"或反过来"页面打不开但接口敞开"。
	//
	// nil = 不启用账号体系：`/ui` 未认证时只显示登录页，登录页会因为端点
	// 不存在而报错（部署没接线时这是**设计**：宁可见到登录页也不要一个
	// 静默放行的控制台）。
	Accounts *account.Store
	Sessions *account.SessionStore

	// ⚠ 2026-10-19 用户新口径：原先的 `RegisterOpen func() bool` 字段**已删除**。
	//
	// 它由装配层按 config 的 `console.disables_register` 提供，决定登录页要不要
	// 显示注册入口。单账号口径下"能不能建号"只有一个答案（账号库空不空），
	// 而那个答案 `webui.go` 的 loginBootstrap() 已经查得到 —— 两个来源必然
	// 有一天互相矛盾（配置说开、库里已有账号 ⇒ 页面给出一个点了 403 的入口）。
	//
	// 配置里的 `console.disables_register` 仍然被解析（不动既有配置文件），
	// 但不再影响门禁与登录页；cmd/server 启动日志会如实说明（不静默忽略）。

	// CookieSecure 会话/令牌 cookie 的 Secure 策略（account.CookieSecureAuto
	// / Always / Never）。空串按 Auto。
	CookieSecure string

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

	// ProviderAlias 上游 ID -> 对外代号（用户要求：不希望别人知道上游）。
	//
	// # 为什么需要（实测 5 处泄露）
	//
	//	1) /v1/models 的 owned_by      -> workbuddy-intl
	//	2) /v1/models 的 id 前缀        -> workbuddy-intl/deepseek-v4.1-flash
	//	3) 响应的 model 字段            -> monkeycode-basic/deepseek-flash
	//	4) 响应的 id 前缀               -> chatcmpl-... / 0217927502...
	//	5) usage 私有字段               -> points_consumed（loomy 独有）
	//
	// 3)4)5) 是上游原始响应透传出来的，同样是身份指纹：
	// 各调一次就能按响应格式把通道分出组来。
	//
	// # nil = 不隐藏（向后兼容）
	//
	// 既有部署行为逐字节不变。配了表之后**所有**上游都走代号，
	// 漏配的用 channel-<hash> 兜底（不能回落成真名，见 aliasFor）。
	ProviderAlias map[string]string

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

	// DisableChatTools 关闭「上游工具自动注入」。
	//
	// 默认 false（开启）—— 用户要求「我要生图的话，它自动调用生图模型」，
	// 而且希望**客户端零改动**（普通聊天客户端不懂工具协议）。
	//
	// 关掉它的场景：某个客户端虽然不带 tools，但对响应形状极敏感
	// （例如自己解析 message 的每个字段）—— 那种客户端遇到工具循环
	// 产生的响应会不适应。留这个开关让运维能局部退回改造前的行为。
	//
	// ⚠ 它只影响**注入**；客户端自己带 tools 时本就不接管，与此开关无关。
	DisableChatTools bool
	// Admin 管理台子树（挂在 /admin/，由 internal/admin 提供）。nil = 不注册该子树。
	Admin http.Handler
	// ModelCatalog 供管理台取模型目录快照（成本系数用）。nil = 本实例不提供该能力。
	//
	// 为什么是函数而不是把 *Handler 传过去：admin 已经在用 `ResetModelsCache func()`
	// 这种「由 server 注入闭包」的方向；反向传 *Handler 会让 admin → server 形成
	// 编译期依赖，而 server 反过来 import admin 是为了挂 /admin/ 子树 —— 一反向就是
	// import cycle。传函数既避开环，也让 admin 不必知道缓存住在哪个包。
	ModelCatalog func() *gateway.ModelCatalog
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
	//
	// ⚠ 它**只**用于 /v1/*（对话、生图、模型目录）。管理台门禁读 adminToken ——
	// 两者是独立的钥匙（用户明确要求：门禁 token 不能和 api 一样）。
	apiKeyMu sync.RWMutex
	apiKey   string

	// adminToken 当前生效的**管理台门禁令牌**（支持运行时轮换）。
	//
	// 与 apiKey 共用一把锁（两个字段都在 apiKeyMu 保护下）—— 它们总是一起
	// 被轮换/读取的场景很少，但共用锁省一个 mutex，而这两处都不是热点路径。
	// 空串 = 还没人轮换过 ⇒ 回落 cfg.AdminToken。
	adminToken string
}

// SetAPIKey 运行时更新管理钥匙（管理台「轮换管理密钥」用）。
func (h *Handler) SetAPIKey(k string) {
	h.apiKeyMu.Lock()
	h.apiKey = k
	h.apiKeyMu.Unlock()
}

// SetAdminToken 运行时更新**管理令牌**（轮换门禁令牌用）。
//
// # 为什么必须与 SetAPIKey 分开（用户报障：轮换后要重启才生效）
//
// 两者是两把独立的钥匙，服务两个不同的面：
//
//	api_key     → /v1/*（对话、生图、模型目录）
//	admin_token → /admin/* 与 /ui 的远程访问
//
// 改造前门禁认 api_key，内存里只有一份副本（h.apiKey），所以"轮换"看起来
// 只需要更新它。现在门禁读 admin_token，就必须有**第二条**更新路径 ——
// 否则轮换写盘成功、内存里门禁还是旧值，表现为"轮换后必须重启"。
func (h *Handler) SetAdminToken(k string) {
	h.apiKeyMu.Lock()
	h.adminToken = k
	h.apiKeyMu.Unlock()
}

// currentAdminToken 返回当前生效的管理令牌。
//
// 回落：没人轮换过（字段为空）时用构造时的 cfg.AdminToken。
func (h *Handler) currentAdminToken() string {
	h.apiKeyMu.RLock()
	v := h.adminToken
	h.apiKeyMu.RUnlock()
	if v != "" {
		return v
	}
	// ⚠ 只回落 cfg.AdminToken，**不**回落 cfg.APIKey（那正是要修掉的倒挂）。
	return strings.TrimSpace(h.cfg.AdminToken)
}

// ListensLoopback 本进程是否只监听回环地址（决定 /ui 是否享受本机免令牌）。
//
// 装配层在启动时算一次（见 cmd/server 的 listensLoopback）。
func (h *Handler) listensLoopback() bool { return h.cfg.ListensLoopback }

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
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), apiKey: cfg.APIKey, adminToken: cfg.AdminToken}
	registerCatalogHost(h) // 让包级 ModelCatalog/ModelCatalogState 能找到本实例的缓存
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// 生图出口（OpenAI Images API 形状）。
	//
	// 与 chat 同一条鉴权（同一个 Bearer）：出口层多一条路径不该多一套凭据
	// —— 调用方已经把 api_key 配在 chat 上了，生图再要一个会让人以为
	// 这是两个服务。额度/限速也复用同一套 apikey 判定。
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.imagesGenerations))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	// 单数别名：部分客户端/探针按 /v1/model 探测，与 /v1/models 同源同响应。
	h.mux.HandleFunc("GET /v1/model", h.withAuth(h.models))
	// 内嵌本地控制台（同源，无需 CORS）。
	// 渠道管理页：与 /ui 同一套身份判据（管理面，不是公开页）。
	h.mux.HandleFunc("GET /ui/channels", h.channelsUI)
	// 分组管理页：与 /ui/channels 同一套身份判据（同为管理面）。
	h.mux.HandleFunc("GET /ui/groups", h.groupsUI)
	h.mux.HandleFunc("GET /ui", h.ui)
	// 带尾斜杠的 /ui/ 也接受（用户习惯打 `http://host:port/ui/?token=...`）：
	// 302 到无斜杠的规范路径，查询参数（如 ?token=）原样保留。
	h.mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui?"+r.URL.RawQuery, http.StatusFound)
	})
	h.mux.HandleFunc("GET /favicon.ico", h.favicon)
	h.mux.HandleFunc("GET /favicon.svg", h.favicon)
	h.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
	// ⚠ `/status` 走**控制台身份**（会话或 admin_token），不走 api_key。
	//
	// 它的回执是 `accounts: Pool.List()` + 上游状态 —— 即**账号池画像**
	// （昵称、积分、上游熔断态）。改造前它挂在 withAuth 上（= 认 api_key），
	// 而 api_key 是发给客户端/机器人的对话凭据 ⇒ "给某人一把对话钥匙"等于
	// "把账号池的运营数据也给他看"。这与 T4 修掉的权限倒挂是同一个病，
	// 只是换了入口，所以一并收口。
	//
	// ⚠ 破坏性变更：以前用 api_key 读 /status 的脚本要改用 admin_token
	// （`Authorization: Bearer <admin_token>`）。记在 CHANGELOG 里。
	h.mux.HandleFunc("GET /status", h.consoleAdminAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// 控制台登录端点（注册/登录/登出/读会话）。
	//
	// ⚠ 它必须挂在下面 `/admin/` 那条**之外**：那些端点是**取得凭据**的地方，
	// 走管理台门禁会死锁（"没登录所以打不开登录接口"）。
	// Go 1.22 的 ServeMux 按"更具体的前缀优先"选路，`/admin/auth/` 比
	// `/admin/` 长 ⇒ 命中它，与注册顺序无关。
	if cfg.AccountAuth != nil {
		h.mux.Handle("/admin/auth/", cfg.AccountAuth)
	}
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
	// ⚠ 2026-10-20：「强制刷新目录」也必须清掉**这一层**（按上游取模型的缓存），
	// 否则按钮点了却还回旧目录 —— 用户点它的意图就是"立刻看到最新目录"。
	resetProviderModelsCache()

	modelCatalogCache.Lock()
	modelCatalogCache.entries = nil // 全上游一起清：刷新意图是"立刻看到最新目录"
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
	return h.validCredential(bearerOf(r))
}

// validCredential 报告凭据字符串是否有效（不依赖请求上下文）。
//
// 与 validBearer 同一套判定，只是入参是裸字符串 —— 供 /ui 的
// `?token=` 查询参数分支复用（远程浏览器无法自定义请求头，
// 查询参数是它唯一能携带凭据的地方）。
func (h *Handler) validCredential(cred string) bool {
	ak := h.currentAPIKey()
	if ak == "" && h.cfg.APIKeys == nil {
		return true
	}
	if ak != "" && cred == ak {
		return true
	}
	if h.cfg.APIKeys != nil && cred != "" {
		_, err := h.cfg.APIKeys.Validate(cred)
		// 额度/限速错误由 withAuth 映射 402/429；这里只回答"凭证是否有效"。
		return err == nil
	}
	return false
}

// bearerOf 取出请求 Authorization 头里的 Bearer 凭据（无则空串）。
func bearerOf(r *http.Request) string {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimPrefix(authz, "Bearer ")
	}
	return ""
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
		// panic → 可诊断的 500（而不是静默断连）。见 harden.go 的文件头。
		// 放在 withAuth 里而不是各个注册点：加新端点时不会漏。
		h.panicRecovery(next)(w, r)
	}
}

// consoleAdminAuth 控制台**管理员**读数的门禁：会话或管理令牌，且角色够。
//
// # 与 withAuth 的分工（两把钥匙各管一段）
//
//	withAuth          /v1/*          用 api_key（发给客户端/机器人）
//	consoleAdminAuth  /status 与 /admin/*  用控制台身份（人登录的会话，或脚本的 admin_token）
//
// 为什么 /status 属于后者：它回的是上游账号池画像（昵称/积分/上游熔断态），
// 是管理面数据，不是"客户端该看到的自己那份配额"。
//
// 判据复用 account.Identify（与 /ui 和 /admin/* 同一份实现）：令牌 → 会话，
// 会话必走 SessionStore.Lookup 的四道校验。角色判据是"必须管理员" ——
// 账号池相关读数属于管理面数据。
//
// ⚠ 2026-10-19 单账号口径：控制台只有一个账号（就是管理员），"成员"角色
// 已取消 ⇒ 这条 `p.IsAdmin()` 判据**不可达**（非管理员身份在 account 那层
// 就被回 ErrMemberRetired，Identify 拿不到身份 ⇒ 上面 401 先返回）。
// 保留它：它是 fail-closed 的那一半，且与 admin.canAccess 同一条口径
// （那边也一样保留判据、删掉成员白名单）。
//
// 未认证回 401 + `code: auth_required`（与 admin 门禁同一个错误码契约，
// 前端据此跳登录页而不是显示"没权限"）。
func (h *Handler) consoleAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.uiIdentify(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "该接口需要控制台身份：请在 /ui 登录，或用 Authorization: Bearer <admin_token>。",
				"code":  "auth_required",
			})
			return
		}
		if !p.IsAdmin() {
			// 文案不再提"成员请用 /admin/stats 看自己的用量"：那条路随成员
			// 角色一起取消了，留着会让人去找一个不存在的面板。
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "账号池读数仅管理员可见。",
				"code":  "forbidden_role",
				"role":  p.Role,
			})
			return
		}
		h.panicRecovery(next)(w, r)
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
		// ⚠ 上游维度（2026-10-18 加）：只有**有过失败记录**的上游会出现，
		// 因此"列表为空"读作"一切正常"，而不是"未知"。
		// ⚠ 加它的理由：上游级熔断此前只影响状态码，界面上看不到 ——
		// 用户遇到"整条上游不通"时控制台一切正常，只能靠日志反推。
		"providers": h.cfg.Pool.ProviderStates(),
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

// DefaultUpstream 默认上游的客户端（装配层注入）。
//
// # 为什么核心只收接口，不收 *upstream.Client
//
// `*upstream.Client` 是 **workbuddy 私有 SDK 的类型**。核心包（出口层）
// 认识它，就等于核心依赖某个具体上游 —— 正是 arch_test 判据 3 禁止的、
// 也是 privateSDKKnownDebt 里 server 那条债的形态。
//
// 改成接口之后，**判据与行为一个字没变**（方法签名逐字对齐），
// 变的只是"核心认识的是一个形状，而不是某个包"。
//
// # 方法集是怎么定出来的（不是照抄 Client 的全部方法）
//
// 只列出口层**真正调用**的四个：FetchModels / FetchModelCatalog /
// RefreshToken / ChatStreamWithIP。Client 上其余方法（签到、成长、旅行、
// 上报…）是 workbuddy 的业务动作，出口层从不碰。
//
// 窄接口的好处不只是"看着干净"：它让"出口层到底需要上游提供什么"
// 变成可读的事实。宽接口会掩盖这一点 —— 后来者会以为出口层依赖全部。
type DefaultUpstream interface {
	// FetchModels 拉默认上游的动态模型目录。
	FetchModels(a *auth.Auth) ([]DefaultModel, error)

	// FetchModelCatalog 拉默认上游的模型目录（含倍率）。
	FetchModelCatalog(a *auth.Auth) (*gateway.ModelCatalog, error)

	// RefreshToken 续期一个凭证。
	//
	// 失败时**应当**返回 `*RefreshError`（装配层的适配器负责包装）。
	// 出口层据此区分"session 已死"（计数门控后禁用）与一般失败
	//（只记一次错）。返回其它错误类型时一律按一般失败处理 ——
	// 那是**保守方向**：不会误禁用账号。
	RefreshToken(a *auth.Auth) error

	// ChatStreamWithIP 发起一次对话流。
	ChatStreamWithIP(a *auth.Auth, body []byte, clientIP string) (rc io.ReadCloser, status int, respBody []byte, err error)
}

// DefaultModel 出口层需要的模型元信息（中立形状）。
//
// # 为什么不用 gateway.ModelInfo
//
// `gateway.ModelInfo` 的窗口字段是 `int`，而默认上游给的是 `int64`
// （上游用 int64 存 token 数）。改 gateway 的字段类型会牵动所有上游的
// 实现与契约测试 —— 那是为了让一个**内部投影**少写两个字段，
// 代价与收益不成比例。
//
// 所以这里保留 int64：出口层拿到后按需 `int64(...)` 提升
// （`entryOf` 本来就收 int64）。适配层做一次转换即可。
//
// ⚠ 刻意**不**收 `Name` / `Efforts`：出口层从不读它们
// （模型名直接用 ID；思考档位由出站改写层处理）。
// 收进来只会让"接口比消费者需要的宽"，而宽接口会掩盖真实依赖。
type DefaultModel struct {
	ID            string
	ContextWindow int64
	MaxTokens     int64
}

// defaultUpstreamAdapter 把 *upstream.Client 适配成 DefaultUpstream。
//
// # 为什么适配器在装配层（cmd/server）而不是这里
//
// 本文件在 corePackages 里，**不得** import 任何具体上游。
// 适配器要同时认识两边，只能住在能同时 import 它们的地方 ——
// 装配层正是那个地方（与 poolAdapter / workbuddyLogin 同一手法）。
//
// 本文件只定义接口；`cmd/server` 提供实现。这样"核心要什么"与
// "上游给什么"的转换发生在唯一的接缝上，而不是散进核心。

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []DefaultModel
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// riskReasoningEfforts 本网关对客户端声明的**可选思考档位**。
//
// ⚠ 为什么是"统一给四档"而不是按模型区分：
//
//	· 上游目录里确实带 `reasoning.supportedEfforts`（见 internal/cline/client.go:618），
//	  但**多数上游不下发**（cline.go:183 明确记着"远端不下发档位"）⇒ 按模型给会大部分为空；
//	· 而**声明多了是安全的**：上游对不认识的档位**静默忽略**（cline.go:17 有实测记录：
//	  `reasoning_effort: 'banana'` 返回 200 且思考量为 0），不会报错；
//	· 反过来**声明少了才是问题**：客户端会以为这个模型不支持某档。
//	⇒ 所以先"一律给全档"，等各上游真下发了再收窄。
//
// ⚠ 只给 **off/low/high/max 四档**：与 DSH 内置的那张表一致（dsh-llm-deepseek 的
//
//	REASONING_EFFORTS）。⚠ 我们网关自己认得更多字面量（原样透传），但对外声明要与
//	消费方能显示的集合对齐 —— 声明了它不认的档，等于在界面上给一个选了没反应的选项。
var riskReasoningEfforts = []string{"off", "low", "high", "max"}

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	// 被限制的 key **只看到自己允许的模型**。
	//
	// 不这么做的话：列表里看得见几十个模型，调哪个都 403 ——
	// 用户会读成"网关坏了"，而不是"这把 key 没这个权限"。
	// 列表如实反映权限，权限才是可发现的。
	// ⚠ 2026-10-20 按用户决定**撤掉渠道口径的收紧**：对外目录直接展示上游实时目录的全部模型。
	//
	// 原设计（T5）是「对外 = 已启用渠道声明的模型 ∩ 令牌白名单 → 反向映射成公开名」。
	// 用户实测后看到的是「除新建的那条渠道有 1 个模型外，其余 12 个上游全是 0 个」——那正是
	// 收紧的表现。用户的决定：**不要模型对外的那些修改，直接展示全部的，渠道的要保留**。
	//
	// ⇒ 目录回到既有口径（上游实时目录 + 令牌白名单，两者都是改造前就有的）；
	//   渠道从此只是 chat 请求上的**额外一条路**（见下面 providerFor 之前那段），
	//   不再决定目录里有什么。
	data := h.filterModelsForRequest(r, h.modelListWithScrub(h.scrubEnabledForRequest(r)))
	// ⚠ 撤收紧 ≠ 渠道的模型消失（2026-10-20 用户第二轮反馈后修的）：
	// 上游目录照旧**全量**展示，同时把**渠道声明的公开名并进来**（只增不减）。
	//
	// 为什么：用户建了渠道却在「可用模型」里找不到它 —— 那等于这条渠道不存在，
	// 而他要的是"直接展示全部的"。回顾一下两次反馈的差别：
	//   第一次「除新建渠道外其余 12 个上游全是 0 个」⇒ 问题在"上游被清空"；
	//   第二次「可用模型 674 个里没有我那条渠道」⇒ 问题在"渠道没进来"。
	// 两次都不是"要不要渠道"，而是**并集**：上游的照旧，渠道的也进来。
	data = h.appendChannelModels(r, data, h.scrubEnabledForRequest(r))
	// ⚠ 2026-10-21：必须在 append **之后**再改一次名 —— append 进来的渠道模型
	//   用的是渠道声明的内部名，绕过了 filterModelsForRequest 那一遍映射。
	//   见 applyPublicNames 的注释（有实测日志证据）。
	data = h.applyPublicNames(r, data)
	// ⚠⚠ 给每条**补上档位声明**（2026-10-18 加，用户要求："我们的项目也需要可以支持选择思考深度"）。
	//
	// 动机：我们**一直有** `reasoning_effort` 的透传与降级（internal/cline/payload.go 的
	// PrepareBodyOptWithEfforts / normalizeReasoningEffort），但**从没告诉过客户端
	// 这个模型支持哪些档** ⇒ DSH 与其它"从 /models 读档位"的客户端**看不到** ⇒ 选不了。
	//
	// ⚠ 为什么在这儿补而不是一路从上游带下来：那条链要穿 `gateway.ModelInfo` →
	// `DefaultModel` → `entryOf`/`prefixed` → 响应共 4~5 处，而**上游本来就多数不下发**
	// （见 riskReasoningEfforts 的注释）⇒ 一路带下来只为传一个常量，不值。
	// ⚠ 等真按模型区分时，再把它并进 ModelInfo（那时这条注释就是"该改哪儿"的路标）。
	//
	// ⚠ 两个键名都给：`reasoning_efforts`（用户点名的那个第三方插件读它）与
	//   同名驼峰都不发 —— 只发一个标准名下划线形式，避免同一语义出现两处真相。
	for _, m := range data {
		if _, ok := m["reasoning_efforts"]; !ok {
			m["reasoning_efforts"] = riskReasoningEfforts
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
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
func (h *Handler) modelListWithScrub(scrub bool) []map[string]any {
	// 单上游模式：与改造前逐字节一致（除账号判据，见 singleProviderModelList）。
	if h.cfg.Provider == nil {
		return h.singleProviderModelList(h.ownedBy(), scrub)
	}

	def := h.defaultProvider()
	out := make([]map[string]any, 0, 16)

	// 1. 默认上游：只列带前缀的一份 —— 收窄后不再同时列裸名，
	//    否则同一个模型会在目录里出现两次（用户报的就是这个重复）。
	//    def == "" 是边界：拼前缀只会得到 "/glm-5.2"，
	//    所以此时保持裸名 —— 每个模型仍然只有一条，只是没有上游前缀。
	for _, e := range h.singleProviderModelList(h.ownedByFor(def, scrub), scrub) {
		if def == "" {
			out = append(out, e)
			continue
		}
		out = append(out, h.prefixed(e, def))
	}

	// 2. 其余上游：只有带前缀的一份。
	//    顺序上排在默认上游之后 —— 既有客户端取 data[0] 时拿到的仍是它熟悉的那个。
	//
	// ⚠ 这里同样先判账号（与 singleProviderModelList 同一条判据）：
	// 没有可用账号的上游**不该**出现在目录里。用户报障的正是这个 ——
	// 未列入"能列模型"的上游在界面上会走空态并说明原因，而列出来的
	// 却被当成"可以用"。
	for _, id := range h.otherProviders(def) {
		// ⚠ 2026-10-20：这里以前是 `h.cfg.Provider.Models(...)` 直连上游 ——
		// 实测 /v1/models 连打 4 次是 36.2s / 2.8s / 5.2s / 23.8s（不收敛），
		// 因为并发请求各拉一遍、且过期目录不能直接用。改走 modelsForProviderCached：
		// 正缓存 1h + 单飞 + 有旧目录就先给旧的（后台刷新）。
		infos, ok := modelsForProviderCached(id, func() ([]gateway.ModelInfo, bool) {
			return h.cfg.Provider.Models(context.Background(), id)
		})
		if !ok || len(infos) == 0 {
			continue // 该上游暂时给不出目录：跳过，不拖垮整个 /v1/models
		}

		// 先把本上游**已有元数据**的条目收成一张表，供下面的众数推断用。
		//
		// 为什么需要（用户报的缺陷 4）：上游的 data.models 只有 18 条，
		// 而 agents[].models 声明的名字更多 —— 后者能调通（实测 200）
		// 但没有元数据。对它们用本上游的典型值兜底，比留空更有用。
		known := make(map[string][2]int64, len(infos))
		for _, mi := range infos {
			if mi.ContextWindow > 0 || mi.MaxOutputTokens > 0 {
				known[mi.ID] = [2]int64{int64(mi.ContextWindow), int64(mi.MaxOutputTokens)}
			}
		}
		fallbackCtx, fallbackOut := inferMetaFor(known)

		for _, mi := range infos {
			ctx, maxOut := int64(mi.ContextWindow), int64(mi.MaxOutputTokens)
			// ⚠ 只在**本上游确实有其它元数据**时才推断。
			// 一个上游一条都没报的话，fallback 会是 0 —— 那就不写字段，
			// 保持「未知」而不是编一个值。
			if ctx == 0 && fallbackCtx > 0 {
				ctx = fallbackCtx
			}
			if maxOut == 0 && fallbackOut > 0 {
				maxOut = fallbackOut
			}
			out = append(out, h.prefixed(h.entryOf(mi.ID, ctx, maxOut, h.ownedByFor(id, scrub)), id))
		}
	}

	// 3. **生图模型**（默认上游 + 其余上游）。
	//
	// # 为什么要单独一步（它们不在上面的目录里）
	//
	// 上游的 `/models` 目录**不含**两个生图模型，或者含了也被对话语境的
	// 过滤逻辑剔除（见 loomy/models.go 的 Unavailable 注释：那个标记
	// 只说明"不能走 chat"）。而 `/v1/images/generations` 恰恰只能用它们 ——
	// 不进目录，调用方就不知道能生图、也不知道该填哪个 model。
	//
	// 用接口断言发现可选能力（与 providerIDs 同一范式）：不实现
	// ImageModelExt 的上游在这里被静默跳过，行为与改造前一致。
	imageIDs := append([]string{def}, h.otherProviders(def)...)
	for _, id := range imageIDs {
		for _, im := range h.imageModelsOf(id) {
			e := h.entryOf(im.ID, 0, 0, h.ownedByFor(id, scrub))
			// 标出生图能力：调用方据此判断"这个模型要用
			// /v1/images/generations，不是 chat/completions"。
			// 非标准字段（OpenAI 没有这个概念），但**不标就无法区分**
			// —— 目录里两个模型看名字都知道是生图，但程序不该靠名字猜。
			e["capabilities"] = []string{"image_generation"}
			if id != "" {
				out = append(out, h.prefixed(e, id))
				continue
			}
			out = append(out, e)
		}
	}
	return out
}

// imageModelsOf 问装配层要某上游声明的生图模型。
//
// 与 providerIDs() 同一范式（可选接口断言）：没实现 ImageModelExt
// 或本部署没接线时返回 nil，调用方静默跳过。
func (h *Handler) imageModelsOf(id string) []gateway.ImageModel {
	if h.cfg.Provider == nil {
		return nil
	}
	lookup, ok := h.cfg.Provider.(interface {
		ImageModels(id string) ([]gateway.ImageModel, bool)
	})
	if !ok {
		return nil
	}
	list, ok := lookup.ImageModels(id)
	if !ok {
		return nil
	}
	return list
}

// imageModelEntries 把某上游声明的生图模型编成目录条目。
//
// # 为什么两条路径都要调它（单上游 / 多上游）
//
// 生图模型**不在**上游的对话目录里（它们走 /chat/completions 会 404，
// 见 loomy/models.go 的 Unavailable 注释）。所以无论部署是单上游还是多上游，
// 都必须在目录里**补一次** —— 漏了任何一条路径，那种部署下
// `/v1/images/generations` 就是"端点在、但没人知道该填哪个 model"。
//
// id 为空串表示"不带前缀"（单上游模式）；非空时加 `<id>/` 前缀
// （与其余条目一致：前缀是网关的路由记号）。
func (h *Handler) imageModelEntries(id, owned string) []map[string]any {
	list := h.imageModelsOf(id)
	if len(list) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, im := range list {
		e := h.entryOf(im.ID, 0, 0, owned)
		// 标出生图能力：调用方据此知道这个模型要走
		// /v1/images/generations 而不是 chat/completions。
		//
		// 为什么不靠名字猜（"seedream"/"image" 看起来就像生图）：
		// 名字是上游可以随时改的展示字符串，而这是**能力声明**。
		// 用名字判断的代码在上游改名那天会静默错。
		e["capabilities"] = []string{"image_generation"}
		if name := strings.TrimSpace(im.Name); name != "" {
			e["name"] = name
		}
		if id != "" {
			e = h.prefixed(e, id)
		}
		out = append(out, e)
	}
	return out
}

// singleProviderModelList 单上游路径：动态列表优先，失败回退静态表。
// owned 是填进 owned_by 的展示值。
//
// # ⚠ 本次新增「无可用账号 ⇒ 空」这一条（用户报障）
//
// 报障原文：*「我一个账号没有，应该所有的[上游]都无法扫描到模型才对」*。
//
// 改造前这个函数只回落静态表，于是两种完全不同的情形共用一个出口：
//
//	没账号   → fetchDynamicModels 返回 nil → 回落静态表 → 报「10 个模型」
//	拉取失败 → fetchDynamicModels 返回 nil → 回落静态表 → 报「10 个模型」
//
// 前者**一个模型都调不了**，界面却显示"有 10 个模型"。现在先判账号。
//
// ⚠ 账号判据用 `AvailableUIDsFor("")` —— 空串的语义是"池子的默认上游"
// （`normalizeProvider` 负责解析），而这正是本函数服务的那个上游：
// 它只在两处被调（单上游模式、多上游模式的**默认上游**，见 modelList）。
//
// ⚠⚠ 这里**刻意不传** `h.cfg.DefaultProvider` / `def`：
// "池子的默认上游"与"出口层配的默认上游"**可能不是同一个**（测试里就是：
// 池子没注入默认、出口层配了 "workbuddy"）。传具体名字会选出一个池子里
// 根本不存在的域 → 候选集为空 → **有账号也被判成没账号** → 目录静默变空。
//
// 这与 `fetchDynamicModels` 里"取号传 \"\" 而不是 h.defaultProvider()"
// 是**同一条教训**（那段注释记录了同一个 32→27 的静默退化）。
//
// 其余上游（非默认）不走这里：它们经 `registryRouter.Models`，那条路径
// 自己按**具体上游 id** 判账号（见 cmd/server/multiprovider.go）。
func (h *Handler) singleProviderModelList(owned string, scrub bool) []map[string]any {
	// 没有可用账号 ⇒ 这个上游的目录**必然**给不出来（动态要凭证、
	// 静态表也与真实可用性无关），所以这里直接空，不再回落。
	//
	// ⚠ 判据用 `AvailableUIDsFor`（只读）而**不是** `PickForExcluding`：
	// 后者会写 `lastUsed`（防并发撞号），而本函数在每次 /v1/models 上都会被调
	// —— 那等于每刷新一次目录就"用掉"一次选号机会，把轮询权重搅歪。
	// 同一判据在 `registryRouter.Models` / `ModelMultipliers` 里也是这么用的。
	if h.cfg.Pool != nil && len(h.cfg.Pool.AvailableUIDsFor("")) == 0 {
		return nil
	}
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		// 与「其余上游」分支同一套兜底（见 models_meta_fallback.go）：
		// 上游的 data.models 与 agents[].models 是两个数组，后者声明的名字
		// 能调通但没有元数据。用本上游的众数补，比留空更有用。
		known := make(map[string][2]int64, len(infos))
		for _, mi := range infos {
			if mi.ContextWindow > 0 || mi.MaxTokens > 0 {
				known[mi.ID] = [2]int64{mi.ContextWindow, mi.MaxTokens}
			}
		}
		fallbackCtx, fallbackOut := inferMetaFor(known)
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			ctx, maxOut := mi.ContextWindow, mi.MaxTokens
			if ctx == 0 && fallbackCtx > 0 {
				ctx = fallbackCtx
			}
			if maxOut == 0 && fallbackOut > 0 {
				maxOut = fallbackOut
			}
			out = append(out, h.entryOf(mi.ID, ctx, maxOut, owned))
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
		"id":      id,
		"object":  "model",
		"created": 1753600000,
	}
	// context_length / max_output_tokens **只在有值时写**。
	//
	// # 为什么 0 不该写出来（用户报的第 1、3 条）
	//
	// 旧实现无条件写，于是上游没报 ctx 时输出 "context_length": 0，
	// 客户端读到的意思是「这个模型上下文长度为 0」—— 一个确切的假事实。
	//
	// 实测两个形态：
	//
	//	004/deepseek-v4.1-flash-1017  "max_output_tokens": 0
	//	001/deepseek-v4.1-flash-1017  两个字段都缺（走渠道路径，压根没填）
	//
	// 前者是「上游没报」，后者是「代码没填」。两种都不该被读成 0。
	// 分不清就**别断言** —— 与全仓「未知不等于零」同一条纪律。
	if ctxWindow > 0 {
		entry["context_length"] = ctxWindow
	}
	if maxTokens > 0 {
		entry["max_output_tokens"] = maxTokens
	}
	// owned_by 只在**有值**时才写。
	//
	// scrub=true（普通 API Key 配了别名）时上游传进来的是空串 ——
	// 那个情形下这个键**整个不该出现**：
	// 一个 "owned_by": "" 仍然在说"这里有个来源字段，只是我没填"，
	// 客户端按它的存在与否分组时照样能分出"这两类是同一组"。
	// 用户的要求是「按别名处理，不能暴露别的」⇒ 连字段都不给。
	if owned != "" {
		entry["owned_by"] = owned
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
	return h.ownedByFor(h.defaultProvider(), false)
}

// ownedByFor 返回指定上游的 owned_by：用它自己的 ID ——
// 每条模型的 `owned_by` 反映**实际上游**，而不是全局一个值。
//
// 默认上游且装配层显式给了 OwnedBy 时以 OwnedBy 为准
// （那是改造前就存在的注入点，行为要保持）。
// 没有上游上下文时回落 "local"。
// ownedByFor 返回指定上游的 owned_by。
//
// scrub=true 时返回**代号**（普通 API Key 配了别名映射的情形）；
// scrub=false 时返回上游真名（管理密钥 / 没配别名的情形）。
//
// # 为什么按钥匙分而不是全局开关（用户三次澄清后的口径）
//
//	管理密钥 config.api_key   → 保持上游原名（运维自用，不需要藏）
//	普通 API Key（配了别名）  → 用代号，不暴露上游
//
// 全局开关满足不了这个区分：同一个部署里两种钥匙同时在用。
// ownedByFor 返回指定上游的 owned_by。
//
// scrub=true（普通 API Key 配了别名）时返回**空**：不标来源。
// scrub=false 时返回上游真名（管理密钥 / 没配别名的情形）。
//
// # 为什么 scrub 时是空而不是代号
//
// 我第一版返回代号（ch-a / ch-b）。实测下来那不是用户要的：
// 代号虽然不含上游名，但**仍然是一个来源标注** —— 客户端照样能把
// 9 个别名分成 4 组，答出「这 4 个模型来自同一处」。
//
// 用户的口径是「设置了就按别名处理，不能暴露别的」。
// 按别名处理 = 只给别名，不给来源。所以这里留空。
func (h *Handler) ownedByFor(id string, scrub bool) string {
	if scrub {
		return ""
	}
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
func (h *Handler) fetchDynamicModels() []DefaultModel {
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

// modelCatalogCache 模型目录（GET /v3/config，成本系数）缓存，**按上游各一份**。
//
// 与 dynamicModelsCache 是**两份**缓存而不是合并成一份，理由与两个端点不能互相替代同源：
//   - 数据源不同：/console/enterprises/personal/models（模型可用性）vs /v3/config（成本系数）；
//   - 失败模式不同：系数拿不到只是少一个观测维度，不该把 /v1/models 的模型列表也拖下水。
//
// # 为什么按上游分域（海外版渠道支持）
//
// 多上游部署下每个上游的 /v3/config 是**各自的事实**（workbuddy 的 glm-5.2 倍率
// 与 workbuddy-intl 的 default-model 倍率互不相干）。单份缓存只能服务默认上游，
// 其余上游的模型在界面上永远显示不出倍率。key = provider 标识（"" = 默认上游，
// 与 Pool 的未打标签语义一致）。
//
// 生命周期策略刻意与 dynamicModelsCache 完全一致（1h 正缓存 + 5min 失败负缓存、
// 惰性拉取、不加 ticker/goroutine），这样「什么时候会打上游」在代码里只有一套心智模型。
type modelCatalogEntry struct {
	cat      *gateway.ModelCatalog
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

var modelCatalogCache struct {
	sync.RWMutex
	entries map[string]*modelCatalogEntry // provider → 目录缓存（惰性创建）
}

// toGatewayCatalog 搬到了装配层（cmd/server/defaultupstream.go）。
//
// # 为什么它不该留在核心
//
// 它的入参是 `*upstream.ModelCatalog` —— **workbuddy 私有 SDK 的类型**。
// 只要核心签名里出现它，核心就依赖了那个上游，无论函数体多"中立"。
//
// 现在 DefaultUpstream 接口直接声明返回 `*gateway.ModelCatalog`，
// 投影由装配层的适配器做（那里可以同时认识两边）。
//
// 函数体**逐字段原样搬走**，包括 MultiplierKnown 那条容易漏的填法：
//
//	MultiplierKnown: m.CreditsRaw != ""
//
// 语义是"上游给了非空 CreditsRaw 就说明它表态过"（哪怕系数解析成 0，
// 那是"免费"而不是"未知"）。这与 admin 的 modelsPreview「保留 0 倍率」
// 一致 —— x0.00 是"免费"这个有意义的事实。

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
// ModelCatalog 返回**默认上游**的模型目录（兼容旧签名；等价 ModelCatalogFor("")）。
func ModelCatalog() *gateway.ModelCatalog {
	if h := catalogHost.Load(); h != nil {
		return h.modelCatalogFor("")
	}
	return nil
}

// ModelCatalogFor 返回指定上游的模型目录；缓存失效时惰性回源一次，失败返回 nil。
//
// provider 为空串 = 默认上游（与 Pool 未打标签语义一致，单上游部署逐字节不变）。
// 多上游部署下每个上游的倍率各拉各的 /v3/config（见 modelCatalogCache 的注释）。
func ModelCatalogFor(provider string) *gateway.ModelCatalog {
	if h := catalogHost.Load(); h != nil {
		return h.modelCatalogFor(provider)
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
	// 状态按默认上游（"" 键）报告：与改造前单份缓存同一语义 ——
	// 状态是"缓存这件事"的观测维度，多上游各自成功/失败不合并成一个数。
	modelCatalogCache.RLock()
	defer modelCatalogCache.RUnlock()
	st := CatalogState{State: "unavailable"}
	e := modelCatalogCache.entries[""]
	if e == nil {
		return st
	}
	if cool := !e.lastFail.IsZero() && time.Since(e.lastFail) < modelsFetchFailCooldown; cool {
		st.Cooldown = true
	}
	if e.cat == nil {
		return st
	}
	st.Models = len(e.cat.Models)
	st.FetchedAt = e.fetched
	if !e.fetched.IsZero() && time.Since(e.fetched) >= dynamicModelsTTL {
		st.State, st.Stale = "stale", true
		return st
	}
	st.State = "ok"
	return st
}

// modelCatalogFor 是 (*Handler) 上的实现体，语义见包级 ModelCatalogFor。
// provider 为空串 = 默认上游。
func (h *Handler) modelCatalogFor(provider string) *gateway.ModelCatalog {
	if h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return nil
	}

	// ⚠ 数据源分两路（多上游倍率改造）：
	//
	//	扩展点（gateway.ModelMultiplierExt）优先 —— codearts/loomy/trae 的
	//	倍率来自**它们自己的官方接口**（ratio_display / 展示名后缀）。
	//	workbuddy 系不实现该扩展点（它的倍率在 /v3/config），自然落入下方回退。
	//
	// 这样每个上游的倍率都由自己的官方数据源回答，且不走 workbuddy 的
	// /v3/config（拿别家凭证打 copilot.tencent.com 只会失败并惩罚无辜账号）。
	if h.cfg.Provider != nil && provider != "" {
		if m, ok := h.cfg.Provider.ModelMultipliers(context.Background(), provider); ok && len(m) > 0 {
			return gateway.CatalogOf(m)
		}
	}

	modelCatalogCache.RLock()
	e := modelCatalogCache.entries[provider]
	if e != nil && e.cat != nil && time.Since(e.fetched) < dynamicModelsTTL {
		cat := e.cat
		modelCatalogCache.RUnlock()
		return cat
	}
	// 失败负缓存：冷却期内不再请求上游（与 dynamicModelsCache 同一套语义）。
	if e != nil && !e.lastFail.IsZero() && time.Since(e.lastFail) < modelsFetchFailCooldown {
		modelCatalogCache.RUnlock()
		return nil
	}
	// 内有未过期目录但已超 TTL：先取出来，全部重取失败时回吐旧目录。
	var stale *gateway.ModelCatalog
	if e != nil {
		stale = e.cat
	}
	modelCatalogCache.RUnlock()

	tried := map[string]bool{}
	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 按 provider 取号：多上游部署下不能拿默认上游的账号去拉别的上游的 /v3/config。
		// provider 为空串时退回 PickForExcluding("")（默认上游，单上游部署逐字节不变）。
		var acct *auth.Auth
		if provider == "" {
			acct = h.cfg.Pool.PickForExcluding("", tried)
		} else {
			for _, uid := range h.cfg.Pool.AvailableUIDsFor(provider) {
				if tried[uid] {
					continue
				}
				acct = h.cfg.Pool.AuthByUID(uid)
				break
			}
		}
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		// ⚠ 这里原先是一处**装配边界**：`cfg.Upstream` 是 workbuddy 的客户端，
		// 它返回 workbuddy 自己的 `*upstream.ModelCatalog`，而缓存与出口层用
		// 中立类型 `gateway.ModelCatalog` —— 所以调一个 toGatewayCatalog 做投影。
		//
		// 现在那个投影**搬到了装配层**（见 cmd/server 的 defaultUpstream 适配器）：
		// 接口直接声明返回 `*gateway.ModelCatalog`，核心拿到的就是中立类型。
		// 投影本身一个字段都没变（逐字段搬运 + MultiplierKnown 的填法），
		// 只是发生的位置从"核心里"挪到了"接缝上"。
		cat, err := h.cfg.Upstream.FetchModelCatalog(acct)
		if err != nil || cat == nil || len(cat.Models) == 0 {
			// 与 fetchDynamicModels 同口径：惩罚该账号，避免下次 Pick 又选中同一个反复失败的号。
			h.cfg.Pool.NoteError(acct.UID)
			continue
		}
		modelCatalogCache.Lock()
		if modelCatalogCache.entries == nil {
			modelCatalogCache.entries = map[string]*modelCatalogEntry{}
		}
		modelCatalogCache.entries[provider] = &modelCatalogEntry{
			cat:      cat,
			fetched:  time.Now(),
			lastFail: time.Time{},
		}
		modelCatalogCache.Unlock()
		return cat
	}

	// 全部尝试失败：进负缓存。已有旧目录时**保留它并返回** ——
	// 成本系数是观测维度，过期的系数比没有系数有用得多（前端会标 stale 提示其不可信）。
	modelCatalogCache.Lock()
	if modelCatalogCache.entries == nil {
		modelCatalogCache.entries = map[string]*modelCatalogEntry{}
	}
	modelCatalogCache.entries[provider] = &modelCatalogEntry{
		cat:      stale,
		lastFail: time.Now(),
	}
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
		// 参数校验用（缺陷 3：这些以前全部直通上游）。
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		MaxTokens   *int64   `json:"max_tokens"`
		Messages    []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	// ⚠ 解析失败必须**当场回 400**（缺陷 2：畸形 JSON 之前报成了 403）。
	//
	// 旧实现是 `_ = json.Unmarshal(...)` —— 把错误丢了。后果：
	// peek.Model 保持空串 → 走到白名单判断 → model=="" 不匹配 → 403
	// model_not_allowed。客户端发了个语法错误，收到的是"你没权限"，
	// 排查方向被完全带偏。
	//
	// 语法错误是**请求无效**（400），与权限无关。这条顺序不能省。
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json",
			"请求体不是合法 JSON："+err.Error())
		return
	}
	// 参数范围校验（缺陷 3）。
	//
	// # 为什么网关要校验（而不是"交给上游"）
	//
	// 实测这些全部 200 直通上游：temperature=-5、max_tokens=-1、top_p=5、
	// role="wizard"、messages=[]。上游多数会自行消化，但：
	//
	//	max_tokens=-1  → 上游当"无限制"，返回完整长回答 → **计费失控**
	//	temperature=-5 → 有的上游 clamp、有的报错、有的按 0 处理
	//
	// 而 OpenAI 规范里这些都是 400。客户端按规范写代码时，
	// 我们的 200 会让它以为参数生效了。
	//
	// 校验只做**规范明确的硬边界**，不做风格性限制（比如不禁止 temperature=2
	// 之外的启发式判断）—— 越界就 400，与官方行为一致。
	if msg := validateChatParams(peek.MaxTokens); msg != "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}

	// publicModel 客户端**请求时用的名字**（别名或裸名）。
	//
	// 脱敏时回填到响应的 model 字段 —— 客户端认的是自己发出去的那个名字，
	// 而上游原名属于身份指纹（monkeycode-basic/deepseek-flash 点名了上游）。
	//
	// ⚠ 必须在 resolveModelAlias **之前**取：那一步会把它换成内部名。
	// 管理密钥不配别名 ⇒ scrub 恒 false ⇒ 响应保持上游原名（用户明确要求）。
	publicModel := strings.TrimSpace(peek.Model)
	scrubResp := h.scrubEnabledForRequest(r)

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
	// ⚠⚠ 渠道口径（中转站模式）—— 接缝**必须**在 providerFor **之前**。
	//
	// # 为什么必须在之前（这是 2026-10-20 修的那个 bug）
	//
	// providerFor 把 "csg/deepseek-v4.1-flash-1017" 读成"上游前缀 csg"，判成未知上游并回
	// 400 unknown_provider —— 而那是前端建的渠道给出的**公开名**。渠道公开名是另一个
	// 命名空间，先过 legacy 的前缀解析就永远走不到渠道转发这一步
	// （实测现象：界面里模型选得好好的，一发就 400「未知的上游前缀」）。
	//
	// # 顺序与回落（按用户 2026-10-20 的决定调整）
	//
	//   - 渠道只是**额外的一条路**，不是替代：名字不属于任何渠道 ⇒ 原样往下走 legacy
	//     （用户要求「直接展示全部的」，原先"没有候选就 404 明确拒绝"的收紧已撤）。
	//   - 渠道真失败（没可用 key / 上游报错）⇒ 如实上报，**不换路**。Key 型渠道与账号池的
	//     根本区别就在这里：换号会让用户不知道请求走了哪条，身份与计费口径也都变了。
	//   - 白名单照样判（对渠道而言公开名就是对外名）：权限不因为"走了渠道"而消失。
	// ⚠⚠ 2026-10-21：**在任何路由/白名单判断之前**，先把 API key 的对外名换成内部名。
	//
	// 顺序不能挪，三条理由：
	//   ① 白名单里存的是**内部**名。拿对外名去判会 403 —— 而那个对外名
	//      恰恰是我们发布给调用方的名字，用户会觉得"列表里有、调了却不行"。
	//   ② `channelServes` / `providerFor` / `forwardViaChannel` 全部按内部名工作，
	//      放在它们之后换就已经走错路了（渠道匹配发生在更早）。
	//   ③ body 里的 model 不需要在这里改：渠道分支由 forwardViaChannel 用
	//      (model 参数, body) 重新写；legacy 分支同理。只改 peek.Model 一处，
	//      所有消费方自动一致 —— 分两处改必然有一天只改了一处。
	if alias := h.resolveModelAlias(r, peek.Model); alias != peek.Model {
		la := strings.TrimSpace(alias)
		orig := strings.TrimSpace(peek.Model)
		log.Printf("[apikey] 模型别名替换 %q → %q", orig, la)
		peek.Model = alias
	}
	if h.channelServes(peek.Model) {
		// 白名单按**客户端发来的名字**判，不按解析后的内部名。
		//
		// 内部名是 resolveModelAlias 换出来的，白名单里存的也是内部名 ——
		// 拿内部名去判会永远通过（那等于白名单失效）；而"拒绝上游真名"
		// 那条判据若作用在内部名上，会把**用别名的合法请求**也拒掉
		//（别名 006/x 换成 workbuddy/x，然后被当成"用了真名"）。
		if !h.modelAllowedForRequest(w, r, publicModel) {
			return
		}
		chStart := time.Now()
		// ★ mode 记**客户端意图**（请求体 stream 字段），不是网关实际怎么转（2026-10-20 修）。
		//
		// 改造前这里是硬编码字面量 "sync"，后果是用户截图问"应该不是非流式吧"：
		// 渠道转发的流式请求在日志里**全部被误报成 sync**。而 peek.Stream 早在解析
		// 请求体时就拿到了（handler.go:1473 的 `Stream bool`），只是从来没传进来。
		//
		// 语义口径与 new-api 的 `is_stream` 对齐：那一列记的也是客户端意图。
		// 用户看日志要回答的是"我这个请求本来该是流式吗"，不是"网关内部怎么转的"。
		chMode := channelLogMode(peek.Stream)
		// ⚠ 传 publicModel（客户端原始名）而不是 peek.Model（已换成内部名）：
		//   forwardViaChannel 用它填 fw.PublicModel，脱敏时回填到响应 model 字段。
		//   传内部名的话脱敏等于没做 —— 实测就是那样：
		//   001 的响应 model 仍然是 csg/deepseek-v4.1-flash-1017。
		//
		// ⚠ 但渠道匹配必须用**内部名**：channelroute.Plan 按渠道声明的上游名查表。
		//   所以这里传两个：匹配用内部名，对外回显用公开名。
		fw, ferr := h.forwardViaChannelAs(r.Context(), peek.Model, publicModel, body)
		if ferr != nil {
			// ★ 渠道失败也写请求日志。改造前渠道分支**一行日志都没有**，后果是实测踩到的：
			//   用户发 csg，日志里一条都搜不到，他只能看到另一个 Python 客户端的 loomy 流量，
			//   于是合理地质问"我用的是 csg 呢不是 LOOMY"。没有日志 = 无法自证。
			st := http.StatusBadGateway
			if ue, ok := gateway.AsUpstreamHTTPError(ferr); ok {
				st = ue.Status
			}
			// ⚠ 2026-10-20 修：失败时 `fw` 是**零值**（forwardViaChannel 返回 nil 语义），
			//   而下面仍用 fw.ChannelName / fw.ChannelID / fw.UpstreamModel ⇒ 全空。
			//   实测后果（用户报"又怎么回事啊"，2026-10-20 09:36 那条 429）：
			//     status=429  provider=channel_error  error=(空)  decision="channel_error:"  channel=(空)
			//   既看不出哪个渠道、也看不出为什么失败 —— 只剩一个状态码，排障等于从零开始。
			//
			//   provider 参数原来传字面量 "channel_error"（那是错误类型，不是 provider），
			//   于是 provider 列对渠道失败毫无信息量。改传空串，由 model 列与 decision 说明。
			//
			//   TODO：根因修法是让 forwardViaChannel 失败时也回填部分 fw
			//   （ChannelID/ChannelName/UpstreamModel），那要动它的返回语义，单独一步做。
			detail := ferr.Error()
			if fw.ChannelName != "" {
				detail = fw.ChannelName + ": " + detail
			}
			logChatRowProvider(0, time.Since(chStart), peek.Model, chMode, "", "",
				st, -1, nil, "channel_error: "+detail, 0,
				gateway.ExtractClientIP(r), r.UserAgent(), fw.ChannelID, fw.UpstreamModel)
			// ⚠ 2026-10-21：渠道分支在 chatStat 之前就 return，所以 key 用量要在这里记。
			//   不记的话界面上「请求」「失败」恒为 0（用户报的那个现象）。
			h.bumpKeyUsage(r, 0, false)
			// ⚠ 走 writeChannelError 而不是写死 502：上游的 429/401 要原样透传给客户端
			// （见 channels_forward.go 里那段"实测踩过"的注释）。
			h.writeChannelError(w, ferr)
			return
		}
		// 旁路采集器：边转发边解析 SSE，拿 ttfb 与 usage（三级兜底见 stream_usage.go）。
		// ⚠ 它只读不写，转发字节与上游完全一致 —— 这是本改造唯一的硬约束。
		cap := newStreamUsage(chStart)
		h.writeChannelStream(w, r, fw, cap, scrubResp)
		// 成功也记：model 记**客户端发来的公开名**，渠道与上游模型各占一列 ⇒
		// 「这条 csg 请求到底走了渠道还是账号池」在日志里一眼可判。
		// tokens 现在来自采集器（末帧 usage → 倒数第二帧 → 文本估算）；
		// 真的一无所获时才记 -1（"未知"哨兵，聚合会排除它）。
		tok, ttfbMs, usageSrc := cap.Result()
		if tok <= 0 {
			tok = -1
		}
		// 纯输出 token（tok/s 的分母依据）。走 usage map 传给 logChatRowProvider，
		// 由 wire.ParseUsageExtras 统一提取 —— 这样账号池路径与渠道路径
		// 用**同一条**提取链路，不会出现两条路径口径分叉。
		outTok, _ := cap.Completion()
		var usageMap map[string]any
		if outTok > 0 {
			usageMap = map[string]any{"completion_tokens": outTok}
		}
		// ⚠ 2026-10-20 修：原来这里传 `0, "", ""`（preMs / clientIP / clientUA 全空），
		//   后果是渠道请求的 client_ip 与 client_ua 在日志里**永远为空**
		//   （实测 500 条里 0 条有 client_ip）。而 loomy/csg 都走这条路径 ⇒
		//   用户问"loomy 那些非流式请求是谁发的"时，日志无法回答。
		//   账号池路径（L1610 st.clientIP）是取了的，渠道路径漏了 —— 两条路的观测能力不该不同。
		logChatRowProvider(ttfbMs, time.Since(chStart), peek.Model, chMode, fw.KeyMask, "channel:"+fw.ChannelName,
			fw.Stream.Status, tok, usageMap, "usage:"+string(usageSrc), 0,
			gateway.ExtractClientIP(r), r.UserAgent(), fw.ChannelID, fw.UpstreamModel)
		// ⚠ 2026-10-21：成功也记（tok 可能是 -1「未知」哨兵，BumpUsage 内部只在 >0 时累加）。
		h.bumpKeyUsage(r, tok, true)
		return
	}
	reqProvider, reqModel, perr := h.providerFor(peek.Model)
	if perr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "unknown_provider", perr.Error())
		return
	}
	// API key 的模型白名单：拿到模型名之后、动手之前立刻判。
	//
	// 位置刻意在 providerFor **之后**：白名单按裸模型名比（见
	// apikey.ModelAllowed），而剥前缀的口径要用选路那一套 —— 先解析出
	// 结构化的模型名，再比，两处判据不会分叉。
	// 同样按客户端发来的名字判（理由见渠道分支那段）。
	if !h.modelAllowedForRequest(w, r, publicModel) {
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

	// 出站前归一化 reasoning_effort（缺陷 1：off 在 workbuddy 系必挂 400）。
	//
	// # 用户实测
	//
	// /v1/models 声明六模型都支持 [off low high max]，而实际：
	//
	//	003 workbuddy      reasoning_effort=off  -> 上游 400
	//	004 workbuddy-intl reasoning_effort=off  -> 上游 400
	//	002 loomy          reasoning_effort=off  -> 200
	//	005 monkeycode     reasoning_effort=off  -> 200
	//
	// 而传 none（不在声明列表里）反而 200 且无推理输出。
	// 也就是说 workbuddy 系把 off 当非法值，真正表达关闭的是
	// **省略这个参数**（或 none）。
	//
	// # 为什么在出口层统一改，而不是改各上游
	//
	// 「off 等价于不传」是**网关对外的契约**（我们在 /v1/models 里
	// 声明了 off），而各上游接不接受是各自的事实。让 12 个上游都实现
	// 一次转换，必然有一半漏掉 —— 那时客户端会在某些通道上莫名 400。
	//
	// ⚠ 400 还有个更坏的副作用（用户实测）：它被算作账号失败，
	//   把 2 个健康号打进冷却，于是**后续正常请求也跟着 503**。
	//   一个参数取值错误污染整个账号池，这是必须在这里拦住的理由。
	// 出站参数覆盖（internal/server/param_override.go，对标 new-api 的 override）。
	//
	// reasoning_effort 的 off→删除 是这套机制的**第一条内置规则**
	//（ReasoningOffOverrideRules），渠道还能追加自己的规则。把它降级成一条
	// 数据规则而不是写死一段代码，是因为「网关要改客户端参数」这件事不止
	// 这一个场景：某个上游要把 max_tokens 改名叫 max_completion_tokens、
	// 某个渠道要强制 temperature、某个要删掉它不认的字段 —— 每来一个就加
	// 一段 if，出口会变成一堵补丁墙。
	//
	// ⚠ 必须用**客户端原始 body** 做输入，不能拿上一轮 attempt 的输出：
	//   渠道 A 的 set 若被带进换到渠道 B 的那一轮，B 的 default 规则就永远
	//   看不到「客户端没传」。outBody 每轮从 body 重新派生（见上面的赋值）。
	overrideRules := ReasoningOffOverrideRules()
	if chOverrides := h.channelParamOverrides(reqProvider); len(chOverrides) > 0 {
		// 内置规则放最前，渠道规则在后 —— 渠道能覆盖内置行为
		//（比如某渠道就是要显式 set reasoning_effort: low）。
		overrideRules = append(overrideRules, chOverrides...)
	}
	if res := ApplyParamOverrides(outBody, overrideRules); res.Changed {
		outBody = res.Body
		if len(res.Problems) > 0 {
			// 覆盖规则只对**能应用的那几条**生效，失败的跳过并留痕。
			// 不因一条坏规则拒掉整个请求 —— 那会让一条配置笔误变成渠道不可用。
			log.Printf("[override] 部分规则未生效 provider=%s: %s", reqProvider, res.Problems[0].Error())
		}
	}
	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	// ⚠⚠ 2026-10-21：日志里的 model 必须用**内部名**，不是客户端发来的对外名。
	//
	// `newChatStat` 内部用 `parseModelFromBody(body)` —— 而 body 里是**原始请求体**，
	// 也就是客户端发来的名字。若那把 API key 配了对外名映射，body 里就是
	// `002/DeepSeek-V4.1-Flash` 这类别名，而它会被原样写进请求日志。
	//
	// 用户的要求：「不希望请求日志出现对外名称 请求日志显示原本的就行」。
	// 理由也成立：日志是**排障证据**，要对得上渠道/上游；别名只是付款方看到的叫法。
	//
	// 渠道分支那边不用改 —— 它本来就用 `peek.Model`（L1524 已替换过）。
	// 两条路径现在口径一致：model 列 = 内部名，别名只出现在客户端那一侧。
	st.model = peek.Model
	st.provider = reqProvider
	// ⚠ 请求来源（纯观测，见 logbuf.Entry.ClientIP 的注释）：
	// 用来指认"是谁在直连网关发非流式" —— 那件事靠日志里的模型/token 规律只能猜，
	// 而有了 IP + UA 就能一次点名。ExtractClientIP 是既有函数（往下 10 行就在用它建 ctx）。
	st.clientIP = gateway.ExtractClientIP(r)
	st.clientUA = r.UserAgent()
	// ⚠ 把首字节耗时喂给上游级统计（2026-10-18 加，见 chatStat.noteLatency 的注释）。
	//
	// ⚠ 为什么在这里赋、在 done() 里调：done() 是**所有出口**的汇合点
	// （同步 / 流式 / 工具循环 / 降级重试都走它），而 chatStat 本身拿不到 Handler。
	// 这与 st.bump 是同一种取舍（回调 + 出口统一调用）。
	//
	// ⚠⚠ 注意它**不改变路由** —— 只是让 pool 知道"这家上游现在有多快"，
	// 供 /status 显示（用户要的"保证请求速度"此前没有可观测的量）与后续的
	// "明显更慢就换一家"提供依据（那一步需要独立开关，见 pool.SlowerThan 的注释）。
	st.noteLatency = h.cfg.Pool.NoteProviderLatency
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
	// lastKind 最近一次上游错误的分类：出口要按它区分"上游抖"与"池子真空"。
	var lastKind gateway.ErrorKind = gateway.ErrKindNone

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
			// ⚠ 必须带 reqProvider：粘性是**池内**概念，只按全池分配会把本会话绑到别的上游的号上（见 session.Config.AvailableFor）
			if uid, ok := h.cfg.Session.Resolve(sessKey, reqProvider); ok {
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

	// 上游 5xx（502/503/504）不是某个账号的过错：一抖就只试 MaxRotate 次
	// 会把它放大成"用户请求失败"。这里为 server 类错误额外加
	// serverRetryExtra 次换号机会（实测场景：讯飞网关抖动数秒，
	// 池里 11 个健康号，旧行为试 3 个就回 503）。
	maxAttempts := h.cfg.MaxRotate
	// tierOffset 重试时逐次**下沉档位**（对标 new-api 的 retry→降档：
	// GetRandomSatisfiedChannel 用 retry 直接索引优先级档，见 model/channel_cache.go）。
	//
	// 只在上游 5xx（ErrKindServer）时下沉：那类错误说明"这一档现在整体不可用"
	// （上游网关抖 / 该批号被限流），换档才有意义；而 4xx（请求本身的问题）
	// 换到哪一档都一样失败，下沉只会白烧低档号。
	tierOffset := 0
	// ⚠⚠ **换上游**相关的两个变量（2026-10-18 加）。
	//
	// 背景（实测）：`deepseek-v41flash` 这个模型在 **loomy / workbuddy / workbuddy-intl** 三家都有
	// （归一后同键，见 upstream_alt.go），而某一时段 loomy 失败率 15.22%、
	// workbuddy 只有 0.13% —— 也就是说"我们要的东西就在隔壁那家"，只是请求被
	// 模型前缀 `loomy/` 钉在了坏的那家上。
	//
	//	switchedUpstream ⚠ **只切一次**：切来切去会变成两个上游之间反复横跳，
	//	  每一跳都要重新选号 + 打上游，比不切更慢。切过一次还没成就认了。
	//	origModel 用户请求的**原始**模型名（带前缀）—— 用它去找"同模型的另一个上游"；
	//	  ⚠ 不能用 reqModel：它会在切换后被改写成新上游的写法，再拿去匹配就找不回来了。
	switchedUpstream := false
	origModel := peek.Model
	for i := 0; i < maxAttempts; i++ {
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
				// ⚠ 判决留痕（抄 new-api 的 reason/source 命名，见 logbuf.Entry.Decision）。
				// 它那边的同义判决叫 `strict_session` / source `session_rule`
				//（service/relay_error.go:27）—— 这里沿用同样的取名，两边日志可对照。
				st.decision = "sticky_unavailable:session_rule"
			}
		}
		if acct == nil {
			// 按 (上游, 模型) 选号：只在该上游的账号里挑，
			// 绝不让 codearts 的请求落到 workbuddy 的账号上。
			// reqProvider 为空（单上游模式 / 裸模型名）时池子按默认上游解释。
			acct = h.cfg.Pool.PickForTier(reqProvider, reqModel, tried, tierOffset)
		}
		// ⚠⚠ **换上游已停用**（2026-10-18，用户明确要求）：
		//   "我调用的是这个模型，但是它优选，选到别的模型去了，别的上游去了。你别乱搞啊"
		//   ⇒ ⚠ **客户端点的是 `loomy/DeepSeek-V4.1-Flash`，就只许在 loomy 的号里选** ——
		//     不管什么理由（更慢、选不出号、熔断），都**不许**把请求发到别的上游去。
		//     "换了上游"这件事对用户是不可见的，他只会看到"我要的模型结果变了"。
		//
		// ⚠ 代码保留（alternateUpstream 与它的单测），只断掉这个调用点：
		//   写在这里的死代码是一份**明确的"当前不启用"记录**，比删掉函数更容易找回，
		//   也避免以后有人凭印象重新接上它。
		if false && acct == nil && !switchedUpstream {
			// ⚠⚠ **换一个同模型的上游再试**（2026-10-18 加）。
			//
			// 这里才是"上游坏了"该有的动作：⚠ 不是更快地失败，而是**换一家**。
			// 实测依据：`deepseekv41flash` 在 loomy / workbuddy / workbuddy-intl 三家都有，
			// 而 loomy 15.22% 失败、workbuddy 0.13% ⇒ 换过去就是两个数量级的差别。
			//
			// ⚠ 只在"选不出号"时切，不在"上游熔断"时切：熔断是"这家现在不通"的证据，
			//   而此刻选号往往还能成功（它只是刚被喂了失败）—— 那时切走会让流量在两家
			//   之间来回跑，反而制造抖动。
			//
			// ⚠ 切过去之后：tierOffset 归零（新上游的档位与旧上游无关）、
			//   tried 清空（那是"已试过的 uid"，换上游后那些 uid 不在候选里了）。
			if alt, altModel := h.alternateUpstream(origModel, reqProvider); alt != "" {
				st.decision = "switched_upstream:" + alt
				reqProvider = alt
				reqModel = altModel
				tierOffset = 0
				tried = map[string]bool{}
				switchedUpstream = true
				continue // ⚠ 用新上游重选号（本轮已消耗，但换来一个真候选集）
			}
		}
		if acct == nil {
			// ⚠ 出口语义（用户实测 503 的真正来源）：
			//
			// 上游 5xx 会喂 NoteError → 熔断把号逐个摘出候选 → 某一轮**选不出号**
			// 就在这里 break。旧代码无论为什么选不出号都置 503 —— 于是
			// "讯飞网关抖 502、池里 11 个号全健康"被你看到的是
			// `503 no_healthy_account: all accounts unavailable`，语义完全错了。
			//
			// 判据用 lastKind：本轮已吃过上游 5xx（ErrKindServer）→ 这是
			// **上游故障**，回 502 upstream_error（客户端应重试）；
			// lastKind 仍是 None → 池子真没号/全在冷却 → 503（重试无用）。
			// ⚠ 刻意**不动** lastErr：那里存着上游 502 的原文（排障最有用的一段），
			// 覆盖成一句概括反而把证据抹掉了。最终状态码由出口按 lastKind 决定。
			st.status = http.StatusServiceUnavailable
			if lastKind == gateway.ErrKindServer {
				st.status = http.StatusBadGateway
			}
			// ⚠⚠ **判决留痕**（抄 new-api 的 `PolicyDecision{Action, Reason, Source}`，
			//   见 internal/logbuf/logbuf.go 的 `Decision` 字段注释）。
			//
			// 这一行是本次改动的**全部动机**：用户看到 503 时，日志里此前只有"503"这一个事实，
			// 读者（包括我）只能猜 —— 2026-10-18 那天我为此猜了四轮（额度 / 容量 / 重试策略全错）。
			// 现在这两条 `reason` 直接把"上游挂了"与"池子真空"分开，与上面两行的状态码判据同源。
			// ⚠⚠ **上游级熔断优先判**（2026-10-18 加）。
			//
			// 账号级熔断的聚合效应是：某个上游整体坏了（实测 loomy 10-17 失败率 26%）时，
			// 它 44 个号会被逐个喂 NoteError、逐个熔断 —— 在"全熔断"之前，
			// 每个请求都要**白试若干个号**才失败。上游级熔断把"这个上游现在整体不通"
			// 变成**即时可知**的事实（见 pool 的 provState 注释）。
			//
			// ⚠ 语义：这是**上游故障**（客户端该重试），所以回 502 而不是 503 ——
			// 与下面 `lastKind == ErrKindServer` 那条同一口径，只是判据更早、更准。
			// ⚠ 这条判断**必须在 `lastKind` 那条之前**：上游熔断时 lastKind 往往是 None
			// （请求在选号阶段就失败了，还没打上游），落到 else 会被报成"池子空了"。
			if _, tripped := h.cfg.Pool.ProviderTripped(reqProvider); tripped {
				st.status = http.StatusBadGateway
				st.decision = "provider_breaker:system"
			} else if lastKind == gateway.ErrKindServer {
				st.decision = "upstream_5xx:system"
			} else {
				st.decision = "pool_unavailable:global"
			}
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
				var re *RefreshError
				if errors.As(err, &re) && re.SessionDead {
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

		// ── 工具循环（上游声明的工具，客户端零改动）──
		//
		// 客户端**不需要**懂工具调用：它照常发一句"帮我画只猫"，
		// 网关替它注入工具、执行工具、把结果回喂给模型收尾。
		//
		// ⚠ 作用域天然是"本上游的"：工具定义与执行都问**装配层**要
		// （`ChatTools(id)` / `ExecuteChatTool(id,…)`），而 id 是
		// 本次被路由到的那个上游。所以 loomy 的请求拿到生图+搜索，
		// workbuddy 的请求（没实现该扩展点）一个都拿不到 ——
		// 不需要在核心写任何 `if provider == "loomy"`。
		//
		// 客户端自己带了 tools 时**不接管**：那说明它懂协议、要自己驱动
		// 工具循环，网关再插一脚会两边打架（见 shouldRunToolLoop）。
		// 工具循环自己写 w，必须把脱敏回调传进去 —— 否则走工具的上游
		//（loomy 等）会绕过所有脱敏，用户看到"有的通道脱了有的没脱"。
		var loopScrub func(obj map[string]any)
		if scrubResp {
			loopScrub = func(obj map[string]any) { scrubObject(obj, publicModel) }
		}
		if tlExt, tlYes := h.shouldRunToolLoop(reqProvider, outBody); tlYes {
			ext := tlExt
			// 流式与**非流式**走两条实现（见 chattool_stream.go 的文件头）：
			//
			//	流式    逐帧转发，只在真调我们的工具时才拦 —— 保住打字机效果
			//	非流式  内部跑完整轮再一次性返回（原本就无流式可言）
			//
			// ⚠ 第一版只有非流式那条，于是**每个** loomy 请求都被缓冲成
			// 一整块，用户看到"文字不是流式的了"——范围远大于生图的回归。
			var done bool
			// loopStats 接收工具循环读到的统计（TTFB/tokens/usage）。
			// ⚠ 这条路径绕过了 chatStatsReader（普通 chat 用它读数），
			// 所以必须自己把统计接回来 —— 否则日志的 TTFB / tok / tok/s
			// 三列全是空的（用户实测报过）。见 chatstat.go 的文件头。
			var loopStats *toolLoopStats
			var werr error
			if peek.Stream {
				done, loopStats, werr = h.runToolLoopStream(w, toolLoopParams{
					ext:        ext,
					scrub:      loopScrub,
					providerID: reqProvider,
					uid:        acct.UID,
					body:       outBody,
					stream:     true,
					start:      st.start,
					ctx:        ctx,
				}, h.chatToolsOf(reqProvider))
			} else {
				done, loopStats, werr = h.runToolLoop(w, toolLoopParams{
					ext:        ext,
					scrub:      loopScrub,
					providerID: reqProvider,
					uid:        acct.UID,
					body:       outBody,
					stream:     false,
					ctx:        ctx,
				})
			}
			if werr != nil {
				// ── 已写出响应 → **绝不能**换号重试 ──
				//
				// 流式路径是边读边写的。一旦写过字节：
				//
				//	状态码收不回来（已发 200）
				//	已发内容收不回来
				//
				// 此时换号重试会往**同一个** ResponseWriter 再写一套 SSE，
				// 客户端收到两段交错的事件流（协议损坏、内容重复）。
				//
				// 所以这是"只能收尾"的收场：记日志 + 保持已发内容不变。
				// 对比之下"未写过"才是可重试的，那种情况照旧换号。
				var committed *ErrResponseCommitted
				if errors.As(werr, &committed) {
					log.Printf("chat tools: 响应已提交，停止重试 uid=%s provider=%s: %v",
						acct.UID, reqProvider, committed.Cause)
					// 账号不算失败：这轮错误是"上游在后续轮次回了 400"，
					// 而非凭证/额度问题 —— 计失败会误伤一个健康的账号。
					h.cfg.Pool.NoteSuccess(acct.UID)
					// 成功即解除自动禁用（与 NoteSuccess 并列，不替代）。
					//
					// ⚠ 这条只在"该号还能被选中"时才有机会跑到。被禁用的号不在候选集里，
					//   永远拿不到下一次成功 —— 恢复通道靠健康检查循环的
					//   AutoDisabledCandidates() 探测（见 admin/health 接线），
					//   否则"成功即恢复"是条死条款。
					h.cfg.Pool.RecordAutoDisableSuccess(acct.UID)
					if sessKey != "" && h.cfg.Session != nil {
						h.cfg.Session.Bind(sessKey, acct.UID)
					}
					loopStats.applyTo(st)
					st.status = http.StatusOK
					return
				}
				// 循环内的硬失败（注入失败/上游连续报错）：
				// 分类后按换号处理，与普通 chat 路径同一条。
				lastErr = werr
				fail(acct.UID)
				continue
			}
			if done {
				// 已经写过响应了（含状态码），直接收工。
				h.cfg.Pool.NoteSuccess(acct.UID)
				// 成功即解除自动禁用（与 NoteSuccess 并列，不替代）。
				//
				// ⚠ 这条只在"该号还能被选中"时才有机会跑到。被禁用的号不在候选集里，
				//   永远拿不到下一次成功 —— 恢复通道靠健康检查循环的
				//   AutoDisabledCandidates() 探测（见 admin/health 接线），
				//   否则"成功即恢复"是条死条款。
				h.cfg.Pool.RecordAutoDisableSuccess(acct.UID)
				if sessKey != "" && h.cfg.Session != nil {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
				// 把循环读到的统计写回 —— 日志的 TTFB/tok/tok/s 靠它。
				loopStats.applyTo(st)
				st.status = http.StatusOK
				return
			}
			// done=false：本轮没跑成（如上游报错），换号重试。
			fail(acct.UID)
			continue
		}

		// 出站：按**选中的账号所属上游**分派到它自己的实现。
		//
		// 这是本次修复的核心。改造前这里恒用 h.cfg.Upstream（= 默认上游的
		// upstream.Client），于是 codearts 的请求被 workbuddy 的客户端发出去 ——
		// 凭证格式不匹配，必然失败，而且失败得很晚（一次完整往返）。
		//
		// cfg.Provider == nil（单上游部署 / 既有测试）时逐字节回退到原行为，
		// 见 chatVia 的注释。
		// ⚠ 记下"到此刻为止我们花掉的时间"（纯观测，不改行为）。
		//
		// ⚠ 它回答的正是用户那个质疑："根本就不是号级速度的问题，而是后端代码调用有 bug"。
		// `ttfb_ms` 把两段混在一起：我们这边（选号 / 取凭证 / 可能刷凭证 / 转换请求体）
		// 加上上游首包。这一列把前者单独拿出来 ⇒ 一眼就能判"慢该怪谁"。
		st.preMs = time.Since(st.start)
		cr, status, terr := h.chatVia(ctx, reqProvider, acct, outBody)
		if terr != nil {
			// ⚠⚠ **本地转换失败**：换号**完全无效**（转换取决于请求体本身，
			//   每个账号背后是同一套转换代码）⇒ 不换号、不惩罚账号，直接回 400。
			//
			// ⚠ 这条必须排在下面的"网络抖动"之前 —— 两者此前共用同一条
			//   `fail(acct.UID); continue`，于是转换失败会白试 N 个号，
			//   最后还被报成"所有账号不可用"（把本地错误误报成池子故障）。
			//   这正是 new-api 的 `ErrOptionWithSkipRetry` 显式声明的那件事。
			if errors.Is(terr, gateway.ErrConvertFailed) {
				st.status = http.StatusBadRequest
				st.decision = "convert_failed:system"
				lastErr = terr
				lastKind = gateway.ErrKindClient
				break
			}
			// ⚠⚠ **确定性的客户端错误也走这条路**（2026-10-18 加）。
			//
			// # 为什么必须在这里判（而不是上面的 status >= 400 段）
			//
			// 实测：loomy 把 400 包成 `*chatError` **从 err 返回**（不是走 status），
			// 报文长这样：`upstream fatal_client (http 400): {"error":{…}}` ——
			// 于是它落到下面那句"网络层抖动 ⇒ 换号"，44 个账号全试了一遍。
			//
			// ⚠ 判据用 **Kind** 而不是 Status：上一版用 `ce.Status == 400` 判过，
			// **没命中**（说明这个 chatError 的 Status 不是 400）——
			// 而 classifyErr 已经把种类算出来了，直接信它更稳。
			//
			// ⚠ 这类错误换号/换提示词都没用：每个账号收到的是同一份 body。
			// 直接回 400 结束，对齐 new-api 的 ErrOptionWithSkipRetry。
			var ce *chatError
			if errors.As(terr, &ce) && ce.Kind == gateway.ErrKindFatalClient {
				st.status = http.StatusBadRequest
				st.decision = "fatal_client:client"
				lastErr = terr
				lastKind = gateway.ErrKindFatalClient
				break
			}
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
			lastKind = kind
			if kind == gateway.ErrKindServer {
				if maxAttempts < h.cfg.MaxRotate+serverRetryExtra {
					maxAttempts++
				}
				// 上游还在抖 → 下一轮沉降一档（先用免费额度那档、再往永久额度沉），
				// 而不是在同一档里把号反复试一遍。
				tierOffset++
			}
			lastErr = &chatError{
				Kind:   kind,
				Status: status,
				Msg:    string(respBody),
			}
			// 内容策略拦截：**不罚账号**，改用降级提示词重试一次。
			//
			// 见 gateway.ErrKindContentBlocked 的长注释：内容问题换号没有意义
			// （每个账号背后是同一套策略），正确的动作是换提示词。
			// 因此这里刻意**不调** applyErrorPolicy —— 它会把账号冷却/熔断，
			// 而账号在这件事上没有任何过错。
			if kind == gateway.ErrKindFatalClient {
				// ⚠⚠ **上下文超限：不换号、不降级提示词，直接结束**（2026-10-18 加）。
				//
				// # 为什么它必须与内容拦截分开处理
				//
				// 内容拦截（ErrKindContentBlocked）的动作是"换提示词重试一次" ——
				// 因为换一份措辞**可能**就过了。而上下文超限换什么都没用：
				// 它是"这次请求的 body 太长"，换号（每个号收到的 body 一样长）、
				// 换提示词（只会更长）都不会改变结果。
				//
				// # 不这么做的实测后果
				//
				// 它此前落到 ErrKindClient（"只换号不罚"）⇒ 我们用一份结构不合法的 body
				// 打网关时，44 个账号**全试了一遍**（耗时 6.7s ~ 45s），
				// 最后报成 `503 no_healthy_account —— 0 禁用、0 冷却`，
				// 把"请求太长"误报成"池子故障"。
				//
				// ⚠ 这也正是用户说的"反复尝试没有额度的渠道或账号"的真身
				// （见知识库 preferences/上下文超限与可重试判定）。
				//
				// 动作对齐 new-api 的 `ErrOptionWithSkipRetry`：回 400 并结束。
				st.status = http.StatusBadRequest
				st.decision = "fatal_client:client"
				lastErr = &chatError{
					Kind:   kind,
					Status: status,
					Msg:    string(respBody),
				}
				lastKind = kind
				break
			}
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
			// 上游 400 = 我们的请求参数有问题，不是账号故障。
			//
			// # 用户实测的缺陷 1（第二半）
			//
			// reasoning_effort=off 让 workbuddy 系回 400，而每个号收到的 body
			// 完全一样 —— 换号必然同样失败。旧逻辑走 applyErrorPolicy + fail：
			//
			//	1) 2 个健康号被打进冷却（一个参数错误污染整个池子）
			//	2) 重试若干轮后报 503 no_healthy_account（把「你传错了」说成「没号」）
			//	3) 报错体里 upstream client (http 400): 后面是空的，
			//	   因为 respBody 没被带出去
			//
			// 正确动作与 ErrKindFatalClient 一致：回 400 并结束，
			// 不冷却、不换号、把上游的错误原文带上。
			//
			// 判据用 status 而不是 kind：401/403 是凭证问题（要换号），
			// 429 是限流（要冷却）—— 都不能回 400。
			if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
				st.status = http.StatusBadRequest
				st.decision = "upstream_bad_request:client"
				lastKind = gateway.ErrKindFatalClient
				lastErr = &chatError{Kind: gateway.ErrKindFatalClient, Status: status, Msg: string(respBody)}
				break
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
			// 脱敏回调：只在客户端用配了别名的普通 API Key 时生效。
			// 管理密钥传 nil ⇒ 帧原样转发（用户要求「对话密钥按原来的上游即可」）。
			var streamScrub wire.StreamScrubber
			if scrubResp {
				streamScrub = func(obj map[string]any) { scrubObject(obj, publicModel) }
			}
			serr := wire.StreamScrubbed(w, stats, streamScrub)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.usage = stats.Usage()
			rc.Close()

			var inband *wire.InBandError
			if errors.As(serr, &inband) {
				if inband.Committed {
					// 内容已流出、状态码已提交，错误帧也已原样下发：
					// 既不能改响应也不再换号（客户端已经"消费"了这个回复）。
					// 只记录，保留可排查的上游原文。
					log.Printf("chat stream uid=%s provider=%s: 上游流内错误（状态码已提交）: %s",
						acct.UID, reqProvider, inband.Message)
					h.cfg.Pool.NoteSuccess(acct.UID)
					// 成功即解除自动禁用（与 NoteSuccess 并列，不替代）。
					//
					// ⚠ 这条只在"该号还能被选中"时才有机会跑到。被禁用的号不在候选集里，
					//   永远拿不到下一次成功 —— 恢复通道靠健康检查循环的
					//   AutoDisabledCandidates() 探测（见 admin/health 接线），
					//   否则"成功即恢复"是条死条款。
					h.cfg.Pool.RecordAutoDisableSuccess(acct.UID)
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
				lastKind = kind
				lastErr = &chatError{
					Kind:   kind,
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
			// 成功即解除自动禁用（与 NoteSuccess 并列，不替代）。
			//
			// ⚠ 这条只在"该号还能被选中"时才有机会跑到。被禁用的号不在候选集里，
			//   永远拿不到下一次成功 —— 恢复通道靠健康检查循环的
			//   AutoDisabledCandidates() 探测（见 admin/health 接线），
			//   否则"成功即恢复"是条死条款。
			h.cfg.Pool.RecordAutoDisableSuccess(acct.UID)
			// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
			st.status = http.StatusOK
			return
		}
		resp, err := wire.Aggregate(rc)
		rc.Close()

		// 非流式同样要拦流内错误：Aggregate 命中错误信封时返回 *InBandError，
		// 而不是合成一个 content:"" 的成功响应（改造前它就是这么被吞掉的）。
		var inband *wire.InBandError
		if errors.As(err, &inband) {
			kind := h.classifyErr(reqProvider, http.StatusOK, []byte(inband.Body))
			lastKind = kind
			lastErr = &chatError{
				Kind:   kind,
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
		// 成功即解除自动禁用（与 NoteSuccess 并列，不替代）。
		//
		// ⚠ 这条只在"该号还能被选中"时才有机会跑到。被禁用的号不在候选集里，
		//   永远拿不到下一次成功 —— 恢复通道靠健康检查循环的
		//   AutoDisabledCandidates() 探测（见 admin/health 接线），
		//   否则"成功即恢复"是条死条款。
		h.cfg.Pool.RecordAutoDisableSuccess(acct.UID)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		// 脱敏（只在客户端用的是配了别名的普通 API Key 时生效）。
		//
		// 管理密钥不受影响 —— 用户明确要求「对话密钥不用管，按原来的上游即可」。
		// DBG
		if scrubResp {
			resp = scrubChatMap(resp, publicModel)
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = totalTokens(resp) // ⚠ 2026-10-20：原来用 completionTokens（只算输出，prompt 全丢）
		st.usage = usageOf(resp)
		return
	}
	// ⚠⚠ **预算用尽留痕**（2026-10-18 加）。
	//
	// 走到这里说明重试循环**正常走完**了（不是 break 出来的）⇒ 换号预算用完仍没成功。
	// 抄 new-api 的同义判决名：它的 `DecideRelayRetry` 在 `retryTimes <= 0` 时
	// 返回 stop / `attempt_budget_exhausted`（service/relay_error.go:44-46）。
	//
	// ⚠ 只在**还没有更具体的留痕**时才写（`st.decision == ""`）：
	// 前面那些判决点（sticky_unavailable / provider_breaker / convert_failed…）
	// 说的都比"预算用尽"更具体，覆盖掉它们等于把证据换成结论。
	//
	// ⚠ 判据与紧邻的下面那段**同源**（那里按 lastKind 决定 msg 与状态码）：
	// 吃过上游 5xx 说明是**上游**的问题，写 upstream_5xx；
	// 否则是池子侧（号都试遍了/全在冷却），写 budget_exhausted。
	if st.decision == "" {
		if lastKind == gateway.ErrKindServer {
			st.decision = "upstream_5xx:system"
		} else {
			st.decision = "budget_exhausted:global"
		}
	}
	// 出口分两种语义（这是用户实测报的那个 503 的根因）：
	//
	//	上游 5xx（服务端故障） → 502 upstream_error：**客户端应该重试**，
	//	  账号本身没问题（实测：讯飞网关抖 502，池里 11 个号全健康，
	//	  旧行为却回 503 no_healthy_account，把"上游抖"报成"账号不可用"）。
	//	池子真空/全冷却         → 503 no_healthy_account：这是池子状态问题，
	//	  重试无用，得去控制台处理。
	msg := "all accounts unavailable (cooling/disabled)"
	if lastKind == gateway.ErrKindServer {
		msg = "上游服务异常（已自动轮换多个账号重试，仍失败）"
	}
	// 把"为什么一个都不可用"写进错误里（真实困惑：上游 402 触发冷却后，旧文案
	// 只说 all accounts unavailable —— 用户据此怀疑"登录失败了"，而真相是
	// "key 有效但账户没钱在冷却"。一句话的差别，省一轮客服）。
	if hint := h.poolUnavailableHint(reqProvider); hint != "" {
		msg += " —— " + hint
	}
	if lastErr != nil {
		// ⚠ 脱敏后再拼进 msg（用户实测的泄露：上游错误原文把 host / IP / base_url
		// 全带出来了，等于向客户端公布后端是谁）。
		//
		// 为什么在**这里**脱敏而不是在 writeOpenAIError 里统一做：
		// writeOpenAIError 还承担本仓自己写的文案（"max_tokens 不能为负"这类），
		// 那些不该过脱敏 —— 它们含 "https://..." 形态字样时会莫名变成 ***。
		// 只在拼入**上游原文**这一处脱敏，判据最清楚。
		//
		// ⚠ 日志里保持原文（logbuf 记的是排障用的证据），只有出站这份要脱敏。
		msg += ": " + gateway.MaskError(lastErr)
	}
	// 用契约错误码，不用内部 kind 字面量（internal/gateway/errcode.go）。
	//
	// 旧代码把 "upstream_error" 写死在这里 —— 那是内部词汇泄漏到契约层。
	// ErrorCodeForKind 是唯一映射点，改词只需改那一张表。
	code := gateway.ErrorCodeForKind(lastKind)
	// ⚠ 这三个 kind 必须回 no_healthy_account，不能用映射表的默认值。
	//
	//	ErrKindNone        —— 池子真空 / 全部冷却（重试无用，得去控制台处理）
	//	ErrKindSessionDead —— 会话失效（要人工重登）
	//	ErrKindAuth        —— 凭证无效
	//
	// 映射表把 None 也叫 upstream_error 是错的：它会把"池里没有账号"报成
	// "上游服务异常"，用户据此去查上游，而问题在自己的账号池。
	// 这条语义在 upstream5xx_test 与 handler_test 里都有断言钉着。
	switch lastKind {
	case gateway.ErrKindNone, gateway.ErrKindSessionDead, gateway.ErrKindAuth:
		code = gateway.CodeNoHealthyAccount
	}
	if lastKind == gateway.ErrKindServer {
		writeOpenAIError(w, http.StatusBadGateway, string(code), msg)
		st.status = http.StatusBadGateway
		return
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, string(code), msg)
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
//	Provider == nil                   → cfg.DefaultClassifier（单上游，逐字节不变）
//	Provider 在 && 上游实现了分类器   → 上游自己的分类（翻译成中立类型）
//	Provider 在 && 上游没实现分类器   → cfg.DefaultClassifier（回落默认上游判据）
//
// # 后两条回落为什么是同一个分支，且为什么**必须**用默认上游的判据
//
// 单上游模式没有"别的上游"概念，行为要与改造前一致。
//
// 而"该上游没实现 ErrorClassifier"的正确解释**不是**"用通用猜测"，
// 而是"用默认上游的判据"：默认上游就是 workbuddy，它的分类判据
// （hardMarkers / sessionDeadMarkers）是 workbuddy 的**上游知识**。
//
// ⚠ 刻意**不**回落到 `gateway.DefaultErrorKind`（只按状态码的通用兜底）：
// 那会丢掉 workbuddy 的两个正文判据 ——
//
//	`{"code":12153,...}` 在 200/401 上不再被判成 session dead
//	（TestChatSessionDeadDisables 会立刻变红）
//	`{"msg":"余额不足"}` 在 400 上不再触发硬冷却
//	（TestChatRotatesOnHardCredit 会立刻变红）
//
// 也就是说：通用兜底会**静默削弱已有判据**。
//
// # 这次改动把回落目标从 upstream.Classify 换成注入的分类器
//
// 原先这里直接调 `upstream.Classify` —— 那是**核心包 import 某个上游的
// 私有 SDK**，正是 privateSDKKnownDebt 里 server 那条债的形态。
//
// 现在改由装配层注入（`cmd/server` 里包一层 `workbuddy.Classify`）：
// **判据一个字没变**（还是 workbuddy 的那份），但核心不再需要认识
// upstream 这个包。这是"把上游知识留在上游、核心只收接口"的常规手法。
//
// 注入为 nil 时（测试里的手工构造、或单上游但没接线的部署）回落到
// `gateway.DefaultErrorKind` —— 只按状态码判断。那是**明确的降级**，
// 不是静默削弱：nil 注入是调用方的显式选择。
func (h *Handler) classifyErr(providerID string, status int, body []byte) gateway.ErrorKind {
	if h.cfg.Provider != nil {
		if kind, ok := h.cfg.Provider.Classify(providerID, status, string(body)); ok {
			return kind
		}
	}
	if h.cfg.DefaultClassifier != nil {
		return h.cfg.DefaultClassifier(status, string(body))
	}
	return gateway.DefaultErrorKind(status, string(body))
}

// RefreshError 续期失败的中立错误（装配层的适配器产出）。
//
// # 为什么需要它（而不是直接看 upstream.Error）
//
// 出口层原先用 `errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead`
// 判断"该不该走计数门控"。`*upstream.Error` 是 workbuddy 私有 SDK 的类型 ——
// 核心为了一个布尔判断依赖了某个上游的 SDK。
//
// 换成这个中立类型后判据完全一样（SessionDead 这个字段就是那个布尔），
// 而核心不再认识 upstream 包。
//
// ⚠ `SessionDead` 用**具名字段**而不是直接复用 gateway.ErrorKind：
// 这里的语义是"刷新路径上要不要走计数门控"，与"这次响应属于哪一类错误"
// 是两件事（后者才是 ErrorKind）。合成一个会让读者以为它们是同一套判据。
type RefreshError struct {
	// SessionDead 凭证会话已死（需人工重登）。
	SessionDead bool
	// Msg 上游原文（保留可排查性）。
	Msg string
	// Err 原始错误（便于日志与 errors.Is 链）。
	Err error
}

func (e *RefreshError) Error() string {
	if e == nil {
		return ""
	}
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "refresh failed"
}

func (e *RefreshError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// chatError 出站循环里累积的最后一个错误（**只用于拼 503 的文案**）。
//
// # 为什么不用 upstream.Error（这次重构把它换掉了）
//
// 原先这里用的是 `*upstream.Error` —— 那是 **workbuddy 私有 SDK 的类型**，
// 而本包是核心出口层。为了一个纯展示用的结构体让核心依赖某个上游的 SDK，
// 正是 privateSDKKnownDebt 里 server 那条债的形态。
//
// 换成 server 自己的类型之后，Kind 直接就是 `gateway.ErrorKind`
// （中立类型），不再需要 upstreamKindOf 那层"中立 → upstream"的翻译 ——
// 那层翻译**存在的唯一理由是 upstream.Error.Kind 是 upstream.ErrKind**。
// 换句话说：这条依赖链被整条剪掉了，而不是被绕开。
type chatError struct {
	// Kind 错误类别（中立类型）。
	Kind gateway.ErrorKind
	// Status 上游返回的 HTTP 状态码（流内错误时为 200 —— 那是事实）。
	Status int
	// Msg 上游原文（原样保留，便于排查）。
	Msg string
}

func (e *chatError) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// softRateReset 问**该上游自己**"这次软限流有没有精确的重置时刻"。
//
// # 两条路径（与 classifyErr 同形状，但回落策略刻意不同）
//
//	Provider == nil（单上游）        → wire.ParseSoftRateReset（逐字节旧行为 + 收窄）
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
	return wire.ParseSoftRateReset(string(body))
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
		return refreshDueAt(acct, cred, h.cfg.RefreshSkew)
	}
	if skew <= 0 {
		// 上游明确声明"不需要提前刷"（只在 401 后被动续期）。
		// ⚠ 必须尊重它，不能用通用窗口覆盖 —— 那正是本函数要修的那类错误。
		return false
	}
	return refreshDueAt(acct, cred, skew)
}

// refreshDueAt 判"距过期不足 within" —— **优先用活凭证的过期时刻**。
//
// # 为什么不能直接 `acct.NeedsRefresh(within)`（实测的真 bug）
//
// `acct` 是**池投影**，而各上游的投影都刻意不抄 ExpiresAt
// （见 raccooncreds.go / clinecreds.go 的「投影纪律」：投影是启动快照，
// 活 secret 被续期后投影不会跟新）。实测 `/admin/accounts` 里
// `expires_at` 键**一个上游都没有** ⇒ `acct.ExpiresAt` 恒为 0。
//
// 而 `auth.NeedsRefresh` 对 `ExpiresAt <= 0` **恒返回 true**：
//
//	func (a *Auth) NeedsRefresh(within time.Duration) bool {
//		if a.ExpiresAt <= 0 { return true }   // ← 恒真
//		return time.Now().Add(within).Unix() >= a.ExpiresAt
//	}
//
// 于是"该不该刷"这个判断**对每个上游都恒为真** ⇒ 每个 chat 请求先做一次
// 续期往返。危害按 refresh_token 的形态分两档：
//
//	可重复使用（cline 等）→ 白费一次往返
//	**一次性**（raccoon / codearts）→ **每次请求烧掉一个 token**
//
// 实测 cline：发一次对话请求后 access_token **被换了**。
// raccoon 更严重：一次性 token 被刷爆 → 续期连续失败 → 账号禁用
// （正是用户报障的那个形态）。
//
// 所以这里优先读 `cred.ExpiresAt` —— 装配层已用
// `gateway.CredentialExpiryExt` 把它从活 secret 上补齐（8 个上游都实现了
// 该扩展点）。只有拿不到时才回落池投影。
//
// # 仍拿不到过期信息时保持**原有**行为（要刷）
//
// 那是"完全不知道什么时候过期"的上游。既有取舍是保守地刷
// （宁可多一次往返，也不要拿一个可能过期的凭证去发请求，让服务端回 401）。
// 本次**不改**这个取舍 —— 修的是"明明知道却不读"。
func refreshDueAt(acct *auth.Auth, cred gateway.Credential, within time.Duration) bool {
	at := cred.ExpiresAt.Unix()
	if at <= 0 && acct != nil {
		at = acct.ExpiresAt
	}
	if at <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= at
}

func (h *Handler) applyErrorPolicy(uid string, kind gateway.ErrorKind) {
	// 自动禁用计数（internal/pool/autodisable.go）：与下面每种策略**并列**累加。
	//
	// # 为什么只在这几种 kind 上计（而不是所有失败）
	//
	// 自动禁用是「这个号持续不好用」的判决，代价是丢一份产能。所以只统计
	// **确实是账号问题**的失败：
	//
	//	HardCredit / SoftRate / SessionDead / NotFound / Server  → 计
	//	Auth / Client / None                                     → 不计
	//
	// 后三种是"换号就能好，但号本身没毛病"（凭证边界、请求问题、分类未知）——
	// 把它们计进去会让一次客户端发错参数的行为，在连发 5 次后禁掉一个健康号。
	//
	// ⚠ 与 NoteError 的关系：并存，不替代。NoteError 喂的是**熔断线**
	//   （3 次短暂歇息），这里喂的是**禁用线**（5 次长期摘除）。两条线判据
	//   不同、动作不同，所以不能合并 —— 合并会让熔断的"短暂"语义被禁用覆盖。
	switch kind {
	case gateway.ErrKindHardCredit, gateway.ErrKindSoftRate,
		gateway.ErrKindSessionDead, gateway.ErrKindNotFound, gateway.ErrKindServer:
		if h.cfg.Pool.RecordAutoDisableFailure(uid, "upstream "+kind.String()) {
			// 刚跨过阈值：记一条明确日志。运维看到"某号自己变灰"时，
			// 这一行是唯一的线索（否则只能猜）。
			log.Printf("[pool] 账号 %s 连续失败达阈值，已自动禁用（原因：%s）", uid, kind.String())
		}
	}
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

// poolUnavailableHint 概括"这个上游域为什么选不出号"（≤3 个号时逐个给
// 状态；多了只给计数，消息不被刷屏）。空串 = 没信息可加。
//
// 只暴露冷却原因/时刻与禁用原因 —— 都是管理台可见信息，不含凭证内容。
func (h *Handler) poolUnavailableHint(provider string) string {
	if h.cfg.Pool == nil {
		return ""
	}
	return poolUnavailableHintOf(h.cfg.Pool.ListFor(provider))
}

// poolUnavailableHintOf 纯函数版（测试直接钉）。
func poolUnavailableHintOf(list []pool.Status) string {
	if len(list) == 0 {
		return "该上游池内暂无账号（先在控制台导入凭证或添加账号）"
	}
	if len(list) > 3 {
		nDis, nCool, nBusy, maxFlight := 0, 0, 0, 0
		for _, st := range list {
			switch {
			case st.Disabled:
				nDis++
			case st.Cooling:
				nCool++
			}
			// ⚠⚠ 在途也要数（2026-10-18 加，见下面那段注释）。
			//   ⚠ 它**与 Cooling 正交**：账号可以既不冷却也没禁用，只是并发名额被占满 ——
			//     那种情况下它照样选不出来。
			//   ⚠ 这里**不判断"是否达上限"**（上限是 Pool 的私有字段，Status 不带它）——
			//     改为报"有几个号在途、最大在途数"；⚠ 上限默认是 3，读者一眼就能对照 ✓。
			if st.InFlight > 0 {
				nBusy++
			}
			if st.InFlight > maxFlight {
				maxFlight = st.InFlight
			}
		}
		// ⚠⚠ **为什么必须把"在途"报出来**（2026-10-18 实测踩到）：
		//   用户看到的 503 原文是 "44 个账号：**0 禁用、0 冷却**" ——
		//   ⚠ 那句话**把两个可能的原因排除掉了，却一个真原因都没给出**，读者只能猜"池子坏了"。
		//   而真原因是**在途名额占满**：主路径 Acquire 后**整个请求期间**持有名额，
		//   而工具循环内再按 uid 取号时**不换号**（见 chattool_send.go:42 的注释）
		//   ⇒ 同一账号并发达到上限时，那条路径**必失败** ⇒ 503。
		//   ⇒ 报错必须给出**真正的那一维**，否则排障只能从零猜（这次就猜了一轮）。
		return fmt.Sprintf("%d 个账号：%d 禁用、%d 冷却、%d 个在途未结束（最大 %d）",
			len(list), nDis, nCool, nBusy, maxFlight)
	}
	parts := make([]string, 0, len(list))
	for _, st := range list {
		uid := st.UID
		if len([]rune(uid)) > 8 {
			uid = string([]rune(uid)[:8])
		}
		switch {
		case st.Disabled:
			r := st.Reason
			if r == "" {
				r = "已禁用"
			}
			parts = append(parts, fmt.Sprintf("%s 禁用(%s，需启用或重新登录)", uid, r))
		case st.Cooling:
			r := st.Reason
			if r == "" {
				r = "冷却中"
			}
			tail := ""
			if !st.Until.IsZero() {
				tail = "，约 " + st.Until.Local().Format("15:04") + " 自动恢复"
			}
			parts = append(parts, fmt.Sprintf("%s %s%s", uid, r, tail))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "；")
}

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
