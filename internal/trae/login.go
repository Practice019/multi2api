// login.go TRAE 的页内登录（gateway.LoginFlow 实现）。
//
// # 流程（对齐其它上游：点按钮 → 打开登录页 → 自动回填，零手动步骤）
//
//	Start() → 起本机回调监听（127.0.0.1:18080/authorize，见 callback.go），
//	          返回**真实 TRAE 登录 URL**（www.trae.cn/authorization）
//	前端    → 把 URL 渲染成链接，用户点开 → TRAE 登录页 → 手机号/验证码登录
//	回调    → 浏览器跳回 127.0.0.1:18080/authorize → 网关自动接收 →
//	          解析 → ExchangeToken → GetUserInfo → 标记就绪（全程自动）
//	Poll()  → 就绪返回凭证（核心落盘 + 并池）
//
// 与 workbuddy 的 OAuth 设备码流程同一形态：authURL 是上游登录页，
// 网关只负责 poll。区别只是回调落在本机端口（TRAE 协议如此）。
package trae

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// loginSessionTTL 一次进行中的登录会话保留多久（等用户走完浏览器流程）。
const loginSessionTTL = 15 * time.Minute

// loginFlow 实现 gateway.LoginFlow。
type loginFlow struct {
	p *Provider

	mu       sync.Mutex
	sessions map[string]*loginEntry
}

// loginEntry 一次进行中的登录。
type loginEntry struct {
	state string // 会话键（buildAuth 标记就绪时反查用）
	at    time.Time

	machineID string
	deviceID  string
	traceID   string

	processing bool // 回调已在处理（防重复消费）
	ready      bool
	cred       gateway.Credential
}

var _ gateway.LoginFlow = (*loginFlow)(nil)

// LoginFlow 返回本上游的登录流程（缓存实例 —— 见 loomy 的同款注释）。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil {
		return nil, false
	}
	p.loginOnce.Do(func() {
		p.loginCached = &loginFlow{p: p, sessions: make(map[string]*loginEntry)}
	})
	return p.loginCached, true
}

// Start 让 *Provider 直接满足 gateway.LoginFlow（ExtOf 是类型断言）。
func (p *Provider) Start() (string, string, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("trae: 登录流程未初始化")
	}
	return f.(*loginFlow).Start()
}

// Poll 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("trae: 登录流程未初始化")
	}
	return f.(*loginFlow).Poll(state)
}

// Configured 报告这份部署**真的能**添加账号。
//
// TRAE 登录需要本机回调端口可绑定；除此之外无外部配置依赖。
// 恒为 true（与 loomy 的短信登录同一判据："任何部署都能加账号"；
// 端口被占时 Start 会明确报错，不在 Configured 里假装不行）。
func (p *Provider) Configured() bool { return p != nil }

// Configured 让 *loginFlow 满足 gateway.LoginFlow。
func (f *loginFlow) Configured() bool { return f != nil && f.p != nil }

// Start 建一次登录：确保回调监听就绪 + 生成设备对 + 返回真实登录 URL。
func (f *loginFlow) Start() (string, string, error) {
	if err := f.p.ensureCallbackServer(); err != nil {
		return "", "", err
	}
	state, err := newLoginState()
	if err != nil {
		return "", "", err
	}
	machineID, err := randomHex32()
	if err != nil {
		return "", "", err
	}
	deviceID, err := randomHex32()
	if err != nil {
		return "", "", err
	}
	traceID, err := randomHex16()
	if err != nil {
		return "", "", err
	}
	f.mu.Lock()
	f.gcLocked()
	f.sessions[state] = &loginEntry{
		state:     state,
		at:        time.Now(),
		machineID: machineID,
		deviceID:  deviceID,
		traceID:   traceID,
	}
	f.mu.Unlock()

	// authURL = 真实的 TRAE 登录页（前端渲染成链接，点开即登录）。
	authURL := BuildLoginURL(machineID, deviceID, traceID)
	return state, authURL, nil
}

// Poll 取回已就绪的凭证；没就绪就是 pending。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	f.mu.Lock()
	e, ok := f.sessions[state]
	if !ok {
		f.mu.Unlock()
		return gateway.Credential{}, errors.New("trae: 登录会话不存在或已结束，请重新发起")
	}
	if !e.ready {
		f.mu.Unlock()
		return gateway.Credential{}, gateway.ErrLoginPending
	}
	delete(f.sessions, state)
	f.mu.Unlock()
	return e.cred, nil
}

// finish 用回调链接换 token（保留给测试与手动兜底路径用）。
func (f *loginFlow) finish(state, callback string) (*Auth, error) {
	f.mu.Lock()
	e, ok := f.sessions[state]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("trae: 登录会话不存在或已结束，请重新发起")
	}
	if e.ready {
		return nil, errors.New("这个会话已经登录完成")
	}
	return f.buildAuth(e, callback)
}

// buildAuth 解析回调 → ExchangeToken（或 userJwt 兜底）→ GetUserInfo → 就绪。
//
// 成功：在持锁下标记 ready 并写入 cred（核心 poll 路径据此落盘 + 并池）。
// 失败：保持未就绪（下次回调可重试），返回错误供日志。
func (f *loginFlow) buildAuth(e *loginEntry, callback string) (*Auth, error) {
	info, err := ParseCallback(callback)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a := &Auth{
		ApiHost:   f.p.client.OAuthHost,
		Domain:    "trae.cn",
		MachineID: e.machineID,
		DeviceID:  e.deviceID,
	}
	if info.RefreshToken != "" {
		// 主路径：refreshToken → ExchangeToken（换 accessToken + 轮换 refreshToken）。
		a.RefreshToken = info.RefreshToken
		if err := f.p.client.RefreshToken(a); err != nil {
			return nil, fmt.Errorf("trae: ExchangeToken 失败: %w", err)
		}
	} else {
		// 兜底：回调里只有 userJwt.Token（无 refreshToken → 无法自动续期）。
		a.AccessToken = info.Token
		a.ExpiresAt = normalizeExpiresAtMilli(info.TokenExpireAt)
		if a.ExpiresAt <= 0 {
			a.ExpiresAt = time.Now().Add(14 * 24 * time.Hour).Unix()
		}
	}
	// 优先取回调 userInfo 的身份，再让 GetUserInfo 确认/补全（失败不阻塞）。
	a.UID = info.UserID
	a.Nickname = info.ScreenName
	a.EnterpriseID = info.TenantID
	if uid, nick, ent, uerr := f.p.client.GetUserInfo(ctx, a); uerr == nil && uid != "" {
		a.UID = uid
		if nick != "" {
			a.Nickname = nick
		}
		if ent != "" {
			a.EnterpriseID = ent
		}
	}
	if a.UID == "" {
		return nil, errors.New("trae: 未能从回调/GetUserInfo 拿到 uid，请确认回调链接完整")
	}

	f.mu.Lock()
	if cur, ok := f.sessions[e.state]; ok {
		cur.ready = true
		cur.cred = gateway.Credential{
			Provider: providerID,
			UID:      a.UID,
			Nickname: a.Nickname,
			// Secret 必须能落盘（核心 pollViaFlow 要求 MarshalAuthFile）。
			Secret: &authFile{a: a},
		}
	}
	f.mu.Unlock()
	return a, nil
}

// gcLocked 回收超时会话（调用方持锁）。
func (f *loginFlow) gcLocked() {
	for k, e := range f.sessions {
		if time.Since(e.at) > loginSessionTTL {
			delete(f.sessions, k)
		}
	}
}

// ── 凭证落盘包装 ────────────────────────────────────────────────────────

// authFile 让一份 *Auth 满足核心的落盘窄接口（MarshalAuthFile）。
type authFile struct{ a *Auth }

// MarshalAuthFile 返回 (文件名, 内容) —— 核心 pollViaFlow 的 authFileWriter 契约。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.a == nil {
		return "", nil, errors.New("trae: 凭证为空")
	}
	raw, err := MarshalAuthFile(f.a)
	if err != nil {
		return "", nil, err
	}
	name := FileName(f.a)
	if name == "" {
		return "", nil, errors.New("trae: 凭证文件名为空")
	}
	return name, raw, nil
}

// ── 随机工具 ────────────────────────────────────────────────────────────

func newLoginState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomHex32() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomHex16() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
