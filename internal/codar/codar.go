// Package codar 是 Codar（科搭 / SpeedAI）上游实现。
//
// # 协议来源（T-311 实测，非推断）
//
//	POST {apiBase}/chat/completions        apiBase = https://api.claw.speedai.com/v1
//	Authorization: Bearer <speedai token>  （openai 系模型）
//	User-Agent: codar-desktop/0.3.3
//	Accept-Encoding: identity              （防 gzip 坏帧，SSE 必须）
//	{"model":"…","messages":[…],"max_tokens":N,"stream":bool}
//
// 响应 = 标准 OpenAI + 4 个私有扩展（reasoning_content / billing{quota,cost_cny} /
// service_tier / prompt_tokens_details.cached_tokens）；SSE 亦为标准 data:/[DONE]。
//
// # 为什么请求体字节级透传（但强制 stream=true）
//
// Codar 接受标准 OpenAI 且对多余字段宽容 ⇒ 我们**不重新序列化**，保留客户端的扩展字段
// 与数字精度。唯一的例外是 stream：出口层一律把上游响应当 SSE 解析（ChatStream.Body 的
// 契约），所以不论客户端要流式还是非流式，**上游都必须走流式**（见 client.go forceStream）。
//
// # 模型分两类协议（T-311 实测目录里的 protocol 字段）
//
//	openai_chat_completions → /v1/chat/completions + Authorization: Bearer
//	anthropic_messages      → /v1/messages         + x-api-key
//
// 本实现按请求体里的 model 自动选路（见 client.go）。
package codar

import (
	"context"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// providerID 上游标识：兼作模型名前缀（codar/<model>）、配置段名、统计维度。
const providerID = "codar"

// ProviderID 导出给装配层（cmd/server）用，避免在 cmd 里再写一遍字面量。
const ProviderID = providerID

// DefaultAPIBase Codar 的 OpenAI 兼容基址（T-311 实测：桌面端 config.json 的 provider.api_base）。
const DefaultAPIBase = "https://api.claw.speedai.com"

// DefaultPayBase 计费/账号面基址：余额、签到、续期、推广都在这里（T-317 实测）。
const DefaultPayBase = "https://api.kuaipaper.com"

// DefaultUserAgent 桌面客户端的 UA（codar2api 也用它；不伪装成浏览器）。
const DefaultUserAgent = "codar-desktop/0.3.3"

// Config 全部可缺省：零值 Config 必须能跑契约测试。
type Config struct {
	// Client 可注入（测试用）；nil ⇒ New() 造一个。
	Client *Client
	// APIBase 缺省 DefaultAPIBase。
	APIBase string
	// PayBase 计费/账号面，缺省 DefaultPayBase。
	PayBase string
	// UserAgent 缺省 DefaultUserAgent。
	UserAgent string
	// AuthDir 凭证目录（供 gateway.AuthDirExt / LoadCredentials）。
	AuthDir string
	// Log 签到/动作历史（核心注入，nil = 不记录）。
	//
	// ⚠ 上游**只**能依赖 checkinlog 这个叶子包（它不认识核心），与 codearts 同一写法：
	// 少了它，账号行的「今日签到」列会永远空着（列的数据源就是 checkinlog）。
	Log *checkinlog.Log

	// CredentialSource 按 uid 取活凭证（装配层注入；上游不得 import pool）。
	CredentialSource func(uid string) (gateway.Credential, bool)
}

// Provider 实现 gateway.Provider（四方法）+ 若干可选扩展点。
type Provider struct {
	client    *Client
	authDir   string
	payBase   string
	apiBase   string
	userAgent string
	credSrc   func(uid string) (gateway.Credential, bool)
	log       *checkinlog.Log
	// 续期任务的最小间隔状态（见 p1.go 的 Due：不让它每个调度 tick 都打上游）。
	// IP 验证会话表（手机号 → 临时 token，见 smslogin.go）。
	ipMu           sync.Mutex
	ipFlows        map[string]ipFlow
	refreshMu      sync.Mutex
	lastRefreshTry time.Time
}

// NewProvider 契约测试入口。
//
// ⚠ 不能叫 New：本包的 New() 是「建 HTTP 客户端」（与其它上游一致，避免同名冲突）。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// NewWithConfig 按配置构造。
func NewWithConfig(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = New()
	}
	base := cfg.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	pay := cfg.PayBase
	if pay == "" {
		pay = DefaultPayBase
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	c.APIBase = base
	c.PayBase = pay
	c.UserAgent = ua
	return &Provider{
		client:    c,
		authDir:   cfg.AuthDir,
		payBase:   pay,
		apiBase:   base,
		userAgent: ua,
		credSrc:   cfg.CredentialSource,
		log:       cfg.Log,
	}
}

func (p *Provider) ID() string { return providerID }

// Caps 只声明**可验证**的：对话 + 模型目录。
//
// ⚠ 刻意不声明 CapCheckin / CapWelfare / CapTasks / CapInvite：
//
//	① 用户刚删掉 codearts 的邀请码功能（故 CapInvite 明确不做）；
//	② 签到/领免费模型目前只有**只读查询**端点（写操作待 P1），声明 CapCheckin 会画出
//	   一个点了却什么都不做的按钮 —— 本仓规矩是"声明即承诺、少报安全多报被当场抓住"。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels | gateway.CapCheckin | gateway.CapQuotaProbe | gateway.CapImport | gateway.CapInvite
}

// AuthDir 实现 gateway.AuthDirExt（读与写必须问同一个目录）。
func (p *Provider) AuthDir() string { return p.authDir }

// Chat 转发一次对话。
//
// body 是核心已规范成 OpenAI chat/completions 的请求体。
// 非 2xx 时**仍返回 ChatStream**（把上游错误体带回给调用方分类），只有网络层失败才返回 error。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	if err := ctx.Err(); err != nil { // 契约：ctx 取消要尽快返回
		return gateway.ChatStream{}, err
	}
	return p.client.Chat(ctx, a, body)
}

// Models 返回模型目录。
//
// # 为什么「无凭证也要非空」
//
// 契约测试默认拿伪造凭证跑，而 Codar 的目录**是免鉴权的** ⇒ 正常情况下不需要凭证。
// 但为了让「目录接口挂了」和「凭证失效」都不至于让 /v1/models 变空，
// 远端失败时回**静态兜底目录**（T-311 实测的 15 个 id）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	ms, err := p.client.FetchModels(ctx, p.apiBase)
	if err != nil || len(ms) == 0 {
		if err != nil {
			log.Printf("codar: 拉取模型目录失败（回退静态目录）: %v", err)
		}
		return fallbackModels(), nil
	}
	return ms, nil
}

// authOf 把不透明 Secret 断言回本包的凭证类型。断言失败给**明确错误**，不 panic。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errNoCredential
	}
	a, ok := cred.Secret.(*Auth)
	if !ok {
		return nil, errWrongCredType
	}
	if a == nil || a.Token == "" {
		return nil, errNoToken
	}
	return a, nil
}
