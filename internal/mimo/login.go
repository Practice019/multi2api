// login.go MiMo 页内登录（gateway.LoginFlow）：X25519 加密回调 OAuth。
//
// 流程（对齐 trae 的 Start→回调→Poll 形态，报告 §4.5-A）：
//
//	Start()  → 生成 X25519 对 + 确保 127.0.0.1:18081 监听 → 返回授权 URL
//	浏览器   → platform.xiaomimimo.com/authorize 登录/授权 →
//	           302 回 http://127.0.0.1:18081/?u=<密文> → 网关解密得 {sk,uid,url}
//	Poll()   → 就绪返回 Credential（Secret 裹 authFile，核心负责落盘+并池）
//
// 手动兜底：无浏览器直达回调端口时，平台 code/callback 页给出回跳 URL，
// 用户把整串 URL（或裸 u 值）粘贴进「手动完成」端点（admin.go），
// 走同一个 buildAuthFromU。
package mimo

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

const loginSessionTTL = 15 * time.Minute

// platformBase 授权站点（可经配置覆盖；测试注入 httptest 域）。
var platformBaseOverride = ""

func (p *Provider) platformBase() string {
	if v := strings.TrimSpace(platformBaseOverride); v != "" {
		return v
	}
	return PlatformBase
}

type loginFlow struct {
	p *Provider

	mu       sync.Mutex
	sessions map[string]*loginEntry
}

type loginEntry struct {
	state      string
	at         time.Time
	oauth      *oauthSession
	process    bool
	ready      bool
	cred       gateway.Credential
	failReason string
}

var _ gateway.LoginFlow = (*loginFlow)(nil)

// LoginFlow 返回本上游的登录流程（缓存实例）。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	p.loginOnce.Do(func() {
		p.loginCached = &loginFlow{p: p, sessions: map[string]*loginEntry{}}
	})
	return p.loginCached, true
}

// Start/Poll/Configured 让 *Provider 直接满足 gateway.LoginFlow（ExtOf 断言用）。

// Start 发起一次登录。
func (p *Provider) Start() (string, string, error) {
	f, _ := p.LoginFlow()
	return f.(*loginFlow).Start()
}

// Poll 收取就绪凭证。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	f, _ := p.LoginFlow()
	return f.(*loginFlow).Poll(state)
}

// Configured Provider 级转发。
func (p *Provider) Configured() bool { return p != nil }

func (f *loginFlow) Configured() bool { return f != nil && f.p != nil }

func (f *loginFlow) Start() (string, string, error) {
	if err := f.p.ensureCallbackServer(); err != nil {
		return "", "", err
	}
	o, err := newOAuthSession()
	if err != nil {
		return "", "", err
	}
	// key_name 复用（避免用户控制台堆垃圾 key）。
	if kn, kerr := loadOrCreateKeyName(f.p.authDir); kerr == nil && kn != "" {
		o.keyName = kn
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%s/", f.p.callbackPort)
	authURL := authorizeURL(f.p.platformBase(), o.pubKeyB64URL(), redirect, o.keyName)
	// 授权 URL 带上 key_name（官方 mimo.ts:76-84 的参数名是 key_name）。
	authURL += "&key_name=" + url.QueryEscape(o.keyName)

	f.mu.Lock()
	f.gcLocked()
	f.sessions[o.state] = &loginEntry{state: o.state, at: time.Now(), oauth: o}
	f.mu.Unlock()
	return o.state, authURL, nil
}

func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.sessions[state]
	if !ok {
		return gateway.Credential{}, errors.New("mimo: 登录会话不存在或已结束，请重新发起")
	}
	if !e.ready {
		return gateway.Credential{}, gateway.ErrLoginPending
	}
	cred := e.cred
	delete(f.sessions, state)
	return cred, nil
}

// completeWithU 解密 u → 构造凭证 → 标记就绪（回调与手动粘贴两路共用）。
func (f *loginFlow) completeWithU(state, u string) error {
	f.mu.Lock()
	e, ok := f.sessions[state]
	f.mu.Unlock()
	if !ok {
		return errors.New("mimo: 登录会话不存在或已结束")
	}
	if e.ready {
		return errors.New("这个会话已经完成")
	}
	// u 可能整条 URL 粘贴进来：只取 u 参数。
	u = extractUParam(u)
	if u == "" {
		return errors.New("mimo: 没找到 u 参数（请粘贴完整回跳 URL 或裸 u 值）")
	}
	sk, uid, accountBase, err := e.oauth.DecryptCallbackU(u)
	if err != nil {
		f.mu.Lock()
		e.failReason = err.Error()
		f.mu.Unlock()
		return err
	}
	a := &Auth{
		Channel:    ChannelPaid,
		Type:       TypeAPI,
		Key:        sk,
		UID:        uid,
		BaseURL:    accountBase,
		Nickname:   "oauth:" + e.oauth.keyName,
		LoggedInAt: time.Now().UTC().Format(time.RFC3339),
		Source:     "oauth",
	}
	if a.UID == "" {
		a.UID = DeriveUID(sk)
	}
	cred := gateway.Credential{
		Provider: providerID,
		UID:      a.UID,
		Nickname: a.Nickname,
		// Secret 必须是可 MarshalAuthFile 的包装（admin pollViaFlow 501 硬要求）。
		Secret: &authFile{a: a},
	}
	f.mu.Lock()
	if cur, ok := f.sessions[e.state]; ok {
		cur.ready = true
		cur.cred = cred
	}
	f.mu.Unlock()
	return nil
}

// extractUParam 从"整条 URL / 裸 u 值"两种粘贴形态取 u。
func extractUParam(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "u=") {
		return s // 裸值
	}
	if qIdx := strings.Index(s, "?"); qIdx >= 0 {
		if q, err := url.ParseQuery(s[qIdx+1:]); err == nil {
			if v := q.Get("u"); v != "" {
				return v
			}
		}
	}
	// 形如 "u=xxxx"
	if strings.HasPrefix(s, "u=") {
		return strings.TrimPrefix(s, "u=")
	}
	return ""
}

func (f *loginFlow) gcLocked() {
	for k, e := range f.sessions {
		if time.Since(e.at) > loginSessionTTL {
			delete(f.sessions, k)
		}
	}
}

// newestPending 回调会话启发式（同 trae：一次登录在途通常只有一个）。
func (f *loginFlow) newestPending() *loginEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gcLocked()
	var target *loginEntry
	var newest time.Time
	for _, e := range f.sessions {
		if e == nil || e.ready || e.process {
			continue
		}
		if e.at.After(newest) {
			target, newest = e, e.at
		}
	}
	return target
}

// markProcessing 防同一回调重复消费。
func (f *loginFlow) markProcessing(e *loginEntry) bool {
	if e == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.ready || e.process {
		return false
	}
	e.process = true
	return true
}

// ── 凭证落盘包装（admin pollViaFlow 契约）───────────────────────────────

type authFile struct{ a *Auth }

// MarshalAuthFile 返回 (文件名, 内容) —— 核心落盘要求（admin.go 501 硬闸）。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.a == nil {
		return "", nil, errors.New("mimo: 凭证为空")
	}
	raw, err := MarshalAuthFile(f.a)
	if err != nil {
		return "", nil, err
	}
	name := FileName(f.a)
	if name == "" {
		return "", nil, errors.New("mimo: 凭证文件名为空")
	}
	return name, raw, nil
}
