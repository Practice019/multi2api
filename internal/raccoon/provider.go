// provider.go 把 Raccoon Work（商汤小浣熊）适配为 gateway.Provider。
//
// # 与其他上游的差异（都已实测，来自参照项目）
//
//	统一信封      业务响应是 {code,message,details,data}，**失败可能是 HTTP 200 + 非 0 code**
//	扫码 code     由客户端本地随机生成，服务端接受任意自造 code
//	手机号        AES-128-CFB 加密（仅短信路径用；本包不用，短信不可程序化）
//	过期判定      expires_at → JWT exp **回退是必需的**（只读前者会让续期静默失效）
//	无每日签到    服务端按日自动发放，**没有端点** → 不声明 CapCheckin
//	有一次性奖励  POST /desktop/v1/login/points/grant（需 X-Client-Platform）
package raccoon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"workbuddy2api/internal/gateway"
)

// Provider 实现 gateway.Provider 与若干可选扩展点。
type Provider struct {
	client  *Client
	authDir string

	// creds 按 uid 取凭证（装配层注入；nil = 管理端点不可用）。
	// 见 adminroute.go 的 credentialSource。
	creds credentialSource

	loginOnce   sync.Once
	loginCached *loginFlow
}

// Config Provider 的可选依赖，全部可缺省。
type Config struct {
	// Client 上游 HTTP 客户端。nil 时用 New()。
	Client *Client
	// APIBase 上游基址。Client 为 nil 时用它构造。
	APIBase string
	// AuthDir 本上游凭证的落盘目录（供 gateway.AuthDirExt 用）。
	AuthDir string
}

// NewProvider 契约测试用的无依赖构造。
//
// ⚠ 名字不能叫 New：本包 client.go 已有 `New() *Client`，同名会编译冲突。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// NewWithConfig 建 Provider。只构造，不做网络请求。
func NewWithConfig(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = New()
		if cfg.APIBase != "" {
			c.APIBase = cfg.APIBase
		}
	}
	return &Provider{client: c, authDir: cfg.AuthDir}
}

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// 只声明**可验证**的两项：对话与动态模型目录。
//
// ⚠ Raccoon **没有签到接口**（服务端按日自动发放每日积分，无端点），
// 所以**不声明** CapCheckin —— 声明了等于给前端画一个点了必然失败的面板。
// 同理不声明 CapQuotaProbe：它没有主动额度探测端点（余额是另一套，
// 由 /points/v1/balance 提供，走 AdminExt）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels
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

// authOf 从 Credential 里取出 Raccoon 的凭证结构。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("raccoon: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok {
		return nil, fmt.Errorf("raccoon: 凭证类型不对，期望 *raccoon.Auth，实际 %T", cred.Secret)
	}
	if a == nil {
		return nil, errors.New("raccoon: 凭证是 nil 指针")
	}
	if a.AccessToken == "" {
		return nil, errors.New("raccoon: 凭证缺少 access_token（唯一鉴权材料）")
	}
	return a, nil
}

// Chat 转发一次对话。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	// 契约要求"ctx 取消时 Chat 必须尽快返回"：先检查一次。
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	outBody := PrepareBody(body)
	rc, status, respBody, err := p.client.ChatStream(ctx, a, outBody)
	if err != nil {
		if len(respBody) > 0 {
			return gateway.ChatStream{Status: status, Body: io.NopCloser(bytesReader(respBody))}, nil
		}
		return gateway.ChatStream{}, err
	}
	if rc == nil {
		rc = io.NopCloser(bytesReader(nil))
	}
	return gateway.ChatStream{Status: status, Body: rc}, nil
}

// Models 返回模型目录（远端优先，失败回退兜底表）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, _ := authOf(cred) // 无凭据也要能给出兜底目录
	remote := p.client.FetchModelCatalog(ctx, a)
	models := mergeModels(remote)

	out := make([]gateway.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, gateway.ModelInfo{
			ID:              m.ID,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxTokens,
		})
	}
	return out, nil
}

// ProbeHealth 探测凭证是否健康（gateway.HealthProbeExt）。
//
// 用 /user_info：**信封 code==0** 才算健康。
// ⚠ 不能用"HTTP 200"当判据 —— 业务失败也是 200。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ui := p.client.FetchUserInfo(ctx, a)
	if ui.ID == "" && ui.Name == "" && ui.Phone == "" {
		return fmt.Errorf("raccoon: 令牌校验未返回用户信息（可能已失效）")
	}
	return nil
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
			UID:      a.UID(),
			Nickname: a.DisplayUID(),
			FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret。
//
// ⚠ 必须与 LoadCredentials **读同一份对象**：各造一份会让"一次性 refresh_token
// 被消费两次"，池里那份永远停在作废的旧值上。
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
				UID:      a.UID(),
				Nickname: a.DisplayUID(),
				FilePath: a.FilePath,
			},
			Secret: a,
		})
	}
	return out, nil
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

// ── 续期（gateway.CredentialRefresher）──────────────────────────────────

// RefreshCredential 续期一份凭证（原地更新 + 落盘）。
//
// ⚠ **必须保留服务端不返回的字段**：实测服务端可能只返回新的 access_token。
// 不保留旧 refresh_token 会让续期一次就把账号变成不可续期。
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	next, err := p.client.RefreshCredential(context.Background(), a)
	if err != nil {
		return err
	}
	// 逐字段写回（不整块赋值：那会把 FilePath 等一起覆盖，让"更新了什么"变隐式）
	a.AccessToken = next.AccessToken
	if next.RefreshToken != "" {
		a.RefreshToken = next.RefreshToken
	}
	if next.ExpiresAt != "" {
		a.ExpiresAt = next.ExpiresAt
	}
	if next.OfficeIdentity != "" {
		a.OfficeIdentity = next.OfficeIdentity
	}
	if next.UserID != "" {
		a.UserID = next.UserID
	}
	if next.Nickname != "" {
		a.Nickname = next.Nickname
	}
	if next.Phone != "" {
		a.Phone = next.Phone
	}
	if next.DeviceID != "" {
		a.DeviceID = next.DeviceID
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

// ── 积分（供 AdminExt 端点使用）─────────────────────────────────────────

// Balance 查余额（装配层的管理端点用）。
func (p *Provider) Balance(ctx context.Context, cred gateway.Credential) (*Balance, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	return p.client.FetchBalance(ctx, a)
}

// ClaimLoginGrant 领一次性登录奖励（装配层的管理端点用）。
func (p *Provider) ClaimLoginGrant(ctx context.Context, cred gateway.Credential) (LoginGrantResult, error) {
	a, err := authOf(cred)
	if err != nil {
		return LoginGrantResult{}, err
	}
	return p.client.ClaimLoginGrant(ctx, a)
}

// bytesReader 把 []byte 包成 io.Reader。
func bytesReader(b []byte) io.Reader {
	return &byteSliceReader{b: b}
}

type byteSliceReader struct {
	b []byte
	i int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
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
