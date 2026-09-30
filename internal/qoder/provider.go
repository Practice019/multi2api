// provider.go 把 Qoder 适配为 gateway.Provider。
//
// # 这个包同时服务两个 provider
//
//	qoder     国际版
//	qodercn   中国版（同协议族、共用同一份 WASM，差异全在 Product 配置里）
//
// 这与参照项目一致：它是两个 `QoderAuth` 实例（`new QoderAuth(ctx)` 与
// `new QoderAuth(ctx, { product: QODER_CN })`），不是两套实现。
//
// # 与其它上游的差异
//
//	推理走**加密端点**（请求体与签名头由 WASM 生成）
//	模型列表是**静态表**（远端端点需 WASM 签名，我们不发那个请求）
//	续期请求体必须带 machine_id
//	错误帧是独立的 `event: error` + 顶层 {code,message,type}
package qoder

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
	// productID 本实例的产品标识（qoder / qodercn）。
	productID string

	// creds 按 uid 取凭证（装配层注入；nil = 管理端点不可用）。
	// 见 adminroute.go 的 credentialSource。
	creds credentialSource

	// quotaSink 把额度写回账号池（装配层注入；nil = 不写，界面额度列恒为空）。
	//
	// # 为什么需要它（用户报「额度是 0」）
	//
	// 本上游有 `RefreshQuota`（`gateway.QuotaExt`），但那个扩展点**只在
	// 有人点「刷新本上游额度」时才被调用** —— 核心**没有**定时任务去扫
	// （全仓只有 workbuddy 一个上游在签到/旅行路径里顺带回写）。
	//
	// 于是重启后额度列是空的，直到有人手动点一次 —— 而用户看到的
	// 「额度 0」正是"我们还没问过上游"被渲染成了 0。
	//
	// 这里给后台任务一个写回通道：续期那一轮**顺带**查一次额度
	//（同一个 host，一次往返），不必新起任务。
	quotaSink func(uid string, q gateway.QuotaView)

	// log 签到历史（装配层注入；nil = 不记历史，但那样界面「今日签到」
	// 列会永远为空 —— 见 checkin.go 的注释）。
	log *checkinlog.Log
	// checkinInterval 自动签到扫描间隔（<=0 = 不注册自动签到任务）。
	// 与 workbuddy / trae 同一个值（30 分钟）。
	checkinInterval time.Duration
	// checkin 共享签到驱动（internal/dailycheckin）。
	checkin *dailycheckin.Driver

	loginOnce   sync.Once
	loginCached *loginFlow
}

// Config Provider 的可选依赖。
type Config struct {
	// Client 上游 HTTP 客户端。nil 时按 Product 构造。
	Client *Client
	// Product 产品配置（零值时用国际版）。
	Product Product
	// Signer WASM 请求签名器（nil = 加密推理不可用，Chat 会明确报错）。
	Signer RequestSigner
	// AuthDir 凭证目录。
	AuthDir string
	// Log 签到历史（nil = 不记历史，但界面「今日签到」列会因此恒为空）。
	Log *checkinlog.Log
	// CheckinInterval 自动签到扫描间隔；<=0 = 不注册自动签到任务。
	//
	// 装配层传 30 分钟（与 workbuddy / trae 同一节奏）。
	CheckinInterval time.Duration
	// QuotaSink 把额度写回账号池（nil = 后台任务不回写额度）。
	//
	// 见 Provider.quotaSink 的注释：没有它，重启后额度列一直是空的
	//（用户报「额度 0」的形态）。
	QuotaSink func(uid string, q gateway.QuotaView)
}

// NewProvider 契约测试用的无依赖构造（国际版）。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// NewProviderCN 中国版的无依赖构造。
func NewProviderCN() gateway.Provider {
	return NewWithConfig(Config{Product: QoderCN})
}

// NewWithConfig 建 Provider。
func NewWithConfig(cfg Config) *Provider {
	prod := cfg.Product
	if prod.ID == "" {
		prod = Qoder
	}
	c := cfg.Client
	if c == nil {
		c = NewWithProduct(prod)
	}
	if cfg.Signer != nil {
		c.Signer = cfg.Signer
	}
	p := &Provider{
		client:          c,
		authDir:         cfg.AuthDir,
		productID:       prod.ID,
		log:             cfg.Log,
		checkinInterval: cfg.CheckinInterval,
		quotaSink:       cfg.QuotaSink,
	}
	// 构造共享签到驱动（按钮 / 自动任务 / 端点 / 写历史 都在它里面）。
	// 必须在字段就位之后调用 —— 它读 p.log / p.checkinInterval / p.ID()。
	p.initCheckin()
	return p
}

// SetSigner 注入 WASM 签名器（装配层在 wasm 层就绪后调用）。
func (p *Provider) SetSigner(s RequestSigner) {
	if p != nil && p.client != nil {
		p.client.Signer = s
	}
}

// ID 上游标识。
func (p *Provider) ID() string { return p.productID }

// DisplayName 界面上给人看的名字（gateway.DisplayNameExt）。
//
// # 为什么 qoder 需要它（用户报的问题）
//
// 两个实例的 id 是 `qoder` 与 `qodercn` —— 只差一个后缀，而它们是
// **两个不同的服务**（不同域名、不同 clientId，见 QoderCN 的注释）。
// 用户在国际版与中国版之间看不出区别，加账号时不知道该点哪个。
//
// 取产品表里的 DisplayName（"Qoder" / "Qoder (中国版)"），
// 于是显示名与产品定义**同源**，不会两处漂移。
func (p *Provider) DisplayName() string {
	if p != nil {
		if n := p.Product().DisplayName; n != "" {
			return n
		}
		return p.productID
	}
	return ""
}

// Caps 能力声明。
//
// 声明三项：对话、模型目录、签到（每日积分领取）。
//
// # 签到为什么可以声明了（此前刻意不声明）
//
// 此前的注释写着「Qoder 的每日积分领取在参照项目里是独立的 credits 模块，
// 但那是**网页端活动**，不经 LLM 网关；本包不实现它」。那个判断有**两处错**：
//
//  1. 它并非"网页端"—— `/sash/api/v1/me/campaigns` 是**客户端 API**，
//     Bearer + Cosy-ClientType 即可访问（**不需要 WASM 签名**，
//     那是推理端点才有的要求）
//  2. 参照实现早期也误判过「Qoder 无积分端点」，原因只是它按 `/api/`
//     前缀搜索，而真实路径是 `/sash/api/`—— 搜索范围问题，不是端点不存在
//
// 现在 credits.go 实现了它（余额 + 活动列表 + 逐个领取），
// 并有 AdminRoutes 暴露出来，所以声明 CapCheckin 是**名实相符**的。
//
// ⚠ 不声明 CapQuotaProbe：没有主动额度探测端点。积分余额（CapCheckin
// 下的 /balance）与"额度探测"不是同一件事 —— 后者是 pool 的主动健康探测，
// Qoder 没有对应端点。
func (p *Provider) Caps() gateway.Capability {
	// CapQuotaProbe：有主动额度探测端点（QuotaExt 已实现）。
	//
	// ⚠ 本能力位是**行内「额度」按钮的判据**（前端 hasCap(pid,"quota-probe")）。
	// 不声明它 → 该上游的账号行里根本不出现那个按钮。
	return gateway.CapChat | gateway.CapModels | gateway.CapCheckin | gateway.CapQuotaProbe |
		gateway.CapImport
}

// Client 上游 HTTP 客户端。
func (p *Provider) Client() *Client { return p.client }

// Product 本实例的产品配置。
func (p *Provider) Product() Product {
	if p == nil || p.client == nil {
		return Qoder
	}
	return p.client.Product
}

// AuthDir 凭证目录（gateway.AuthDirExt）。
func (p *Provider) AuthDir() string {
	if p != nil && p.authDir != "" {
		return p.authDir
	}
	return ""
}

func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("qoder: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok {
		return nil, fmt.Errorf("qoder: 凭证类型不对，期望 *qoder.Auth，实际 %T", cred.Secret)
	}
	if a == nil {
		return nil, errors.New("qoder: 凭证是 nil 指针")
	}
	if a.AccessToken == "" {
		return nil, errors.New("qoder: 凭证缺少 accessToken（唯一鉴权材料）")
	}
	return a, nil
}

// Chat 转发一次对话（走加密端点）。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	rc, status, respBody, err := p.client.ChatStream(ctx, a, body)
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

// Models 返回模型目录（**静态表，不发网络请求**）。
//
// ⚠ 为什么用静态表：远端 `GET /algo/api/v2/model/list` 需 **WASM 签名**，
// 故参照实现也不发这个请求 —— 表就是权威。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	models := ModelsFor(p.productID)
	out := make([]gateway.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, gateway.ModelInfo{
			ID:              m.ID,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: 0, // 静态表里没有该字段（上游不下发）
		})
	}
	return out, nil
}

// ProbeHealth 探测凭证是否健康（gateway.HealthProbeExt）。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ui := p.client.FetchUserInfo(ctx, a)
	if ui.UID == "" && ui.Nickname == "" {
		return fmt.Errorf("qoder: 令牌校验未返回用户信息（可能已失效）")
	}
	return nil
}

// Classify 错误分类（gateway.ErrorClassifier）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	return Classify(status, body)
}

// ── 凭证读写 ────────────────────────────────────────────────────────────

// LoadCredentials 读取 dir 下本产品的凭证。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDirFor(p.effectiveDir(dir), p.productID)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.Credential{
			Provider: p.productID, UID: a.UIDValue(),
			Nickname: a.Nickname, FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	list, err := LoadDirFor(p.effectiveDir(dir), p.productID)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: p.productID, UID: a.UIDValue(),
				Nickname: a.Nickname, FilePath: a.FilePath,
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

// ── 续期 ────────────────────────────────────────────────────────────────

// RefreshCredential 续期（原地更新 + 落盘）。
//
// ⚠ machine_id 必须保留 —— 续期请求体要用它。
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
	// ⚠ 绝对毫秒时刻（不是相对秒）：它是「Token」列的唯一权威。
	//
	// 旧实现写 `a.ExpiresIn = next.ExpiresIn`，而那个字段来自上游响应里
	// **不存在**的 `expires_in` → 恒为 0 → 界面永远显示 `—`（用户报障）。
	if next.ExpiresAt > 0 {
		a.ExpiresAt = next.ExpiresAt
	}
	if next.RefreshTokenExpiresAt > 0 {
		a.RefreshTokenExpiresAt = next.RefreshTokenExpiresAt
	}
	if err := saveAuthFile(a); err != nil {
		return nil // 落盘失败不让刷新失败
	}
	return nil
}

// Renewable 报告是否可续期。
func (p *Provider) Renewable(cred gateway.Credential) bool {
	a, err := authOf(cred)
	if err != nil {
		return false
	}
	return a.Renewable()
}

// LoginFlow 返回（并缓存）登录流程。
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
func byteReader(b []byte) io.Reader { return &sliceReader{b: b} }

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

// 编译期断言。
var (
	_ gateway.Provider               = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.ErrorClassifier        = (*Provider)(nil)
	_ gateway.HealthProbeExt         = (*Provider)(nil)
	_ gateway.CredentialRefresher    = (*Provider)(nil)
)
