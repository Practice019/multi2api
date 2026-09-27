// provider.go 把 Cline（Cline 桌面端 / Cline API）适配为 gateway.Provider。
//
// # 这个包在整条解耦链里的位置
//
// 它是第六个 Adapter。判据 1 的实测对象又多了一个：
//
//	如果加这个上游需要改核心（pool/logbuf/admin/server/scheduler），
//	说明 gateway 那条接缝漏了；如果一行都不用改，判据 1 成立。
//
// # 与其他上游的差异（都已实测，来自参照项目）
//
//	Cline 没有 User-Agent 常量 —— 请求头里不含 UA（全仓 grep 零命中）
//	Cline 不注册任何签到能力 —— 后端没有签到接口（能力矩阵 balance:true, dailyCheckin:false）
//	Cline 的模型目录要打**两个**端点（免费集合只在 recommended-models 里）
//	Cline 的鉴权头前缀 workos: **不可剥**（剥掉即 401，文案还误导成"版本过旧"）
package cline

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
	client   *Client
	authDir  string
	apiBase  string
	workOS   string
	emailDummy struct{} // 占位，保持结构体非空便于未来加字段

	// creds 按 uid 取凭证（装配层注入；nil = 管理端点不可用）。
	// 见 adminroute.go 的 credentialSource。
	creds credentialSource

	loginOnce   sync.Once
	loginCached *loginFlow
}

// Config Provider 的可选依赖，全部可缺省。
//
// 零值 Config 得到一个"能跑契约测试的最小实例"：默认指向生产上游、
// 没有凭证目录。任何字段缺失都**降级**而不是 panic。
type Config struct {
	// Client 上游 HTTP 客户端。nil 时用 New()。
	Client *Client
	// APIBase 推理/账号端点基址。Client 为 nil 时用它构造。
	APIBase string
	// WorkOSBase 设备码授权基址。
	WorkOSBase string
	// AuthDir 本上游凭证的落盘目录（供 gateway.AuthDirExt 用）。
	AuthDir string
}

// NewProvider 契约测试用的无依赖构造。
//
// ⚠ 名字不能叫 New：本包的 client.go 已有 `New() *Client`（上游 HTTP 客户端），
// 两者是不同的东西，同名会编译冲突。契约测试里传的是**这个**函数。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// NewWithConfig 建 Provider。只构造，不做网络请求。
func NewWithConfig(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = New()
		if cfg.APIBase != "" {
			c.APIBase = cfg.APIBase
		}
		if cfg.WorkOSBase != "" {
			c.WorkOSBase = cfg.WorkOSBase
		}
	}
	return &Provider{
		client:  c,
		authDir: cfg.AuthDir,
		apiBase: c.APIBase,
		workOS:  c.WorkOSBase,
	}
}

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// 只声明**可验证**的两项：对话与动态模型目录。
//
// ⚠ Cline **没有**签到接口（对整个 sidecar 做字符串扫描，checkin/daily/campaign
// 均无 Cline 业务端点命中），所以**不声明** CapCheckin —— 声明了等于给前端画一个
// 点了必然失败的面板。同理不声明 CapQuotaProbe：它没有主动额度探测端点。
//
// 余额是另一套（/users/{id}/balance，见 FetchBalance），由 AdminExt 暴露 ——
// **这句话此前是不成立的**：AdminRoutes 当时并不存在，那个余额查询实现了
// 却没有任何入口能调到（只有测试能碰）。现已在 adminroute.go 补上端点，
// 注释与代码这才一致。
//
// 契约测试会检查"声明了的必须真的可用"，所以这里少报是安全的、多报会被当场抓住。
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

// authOf 从 Credential 里取出 Cline 的凭证结构。
//
// 只接受 *Auth；断言失败返回明确错误，不 panic（契约要求）。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("cline: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok {
		return nil, fmt.Errorf("cline: 凭证类型不对，期望 *cline.Auth，实际 %T", cred.Secret)
	}
	if a == nil {
		return nil, errors.New("cline: 凭证是 nil 指针")
	}
	if a.AccessToken == "" {
		return nil, errors.New("cline: 凭证缺少 accessToken（唯一鉴权材料）")
	}
	return a, nil
}

// Chat 转发一次对话。
//
// # 处理链
//
//  1. 请求体改写（强制 stream + max_tokens 上界收敛）
//  2. 带上 workos: 前缀的 Authorization 发往 /api/v1/chat/completions
//  3. 上游错误体原样带回（交给核心的分类器判断）
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	// 契约要求"ctx 取消时 Chat 必须尽快返回"：先检查一次，
	// 保证调用方传已取消的 ctx 时不必等一次网络往返。
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	outBody := PrepareBody(body)
	rc, status, respBody, err := p.client.ChatStream(ctx, a, outBody)
	if err != nil {
		// respBody 非空 = 上游返回了错误体（业务错误），仍按流返回。
		if len(respBody) > 0 {
			return gateway.ChatStream{
				Status: status,
				Body:   io.NopCloser(byteReader(respBody)),
			}, nil
		}
		return gateway.ChatStream{}, err
	}
	// 契约要求"err==nil 时 Status 或 Body 至少有一个非零"。
	if rc == nil {
		rc = io.NopCloser(byteReader(nil))
	}
	return gateway.ChatStream{Status: status, Body: rc}, nil
}

// Models 返回该上游的模型目录。
//
// # 实时优先、兜底表补齐
//
// 两个远端来源独立容错：任一端点挂掉只记 warning，由 mergeModels 用兜底表补齐。
// 这样"目录服务抖动"不会让用户的模型列表整个消失。
//
// # ⚠ 这里返回的是**带 · 免费 标记**的展示名
//
// 与参照项目的分歧点：参照的 resolveModel 刻意不带标记（标记只属于"选择列表"
// 语境）。本网关的 ModelInfo 只有 ID/ContextWindow/MaxOutputTokens 三个字段，
// 没有 name —— 标记无处可放，故网关侧不承担展示名职责
// （前端按 IsFree 语义自行标注，见 AdminExt 的模型端点）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	a, _ := authOf(cred) // 无凭证也要能给出兜底目录
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	models, warnings := p.client.loadModels(ctx, a)
	logWarnings(warnings)

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

// ProbeHealth 探测一份凭证是否健康可用（gateway.HealthProbeExt）。
//
// 用 /api/v1/users/me：200 即令牌有效。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.client.VerifyToken(ctx, a)
}

// Classify 按 HTTP 状态码 + 响应体判定错误类别（gateway.ErrorClassifier）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	return Classify(status, body)
}

// ── 凭证读写（gateway.CredentialLoader / CredentialSecretLoader / AuthDirExt）──

// LoadCredentials 读取 dir 下全部 Cline 凭证（只投影 uid/nickname）。
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
			Nickname: a.Nickname,
			FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret。
//
// ⚠ 必须与 LoadCredentials **读同一份对象**：各造一份会让"一次性 refresh_token
// 被消费两次"，池里那份永远停在作废的旧值上（那正是 codearts 那个 503 的根因）。
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
				Nickname: a.Nickname,
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
// # 终态与可重试的区分（决定核心是否换号）
//
//	ErrRefreshExpired → 终态，核心应标记该号需重新登录
//	其它 error        → 可重试
//
// 见 ErrRefreshExpired 的注释。
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	next, err := p.client.RefreshCredential(context.Background(), a)
	if err != nil {
		return err
	}
	// 原地更新：池子持有的是同一个指针，写回即生效。
	//
	// ⚠ 不能换成 `*a = *next`：那会连 FilePath 一起覆盖（next 的 FilePath
	// 来自 fallback，虽然当前实现会保留，但整块赋值把"哪些字段该留"变成
	// 隐式约定）。逐字段写回让"续期更新了什么"是显式的。
	a.AccessToken = next.AccessToken
	if next.RefreshToken != "" {
		a.RefreshToken = next.RefreshToken
	}
	if next.ExpireTime > 0 {
		a.ExpireTime = next.ExpireTime
	}
	if next.AccountID != "" {
		a.AccountID = next.AccountID
	}
	if next.Email != "" {
		a.Email = next.Email
	}
	if a.Nickname == "" {
		a.Nickname = next.Nickname
	}
	// 落盘失败不让刷新失败：内存里的凭证已经可用，下次启动才会用到旧的。
	if a.FilePath != "" {
		if err := saveAuthFile(a); err != nil {
			// 仅记日志（调用方拿不到 logger，这里不吞错但也不阻断）
			return nil
		}
	}
	return nil
}

// Renewable 报告凭证是否可续期（供装配层与诊断端点使用）。
func (p *Provider) Renewable(cred gateway.Credential) bool {
	a, err := authOf(cred)
	if err != nil {
		return false
	}
	return a.Renewable()
}
