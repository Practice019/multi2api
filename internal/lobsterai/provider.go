// provider.go 把 LobsterAI（有道龙虾）适配为 gateway.Provider。
//
// # 与其他上游的差异（都已实测，来自参照项目）
//
//	不同源       与 CodeBuddy 系**完全不同**：登录方式、请求头、续期载荷、
//	             签到流程都不一样。所以是独立一套实现，只共用架构模式。
//	登录         本地回调服务器 + authCode 换 token（要绑端口）
//	统一信封     {code,msg,data}，**失败可能是 HTTP 200 + 非 0 code**
//	聊天         **裸 SSE，不套信封**（模型列表套信封 —— 最容易搞错的一点）
//	仅 SSE       stream:false → 500
//	两个能力头   X-LobsterAI-Client-Capabilities 是**模型列表的准入条件**
//	             （不带它 kimi-k3 不会出现）
//	有签到       三步：槽位 → 上下文 → 领取
package lobsterai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// Provider 实现 gateway.Provider 与若干可选扩展点。
type Provider struct {
	client  *Client
	authDir string

	loginOnce   sync.Once
	loginCached *loginFlow

	// creds 凭证访问器（装配层注入，见 adminroute.go）。
	creds credentialSource

	// log 签到历史（装配层注入；nil = 不记历史，但那样界面「今日签到」
	// 列会永远为空 —— 见 checkin.go 的注释）。
	log *checkinlog.Log
	// checkinInterval 自动签到扫描间隔（<=0 = 不注册自动签到任务）。
	//
	// 与 workbuddy / trae **同一个值**（30 分钟）—— 用户要求
	// 「所有上游共用一个签到的接口」，节奏也统一。
	checkinInterval time.Duration
	// checkin 共享签到驱动（internal/dailycheckin）。
	//
	// 按钮 / 自动任务 / 手动端点 / 写历史 四件事都由它提供 ——
	// 上游侧只实现"列账号、查状态、领取"三个方法。
	checkin *dailycheckin.Driver
}

// Config Provider 的可选依赖，全部可缺省。
type Config struct {
	// Client 上游 HTTP 客户端。nil 时用 New()。
	Client *Client
	// APIBase / PortalBase / VersionAPI 三个基址（Client 为 nil 时用）。
	APIBase    string
	PortalBase string
	VersionAPI string
	// AuthDir 本上游凭证的落盘目录（供 gateway.AuthDirExt 用）。
	AuthDir string
	// Log 签到历史（nil = 不记历史，但界面「今日签到」列会因此恒为空）。
	Log *checkinlog.Log
	// CheckinInterval 自动签到扫描间隔；<=0 = 不注册自动签到任务。
	//
	// 装配层传 30 分钟（与 workbuddy / trae 同一节奏）。
	CheckinInterval time.Duration
}

// NewProvider 契约测试用的无依赖构造。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// NewWithConfig 建 Provider。只构造，不做网络请求。
func NewWithConfig(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = New()
		if cfg.APIBase != "" {
			c.APIBase = cfg.APIBase
		}
		if cfg.PortalBase != "" {
			c.Portal = cfg.PortalBase
		}
		if cfg.VersionAPI != "" {
			c.VersionA = cfg.VersionAPI
		}
	}
	p := &Provider{
		client:          c,
		authDir:         cfg.AuthDir,
		log:             cfg.Log,
		checkinInterval: cfg.CheckinInterval,
	}
	// 构造共享签到驱动（按钮 / 自动任务 / 端点 / 写历史 都在它里面）。
	// 必须在字段就位之后调用 —— 它读 p.log 与 p.checkinInterval。
	p.initCheckin()
	return p
}
// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// 声明对话、动态模型目录、**签到**（LobsterAI 有真实的三步签到接口）。
//
// ⚠ 不声明 CapQuotaProbe：它没有"主动额度探测"端点，
// 余额走 /api/user/profile-summary（由 AdminExt 暴露），两者是不同的事。
func (p *Provider) Caps() gateway.Capability {
	// CapQuotaProbe：有主动额度探测端点（QuotaExt 已实现）。
	//
	// ⚠ 本能力位是**行内「额度」按钮的判据**（前端 hasCap(pid,"quota-probe")）。
	// 不声明它 → 该上游的账号行里根本不出现那个按钮。
	return gateway.CapChat | gateway.CapModels | gateway.CapCheckin | gateway.CapQuotaProbe |
		gateway.CapImport
}

// Client 上游 HTTP 客户端（供装配层与测试使用）。
func (p *Provider) Client() *Client { return p.client }

// AuthDir 本上游的凭证落盘目录（gateway.AuthDirExt）。
func (p *Provider) AuthDir() string {
	if p != nil && p.authDir != "" {
		return p.authDir
	}
	return ""
}

func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("lobsterai: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok {
		return nil, fmt.Errorf("lobsterai: 凭证类型不对，期望 *lobsterai.Auth，实际 %T", cred.Secret)
	}
	if a == nil {
		return nil, errors.New("lobsterai: 凭证是 nil 指针")
	}
	if a.AccessToken == "" {
		return nil, errors.New("lobsterai: 凭证缺少 accessToken（唯一鉴权材料）")
	}
	return a, nil
}

// Chat 转发一次对话。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	outBody := PrepareBody(body)
	rc, status, respBody, err := p.client.ChatStream(ctx, a, outBody)
	if err != nil {
		if len(respBody) > 0 {
			return gateway.ChatStream{Status: status, Body: io.NopCloser(byteReader(respBody))}, nil
		}
		return gateway.ChatStream{}, err
	}
	if rc == nil {
		rc = io.NopCloser(byteReader(nil))
	}
	return gateway.ChatStream{Status: status, Body: rc}, nil
}

// Models 返回模型目录。
//
// ⚠ 必须带两个 X-LobsterAI-Client-* 头，否则**永久缺少 kimi-k3**。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	ms, err := p.client.FetchModels(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.ModelInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, gateway.ModelInfo{ID: m.ID})
	}
	return out, nil
}

// ProbeHealth 探测凭证是否健康（gateway.HealthProbeExt）。
//
// 用 profile-summary：信封 code==0 才算健康
//（⚠ 不能用 HTTP 200 当判据 —— 业务失败也是 200）。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = p.client.FetchBalance(ctx, a)
	return err
}

// Classify 按 HTTP 状态码 + 响应体判定错误类别（gateway.ErrorClassifier）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	return Classify(status, body)
}

// ── 凭证读写 ────────────────────────────────────────────────────────────

// LoadCredentials 读取 dir 下全部凭证（只投影 uid/nickname）。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.Credential{
			Provider: providerID,
			UID:      a.UIDValue(),
			Nickname: a.Nickname,
			FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: providerID,
				UID:      a.UIDValue(),
				Nickname: a.Nickname,
				FilePath: a.FilePath,
			},
			Secret: a,
		})
	}
	return out, nil
}

func (p *Provider) effectiveDir(dir string) string {
	if dir != "" {
		return dir
	}
	if p != nil {
		return p.authDir
	}
	return ""
}

// ── 续期（gateway.CredentialRefresher）──────────────────────────────────

// RefreshCredential 续期一份凭证（原地更新 + 落盘）。
//
// ⚠ **uuid / first_keyfrom / latest_keyfrom 必须原样保留** ——
// 它们是续期请求体的必需字段，丢了续期会被服务端拒绝。
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	next, err := p.client.RefreshCredential(context.Background(), a)
	if err != nil {
		return err
	}
	a.AccessToken = next.AccessToken
	if next.RefreshToken != "" {
		a.RefreshToken = next.RefreshToken
	}
	if next.ExpiresAt != "" {
		a.ExpiresAt = next.ExpiresAt
	}
	if next.UID != "" {
		a.UID = next.UID
	}
	if next.UserID != "" {
		a.UserID = next.UserID
	}
	if next.Nickname != "" {
		a.Nickname = next.Nickname
	}
	// 落盘失败不让刷新失败（内存里的凭证已可用）
	if err := saveAuthFile(a); err != nil {
		return nil
	}
	return nil
}

// Renewable 报告凭证是否可续期。
func (p *Provider) Renewable(cred gateway.Credential) bool {
	a, err := authOf(cred)
	if err != nil {
		return false
	}
	return a.Renewable()
}

// ── 签到与余额（供 AdminExt 端点使用）────────────────────────────────────

// ClaimDailyCheckin 执行每日签到（装配层的管理端点用）。
func (p *Provider) ClaimDailyCheckin(ctx context.Context, cred gateway.Credential) ClaimOutcome {
	a, err := authOf(cred)
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: err.Error()}
	}
	return p.client.ClaimDailyCheckin(ctx, a)
}

// Balance 查余额（装配层的管理端点用）。
func (p *Provider) Balance(ctx context.Context, cred gateway.Credential) (*Balance, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	return p.client.FetchBalance(ctx, a)
}

// LoginFlow 返回（并缓存）登录流程实例。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil {
		return nil, false
	}
	p.loginOnce.Do(func() {
		p.loginCached = &loginFlow{p: p, ses: map[string]*loginEntry{}}
	})
	return p.loginCached, true
}

// byteReader 把 []byte 包成 io.Reader。
func byteReader(b []byte) io.Reader {
	if b == nil {
		return &emptyReader{}
	}
	return &sliceReader{b: b}
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// 编译期断言：Provider 满足的扩展点。
var (
	_ gateway.Provider               = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.ErrorClassifier        = (*Provider)(nil)
	_ gateway.HealthProbeExt         = (*Provider)(nil)
	_ gateway.CredentialRefresher    = (*Provider)(nil)
)
