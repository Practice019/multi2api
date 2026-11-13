// provider.go PhanthyWork 作为 gateway.Provider 的接缝实现。
//
// # 这个包在整条解耦链里的位置
//
// 它是 gateway.Provider 的又一个实现。判据 1（"加一个上游 = 加一个目录 +
// 实现接口 + 配置加一段 + 注册一行，核心零改动"）在这里的第 N 次实测。
//
// # 上游事实的来源（全部来自只读规格报告，本包不联网探测）
//
//	docs/reports/phanthywork/phanthywork-2api-protocol-report.md  五步链路 + 12 个坑
//	docs/reports/phanthywork/phanthywork-auth-and-credentials-report.md  登录/凭据/续期
//	docs/reports/phanthywork/phanthywork-endpoint-report.md        本地网关端点面
//	docs/reports/phanthywork/phanthywork-full-platform-capability-report.md  能力全貌
//
// # 本轮的边界（用户明确裁定）
//
//	调模型这条路**已实测 HTTP 200**（POST /v1/messages，usage.output_tokens=224）；
//	"造新号"端到端**未验证**（报告 §6.9 明令「别写成注册可协议化」）
//	⇒ **本上游不做注册**，只做"有号之后"的 2API 链路。
//
// # 能力位（声明即承诺，少报是安全的）
//
//	声明：CapChat | CapModels | CapQuotaProbe | CapCheckin | CapImport
//	**不声明**：CapInvite / CapTasks / CapWelfare / CapGrowth / CapTravel
//	（那几项在**这个平台的协议里没有对应端点**，不是"本轮不做"）
//
// 各能力的落点（改动时先读对应文件，那里的注释记着实测口径）：
//
//	CapCheckin     daily-login/claim（body 空对象 + 幂等键用**服务端** server_date）  checkin.go
//	CapQuotaProbe  activities/summary 的四窗口额度                               quota.go
//	CapImport      官方 / 网关 / 扁平三种凭证形态的粘贴导入                        import.go
//
// ⚠ 签到**自动跑**这件事不由本包的 `Enabled` 决定：走的是核心的「统一签到」任务
//
//	（scheduler → ExtOf[DailyCheckinExt] → CheckinAll）。详见 checkin.go 的 initCheckin 注释。
package phanthywork

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/anthroconv"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// ── 冻结接口（不得改名）───────────────────────────────────────────────────

// providerID 上游标识。
//
// 会被用作：模型名前缀（"phanthywork/xxx"）、配置里的 provider key、
// 凭证目录名、统计维度。格式受 gateway 约束（^[a-z][a-z0-9-]*$），
// 有契约测试守着。
const providerID = "phanthywork"

// ProviderID 导出上游标识，供装配层（cmd/server）使用。
const ProviderID = providerID

// DefaultAPIBase 上游基址（冻结常量）。
//
// 留成常量而不是写死在 client 里：测试与自建反代要靠它拼。
const DefaultAPIBase = "https://code.phanthy.com"

// DefaultUA 默认 User-Agent（**冻结常量**）。
//
// ⚠ 这**是**坑②：官方客户端是 Bun 编译的二进制，UA 就是运行时默认值。
// 不是 Bun/1.3.14（Node 默认、包名/版本、浏览器 UA 都算）⇒
// 403 {"error_code":"TM.00000001","error_msg":"网络繁忙，请稍后重试"}，
// 而那句文案说"网络繁忙"，重试和换账号都没用，完全看不出是 UA 的锅。
//
// ⚠ 这条值是**具体版本**，不是 `bun/*` 通配。
const DefaultUA = "Bun/1.3.14"

// InferenceTokenTTL 推理令牌的 TTL（冻结常量，180 秒）。
//
// 报告 §2.5 实测：`exp - iat = 1794444404 − 1794444224 = 180`。
// ⚠ 它比 access_token 的 1800 秒短**一个数量级**：不换就用不了的典型症状是
// 403 request_not_authorized（见坑④），而那个错误码同时由"签名错"触发，
// 所以必须主动换、不能指望从错误码里看出原因。
const InferenceTokenTTL = 180 * time.Second

// AccessTokenTTL access_token 的寿命（**冻结常量**，1800 秒）。
//
// 报告 §3.3 实测：JWT 的 exp-iat = 1800，且有**三处独立印证**
// （JWT 自身 / 代码兜底 / 落盘时刻差值）。
//
// ⚠ 它与 InferenceTokenTTL 相差一个数量级，两者**不要混用**：
//
//	AccessTokenTTL    1800s —— Bearer（续期走 /oauth/token）
//	InferenceTokenTTL  180s —— 推理令牌（每次调模型前按 TTL+余量换新）
//
// 后台续期任务的扫描间隔由**这个**寿命算出来（该刷点 = 寿命的一半 = 900s），
// 而不是由推理令牌的 180s 算 —— 后者会把间隔压到分钟以下（每刷一次就轮换
// 一次 refreshToken，坑⑤）。
const AccessTokenTTL = 1800 * time.Second

// InferenceTokenMargin 换令牌的**提前量**。
//
// 抄报告 §3④ 给的那一行判据（余量不足 30 秒就重换）：
//
//	if payloadOf(token).exp * 1000 <= Date.now() + 30000 { token = await renewInference() }
//
// 留余量而不是"等它过期"：180 秒的 TTL 配一次完整请求（含换令牌、签名、
// 网络往返）足够把"刚好没过期"拖成"发出时已过期"。
const InferenceTokenMargin = 30 * time.Second

// OfficialClientVersion 注册 body 里的 version 字段（**官方客户端版本号**），
// 同时也是 `x-phanthy-client-version` 请求头的值。
//
// ⚠ 这**不是**本项目/插件版本号，写成 0.1.0 服务端不认。
//
// ★ 2026-11-13 由 2.48.0 升到 **2.51.0**。依据（二进制取证，见
// docs/phanthywork-inference-protocol.md 引的对照实现）：
//
//	「旧值 2.48.0 已经落后两个小版本：服务端把『来源=桌面端但版本对不上』
//	  的客户端当异常，风控会记账。」
//
// 官方桌面端发新版时要跟着改这一处（它同时影响注册 body 与推理请求头）。
const OfficialClientVersion = "2.51.0"

// Edition 注册 body 的 edition 字段（固定 phanthy_work）。
const Edition = "phanthy_work"

// clientSource 请求头 x-phanthy-client-source 的值。
//
// 与 UA 同一条判据：实测 200 的请求形态里它等于 "desktop"。
const clientSource = "desktop"

// anthropicVersion /v1/messages 的 anthropic-version 头。
//
// 上游是 Anthropic Messages 协议（报告 §0 摘要：模型通道是 Anthropic 格式，
// **不是** OpenAI 的 /v1/chat/completions）。
const anthropicVersion = "2023-06-01"

// ── 端点路径（全部来自报告，逐字）───────────────────────────────────────

const (
	// pathAuthorize 授权页（浏览器打开）。
	pathAuthorize = "/oauth/authorize"
	// pathOAuthToken 换 token 与续期**同一个路径**。
	//
	// ⚠ 不是 /v1/oauth/token —— 那个是内嵌 Anthropic 官方 OAuth 的路径，
	// 两条路互不相干（报告 §2.1）。
	pathOAuthToken = "/oauth/token"
	// pathProfile 账号档案（account.uuid 的来源）。
	pathProfile = "/api/oauth/profile"
	// pathRegister 设备注册（**写操作**）。
	pathRegister = "/api/oauth/desktop-installations/register"
	// pathActivitiesSummary 账号状态的唯一读入口（推理令牌 + server_time +
	// credits + daily + feature_flags，见 client.go 的 summaryResponse）。
	pathActivitiesSummary = "/api/oauth/activities/summary"
	// pathDailyLoginClaim 每日签到领取（**写操作**，但幂等：重复领回 200）。
	pathDailyLoginClaim = "/api/oauth/activities/daily-login/claim"
	// pathModels 云端模型目录（**只带 Bearer**）。
	pathModels = "/api/phanthy_code/models"
	// pathMessages 推理入口（**旧版 Anthropic Messages**）。
	//
	// ⚠ 2026-11-13 起这只是**兼容路径**：新版 Phanthy Codeo 内核（0.2.13）
	//    已经**完全不碰**它（抓包实证：一次都没出现）。默认走 pathChatCompletions。
	//    保留它是因为旧版 claude-sidecar 内核可能还在用，且切换成本低。
	pathMessages = "/v1/messages"
	// pathChatCompletions 推理入口（**OpenAI 兼容，新版内核唯一在用的那条**）。
	//
	// 抓包实证（`BUN_CONFIG_VERBOSE_FETCH=curl` 跑 `phanthy-codeo run`）：
	//   全程只出现两条 POST：
	//     POST /api/event_logging/batch
	//     POST /v1/chat/completions        ← 推理走这条
	//   `/v1/messages` **一次都没出现**。
	//
	// 协议特征（与 /v1/messages 的差异，逐条实测）：
	//   · body 是 **OpenAI 格式**（messages/stream/stream_options.include_usage），
	//     **不是** Anthropic 格式（没有 system 顶层字段、没有 anthropic-version）
	//   · **不签名**（没有 x-desktop-* / x-phanthy-proof-*）
	//   · 带 4 个会话头：x-phanthy-client / -project / -request / -session
	pathChatCompletions = "/v1/chat/completions"
)

// Protocol 推理协议选择（见 Config.Protocol / phanthywork.protocol）。
type Protocol string

const (
	// ProtocolChat 走 POST /v1/chat/completions（OpenAI 兼容，**新版内核**，默认）。
	//
	// body 与响应都**原样透传**：不再做 OpenAI→Anthropic 转换，也不再做
	// Anthropic SSE→OpenAI 转换 —— 上游本来就是 OpenAI 形状，转两次纯属自找 bug。
	ProtocolChat Protocol = "chat"
	// ProtocolMessages 走 POST /v1/messages（Anthropic + 12 段签名，**旧版内核**）。
	//
	// 保留给旧版 claude-sidecar 内核；新版内核用不到。
	ProtocolMessages Protocol = "messages"
)

// ParseProtocol 解析协议名（大小写不敏感，空值 = 默认 chat）。
func ParseProtocol(s string) Protocol {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "messages", "message", "anthropic", "v1messages":
		return ProtocolMessages
	default:
		return ProtocolChat
	}
}

// ── 超时（逐个端点，别用一刀切）──────────────────────────────────────────

const (
	// httpTimeout 普通 JSON 请求。
	httpTimeout = 20 * time.Second
	// exchangeTimeout 登录换 token（官方客户端用 20 秒）。
	exchangeTimeout = 20 * time.Second
	// refreshTimeout 续期的**单次**超时（官方客户端 8 秒 × 最多 3 次）。
	refreshTimeout = 8 * time.Second
	// refreshCallTimeout 一次续期调用的总预算（含重试与退避）。
	refreshCallTimeout = 45 * time.Second
	// registerTimeout 设备注册（官方实测约 1 秒，给足余量）。
	registerTimeout = 20 * time.Second
	// summaryTimeout 换推理令牌。
	summaryTimeout = 20 * time.Second
	// modelsTimeout 拉模型目录。
	modelsTimeout = 20 * time.Second
	// inferenceTimeout 调模型（**请求建立**阶段的超时；流本身不限时）。
	//
	// ⚠ 不能给 http.Client 设总超时：SSE 流会被总时长掐断
	//（那是"跑一会儿全卡住"的经典成因）。
	inferenceTimeout = 120 * time.Second
)

// 续期的几个小参数。
const (
	// refreshMaxAttempts 续期重试上限（官方实现是 3 次，仅 429/5xx）。
	refreshMaxAttempts = 3
	// maxBackoff 退避上限（官方实现 >2000ms 就放弃，这里给到 2 秒级）。
	maxBackoff = 2 * time.Second
	// refreshLead 判"要不要续"的**兜底**提前量。
	//
	// ⚠ 它只在"上游给不出窗口"（RefreshSkew 回 ok=false）时使用 ——
	// 真正的主判据是 RefreshSkew（"剩余 ≤ 寿命一半"），由 needsRefresh 统一实现。
	// 两处判据不一致的后果见 needsRefresh 的注释（"后台每轮空转、日志还写成功"）。
	refreshLead = 2 * time.Minute
	// refreshWaitCap 被并发闸合并后等待别人刷完的上限。
	refreshWaitCap = 30 * time.Second
	// inferenceTokenWaitCap 等别人换推理令牌的上限。
	inferenceTokenWaitCap = 30 * time.Second
)

// ── Config / Provider ────────────────────────────────────────────────────

// Provider 实现 gateway.Provider 与若干可选扩展点。
//
// ⚠ **所有扩展点的方法都直接挂在 *Provider 上**（不是访问器）。
// 扩展点靠类型断言发现（gateway.ExtOf），而"返回接口的访问器方法不参与
// 方法集匹配" —— 本仓实测过四个上游因为写成访问器而在前端一直没有
// 「＋ 添加账号」按钮。login.go 顶部有一组编译期断言把这件事钉住。
type Provider struct {
	client  *Client
	authDir string

	// mu 保护下面四个注入点与本地 creds 表。
	mu sync.RWMutex

	// credSrc 核心给的**活凭证来源**（账号池里的 Secret）。
	//
	// 与 minimax / zcode / raccoon 同款：池子暴露的只是**核心投影**
	// （uid/nickname/filePath），而本包要的是 `*phanthywork.Auth`（含
	// access_token 与三把密钥）—— 那个东西在 `pool.SecretOf` 里。
	// 装配点：cmd/server 的 `p.SetCredentialSource(...)`。
	credSrc func(uid string) (gateway.Credential, bool)

	// uidEnumerator 核心给的 uid 枚举。
	//
	// ⚠ 为什么不能只读本包的 creds：生产上凭证由注入的凭证源提供，
	// `creds` 只有测试会填。只读本地表会让后台任务在生产上永远扫不到账号
	// （而这是**静默**的：Jobs() 照常注册，Run 每轮空转）。
	uidEnumerator func() []string

	// creds 本实例自己维护的凭证表（**只有测试会填**）。
	//
	// 保留它的理由是测试需要一条"不起池子也能验后台任务"的路；
	// 生产路径一律走 credSrc（见 cred 的顺序）。
	creds map[string]*Auth

	// protocol 推理协议（"" / chat = 新版内核的 /v1/chat/completions；
	// messages = 旧版内核的 /v1/messages + 签名）。见 Protocol / SetProtocol。
	protocol Protocol

	// checkin 签到能力的共享驱动（internal/dailycheckin）。
	//
	// nil = 还没构造（构造器里调 initCheckin；SetCheckinLog 也会重建）。
	checkin *dailycheckin.Driver
	// checkinLog 签到历史日志（装配层注入；nil = 不写历史）。
	checkinLog *checkinlog.Log
	// checkinDisabled 核心给的"这个号已被禁用"判据（nil = 全部视为可用）。
	checkinDisabled func(uid string) bool

	// quotaAuthFailed 最近一次额度查询"是否因凭据失效而失败"（uid → bool）。
	//
	// 它就是 gateway.QuotaAuthFailureExt 的存储：核心在一次额度刷新的收尾处问它，
	// 据此决定"该不该摘号"。成功查询会**清掉**对应条目（否则一个重登后的号会被一直误判）。
	quotaAuthFailed map[string]bool

	// jobsRanOnce 后台任务是否跑过至少一轮。
	//
	// 用它实现"启动时不一次性刷所有账号"（见 runRefresh 的注释）。
	jobsRanOnce bool

	// loginOnce / login 缓存**同一个** loginFlow 实例（sessions map 挂在它身上，
	// 每次新建就等于每次一份空 map ⇒ Poll 永远找不到 state）。
	loginOnce sync.Once
	login     *loginFlow

	// inferGates 换推理令牌的并发闸（**按账号键**）。
	//
	// 与 Auth.gate（续期闸）分开：两件事的键与语义不同 —— 续期是
	// "refreshToken 轮换，同账号必须串行"，换令牌是"180 秒一小换，合并掉
	// 重复请求就够了"。混用一张表会让"刷新成功"误挡"换令牌"。
	inferMu    sync.Mutex
	inferGates map[string]*refreshGate

	// inviteMu / inviteRows 邀请码面板的**当日行缓存**（invite.go）。
	//
	// ⚠ 它存在的唯一目的是护栏②：取码是**写**接口（POST /referrals/code，get-or-create），
	//   同一账号同一自然日最多只能打一次 ⇒ 命中缓存就不再调上游。
	//   懒初始化（见 storeInviteRow）：构造器不碰它，零值可用。
	inviteMu   sync.Mutex
	inviteRows map[string]*inviteRow
}

// SetCredentialSource 装配活凭证来源（**生产路径**：池里的 Secret）。
//
// 与 minimax 的同名方法形态一致 —— 核心侧（cmd/server）按同一个约定接线，
// 换上游时不需要改核心。
func (p *Provider) SetCredentialSource(fn func(uid string) (gateway.Credential, bool)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.credSrc = fn
	p.mu.Unlock()
}

// SetUIDEnumerator 装配 uid 枚举来源（核心知道池里有哪些号）。
func (p *Provider) SetUIDEnumerator(fn func() []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.uidEnumerator = fn
	p.mu.Unlock()
}

// SetLocalCredential 给本实例塞一份凭证（**测试专用**：不起池子也能验后台任务）。
func (p *Provider) SetLocalCredential(uid string, a *Auth) {
	if p == nil || uid == "" {
		return
	}
	p.mu.Lock()
	if p.creds == nil {
		p.creds = map[string]*Auth{}
	}
	p.creds[uid] = a
	p.mu.Unlock()
}

// cred 取某 uid 的**活凭证**。
//
// 顺序：先问注入的凭证源（生产路径：池里那个 `*Auth`），再回落本实例的
// creds（测试路径）。**反过来会让生产永远读到空 map**（那正是 zcode 上
// 踩过的坑：端点返回 200 + 空账号列表，看起来像"没有账号"）。
func (p *Provider) cred(uid string) *Auth {
	if p == nil || uid == "" {
		return nil
	}
	p.mu.RLock()
	src := p.credSrc
	local := p.creds[uid]
	p.mu.RUnlock()

	if src != nil {
		if c, ok := src(uid); ok {
			if a, ok2 := c.Secret.(*Auth); ok2 && a != nil {
				return a
			}
		}
	}
	return local
}

// Config Provider 的可选依赖，**全部可缺省**。
//
// 零值 Config 必须给出一个"能跑契约测试的最小实例"：默认指向生产上游、
// 没有凭证目录。任何字段缺失都**降级**而不是 panic。
type Config struct {
	// AuthDir 本上游凭证的落盘目录（供 gateway.AuthDirExt 用）。
	//
	// 按上游分子目录的部署里它是 `<auths>/phanthywork`。
	AuthDir string
	// APIBase 上游基址。空 = DefaultAPIBase。
	APIBase string
	// UA User-Agent。空 = DefaultUA（**不要**传空字符串以外的值去"绕"坑②）。
	UA string
	// ClockSkewMS 时钟偏移基线（毫秒，本机 - 服务端）。
	//
	// 默认 0：偏移由 activities/summary 的 server_time **自动校准**（坑①）。
	// 这个字段只是给"本机确实有固定偏移、且不想等第一次换令牌"的部署用。
	ClockSkewMS int64
	// Protocol 推理协议："" / "chat" = /v1/chat/completions（默认，新版内核）；
	// "messages" = /v1/messages（旧版内核，Anthropic + 签名）。
	//
	// 配置键：`phanthywork.protocol`。
	Protocol string
}

// NewProvider 契约测试入口（**冻结签名**）。
//
// ⚠ 名字不能叫 New：本包的 New() 是 HTTP 客户端（见 client.go），
// 与 Provider 是不同的东西，同名会编译冲突。
func NewProvider(cfg Config) *Provider {
	c := New()
	if cfg.APIBase != "" {
		c.APIBase = cfg.APIBase
	}
	if cfg.UA != "" {
		c.UA = cfg.UA
	}
	// ⚠ **委托给 NewWithClient，不要在这里再拼一遍 &Provider{…}**。
	//
	// 这里曾经自己拼了一份构造，于是漏掉 `initCheckin()`（而 NewWithClient 有）⇒
	// 用契约工厂造出来的实例 `DailyActions()` / `CheckinAll()` **恒空**：manifest 的
	// daily_actions 里没有本上游 ⇒ 卡片上「全部签到」不亮；而契约测试全绿（它只看 4 个方法）。
	// 生产路径走 main.go（那边会注入 checkinLog 并重跑 initCheckin）所以不受影响 ——
	// 这正是"只在工厂形态下存在、所有拿工厂当生产的守卫集体失明"的那类洞。
	// 断言：checkin_auto_test.go 的 TestContractFactoryConstructsCheckinDriver（先红后绿）。
	//
	// 构造**不做网络请求**这条硬要求不变：initCheckin 只是组装共享驱动。
	return NewWithClient(c, cfg.AuthDir, cfg.ClockSkewMS)
}

// NewWithClient 用给定客户端构造（测试用：把基址指向假上游）。
//
// 契约测试注册的是 `func() gateway.Provider { return NewProvider(Config{}) }`，
// 所以构造必须**不做任何网络请求**（这是契约的硬要求：构造不 panic、
// 也不该有副作用）。
func NewWithClient(c *Client, authDir string, skewMS int64) *Provider {
	if c == nil {
		c = New()
	}
	if c.APIBase == "" {
		c.APIBase = DefaultAPIBase
	}
	if c.UA == "" {
		c.UA = DefaultUA
	}
	c.setSkew(skewMS)
	p := &Provider{client: c, authDir: authDir, creds: map[string]*Auth{}, inferGates: make(map[string]*refreshGate, 2)}
	p.protocol = ProtocolChat // 默认走新版内核的 OpenAI 兼容路径
	p.initCheckin()
	return p
}

// SetProtocol 设置推理协议（"chat" 默认 / "messages" 旧版）。
//
// 为什么做成 setter 而不是改 NewWithClient 签名：后者被一批契约/单元测试直接调用，
// 改签名会把"协议选择"这个纯编排决策扩散成全仓改动。默认值放在构造函数里，
// 只有需要旧版 Anthropic 路径的部署才显式设。
func (p *Provider) SetProtocol(name string) {
	if p == nil {
		return
	}
	p.protocol = ParseProtocol(name)
}

// Protocol 当前生效的推理协议。
func (p *Provider) Protocol() Protocol {
	if p == nil || p.protocol == "" {
		return ProtocolChat
	}
	return p.protocol
}

// protocolPath 当前协议对应的推理路径。
func (p *Provider) protocolPath() string {
	if p.Protocol() == ProtocolMessages {
		return pathMessages
	}
	return pathChatCompletions
}

// Client 上游 HTTP 客户端（供装配层与测试使用）。
func (p *Provider) Client() *Client { return p.client }

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明（**只声明可验证的**）。
//
//	CapChat   必须（所有上游都要能对话）
//	CapModels 支持动态模型目录（Models() 在任何情况下都返回非空）
//
// 少报是安全的、多报会被契约测试当场抓住（见包注释里的 follow-up 清单）。
func (p *Provider) Caps() gateway.Capability {
	// ⚠ 声明即承诺（多报会被契约测试当场抓住）。这里的每一项都有对应实现与断言：
	//
	//	CapChat         Provider.Chat 打通（真机 200，见 preparebody_test.go 的 502 回归）
	//	CapModels       Models() 在任何情况下都返回非空（含无凭证）
	//	CapQuotaProbe   QuotaExt.QueryQuota：activities/summary 读额度（quota.go）
	//	CapCheckin      每日签到：共享驱动 + 单/全量端点（checkin.go）
	//	CapImport       批量导入：粘贴官方/网关/扁平三种形态（import.go）
	//	CapInvite       邀请码面板：GET /admin/phanthywork/invite（invite.go）
	//	                ⚠ 它是**读了会写**的面板 —— 取码只能用 POST /referrals/code（平台没有只读字段，
	//	                2026-11-12 取证：GET 该路径回 405），护栏见 invite.go 的文件头。
	return gateway.CapChat | gateway.CapModels | gateway.CapQuotaProbe |
		gateway.CapCheckin | gateway.CapImport | gateway.CapInvite
}

// AuthDir 本上游的凭证落盘目录（gateway.AuthDirExt）。
//
// ⚠ 读与写必须问**同一个**目录：核心的 pollViaFlow 用它落盘，
// 装配层的 syncXxxAccounts 用它读 —— 两边不一致的形态是
// "写成功但池子里没有"（用户看到账号没进池，而日志说成功）。
func (p *Provider) AuthDir() string {
	if p == nil {
		return ""
	}
	return p.authDir
}

// DisplayName 界面中文名（gateway.DisplayNameExt）。
func (p *Provider) DisplayName() string { return "PhanthyWork" }

// ── 凭证投影（gateway.CredentialLoader / CredentialSecretLoader）──────────

// LoadCredentials 读取 dir 下全部凭证（只投影 uid/nickname/file）。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		// ⚠ 给每份凭证补上配置里的时钟基线：从磁盘读出来的 *Auth 不会经过
		// 构造器（json.Unmarshal 直接造），所以 baseSkewMS 默认是 0 ——
		// 那样 Config.ClockSkewMS 对"文件在盘上"的部署就完全失效了。
		// 校准（applyServerTime）之后会覆盖它，所以这里只是"还没校准时的兜底"。
		a.baseSkewMS = p.client.baselineSkewMS()
		out = append(out, p.project(a))
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials **同源**，但额外给出 Secret。
//
// ⚠ 两者必须读同一份对象：各造一份会让"轮换式 refreshToken 被消费两次"。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		a.baseSkewMS = p.client.baselineSkewMS() // 同上：磁盘加载的凭证也要有基线
		out = append(out, gateway.CredentialSecret{Credential: p.project(a), Secret: a})
	}
	return out, nil
}

// project 做池子投影。
//
// ⚠ **只给 {Provider, UID, Nickname, FilePath}**：别顺手抄 ExpiresAt ——
// 投影是启动快照，而续期会原地更新 secret 那份，两者必然分叉，
// 喂一个会过期的假值比 0 更糟（过期权威只走 CredentialExpiryExt）。
func (p *Provider) project(a *Auth) gateway.Credential {
	if a == nil {
		return gateway.Credential{}
	}
	return gateway.Credential{
		Provider: providerID,
		UID:      a.UID(),
		Nickname: a.Nickname(),
		FilePath: a.FilePath,
	}
}

// effectiveDir 解析生效的凭证目录。
func (p *Provider) effectiveDir(dir string) string {
	if dir != "" {
		return dir
	}
	if p != nil {
		return p.authDir
	}
	return ""
}

// ── 过期 / 令牌（CredentialExpiryExt / CredentialTokenExt）───────────────

// TokenExpiry 报告凭证的到期时刻（**Unix 秒**）。
//
// ⚠ 单位是秒（契约要求），而本包内部字段是毫秒 —— 转换在 Auth.ExpiresAt
// 里发生一次。返回 (0,false) 表示"不知道"，前端渲染 `—`，
// **绝不**返回 (0,true)（那会被渲染成"1970 年已过期"，比不显示更糟）。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return 0, false
	}
	return a.ExpiresAt()
}

// HasToken 报告这份凭证是否带可用的 access token（只读、不发网络）。
func (p *Provider) HasToken(cred gateway.Credential) bool {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return false
	}
	return a.OAuth.AccessToken != ""
}

// ── Chat ─────────────────────────────────────────────────────────────────

// Chat 转发一次对话（gateway.Provider 的第 3 个方法）。
//
// # 处理链（每一步都是报告里的一个坑）
//
//	① ctx 取消 → 立刻返回（契约要求"不尊重取消会占满并发额度"）
//	② 换 body：OpenAI → Anthropic（上游只认 /v1/messages），并**剥掉**
//	   phanthywork/ 前缀（坑③：带前缀 ⇒ 403 model_not_allowed）
//	③ 确保设备注册过（409 = 终态，直接失败）
//	④ 确保推理令牌新鲜（坑④：TTL 只有 180 秒，余量不足就换新）
//	⑤ 12 段 proof 签名（坑⑦⑨⑩⑫ 全在这一步）
//	⑥ 原样发出**最终字节**（坑⑧：中途不能重新序列化）
//
// 非 2xx **也算正常返回**（把上游错误体带回来给核心分类），只有网络层
// 失败才返回 error —— 与 gateway.ChatStream 的契约一致。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	if a.OAuth.AccessToken == "" {
		return gateway.ChatStream{}, errNoAccessToken
	}

	if p.Protocol() == ProtocolMessages {
		return p.chatMessages(ctx, a, body)
	}
	return p.chatCompletions(ctx, a, body)
}

// chatCompletions 走 POST /v1/chat/completions（OpenAI 兼容，**新版内核**，默认路径）。
//
// # 与 chatMessages 的三点本质差异（全部来自抓包实证）
//
//	① **不签名**：客户端全程不发 x-desktop-* / x-phanthy-proof-*，
//	   也**不调** activities/summary 换推理令牌 —— 直接拿 access_token 当 Bearer。
//	   ⇒ 本路径**不依赖** ensureInferenceToken / buildProof，少两个失败点。
//	② 请求体**不做协议转换**（见 PrepareBodyChat）。
//	③ 响应是 **OpenAI SSE**，原样透传给核心 —— 不像 chatMessages 要 Anthropic→OpenAI 翻译。
//
// 会话头（4 个）的含义（抓包原文可见）：
//
//	x-phanthy-client   = "cli"            客户端类型
//	x-phanthy-project  = "global"         项目作用域
//	x-phanthy-request  = uuid             本次请求 id（服务端日志/去重用）
//	x-phanthy-session  = "ses_<cuid2>"    会话 id
func (p *Provider) chatCompletions(ctx context.Context, a *Auth, body []byte) (gateway.ChatStream, error) {
	outBody, err := PrepareBodyChat(body)
	if err != nil {
		// 整形失败是**本地请求体的问题**（各账号一致），不该换号重试。
		return gateway.ChatStream{}, err
	}

	// ⚠ 不用 defer cancel()：成功路径要把 cancel 交给流（与 chatMessages 同一条纪律，
	// 见那里的注释与 TestStreamSurvivesAfterChatReturns）。
	reqCtx, cancel := context.WithTimeout(ctx, inferenceTimeout)
	resp, err := p.client.ChatCompletionsStream(reqCtx, a, pathChatCompletions, outBody)
	if err != nil {
		cancel()
		return gateway.ChatStream{}, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// ✅ 上游回的就是 OpenAI SSE ⇒ **原样透传**，一个字节都不改。
		return gateway.ChatStream{Status: resp.StatusCode, Body: cancelOnClose(resp.Body, cancel)}, nil
	}
	// 非 2xx：读完错误体、关连接，然后**正常返回**（核心要分类它）。
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return gateway.ChatStream{Status: resp.StatusCode, Body: io.NopCloser(bytes.NewReader(raw))}, nil
}

// chatMessages 走 POST /v1/messages（Anthropic + 12 段签名，**旧版内核**）。
//
// 保留给旧版 claude-sidecar 内核；新版内核（0.2.13+）已完全不用这条路径。
func (p *Provider) chatMessages(ctx context.Context, a *Auth, body []byte) (gateway.ChatStream, error) {
	outBody, err := PrepareBody(body)
	if err != nil {
		// 转换失败是**本地请求体的问题**（各账号一致），不该换号重试。
		return gateway.ChatStream{}, err
	}

	token, err := p.ensureInferenceToken(ctx, a)
	if err != nil {
		var ue *UpstreamError
		if errors.As(err, &ue) && ue.Status != 0 {
			// 上游给了错误体 = 业务错误：仍按流返回（让核心分类器判）。
			return gateway.ChatStream{Status: ue.Status, Body: io.NopCloser(bytes.NewReader([]byte(ue.Body)))}, nil
		}
		return gateway.ChatStream{}, err
	}

	// ⚠ 这里**不能**用 defer cancel()：成功路径要把 cancel 交给流（见 cancelOnClose），
	// 而 defer 会在函数返回的瞬间取消 reqCtx —— 那会把刚拿到的 SSE 流掐断。
	// 症状是"短回答能出来、长回答半路断"（体量小的时候读得到，因为 Go 已经缓冲了）。
	// 这是自查时发现的真 bug，回归断言见 TestStreamSurvivesAfterChatReturns。
	reqCtx, cancel := context.WithTimeout(ctx, inferenceTimeout)
	proof, err := p.client.buildProof(a, http.MethodPost, pathMessages, outBody, token, nowMS())
	if err != nil {
		cancel()
		return gateway.ChatStream{}, err
	}
	resp, err := p.client.ChatStream(reqCtx, a, pathMessages, outBody, proof)
	if err != nil {
		// 网络层失败：连流都没有，直接取消并返回 error。
		cancel()
		return gateway.ChatStream{}, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// ⚠⚠ **必须在这里把 Anthropic SSE 翻成 OpenAI SSE**（本包内，不能留给核心）。
		//
		// 上游的入口是 /v1/messages，回的是 Anthropic 帧（content_block_delta / thinking_delta /
		// text_delta）；而核心按 **OpenAI 帧**解析 —— wire.Aggregate / wire.Stream 只认
		// `choices[].delta.content` 与 `delta.reasoning_content`。两边对不上时的形态是
		// **静默空 content**：200、不报错、不中断、SSE 也完整，客户端只拿到 `content:""`，
		// 而 `reasoning_content` 连字段都不存在（转换前它从未进过聚合器）。
		//
		// 2026-11-12 真机 7 次实测（phanthywork/glm-5.3-flash ×5、kimi-k3 ×1、
		// deepseek-v4.1-flash-1017 ×1）：**全部 content=""**；而同一模型同一提示词走
		// 另一条带转换的探针路，256/1024 各拿到过正文。⇒ 「同一请求、两条路、一个有正文
		// 一个没有」的唯一差别就是这一步转换。
		//
		// ⚠ 同一个坑本仓踩过三次，读这一行的人请顺手看看那三处（它们都是对的）：
		//
		//	internal/minimax/provider.go:195-211   （注释里记着"非流式恒回 SSE ⇒ 空 content"）
		//	internal/zcode/provider.go:413-419     （JWT 通道）
		//	internal/monkeycode/provider.go:319
		//
		// **一个上游的响应协议属于它自己的职责** —— 核心不认识任何具体上游的协议。
		pr, pw := io.Pipe()
		go func() {
			defer resp.Body.Close()
			cerr := anthroconv.AnthropicSSEToOpenAI(resp.Body, pw)
			// CloseWithError 把转换错误交给读端 ⇒ 核心按"上游流解析失败"回 502，
			// **不会**伪装成一个空回答（"转换器有 bug" 绝不能变成 "模型答了空话"）。
			_ = pw.CloseWithError(cerr)
		}()
		// cancel 仍挂在读端的 Close 上（与改前同一语义：读完/关流时才释放请求上下文）。
		return gateway.ChatStream{Status: resp.StatusCode, Body: cancelOnClose(pr, cancel)}, nil
	}
	// 非 2xx：读完错误体、关掉连接，然后**正常返回**（核心要分类它）。
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return gateway.ChatStream{Status: resp.StatusCode, Body: io.NopCloser(bytes.NewReader(raw))}, nil
}

// cancelOnClose 把 cancel 挂到 Close 上（成功路径必须手动管理生命周期）。
func cancelOnClose(rc io.ReadCloser, cancel context.CancelFunc) io.ReadCloser {
	return &cancelReadCloser{rc: rc, cancel: cancel}
}

// cancelReadCloser 在 Close 时同时取消请求上下文。
type cancelReadCloser struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

// Read 透传。
func (c *cancelReadCloser) Read(b []byte) (int, error) { return c.rc.Read(b) }

// Close 关流并取消上下文（**二次 Close 安全** —— 契约要求）。
func (c *cancelReadCloser) Close() error {
	err := c.rc.Close()
	c.once.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
	})
	return err
}

// PrepareBodyChat 新版内核（/v1/chat/completions）的请求体整形。
//
// 与 PrepareBody 的**根本区别：不做协议转换**。
//
// 上游 `/v1/chat/completions` 本来就是 OpenAI 格式，出口层也是 OpenAI 格式 ——
// 中间再 OpenAI→Anthropic 转一次，等于把同一份东西翻译两遍，凭空制造 bug 面。
//
// 只做两处**必要**改写（都用 RawMessage，其余字段逐字节保留）：
//
//	① model 剥掉 `phanthywork/` 前缀（出口层带前缀分派，上游只认裸名）
//	② stream 强制 true —— 与 PrepareBody 同一条理由（见那里的长注释）：
//	   出口层按 SSE 解析，而上游按 body 的 stream 字段决定回 SSE 还是一次性 JSON；
//	   把 false 透传上去只会得到「upstream stream contained no valid data events」502。
//
// ⚠ **不要**在这里改 max_tokens 等字段：新版内核把 max_tokens 原样传给上游
// （抓包实测 32000 直传），我们照做即可。
func PrepareBodyChat(openAIBody []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(openAIBody, &m); err != nil {
		// 非 JSON 属于异常，但**不要**在这里吞掉或臆造：
		// 原样发出去让上游回它自己的错误，比本地猜一个错误码更有信息量。
		return openAIBody, nil
	}
	changed := false
	// ① stream 强制 true
	if v, ok := m["stream"]; !ok || string(v) != "true" {
		m["stream"] = json.RawMessage("true")
		changed = true
	}
	// ② model 剥前缀
	if mv, ok := m["model"]; ok {
		var model string
		if json.Unmarshal(mv, &model) == nil {
			if bare := stripProviderPrefix(model); bare != model {
				m["model"] = json.RawMessage(strconv.Quote(bare))
				changed = true
			}
		}
	}
	if !changed {
		return openAIBody, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: phanthywork: chat 请求体整形失败: %v", gateway.ErrConvertFailed, err)
	}
	return out, nil
}

// PrepareBody 把出口层的 OpenAI 请求体编成上游要的形态（**只序列化一次**）。
//
// 三步，顺序**不能换**：
//
//	① 按 OpenAI 里的模型名取输出上限（anthroconv 需要它填 max_tokens）
//	② OpenAI → Anthropic（上游的入口是 /v1/messages）
//	③ 在转换产物上做**两处必要的改写**：
//	     · stream 强制 true   ← ★ 真机踩出来的 502 根因，见下
//	     · model 剥掉 phanthywork/ 前缀（坑③：带前缀 ⇒ 403 model_not_allowed）
//
// ⚠ 第 ③ 步之后**必须**用这一步的产物去签名与发送（坑⑧：签名覆盖最终字节）。
// 所以本函数是整条链上**唯一**一次"改完再序列化"，之后不再有任何改写 ——
// 这是 crypto.go 的 RequestBodyForHash 那条纪律的落点。
//
// # ★ 为什么必须强制 stream=true（真机 502 upstream_parse 的根因）
//
// 上游的流式与否由 **body 的 stream 字段**决定，不是由 Accept 头决定
// （client.go 里那个 Accept: text/event-stream 只影响响应协商）。
// 而 anthroconv 对 stream 是**原样透传**的（见 anthropic.go 那句
// `for _, k := range []string{"temperature","top_p","stream"}`）。
// 于是客户端发 stream:false（或缺省）时，上游回的是**一次性 JSON**，
// 而出口层按 SSE 解析 ⇒ 一个事件都读不到 ⇒
//
//	HTTP 502 {"error":{"code":"upstream_parse",
//	           "message":"upstream stream contained no valid data events"}}
//
// 本仓早有记录（anthroconv 的测试注释写着「stream:false 恒 502」），
// codar 是标准修法（`m["stream"] = json.RawMessage("true")` + 回归断言）：
// **出口层自己有 Aggregate 路径**把 SSE 折成非流式响应，所以"把客户端的
// false 透传给上游"没有任何收益，只有这一条 502。
//
// ⚠ 用 map[string]json.RawMessage 而不是解成结构体再拼回去：
// RawMessage 让**其余字段逐字节保留**（数字精度、嵌套结构、未知字段都不动），
// 只改我们明确要改的那两个键。非 JSON 产物原样透传，让上游回它自己的错误。
func PrepareBody(openAIBody []byte) ([]byte, error) {
	limit := anthroconv.MaxOutputFor(modelOfBody(openAIBody))
	converted, err := anthroconv.OpenAIToAnthropicWithLimit(openAIBody, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: phanthywork: OpenAI→Anthropic 转换失败: %v", gateway.ErrConvertFailed, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(converted, &m); err != nil {
		// 转换产物不是 JSON 属于异常，但**不要**丢掉它：原样发出去由上游报错，
		// 比在这里吞掉更有信息量。
		return converted, nil
	}
	changed := false
	// ① stream 强制 true（**注意**：这一步必须在签名之前）
	if v, ok := m["stream"]; !ok || string(v) != "true" {
		m["stream"] = json.RawMessage("true")
		changed = true
	}
	// ② model 剥前缀（坑③）
	if mv, ok := m["model"]; ok {
		var model string
		if json.Unmarshal(mv, &model) == nil {
			if bare := stripProviderPrefix(model); bare != model {
				m["model"] = json.RawMessage(strconv.Quote(bare))
				changed = true
			}
		}
	}
	if !changed {
		// 已经是最终形态：**原样返回**，连一次多余的 Marshal 都不做。
		return converted, nil
	}
	out, merr := json.Marshal(m)
	if merr != nil {
		return converted, nil
	}
	return out, nil
}

// modelOfBody 从 OpenAI 请求体里取模型名（取不到返回空串）。
func modelOfBody(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// ── 设备注册（一次性写操作）──────────────────────────────────────────────

// ensureReady **所有需要设备身份的出站调用的统一入口**。
//
// # 它修的是什么（2026-11-13 用户报障：手动新加的账号被自动禁用）
//
//	本包的设备注册是**懒注册**，而原先只有 Chat 那条路会走到 `ensureInstallation`；
//	额度探测 / 签到 / 取邀请码都"直接拿本地派生的 installationId 去签名"（`installationID()`
//	是本地派生的、永远非空）⇒ 新账号这些路径必然打到上游但**从未注册** ⇒ 上游拒 ⇒
//	而拒的原因被判成"账号层失效" ⇒ 撞上核心的"一次即摘"，一个 oauth 完全健康的号被禁。
//
// ⇒ 判据只留这一处：**需要设备身份的路径一律先调它**，别再各写一份（本仓"两处判据打架"的教训）。
//
// ⚠ 需要**推理令牌**的路径（Chat / 签到 claim）在这一步之后照旧调 `ensureInferenceToken` ——
//
//	那是另一个凭据（180 秒），不属于"设备就绪"。
func (p *Provider) ensureReady(ctx context.Context, a *Auth) error {
	if a == nil {
		return errNilAuth
	}
	return p.ensureInstallation(ctx, a)
}

// ensureInstallation 确保这份凭证已经完成设备注册。
//
// 三态：
//
//	已注册过            → 直接返回
//	熔断期内（含 409）  → 返回错误，**不打上游**
//	没注册过            → 打一次注册（并落盘状态）
//
// ⚠ 409 是**终态**（坑⑥）：记 ConflictAtMS 之后期内零请求 ——
// 每次启动都重发等于持续触发服务端设备风控，还可能把该账号的其它调用
// 一起拖下水（报告 §2.4 引开源实现的 24 小时熔断）。
func (p *Provider) ensureInstallation(ctx context.Context, a *Auth) error {
	if a.Reg.RegisteredAtMS > 0 {
		return nil
	}
	nowLocal := nowMS()
	// 熔断期内：不再发请求（成功前的尝试也受 24 小时窗口管）。
	if a.Reg.ConflictAtMS > 0 || a.Reg.LastAttemptMS > 0 {
		last := a.Reg.ConflictAtMS
		if a.Reg.LastAttemptMS > last {
			last = a.Reg.LastAttemptMS
		}
		if last > 0 && nowLocal-last < registrationCooldown.Milliseconds() {
			if a.Reg.ConflictAtMS > 0 {
				return fmt.Errorf("%w（熔断期内不再重试）", ErrRegistrationConflict)
			}
			return fmt.Errorf("phanthywork: 设备注册在熔断期内（上次尝试 %s 前），暂不重试",
				time.Duration(nowLocal-last)*time.Millisecond)
		}
	}
	// accountScope 是 device canonical 的必需段，而且**只有服务端知道权威值**：
	//
	//	报告 §2.4 把它的来源写成 `GET /api/oauth/profile` 的 account.uuid「原字符串」，
	//	并且专门列出一条错写法：用 JWT 的 sub / 邮箱代替 ⇒ 400 desktop_account_scope_invalid。
	//
	// 所以这里**每次注册前都问一次**（而不是「本地有就用本地的」）：
	// 注册是一次性写操作，多一次只读 GET 换「用对了权威值」是划算的；
	// 而用错值的代价是一条看不出原因的 400，加上 24 小时熔断。
	//
	// profile 拿不到时才回落到本地能推出来的那个（JWT 的 sub）—— 那是降级，不是等价。
	if scope, nickname, err := p.client.FetchAccountScope(ctx, a); err == nil {
		a.AccountScope = scope
		if a.NicknameValue == "" || a.NicknameValue == a.UID() {
			a.NicknameValue = nickname
		}
	} else if a.accountScope() == "" {
		return fmt.Errorf("phanthywork: 取不到 accountScope（签名必须要它）: %w", err)
	}
	resp, err := p.client.RegisterDevice(ctx, a)
	attemptAt := nowMS()
	a.Reg.LastAttemptMS = attemptAt
	if err != nil {
		if errors.Is(err, ErrRegistrationConflict) {
			a.Reg.ConflictAtMS = attemptAt
		}
		// 落盘失败不掩盖真实错误（状态记在内存里也已生效）。
		_ = saveAuthFile(a)
		return err
	}
	a.Reg.RegisteredAtMS = attemptAt
	a.Reg.ReasonCode = resp.RewardReason
	// ⚠⚠ **绝对不要**把服务端返回的 `installation.id` 覆盖到 `a.Device.InstallationID`。
	//
	// 2026-11-13 真机排查（当时症状：注册 200、换推理令牌也 200，但调模型恒 500 空体）：
	//
	//	注册响应 installation.id = "cmwf51f0r0yaeo93jz49b1is6"   ← 这是**安装记录 id**
	//	而签名要用的 installationId = "di_" + b64url(sha256(裸 32 字节 identity 公钥))
	//	                            = "di_URB32wqvod6Ve1EAZxJCwF05PdDgnn0Y9tJvO28Ok2o"
	//
	// 两者**不是同一个东西**。用服务端返回的那个去签 `desktop-device-v1`，
	// 服务端算出来的串必然不同 ⇒ 之后的推理请求全部 500（服务端在解析阶段抛异常，
	// 所以是空体 500，不是 403；一路查不到原因）。
	//
	// 对照实现（magpie-PhanthyCode 的 `production-auth.mjs` L13）写得很明确：
	//   「installationId 由 **identity 公钥**派生（di_ + base64url(sha256(裸 32 字节公钥))），
	//     与 accountScope 无关」
	// 且它的 `isInstallationReady` 把「id 与公钥派生值不一致」直接判为**未就绪** ——
	// 也就是说服务端返回的那个 id 从来不参与签名。
	//
	// 服务端返回的 id 只适合做日志/展示；签名一律用派生值（见 `a.installationID()`）。
	if id := strings.TrimSpace(resp.Installation.ID); id != "" && id != a.Device.InstallationID {
		// 仅记录，不改 a.Device.InstallationID —— 见上面的长注释。
		LOADEDInstallationNote(id)
	}
	_ = saveAuthFile(a)
	return nil
}

// LOADEDInstallationNote 记录服务端返回的安装记录 id（与签名用的 di_ 值不同，仅诊断用）。
//
// 单独一个函数是为了让上面那段"不要覆盖"的意图在调用点上足够显眼。
func LOADEDInstallationNote(serverInstallationID string) {
	_ = serverInstallationID // 当前只作为"读到这里"的标记；需要时接日志
}

// ── 换推理令牌（TTL 180 秒）──────────────────────────────────────────────

// inferGate 取该账号的换令牌并发闸（按 uid 隔离）。
func (p *Provider) inferGate(uid string) *refreshGate {
	p.inferMu.Lock()
	defer p.inferMu.Unlock()
	if p.inferGates == nil {
		p.inferGates = make(map[string]*refreshGate, 2)
	}
	if uid == "" {
		uid = "anon"
	}
	g, ok := p.inferGates[uid]
	if !ok {
		g = &refreshGate{}
		p.inferGates[uid] = g
	}
	return g
}

// ensureInferenceToken 返回一个**新鲜的**推理令牌（坑④ 的落地）。
//
// 判据链：
//
//	缓存里的令牌余量 > 30 秒 → 直接用它
//	否则                    → 确保注册过 → 打 activities/summary 换新
//
// ⚠ 换令牌的同时**用响应里的 server_time 校准时钟**（坑①）：
// 报告说"每次启动都要重新校准，不能缓存一个固定值"（本机时钟一直在漂），
// 而这里是本包唯一能拿到服务端时间的常规路径（除登录之外）。
// 校准必须在**这次请求之后**做（这次请求用的还是旧偏移），下一次生效 ——
// 所以"第一次请求带着本机时间 → 403"这个现象在**换令牌这一步就已经被
// 观察到**：本包把 server_time 的校准放在换令牌成功之后，
// 于是紧随其后的 /v1/messages 一定用的是校准后的时间。
func (p *Provider) ensureInferenceToken(ctx context.Context, a *Auth) (string, error) {
	nowLocal := nowMS()
	if a.inferenceTokenValid(nowLocal) {
		return a.cachedInferenceToken().Token, nil
	}
	if err := p.ensureInstallation(ctx, a); err != nil {
		return "", err
	}
	key := a.refreshKey()
	gate := p.inferGate(a.UID())
	// ⚠ replayOK=false：只合并**进行中**的换令牌请求，不回放上一个结果。
	//
	// 理由：180 秒的令牌"上一次成功"完全可能是已经过期的那一份，
	// 回放它等于把一次必然 403 的请求发出去（而回放的初衷是"省一次往返"，
	// 在这里省的是"正确性"）。这条是实测暴露的：手动把令牌改成过期后再调，
	// 回放窗口把旧令牌又送了回来，于是 summary 只被打了 1 次。
	call, initiator := gate.acquire(key, nowMS(), false)
	if !initiator {
		select {
		case <-call.done:
		case <-time.After(inferenceTokenWaitCap):
			return "", fmt.Errorf("phanthywork: 等待同账号的换推理令牌结果超时")
		}
		if call.err != nil {
			return "", call.err
		}
		if t := a.cachedInferenceToken().Token; t != "" {
			return t, nil
		}
		return "", fmt.Errorf("phanthywork: 换推理令牌被合并，但缓存里仍没有令牌")
	}
	resp, localMS, err := p.client.RequestInferenceToken(ctx, a)
	if err != nil {
		gate.finish(key, call, "", err)
		return "", err
	}
	// ★ 时钟校准（坑①）：用响应里的 server_time。
	//
	// 报告 §3① 的实测对照：本机时间 → 403 request_not_authorized；
	// server_time 校准 → 200。而那句文案**完全指不到时钟上**。
	if resp.ServerTime != "" {
		a.applyServerTime(resp.ServerTime, localMS)
		// 客户端口径也同步一次（登录流程与"还没账号"的路径用它）。
		p.client.setSkew(a.skew())
	}
	token := strings.TrimSpace(resp.DesktopInferenceToken)
	exp, ok := inferenceTokenExpiry(token)
	if !ok || exp <= 0 {
		// 令牌不是合法 JWT（缺 exp）：**保守按 TTL 兜底**而不是报错 ——
		// 报错会让一次可用请求白白失败，而"按 TTL 计时"最多多换一次令牌。
		exp = localMS + InferenceTokenTTL.Milliseconds()
	}
	a.storeInferenceToken(token, exp, localMS)
	gate.finish(key, call, token, nil)
	return token, nil
}

// inferenceTokenExpiry 从推理令牌（RS256 JWT，实测 813 字符）里读 exp（毫秒）。
func inferenceTokenExpiry(token string) (int64, bool) {
	c, ok := ParseJWTClaims(token)
	if !ok || c.Exp <= 0 {
		return 0, false
	}
	return int64(c.Exp) * 1000, true
}

// ── 辅助 ─────────────────────────────────────────────────────────────────

// authOf 把不透明 Secret 断言回本包的凭证类型。
//
// 断言失败给**明确错误**，不 panic（契约要求"任何输入都不得 panic"）。
// ⚠ `(*Auth)(nil)` 的断言**会成功**，所以 nil 检查不是多余的。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("phanthywork: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok {
		return nil, fmt.Errorf("phanthywork: 凭证类型不对，期望 *phanthywork.Auth，实际 %T", cred.Secret)
	}
	if a == nil {
		return nil, errNilAuth
	}
	return a, nil
}

// platformName 注册 body 的 platform 字段（windows / macos / linux）。
//
// 用 runtime.GOOS 直译而不是写死 windows：本包在别的平台上也应当是"对的"
// （写死 windows 在 macos 上跑会得到一个服务端认为一致的记录，但那不代表
// 真实平台 —— 报告把这三档列为客户端自报的事实）。
func platformName() string { return runtimeGOOS() }

// registerIdempotencyKey 设备注册的幂等键：`register:<installationId>`。
func registerIdempotencyKey(installationID string) string {
	return "register:" + installationID
}

// randomUUID 生成一个 uuid v4（x-request-id 用；报告实测是 uuid v4）。
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexdigits[v>>4], hexdigits[v&0x0f])
	}
	return string(out)
}
