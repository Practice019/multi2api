// provider.go 把 Loomy（讯飞 Loomy 桌面客户端的模型服务）适配为 gateway.Provider。
//
// # 本包在整条解耦链里的位置
//
// 它是**第三个 Adapter**，也是判据 1 的第二次实测：
//
//	第一个（workbuddy）→ 接缝还没被抽象出来
//	第二个（codearts） → 证明"加一个上游 = 加一个目录"，但它自己很重
//	第三个（本包）     → 证明"轻上游"同样成立：契约的最小实现只有 4 个方法，
//	                     其余能力**一个都不用实现**
//
// 对比三个上游的实现量（这组对比本身就是判据 1 的度量）：
//
//	workbuddy → Client + 提示词/身份伪造/签到/成长/旅行/活动/看板 + 22 条管理端点
//	codearts  → SDK-HMAC 签名 + DPoP + STS 续期 + 单所有者 store + 福利/订阅
//	loomy     → 一个 HTTP 转发 + 一份静态模型表 + 一个分类器
//
// # 本包实现的核心扩展点（本轮扩充过，见下）
//
//	实现：AuthDirExt / CredentialLoader(+Secret) / ErrorClassifier / ResetPolicyExt
//	      **AccountColumnsExt / QuotaExt / LoginFlow / AdminExt / CredentialLifetimeExt**
//	不实现：JobExt / CredentialRefresher / RefreshSkewExt / SoftRateExt
//	      / CredentialExpiryExt
//
// "不实现"不是"懒得做"，每一条都有具体理由：
//
//	JobExt              没有签到、没有续期、没有活动 —— 没有任何要定时做的事
//	CredentialRefresher 没有可刷的凭证（无 refresh token，见 credential.go）
//	RefreshSkewExt      与上一条同因：没有续期就没有"多早算该刷"
//	SoftRateExt         上游没有给出"限流何时解除"的信息，硬冷却即可
//	CredentialExpiryExt 它**没有可报的到期时刻**（session 不绑时间）。
//	                    实现一个恒 (0,false) 的空壳只会看起来"在做检查"；
//	                    正确的表达在 CredentialLifetimeExt（见 accountview.go）。
//
// # 本轮为什么把"不实现"改写成了"实现"（这是订正，不是扩张）
//
// 上一轮这四条"不实现"的理由分别是"没有管理端点 / 余额只在客户端本地缓存 /
// 登录在桌面客户端 / 没有到期时间"。前三条**共享同一个错误前提**：
// 它们都把"没有 HTTP 端点"当成了"没有数据"。
//
// 实测（见 clientstore.go 的注释）证明：客户端把登录态与积分摘要**明文缓存在
// 本机 Local Storage 里**。于是——
//
//	LoginFlow            读客户端已登录的那份 session → 有了"页内添加账号"
//	QuotaExt             读 loomy-points-summary       → 有了额度
//	AdminExt             把上面两件事的事实暴露成一条诊断端点
//	AccountColumnsExt    报自己那套列（去掉签到/Token 两列不适用的）
//	CredentialLifetimeExt 如实回答"这份凭证不会过期"
//
// 而"没有到期时间"那条理由**依然成立** —— 所以它留在"不实现"里，
// 换了另一种表达方式。这一条没有被推翻，只是换了个接口说同一件事。
//
// 这正是 gateway 把扩展点做成**可选类型断言**而不是 Provider 方法的价值：
// 一个纯转发的上游可以只写 100 行就接入，也可以后来**逐个**补上能力，
// 而不必为用不到的能力各写一个空实现。
package loomy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"

	"workbuddy2api/internal/gateway"
)

// providerID 上游标识。
//
// 会被用作：
//   - 模型名前缀（"loomy/deepseek-v4-flash-0731"）
//   - 配置里的 provider key
//   - 统计维度（by_provider）
//
// 格式受 gateway 约束（^[a-z][a-z0-9-]*$），有契约测试守着。
const providerID = "loomy"

// ProviderID 导出上游标识，供**装配层**（cmd/server）使用。
//
// 装配层要把 loomy 账号并入核心账号池（pool.SyncToDirWithSecrets 的
// provider 参数），并据此判断"这个号属于哪个上游"。
// 与另两个上游的同名常量同一理由：标识的唯一权威仍是 providerID，
// 导出面越小越好。
const ProviderID = providerID

// Provider 实现 gateway.Provider（以及若干可选扩展点，见包注释）。
type Provider struct {
	client *Client
	// authDir 凭证目录。供 AuthDirExt 使用 —— 核心据此按上游重载 auths。
	authDir string

	// clientDataDir 本机 Loomy 客户端的数据目录（Local Storage/leveldb）。
	//
	// 空串 = **自动探测**（见 effectiveClientDir / DefaultClientStoreDir）。
	// 之所以允许为空，是因为"客户端在哪"在两个平台上不一样、而且
	// 多数部署不需要配它；显式配置只是为了"网关与客户端不在同一台机器、
	// 但数据目录被同步过来了"这种少见形状。
	clientDataDir string

	// loginOnce / loginCached 缓存 LoginFlow 实例。
	//
	// ⚠ 必须缓存：每次新建会让 `sessions` map 各是一份空的，于是
	// start 存下的会话在 poll 里找不到（codearts 那边端到端实测抓过这个 bug）。
	// 见 login.go 的 LoginFlow()。
	loginOnce   sync.Once
	loginCached *loginFlow

	// sms 讯飞账号网关的短信登录客户端（手机号 + 验证码，见 smslogin.go）。
	//
	// 与 client（模型代理）是**两个完全不同的上游**：一个 loomyad.xunfei.cn、
	// 一个 account.xfinfr.com，鉴权方式也毫无关系（Bearer session vs HMAC-SHA1
	// 请求签名）。所以是两个字段、两个类型，不合成一个"上游客户端"。
	sms *SMSClient

	// loginMode 见 Config.LoginMode（已归一化：auto/sms/local）。
	loginMode string
}

// NewProvider 建一个 Loomy Provider（契约测试用的无依赖构造）。
//
// 只构造，不做网络请求 —— 契约测试会多次调用 factory，不能有副作用。
//
// ⚠ 名字不能叫 New：本包的 client.go 已经有 `New() *Client`（上游 HTTP 客户端），
// 两者是不同的东西，同名会编译冲突。契约测试里传的是**这个**函数。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// Config Provider 的可选依赖，全部可缺省。
//
// 零值 Config 得到一个"能跑契约测试的最小实例"：默认指向生产上游、
// 没有凭证目录。任何字段缺失都**降级**而不是 panic。
type Config struct {
	// Client 上游 HTTP 客户端。nil 时用 New()。
	Client *Client
	// BaseURL 上游基址。Client 为 nil 时用它构造客户端；
	// 两者都给时以 Client 为准（显式注入优先）。
	BaseURL string
	// AuthDir 凭证目录（如 `auths/loomy`）。
	AuthDir string
	// ClientDataDir 本机 Loomy 客户端的数据目录（Local Storage/leveldb）。
	//
	// 留空 = 自动探测（Windows `%APPDATA%\Loomy\...`，macOS/Linux 同理，
	// 见 DefaultClientStoreDir）。它决定三件事能不能做：
	//
	//	页内「添加账号」  读客户端已登录的 session
	//	「额度」列        读客户端缓存的积分摘要
	//	诊断端点         两者的事实
	//
	// ⚠ 自动探测返回空串时**不是错误** —— 网关跑在服务器上、客户端在
	// 用户机器上，是正常部署形态。此时前两件事如实降级成"做不到"：
	// 「添加账号」按钮**不出现**（Configured() 为 false），额度显示 `—`。
	ClientDataDir string

	// ── 短信登录（讯飞账号网关，见 smslogin.go）──────────────────────────
	//
	// 四个字段全部留空 = 用 smslogin.go 里那套**默认值**（生产网关 + GM3LOOMY
	// + 随客户端分发的 access key）。之所以给默认值而不是"必须配"：
	// 那对 key 本来就在安装包里以混淆形式明文存在，要用户去解一遍不合理。
	//
	// 覆盖口留在这里是为了两件事：
	//	· 上游换网关/换 key 时不用重新编译
	//	· 测环境（accounttest.xfinfr.com）与 hermetic 测试能指向假上游
	SMSBaseURL         string
	SMSAppID           string
	SMSAccessKeyID     string
	SMSAccessKeySecret string

	// LoginMode 登录方式：`auto`（默认）/ `sms` / `local`。
	//
	//	auto   先试本机拾取，不行再走手机号验证码（默认；不无谓发短信）
	//	sms    **只**走手机号验证码 —— 想加一个"另一个号"时必须用它：
	//	       本机客户端登录的是 A，auto 会一直把 A 加回来，永远加不到 B
	//	local  只允许本机拾取（不给表单页）
	//
	// 这个旋钮是**产品决策**，不放进上游协议：三条路径都是真的，
	// 只是"默认先走哪条"取决于用户想干什么。
	LoginMode string
}

// NewWithConfig 按配置建一个 Loomy Provider。
func NewWithConfig(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = NewWithBase(cfg.BaseURL)
	}
	return &Provider{
		client:        c,
		authDir:       cfg.AuthDir,
		clientDataDir: cfg.ClientDataDir,
		sms: NewSMSClient(cfg.SMSBaseURL, cfg.SMSAppID,
			cfg.SMSAccessKeyID, cfg.SMSAccessKeySecret),
		loginMode: normalizeLoginMode(cfg.LoginMode),
	}
}

// SetClient 替换上游 HTTP 客户端（启动期一次性注入）。
func (p *Provider) SetClient(c *Client) {
	if c != nil {
		p.client = c
	}
}

// SetAuthDir 替换凭证目录（启动期一次性注入）。
func (p *Provider) SetAuthDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		p.authDir = dir
	}
}

// SetClientDataDir 替换本机客户端数据目录（启动期一次性注入）。
//
// 显式配置优先于自动探测；空串**不会**覆盖已有值（与 SetAuthDir 同一条：
// "没配"与"显式清空"在这里不是两件需要区分的事，而空串传进来
// 让它回落到自动探测反而更符合调用方的意图）。
func (p *Provider) SetClientDataDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		p.clientDataDir = dir
	}
}

// ClientDataDir 暴露**配置里**的客户端数据目录（可能是空串）。
//
//	空串不等于"没有客户端" —— 那要看 effectiveClientDir 的自动探测结果。
//
// 这个方法只回报配置事实，供装配层与测试断言"配置有没有生效"。
func (p *Provider) ClientDataDir() string {
	if p == nil {
		return ""
	}
	return p.clientDataDir
}

// effectiveClientDir 决定实际使用的客户端数据目录：显式配置 > 自动探测。
//
// 自动探测**每次调用都做**（一次 os.Stat），而不是构造时缓存：
// 用户完全可能"先把网关跑起来、之后才装并登录客户端"。缓存会让那种顺序
// 需要重启网关才能用上「添加账号」—— 而重启是本项目一直在削的认知成本。
// 一次 stat 的代价远低于一个"为什么按钮不出现"的疑问。
func (p *Provider) effectiveClientDir() string {
	if p == nil {
		return ""
	}
	if d := strings.TrimSpace(p.clientDataDir); d != "" {
		return d
	}
	return DefaultClientStoreDir()
}

// smsClient 取短信登录客户端；从未注入时**就地兜一个默认实例**。
//
// nil 安全是必须的：契约测试会 `NewProvider()` 出一个零值 Provider 并
// 真调 `AdminRoutes()` 里的 handler —— 那条路径不能因为 sms 为 nil 而 panic。
func (p *Provider) smsClient() *SMSClient {
	if p == nil {
		return NewSMSClient("", "", "", "")
	}
	if p.sms == nil {
		p.sms = NewSMSClient("", "", "", "")
	}
	return p.sms
}

// SMSClient 暴露底层短信登录客户端（供装配层与测试使用）。
func (p *Provider) SMSClient() *SMSClient { return p.smsClient() }

// Client 暴露底层客户端（供装配层与测试使用）。
func (p *Provider) Client() *Client { return p.client }

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// Loomy 实际具备三项：
//
//	CapChat        对话（POST /chat/completions，实测流式/function_calling/视觉/推理全通）
//	CapModels      模型目录（GET /models，实测 12 条）
//	CapQuotaProbe  **本轮补上的** —— 从本机客户端缓存的积分摘要读额度
//	               （见 quota_ext.go）。它打的不是上游端点（那不存在，实测
//	               17 条候选路径全 404），而是**主动读取一份权威的本地事实**；
//	               能力位问的是"能不能主动拿到额度"，答案是能。
//
// 其余四项**不声明**，每一项都对应一个它确实没有的东西：
//
//	CapCheckin     没有每日签到端点（积分按自然日**自动**重置，不需要领取）
//	CapGrowth      没有成长中心/任务体系
//	CapTravel      没有猫猫旅行
//	CapWelfare     没有福利领取
//
// ⚠ 声明 CapQuotaProbe 有一条**契约代价**：gateway 把该能力位归入
// `unverifiableCaps`，于是"声明了就必须实现 AdminExt 且路由非空、结构完整、
// handler 不 panic"（见 contract.go 的 probeCapabilities / verifyAdminRoutes）。
// loomy 为此提供了 `/admin/loomy/client-store` 这条**真的有用**的诊断端点 ——
// 它不是为过检查而造的空壳，理由见 adminroute.go 的注释。
//
// ⚠ 声明 CapModels 意味着 Models() 必须真的返回非空目录 ——
// 契约测试会验证这一条（见 contract.go 的 capabilityProbes）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels | gateway.CapQuotaProbe |
		// 本轮为 loomy 的专属标签页加的（见 points.go 与 adminroute.go）：
		//	CapTasks   新手任务（GET/POST /api/v1/onboarding/tasks*，8 项共 10000 分）
		//	CapInvite  邀请码（GET/POST /api/v1/points/activation 等）
		// 两者都进了 gateway.unverifiableCaps，因此同时要求实现 AdminExt ——
		// loomy 有（诊断端点 + 本轮这两组端点）。
		gateway.CapTasks | gateway.CapInvite
}

// Chat 转发一次对话请求。
//
// 做的事很少，因为 Loomy 本身就是 OpenAI 兼容端点 —— 没有签名、没有会话槽、
// 没有通道选择。全部工作是：
//
//  1. 凭证类型断言（不匹配返回明确错误，**不 panic**）
//  2. ctx 已取消则立刻返回（契约要求；否则前端断开后上游调用仍在跑）
//  3. 出站请求体改写（强制 stream + 裁剪超限 max_tokens，见 client.prepareBody）
//  4. 装**对话那条轨**的鉴权头（Authorization: Bearer，见 client.go 的双轨说明）
//
// status 的语义与另两个上游对齐：非 2xx **不返回 error**，
// 而是把状态码与上游错误体一起返回，让调用方统一处理"业务错误"（换号/冷却）
// 与"传输错误"。这一条很重要 —— 它让 errorclassifier 能拿到完整正文。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	// 契约要求"ctx 取消时 Chat 必须尽快返回"。这里先检查一次，
	// 保证调用方传已取消的 ctx 时不必等一次网络往返。
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	rc, status, respBody, err := p.client.ChatStream(ctx, a, body)
	if err != nil {
		// respBody 非空 = 上游返回了错误体（业务错误），仍按流返回。
		if len(respBody) > 0 {
			return gateway.ChatStream{
				Status: status,
				Body:   io.NopCloser(bytes.NewReader(respBody)),
			}, nil
		}
		return gateway.ChatStream{}, err
	}
	// 契约要求"err==nil 时 Status 或 Body 至少有一个非零"。
	// 正常分支两者都有；rc 为 nil 只可能来自上游给了 2xx 却没有 body
	// （httptest 的某些构造），那种情况下用空流兜住，避免调用方
	// 拿到 nil 再去 Close 而 panic。
	if rc == nil {
		rc = io.NopCloser(bytes.NewReader(nil))
	}
	return gateway.ChatStream{Status: status, Body: rc}, nil
}

// Models 返回模型目录。
//
// # 实时优先、静态兜底（与 codearts 的"纯静态"不同）
//
//	实时拿得到 → 用实时的（上游新增/下架模型能自动跟随）
//	实时拿不到 → 用静态快照（保证目录不会因为一次抖动/session 失效而整片消失）
//
// 为什么不能只靠实时：Models() 是 /v1/models 的数据源，它一空，
// 客户端就判定"模型不存在"并报错 —— 而真相只是这一轮没拉到。
// 为什么不能只靠静态：那会让上游新增的模型永远不出现（手动改代码才能用）。
//
// # 为什么过滤掉"已知不可用"的模型
//
// 手册实测两个生图模型（doubao-seedream-5-lite / qwen-image-3.0-pro）
// 在 /models 里**列着**，但一用就 404「该模型暂未开放」。
// 把它们继续挂在目录里，等于给客户端一个必然失败的选择 ——
// 与 codearts 过滤"额度当天耗尽"的模型是同一条判据：
// **目录是客户端唯一的选型依据，挂在上面就得能用。**
//
// 过滤是安全的：它只是不显示，请求仍然能发（上游的错误会如实返回），
// 而且上游一旦真的开放，实时目录会把它带回来（filterUnavailable 只按
// 静态表里的 Unavailable 标记过滤，不按 ID 白名单）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if _, err := authOf(cred); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	live, lerr := p.client.ModelList(ctx, cred.Secret.(*Auth))
	if lerr == nil && len(live) > 0 {
		return toModelInfos(filterUnavailable(live)), nil
	}
	if lerr != nil {
		// 只记日志、不返回错误：静态快照是**设计好的**回落路径，
		// 不是失败。把它报成 error 会让出口层跳过该上游 → 目录反而空了。
		log.Printf("loomy: 实时模型目录拉取失败，回落到静态快照: %v", lerr)
	}
	return toModelInfos(AvailableModels()), nil
}

// filterUnavailable 剔除静态表里已标记"上游下架"的模型。
//
// 只在实时目录里做（静态表本身已经过滤过）。匹配用大小写不敏感的 ID 比较：
// 上游对同一模型的写法可能大小写不稳（`Kimi-k2.6` 这种混合形态很容易被改）。
func filterUnavailable(live []Model) []Model {
	out := make([]Model, 0, len(live))
	for _, m := range live {
		if known, _, ok := ResolveModel(m.ID); ok && known.Unavailable {
			continue
		}
		out = append(out, m)
	}
	return out
}

// toModelInfos 把本包的 Model 投影成中立契约 gateway.ModelInfo。
//
// 实时条目的上下文字段可能缺失（0）—— 那时用静态表里的值补上，
// 让"上游只回了 id"的极简目录也能带上窗口信息。
func toModelInfos(ms []Model) []gateway.ModelInfo {
	out := make([]gateway.ModelInfo, 0, len(ms))
	for _, m := range ms {
		cw, mo := m.ContextWindow, m.MaxOutputTokens
		if known, _, ok := ResolveModel(m.ID); ok {
			if cw <= 0 {
				cw = known.ContextWindow
			}
			if mo <= 0 {
				mo = known.MaxOutputTokens
			}
		}
		out = append(out, gateway.ModelInfo{
			ID:              m.ID,
			ContextWindow:   cw,
			MaxOutputTokens: mo,
		})
	}
	return out
}

// authOf 从 Credential 里取出 Loomy 的凭证结构。
//
// Secret 是 any（各上游凭证结构不同），所以必须类型断言。
// **断言失败要返回明确错误，不能 panic** —— 这是契约要求，也有测试守着。
//
// ⚠ 只接受 `*loomy.Auth`，**不接受** `*auth.Auth`。
//
// 这条与 codearts 的同名函数同源（那边为此留了一段长注释）：核心在拿不到
// 不透明 secret 时会回落成 `cred.Secret = e.a`（`*auth.Auth`），
// 于是这里会报"凭证类型不对"。那是**正确**的行为 —— 与其用一个
// session 为空的投影去发请求、拿到一句含糊的 401，不如立刻说清类型不对。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("loomy: 凭证为空（Credential.Secret 未设置）")
	}
	switch v := cred.Secret.(type) {
	case *Auth:
		if v == nil {
			return nil, errors.New("loomy: 凭证是 nil 指针")
		}
		if strings.TrimSpace(v.Session) == "" {
			return nil, errors.New("loomy: 凭证缺少 session（唯一鉴权材料，见 credential.go）")
		}
		return v, nil
	case *authFile:
		// 落盘包装（`LoginFlow.Poll` 交给核心的那个形态）里裹的是**同一份** *Auth。
		//
		// # 为什么必须放行它（与用户实测报的"添加账号不对"直接相关）
		//
		// "添加账号"路径上核心拿到的 `Secret` 是 `*authFile`（它要能满足
		// `MarshalAuthFile` 才能落盘，见 login.go 的说明），而账号表路径上
		// 从池子里拿出来的是 `*loomy.Auth`。**两条路径是同一个凭证的两个外壳。**
		//
		// 只认 `*Auth` 的后果不是崩溃，而是"弹窗里说不出「永久」"：
		// 核心问 `CredentialLifetimeExt.NeverExpires` → 这里报类型不对 →
		// 上游按契约只能回 false → 弹窗显示 `—`，而**同一份凭证**
		// 在账号表里显示「永久」。两处渲染点自相矛盾，正是本项目反复吃的形态。
		//
		// 所以这里解开包装，让两个外壳在方法集上等价。
		if v == nil || v.a == nil {
			return nil, errors.New("loomy: 凭证是空的落盘包装")
		}
		if strings.TrimSpace(v.a.Session) == "" {
			return nil, errors.New("loomy: 凭证缺少 session（唯一鉴权材料，见 credential.go）")
		}
		return v.a, nil
	default:
		return nil, fmt.Errorf("loomy: 凭证类型不对，期望 *loomy.Auth，实际 %T", cred.Secret)
	}
}

// ── 扩展点实现 ──────────────────────────────────────────────────────────

// AuthDir 本上游的凭证落盘目录（gateway.AuthDirExt）。
//
// 核心据此做"按上游重载 auths"：它只问"你的目录在哪"，
// 不认识目录里文件长什么样 —— 那是 LoadCredentials 的事。
func (p *Provider) AuthDir() string { return p.authDir }

// LoadCredentials 读取 dir 下本上游的全部凭证（gateway.CredentialLoader）。
//
// 只把 uid / nickname 投影出去（核心要的就是这两个：池子主键与展示名），
// **核心不解释凭证内容**。凭证本身走下面的 WithSecrets 版本。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	auths, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(auths))
	for _, a := range auths {
		out = append(out, gateway.Credential{
			Provider: providerID,
			UID:      a.UID,
			Nickname: a.Nickname,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret
// （gateway.CredentialSecretLoader）。
//
// # 为什么必须实现它（而不是只用 LoadCredentials）
//
// 只用投影版会有个静默缺口：手工往 `auths/loomy/` 拷一份凭证再点「重载 auths」，
// 池里会出现该 uid 但 `e.secret == nil` → 选中它时 authOf 报"凭证为空"
// → **每次请求都失败，而界面上一切正常**。
//
// 这正是 codearts 那边评审 R2 修的缺口（见 gateway.CredentialSecretLoader
// 的注释）。loomy 凭证更简单，但这个口子一样会漏。
//
// ⚠ "同源"是硬要求：本函数与 LoadCredentials 都必须走同一个 LoadDir，
// 否则会造出两份 `*loomy.Auth`，池里那份与这里这份分叉。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	auths, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(auths))
	for _, a := range auths {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: providerID,
				UID:      a.UID,
				Nickname: a.Nickname,
			},
			Secret: a, // 不透明；核心只保管
		})
	}
	return out, nil
}

// effectiveDir 决定用哪个目录：调用方给的优先，否则用装配时注入的。
//
// 核心在"按上游重载"时传的是 AuthDir() 的返回值；直接调本函数的调用方
// （测试、cmd/login）可能传自己的。两者都支持，且**优先级明确**：
// 显式入参 > 装配注入，避免"传了参数却用了别处配置"这种难查的分叉。
func (p *Provider) effectiveDir(dir string) string {
	if strings.TrimSpace(dir) != "" {
		return dir
	}
	return p.authDir
}

// 编译期断言：Provider 实现核心契约与它声称支持的全部扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"路由神秘地没挂上"。
// ⚠ 这些断言**不是**架构约束的发现机制（那由 arch_test.go 的包级依赖图
// 给出，见它关于三次绕过教训的长注释）—— 它们的作用是编译期自检。
var (
	_ gateway.Provider               = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.ErrorClassifier        = (*Provider)(nil)
	_ gateway.ResetPolicyExt         = (*Provider)(nil)
	_ gateway.AccountColumnsExt      = (*Provider)(nil)
	_ gateway.CredentialLifetimeExt  = (*Provider)(nil)
	_ gateway.QuotaExt               = (*Provider)(nil)
	_ gateway.LoginFlow              = (*Provider)(nil)
	_ gateway.AdminExt               = (*Provider)(nil)
)
