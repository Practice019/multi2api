// backend.go 把 codearts 包适配到 server.Backend 接口。
//
// 存在的原因：pool / session / handler / admin 都基于 auth.Auth 与
// upstream.ModelInfo 这套"对外词汇表"构建，而 CodeArts 有自己的
// 凭证形态（AK/SK + DPoP）与错误分类。适配层做两件事：
//  1. 凭证双向投影（auth.Auth <-> codearts.Auth），并缓存映射，
//     保证 handler 拿到的 *auth.Auth 始终对应同一个 CodeArts 账号；
//  2. 把 CodeArts 的错误分类映射到 pool 能理解的 upstream.ErrKind。
package codearts

import (
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// Backend 包装 *Client，使其满足 server.Backend 接口。
type Backend struct {
	*Client

	mu sync.Mutex
	// byUID 把 auth.Auth.UID 映射到 CodeArts 账号，避免每次请求重建。
	byUID map[string]*Auth
	// byPtr 反向映射：handler 传回来的 *auth.Auth 指针 -> CodeArts 账号。
	// 用指针做 key 是因为同一个 uid 允许存在多份凭证（多账号池场景下不会，
	// 但热重载/测试时会）。
	byPtr map[*auth.Auth]*Auth

	// OnAuthChanged 在凭证被刷新后回调（供 pool 同步展示）。
	OnAuthChanged func(a *auth.Auth)
}

// NewBackend 从 CodeArts 凭证目录构建 Backend。
func NewBackend(authDir string) (*Backend, error) {
	creds, err := LoadDir(authDir)
	if err != nil {
		return nil, fmt.Errorf("加载 CodeArts 凭证: %w", err)
	}
	b := &Backend{
		Client: New(),
		byUID:  make(map[string]*Auth, len(creds)),
		byPtr:  make(map[*auth.Auth]*Auth, len(creds)),
	}
	for _, c := range creds {
		b.byUID[c.UID] = c
	}
	return b, nil
}

// Reload 重新扫描凭证目录，替换内存中的账号集合。
//
// 供管理台的「刷新账号」按钮调用 —— 用户跑完 cmd/login 后点一下即可让新账号进池。
//
// 注意：这是**替换**而不是追加。已在池中的旧账号如果对应文件被删除，
// 会随之消失。这与 pool.SyncToDir 的语义一致（以目录为准）。
func (b *Backend) Reload(authDir string) error {
	creds, err := LoadDir(authDir)
	if err != nil {
		return fmt.Errorf("重载 CodeArts 凭证: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.byUID = make(map[string]*Auth, len(creds))
	b.byPtr = make(map[*auth.Auth]*Auth, len(creds))
	for _, c := range creds {
		b.byUID[c.UID] = c
	}
	return nil
}

// Accounts 返回投影后的 auth.Auth 列表，供 pool.SyncToDir 使用。
//
// 投影规则：
//
//	auth.Auth.AccessToken  <- codearts.SecurityToken（仅用于展示与存在性判断）
//	auth.Auth.RefreshToken <- codearts.RefreshToken
//	auth.Auth.ExpiresAt    <- codearts.ExpiresAt
//	auth.Auth.UID          <- codearts.UID
//
// 注意 AccessToken 放的是 securityToken 而非 AK —— 因为 pool 只用它做
// "凭证是否为空"的判断；真正的签名材料留在 codearts.Auth 里。
func (b *Backend) Accounts() []*auth.Auth {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*auth.Auth, 0, len(b.byUID))
	for _, c := range b.byUID {
		out = append(out, b.project(c))
	}
	return out
}

// project 把 codearts.Auth 投影成 auth.Auth，并登记双向映射。
func (b *Backend) project(c *Auth) *auth.Auth {
	a := &auth.Auth{
		AccessToken:  c.SecurityToken,
		RefreshToken: c.RefreshToken,
		ExpiresAt:    c.ExpiresAt,
		Domain:       "codearts",
		UID:          c.UID,
		Nickname:     c.Nickname,
		FilePath:     c.FilePath,
	}
	b.byPtr[a] = c
	return a
}

// resolve 把 handler 传来的 *auth.Auth 还原成 CodeArts 账号。
func (b *Backend) resolve(a *auth.Auth) (*Auth, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.byPtr[a]; ok {
		return c, nil
	}
	if c, ok := b.byUID[a.UID]; ok {
		b.byPtr[a] = c
		return c, nil
	}
	return nil, fmt.Errorf("codearts: 未知账号 uid=%s", a.UID)
}

// syncBack 把刷新后的 CodeArts 凭证回写到投影对象上。
func (b *Backend) syncBack(a *auth.Auth, c *Auth) {
	a.AccessToken = c.SecurityToken
	a.RefreshToken = c.RefreshToken
	a.ExpiresAt = c.ExpiresAt
}

// ---------------- server.Backend 实现 ----------------

// ChatStream 实现 server.Backend。
func (b *Backend) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	c, err := b.resolve(a)
	if err != nil {
		return nil, 0, []byte(err.Error()), nil
	}
	// 提前续期：STS 只有约 30 分钟，等到 401 再刷会浪费一次往返。
	if c.NeedsRefresh(3*time.Minute) && c.RefreshToken != "" {
		if rerr := b.Client.RefreshToken(c); rerr != nil {
			log.Printf("codearts: 预续期失败（继续尝试原凭证）: %v", rerr)
		} else {
			b.syncBack(a, c)
			if b.OnAuthChanged != nil {
				b.OnAuthChanged(a)
			}
		}
	}

	rc, status, respBody, err := b.Client.ChatStream(c, body)
	b.syncBack(a, c) // 内部重试续期后也同步一次
	return rc, status, respBody, err
}

// RefreshToken 实现 server.Backend。
func (b *Backend) RefreshToken(a *auth.Auth) error {
	c, err := b.resolve(a)
	if err != nil {
		return err
	}
	if err := b.Client.RefreshToken(c); err != nil {
		return err
	}
	b.syncBack(a, c)
	if b.OnAuthChanged != nil {
		b.OnAuthChanged(a)
	}
	return nil
}

// FetchModels 实现 server.Backend。
//
// CodeArts 没有公开的 /models 端点（实测 /api/v2/models 等均 404），
// 模型清单来自客户端本地配置。这里返回一份静态目录，
// 内容依据内核日志实证的 inferhub provider 模型列表。
func (b *Backend) FetchModels(a *auth.Auth) ([]upstream.ModelInfo, error) {
	if _, err := b.resolve(a); err != nil {
		return nil, err
	}
	return KnownModels(), nil
}

// Models 实现 server.Backend：返回静态模型目录（不发网络请求）。
//
// 这是 /v1/models 在"没有健康账号"时的回退来源 ——
// 它必须描述 CodeArts 自己的模型，而不是复用别家上游的硬编码表。
func (b *Backend) Models() []upstream.ModelInfo {
	return KnownModels()
}

// Welfare / Subscription 的实现委托给 Client，只做凭证解析。
//
// 为什么不塞进 server.Backend 接口：福利与订阅是 CodeArts 特有的运营能力，
// 硬塞进通用接口会逼 CodeBuddy 侧实现一堆空方法。
// 管理台通过类型断言按需取用（见 cmd/server 的接线）。

// FetchWelfare 列出当前账号的福利活动。
func (b *Backend) FetchWelfare(a *auth.Auth) ([]WelfareItem, error) {
	c, err := b.resolve(a)
	if err != nil {
		return nil, err
	}
	return b.Client.FetchWelfare(c)
}

// ClaimAllWelfare 领取所有当前可领的福利。
func (b *Backend) ClaimAllWelfare(a *auth.Auth) ([]WelfareResult, error) {
	c, err := b.resolve(a)
	if err != nil {
		return nil, err
	}
	return b.Client.ClaimAllWelfare(c)
}

// FetchSubscription 查套餐与额度。
func (b *Backend) FetchSubscription(a *auth.Auth) (*Subscription, error) {
	c, err := b.resolve(a)
	if err != nil {
		return nil, err
	}
	return b.Client.FetchSubscription(c)
}

// Welfare 实现 admin.WelfareProvider（按 uid 取账号）。
//
// 为什么接口用 uid 字符串而不是 *auth.Auth：
// admin 层不该知道上游的凭证类型；它只持有账号 id。
// 这里负责把 uid 解析回具体的 codearts.Auth。
type Welfare struct{ b *Backend }

// NewWelfare 构造 provider（传 nil 或未接线时由调用方判断）。
func (b *Backend) NewWelfare() *Welfare { return &Welfare{b: b} }

func (w *Welfare) byUID(uid string) (*Auth, error) {
	w.b.mu.Lock()
	defer w.b.mu.Unlock()
	if c, ok := w.b.byUID[uid]; ok {
		return c, nil
	}
	// uid 也可能是投影对象的 UID（与凭证 UID 相同），再兜一层
	for _, c := range w.b.byUID {
		if c.UID == uid || c.AccessKey == uid {
			return c, nil
		}
	}
	return nil, fmt.Errorf("未知账号 uid=%s", uid)
}

// FetchWelfare 实现 admin.WelfareProvider。
func (w *Welfare) FetchWelfare(uid string) ([]WelfareItem, error) {
	c, err := w.byUID(uid)
	if err != nil {
		return nil, err
	}
	return w.b.Client.FetchWelfare(c)
}

// ClaimAllWelfare 实现 admin.WelfareProvider。
func (w *Welfare) ClaimAllWelfare(uid string) ([]WelfareResult, error) {
	c, err := w.byUID(uid)
	if err != nil {
		return nil, err
	}
	return w.b.Client.ClaimAllWelfare(c)
}

// FetchSubscription 实现 admin.WelfareProvider。
func (w *Welfare) FetchSubscription(uid string) (*Subscription, error) {
	c, err := w.byUID(uid)
	if err != nil {
		return nil, err
	}
	return w.b.Client.FetchSubscription(c)
}

// MarkQuota 就地把某模型标记为额度耗尽（实现 server 侧的类型断言接口）。
//
// 由请求路径在撞到真实额度错误时调用：比后台探测更省（零额外请求），
// 且更及时（错误发生的那一刻就记下）。
func (b *Backend) MarkQuota(model, reason string) { MarkQuotaExhausted(model, reason) }

// ClearQuota 清除某模型的额度标记（实现 server 侧的类型断言接口）。
//
// 由请求路径在成功调用后调用：额度可能周期性恢复，
// 成功后旧标记必须失效，否则用户会一直看到过期状态。
func (b *Backend) ClearQuota(model string) { ClearQuota(model) }

// QuotaStates 返回各模型额度状态（只读缓存）。
//
// 返回 map[string]QuotaState —— 不是 server.ModelQuota，因为
// codearts 不能 import server（server 已 import codearts，会成环）。
// 两者字段名完全一致，由 cmd/server 的适配层做一次转换。
//
// 只在**探测过或撞到过**额度错误时才有条目；未确认的模型不出现在结果里，
// 这样 /v1/models 只标记确实耗尽的那几个，不会误伤可用的模型。
func (b *Backend) QuotaStates() map[string]upstream.ModelQuota {
	states, _ := QuotaStates()
	if len(states) == 0 {
		return nil
	}
	out := make(map[string]upstream.ModelQuota, len(states))
	for k, v := range states {
		out[k] = upstream.ModelQuota{
			Exhausted: v.Exhausted,
			Reason:    v.Reason,
			CheckedAt: v.CheckedAt,
		}
	}
	return out
}

// ProbeQuotaNow 主动探测全部模型额度（供管理台手动触发）。
// ProbeQuota 实现 admin.QuotaProber：逐个探测模型额度并返回扁平视图。
//
// 串行 + 间隔：账号只有 3 个并发名额，并发探测会互相挤掉，
// 拿到的就是"并发上限"而不是真实额度状态 —— 那比不探测更误导。
func (b *Backend) ProbeQuota() (map[string]any, error) {
	creds := b.Accounts()
	if len(creds) == 0 {
		return nil, fmt.Errorf("账号池为空，无法探测额度")
	}
	c, err := b.resolve(creds[0])
	if err != nil {
		return nil, err
	}
	states := b.Client.ProbeAllQuota(c)

	out := make(map[string]any, len(states))
	for k, v := range states {
		out[k] = map[string]any{
			"exhausted":  v.Exhausted,
			"reason":     v.Reason,
			"checked_at": v.CheckedAt,
		}
	}
	return out, nil
}

// FetchModelCatalog 实现 server.Backend。

// FetchModelCatalog 实现 server.Backend。
//
// 倍率优先从 AgentCenter 接口**动态拉取**（带 1h 缓存）；
// 拉不到时降级到静态表（knownModels 里的实测快照）。
// 这样上游调价不需要改代码，而接口抖动时仍有值可显示。
func (b *Backend) FetchModelCatalog(a *auth.Auth) (*upstream.ModelCatalog, error) {
	c, err := b.resolve(a)
	if err != nil {
		return nil, err
	}

	live, ferr := b.Client.FetchMultipliers(c)
	if ferr != nil {
		log.Printf("codearts: 动态拉取倍率失败，降级用静态表: %v", ferr)
	}

	cat := &upstream.ModelCatalog{}
	for _, m := range knownModels {
		if !m.Verified {
			continue
		}
		mult, known := m.Multiplier, m.MultiplierKnown
		// 动态值优先
		if v, ok := live[m.ID]; ok {
			mult, known = v, true
		}
		if !known {
			continue // 上游未下发 —— 不进目录，避免被读成"免费"
		}
		cat.Models = append(cat.Models, upstream.ModelCatalogEntry{
			ID:                m.ID,
			Name:              m.Name,
			Vendor:            "codearts",
			Tags:              []string{"codearts"},
			CreditsRaw:        fmt.Sprintf("x%.2f credits", mult),
			Multiplier:        mult,
			MaxInputTokens:    m.ContextWindow,
			SupportsToolCall:  true,
			SupportsImages:    false,
			SupportsReasoning: m.Reasoning,
		})
	}
	return cat, nil
}

// ClassifyUpstream 把 CodeArts 的错误分类映射到 upstream.ErrKind，
// 使 pool 的冷却策略无需改动即可复用。
func ClassifyUpstream(status int, body string) upstream.ErrKind {
	switch Classify(status, body) {
	case ErrHardCredit:
		return upstream.ErrHardCredit
	case ErrSoftRate:
		return upstream.ErrSoftRate
	case ErrNotFound:
		return upstream.ErrNotFound
	case ErrServer:
		return upstream.ErrServer
	case ErrClient, ErrAuth:
		return upstream.ErrClient
	default:
		return upstream.ErrNone
	}
}
