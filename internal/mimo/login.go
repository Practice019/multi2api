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
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

const loginSessionTTL = 15 * time.Minute

// route 自动收割（登录后未命中本机小米会话时，挂起等待并轮询探测）：
// 用户在浏览器登录 account.xiaomi.com 的瞬间，网关自动命中 → route 化 → 完成。
// 超时窗口结束仍未出现 → 回落 paid 兜底（登录不失败，只是没有免费通道）。
const (
	routeWaitWindow  = 3 * time.Minute
	routePollEvery   = 5 * time.Second
)

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

	// waitRoute：OAuth 已成功但本机无该账号的小米会话 → 挂起等待自动收割。
	// 轮询（watchRoute）命中浏览器/桌面会话后 route 化并 ready；超时回落 paid。
	waitRoute     bool
	routeDeadline time.Time
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
	// manual 模式不需要本机回调端口（结果靠用户粘贴 code 回来）—— 不去绑，
	// 否则端口被占会让"服务器上的手动登录"被无关故障误杀。
	if f.p.oauthRedirectMode != "manual" {
		if err := f.p.ensureCallbackServer(); err != nil {
			return "", "", err
		}
	}
	o, err := newOAuthSession()
	if err != nil {
		return "", "", err
	}
	// key_name 复用（避免用户控制台堆垃圾 key）。
	if kn, kerr := loadOrCreateKeyName(f.p.authDir); kerr == nil && kn != "" {
		o.keyName = kn
	}
	// redirect_uri 两态：
	//   auto   → 本机 127.0.0.1:port（浏览器与网关同机才收得到回调）
	//   manual → 平台 code/callback 页（服务器部署：授权后页面展示密文，
	//            用户复制回跳 URL 粘贴进控制台，走 /admin/mimo/login/complete）
	redirect := fmt.Sprintf("http://127.0.0.1:%s/", f.p.callbackPort)
	if f.p.oauthRedirectMode == "manual" {
		redirect = f.p.platformBase() + "/authorize/code/callback"
	}
	authURL := authorizeURL(f.p.platformBase(), o.pubKeyB64URL(), redirect, o.keyName)

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

	// ── 登录一次即打通客户端计费（route）通道 ─────────────────────────
	// 统一从本机浏览器/桌面会话按【本次登录账号 uid】精确取 passToken 套：
	//   命中        → route 化 + SSO 预换（免费服务端计费），登录直接完成；
	//   未命中      → **挂起等待自动收割**：UI 保持"登录中"，用户去浏览器
	//                 登录 account.xiaomi.com（该账号），网关轮询自动命中
	//                 → route 化 → 完成；窗口(3min)超时 → 回落 paid 兜底。
	// 全程不需要手动复制 Cookie、不需要本机桌面客户端。
	routeReady := false
	if f.p.probeDesktopCookies != nil {
		if ck, cerr := f.p.probeDesktopCookies(a.UID); cerr == nil && ck.PassToken != "" && ck.CUserID != "" && (ck.UserID == "" || ck.UserID == a.UID) {
			a.Channel = ChannelRoute
			a.PassToken = ck.PassToken
			a.CUserID = ck.CUserID
			if ck.DeviceID != "" {
				a.DeviceD = ck.DeviceID
			}
			if ck.UserID != "" && a.UID == "" {
				a.UID = ck.UserID
			}
			if serr := f.p.client.SSOFresh(context.Background(), a); serr != nil {
				log.Printf("mimo: 登录完成，route SSO 预换失败（对话时自动重试）uid=%s: %v", shortUID(a.UID), serr)
			} else {
				log.Printf("mimo: 登录已打通 route 通道（SSO 预换成功，客户端计费）uid=%s", shortUID(a.UID))
			}
			routeReady = true
		} else if cerr != nil {
			log.Printf("mimo: 登录完成但本机会话未命中 —— 进入等待自动收割（请用该账号在浏览器登录 https://account.xiaomi.com，3 分钟内自动完成）uid=%s: %v", shortUID(a.UID), cerr)
		}
	}
	if routeReady {
		f.finishLogin(e, a)
		return nil
	}

	// 未命中：挂起等待（UI 保持"登录中"），后台轮询自动收割本机会话。
	f.mu.Lock()
	e.waitRoute = true
	e.routeDeadline = time.Now().Add(routeWaitWindow)
	f.mu.Unlock()
	go f.watchRoute(e, a)
	return nil
}

// watchRoute 后台等待/收割本账号的小米会话：
//  1. 先轮询读本机 Cookie 库（兼容用户在自己浏览器登录，给 3 轮机会）；
//  2. 未命中 → 自启受控浏览器（Edge/Chrome 独立实例自动弹出窗口），
//     用户在该窗口登录该账号，CDP 监控收割 Cookie；
//  3. 命中 → route 化 + SSO 预换 → 完成登录；全部失败/超时 → 回落 paid。
func (f *loginFlow) watchRoute(e *loginEntry, a *Auth) {
	// 阶段 1：读库轮询（每 5s，最多 3 轮 ≈15s）
	for i := 0; i < 3; i++ {
		ck, cerr := f.probeOnce(a.UID)
		if ck != nil {
			f.applyRoute(e, a, ck)
			return
		}
		if cerr != nil {
			log.Printf("mimo: 等待收割读库未命中（第 %d 轮）uid=%s: %v", i+1, shortUID(a.UID), cerr)
		}
		time.Sleep(routePollEvery)
	}

	// 阶段 2：自启受控浏览器，用户登录后自动收割（阻塞等待，最长窗口剩余）。
	log.Printf("mimo: 自动弹出受控浏览器，请在弹出窗口登录该小米账号（3 分钟超时）uid=%s", shortUID(a.UID))
	if f.p.launchBrowserLogin != nil {
		if ck, lerr := f.p.launchBrowserLogin(a.UID); lerr == nil && ck != nil && ck.PassToken != "" && ck.CUserID != "" {
			f.applyRoute(e, a, ck)
			return
		} else if lerr != nil {
			log.Printf("mimo: 受控浏览器登录未完成（回落 paid）uid=%s: %v", shortUID(a.UID), lerr)
		}
	}
	log.Printf("mimo: 等待收割结束，回落 paid（登录未失败；可重新登录重试）uid=%s", shortUID(a.UID))
	f.finishLogin(e, a)
}

// probeOnce 读库探测一次，返回命中结果或错误。
func (f *loginFlow) probeOnce(uid string) (*DesktopCookie, error) {
	if f.p.probeDesktopCookies == nil {
		return nil, fmt.Errorf("probe disabled")
	}
	ck, err := f.p.probeDesktopCookies(uid)
	if err != nil || ck == nil || ck.PassToken == "" || ck.CUserID == "" {
		return nil, err
	}
	if ck.UserID != "" && ck.UserID != uid {
		return nil, fmt.Errorf("session of uid=%s, want %s", ck.UserID, uid)
	}
	return ck, nil
}

// applyRoute 用命中的会话套 route 化 + SSO 预换 + 完成登录。
func (f *loginFlow) applyRoute(e *loginEntry, a *Auth, ck *DesktopCookie) {
	a.Channel = ChannelRoute
	a.PassToken = ck.PassToken
	a.CUserID = ck.CUserID
	if ck.DeviceID != "" {
		a.DeviceD = ck.DeviceID
	}
	if ck.UserID != "" && a.UID == "" {
		a.UID = ck.UserID
	}
	if serr := f.p.client.SSOFresh(context.Background(), a); serr != nil {
		log.Printf("mimo: 收割命中，SSO 预换失败（对话时自动重试）uid=%s: %v", shortUID(a.UID), serr)
	} else {
		log.Printf("mimo: 收割命中，route 通道打通（SSO 预换成功，客户端计费）uid=%s", shortUID(a.UID))
	}
	f.finishLogin(e, a)
}

// finishLogin 以最终 Auth 构造 Credential 并标记就绪（命中 route / 超时 paid 共用）。
func (f *loginFlow) finishLogin(e *loginEntry, a *Auth) {
	cred := gateway.Credential{
		Provider: providerID,
		UID:      a.UID,
		Nickname: a.Nickname,
		// Secret 必须是可 MarshalAuthFile 的包装（admin pollViaFlow 501 硬要求）。
		Secret: &authFile{a: a},
	}
	f.mu.Lock()
	if cur, ok := f.sessions[e.state]; ok && !cur.ready {
		cur.waitRoute = false
		cur.ready = true
		cur.cred = cred
	}
	f.mu.Unlock()
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
