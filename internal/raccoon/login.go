// login.go Raccoon 的登录流程（微信扫码轮询）。
//
// # 关键事实：code 由客户端本地随机生成，服务端接受任意自造 code
//
// 实测（AGENTS.md:3539-3541）：POST /login_with_qrcode_code 带一个本地生成的
// 32 位 hex 就返回 200 + {"status":"pending"}。扫码后服务端把该 code 置为 success。
//
// 正因如此，我们**完全不需要**官方 `office-raccoon://auth/callback` 那条链路
// （那条也不可用：回调地址写死在 Web bundle 里，改不成 localhost）。
//
// # 与参照项目的一处差异（刻意）
//
// 参照项目把二维码渲染在一个**本机 HTTP 登录页**里（127.0.0.1 随机端口，
// 页面内联 SVG 二维码 + 阿里云滑块脚本）。本网关是纯后端，没有那个页面 ——
// 改为把**二维码承载 URL** 作为登录链接返回，由前端自行渲染成二维码。
//
// 这样做的好处：不绑端口、不注入 HTML、不需要 QR 编码实现；
// 且二维码内容（URL）本来就是扫码要的东西，页面只是它的可视化 ——
// 参照项目自己也把"SVG 转义引号"列为踩过的坑（AGENTS.md:3654-3666）。
//
// 短信路径**不实现**：它要求 `captcha_param`，而那只能由 AliyunCaptcha.js
// 在浏览器里执行滑块后产出（raccoon-login-page.ts:417,506-524）。
// 纯 Go 无法程序化完成，源码侧也把短信列为次选。
package raccoon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// loginEntry 一次在途登录的会话。
type loginEntry struct {
	state string
	// nonce 回调防串号标识（塞进 redirect 的 query，见 Start）。
	nonce string
	// port 本机回调端口（仅用于日志与排障）。
	port     int
	issuedAt time.Time
	// ready/failRsn/cred 由 finishLogin / markFailed 写入，Poll 读取。
	ready   bool
	cred    gateway.Credential
	failRsn string
	mu      sync.Mutex
}

// authCallbackPath 本机回调的路径。
const authCallbackPath = "/raccoon/callback"

// browserLoginAppName 浏览器登录 URL 里的 appname。
//
// 取官方桌面端用的那一个（`/code/authorize` 的 appname 参数）——
// 它只是展示在授权页上的产品名，不影响鉴权。
const browserLoginAppName = "办公小浣熊客户端"

// loginTimeout 登录会话的有效期。
//
// 5 分钟：登录需要人操作（打开浏览器 → 登录 → 过滑块 → 授权），
// 与参照项目的 RACCOON_LOGIN_TIMEOUT_MS 同值。
const loginTimeout = 5 * time.Minute

// randomNonce 生成 32 位 hex 随机串（回调防串号标识）。
func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// markFailed 标记会话失败（Poll 会把它转成错误）。
func (f *loginFlow) markFailed(e *loginEntry, reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	// 已有结果就不覆盖：先到的成功比后到的超时更可信。
	if !e.ready && e.failRsn == "" {
		e.failRsn = reason
	}
	e.mu.Unlock()
}

// loginFlow 实现 gateway.LoginFlow。
type loginFlow struct {
	p   *Provider
	mu  sync.Mutex
	ses map[string]*loginEntry
}

// LoginFlow 返回（并缓存）登录流程实例。
//
// ⚠ 必须缓存：每次新建会让 sessions map 各是一份空的，于是 start 存下的会话
// 在 poll 里找不到（其它上游端到端实测抓过这个 bug）。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil {
		return nil, false
	}
	p.loginOnce.Do(func() {
		p.loginCached = &loginFlow{p: p, ses: map[string]*loginEntry{}}
	})
	return p.loginCached, true
}

// Configured 报告这份部署真的能走登录流程吗。
//
// Raccoon 的登录只看 API 基址可达，没有"是否配了 OAuth 客户端"这一层。
func (f *loginFlow) Configured() bool { return f != nil && f.p != nil }

// ── 让 *Provider 直接满足 gateway.LoginFlow ────────────────────────────────
//
// ⚠ 这三个转发方法是**必需的**：核心用 `ExtOf[LoginFlow](p)`（类型断言）
// 发现扩展点，而访问器 `LoginFlow() (LoginFlow, bool)` 的返回值
// **不参与方法集匹配** —— 断言看不见它。
//
// 少了它们，本包测试全绿（测试走访问器）但 `/admin/providers` 下发
// `login:null`，界面上没有「＋ 添加账号」按钮。实测事故就是这个形态，
// 现由 gateway.RunProviderContract 的 verifyExtensionsDiscoverable 守住。

// Start 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Start() (string, string, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("raccoon: 登录流程未初始化")
	}
	return lf.Start()
}

// Poll 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("raccoon: 登录流程未初始化")
	}
	return lf.Poll(state)
}

// Configured 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Configured() bool {
	lf, ok := p.LoginFlow()
	return ok && lf.Configured()
}

// Start 发起一次登录：起本地回调端口 + 返回**浏览器登录 URL**。
//
// 返回 (state, loginURL, error)。state 供 Poll 使用。
//
// # 为什么改走「浏览器登录」而不是旧的「直接给二维码内容」（用户报障）
//
// 旧实现返回 `https://xiaohuanxiong.com/login/mp?code=<32hex>` —— 那是
// **微信落地页**，只能在微信里打开：
//
//	官网主 bundle 里 `login/mp` 出现 **0 次**（它只被当作二维码内容拼出来）
//	微信 UA 与桌面 UA 请求它，返回的是**同一份 SPA 外壳**（路由表无 `mp`）
//
// 于是「用浏览器打开」和「直接点开这串链接」都是死路。而
// `/login?appname=…&redirect=…` 是**商汤官方 VS Code 扩展自己的浏览器登录
// 机制**（`Raccoon-VSCode/src/raccoonClient/raccoonClinet.ts` 的 getAuthUrl，
// browser 模式就是拼这个），`redirect` 由调用方指定。
//
// 走这条路后，**微信扫码与手机号短信都在官方页面上完成** ——
// 我们不碰阿里云滑块、不逆向，而且两种登录方式都可用。
//
// ⚠ 实测（2026-10-05，真实账号）：整条链路跑通，回调收到
// `authorization_code=ac_…`，换回 `{"code":0}` + 真实 access_token。
// `redirect` **没有白名单限制**。
func (f *loginFlow) Start() (string, string, error) {
	if f == nil || f.p == nil {
		return "", "", errors.New("raccoon: 登录流程未配置")
	}
	// ① 绑一个本机回环端口收回调（与 lobsterai 同一手法：只绑 127.0.0.1，
	//    端口交给内核选，避免与用户的其它服务抢固定端口）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("raccoon: 回调端口监听失败: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// ② nonce 防串号：回调只认自己发出去的那一个。
	//
	// ⚠ 用 nonce 而不是 `state` 参数：官方的 `/code/authorize` 只转发
	// `authorization_code`，**不复制**我们塞进 `/login` 的其它参数
	// （bundle 里 `_r` 只读 redirect/appname/ide）。所以防串号的标识
	// 必须塞进 **redirect 自己的 query**，它会以 `&` 追加 code 而不覆盖。
	nonce, err := randomNonce()
	if err != nil {
		_ = ln.Close()
		return "", "", fmt.Errorf("raccoon: 生成 nonce 失败: %w", err)
	}

	state := nonce // state 即 nonce（天然唯一且随机）

	e := &loginEntry{
		state:    state,
		nonce:    nonce,
		port:     port,
		issuedAt: time.Now(),
	}
	f.mu.Lock()
	f.ses[state] = e
	f.mu.Unlock()

	go f.serveAuthCallback(ln, e)

	webflow := fmt.Sprintf("http://127.0.0.1:%d%s?nonce=%s", port, authCallbackPath, nonce)
	return state, BrowserLoginURL(browserLoginAppName, webflow), nil
}

// serveAuthCallback 跑本机回调服务器，拿到 authorization_code 后立刻换凭证。
//
// 与 lobsterai 的 serveCallback 同构（那边是 portal 的 redirect_uri 回调）。
func (f *loginFlow) serveAuthCallback(ln net.Listener, e *loginEntry) {
	mux := http.NewServeMux()
	done := make(chan struct{})
	var closeOnce sync.Once
	finish := func() { closeOnce.Do(func() { close(done) }) }

	mux.HandleFunc(authCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		gotNonce := r.URL.Query().Get("nonce")
		code := r.URL.Query().Get("authorization_code")

		// ⚠ nonce 必须匹配（防跨会话串号：同一台机器上可能有多个在途登录）。
		if gotNonce != e.nonce {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "nonce 不匹配，请回到控制台重新发起登录")
			return
		}
		if strings.TrimSpace(code) == "" {
			// 回调来了但没带 code：说明上游没按预期转发（机制变了）。
			// 如实说清楚，而不是静默等超时。
			f.markFailed(e, "上游回调没有携带 authorization_code（登录链路可能已变更）")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "回调缺少 authorization_code")
			finish()
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(requestTimeoutMS)*time.Millisecond)
		a, err := f.p.client.ExchangeAuthorizationCode(ctx, code)
		cancel()
		if err != nil {
			f.markFailed(e, err.Error())
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "换取登录凭证失败："+err.Error())
			finish()
			return
		}
		// ⚠ 复用 finishLogin 而不是自己拼凭据 —— 它多做了三件必要的事：
		//
		//	① 从 JWT 的 exp 算 expires_at（缺了它账目过期时间未知）
		//	② EnrichCredential 补昵称/手机号（展示用，失败不影响可用性）
		//	③ 领一次性登录奖励
		//
		// 以及 `Secret: &authFile{...}` 这个**必需**的包装 ——
		// 直接放 `*Auth` 会让授权成功后停在"凭证结构尚未接入落盘"，
		// 用户看到的是"登录不了"。
		f.finishLogin(e, a.AccessToken, a.RefreshToken)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<h2>登录成功 ✅</h2><p>可以关闭此页面，回到控制台。</p>")
		finish()
	})
	// 浏览器常请求 /favicon.ico：给 204，避免它落进 404 被误读成错误。
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	// 超时或完成后关服务器（释放端口）。
	// 与 lobsterai 同值：登录是需要人操作的流程，5 分钟是常见体验下限。
	timeout := time.NewTimer(loginTimeout)
	defer timeout.Stop()
	select {
	case <-done:
	case <-timeout.C:
		f.markFailed(e, "登录超时（5 分钟内未完成）")
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// Poll 查询授权结果。未完成时返回 gateway.ErrLoginPending。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	if f == nil {
		return gateway.Credential{}, errors.New("raccoon: 登录流程未配置")
	}
	f.mu.Lock()
	e, ok := f.ses[state]
	f.mu.Unlock()
	if !ok {
		return gateway.Credential{}, errors.New("raccoon: 登录会话不存在或已结束，请重新发起")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failRsn != "" {
		return gateway.Credential{}, errors.New(e.failRsn)
	}
	if !e.ready {
		return gateway.Credential{}, gateway.ErrLoginPending
	}
	cred := e.cred
	f.mu.Lock()
	delete(f.ses, state)
	f.mu.Unlock()
	return cred, nil
}

func (f *loginFlow) finishLogin(e *loginEntry, accessToken, refreshToken string) {
	a := &Auth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}
	if ms := decodeJWTExpMS(accessToken); ms > 0 {
		a.ExpiresAt = fmt.Sprintf("%d", ms)
	}
	// 尽力补全展示字段（失败不影响登录，见 EnrichCredential）
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(requestTimeoutMS)*time.Millisecond)
	f.p.client.EnrichCredential(ctx, a)
	cancel()
	if a.Nickname == "" {
		a.Nickname = a.DisplayUID()
	}

	cred := gateway.Credential{
		Provider: providerID,
		UID:      a.UID(),
		Nickname: a.DisplayUID(),
		// ⚠ 必须是 authFile 包装，不能直接放 `a` —— 核心落盘要求
		// Secret 实现 MarshalAuthFile（只有上游知道自己凭证的文件名与形状）。
		// 直接放 `a` 会让浏览器授权成功后停在「凭证结构尚未接入落盘」，
		// 用户看到的是"登录不了"。
		Secret: &authFile{a: a},
	}
	if ms := a.ExpiresAtMS(); ms > 0 {
		cred.ExpiresAt = time.UnixMilli(ms)
	}

	// 登录成功后领一次性登录奖励（失败只记日志，不影响登录）
	if res, err := f.p.client.ClaimLoginGrant(context.Background(), a); err == nil && res.Claimed {
		// 有意不在这里打日志噪声：由装配层的管理端点展示
		_ = res
	}

	e.mu.Lock()
	e.ready = true
	e.cred = cred
	e.mu.Unlock()
}

// markFailed 标记会话失败。
// 编译期断言。
var _ gateway.LoginFlow = (*loginFlow)(nil)

// ── 凭证落盘包装 ────────────────────────────────────────────────────────

// authFile 让一份 *Auth 满足核心的落盘窄接口（MarshalAuthFile）。
//
// # 为什么用包装类型而不是给 *Auth 直接加方法
//
// `*Auth` 有两个身份，且**形状不同**：
//
//	池内 secret   运行期用的结构
//	落盘文件      嵌套形 {"auth":{...},"account":{...}}（见 MarshalAuthFile）
//
// 让同一个类型承担两者，意味着"池里那份"和"盘上那份"共用一个序列化路径，
// 而它们的字段集并不相同。包装类型把"落盘形态"这件事显式化，
// 也与 trae / loomy / codearts 的既有做法一致。
type authFile struct{ a *Auth }

// MarshalAuthFile 返回 (文件名, 内容) —— 核心 pollViaFlow 的 authFileWriter 契约。
//
// ⚠ 不实现它就等于"登录不了"：浏览器授权会成功，但核心落盘那一步
// 会以 501 拒绝（「该上游的凭证结构尚未接入落盘」）。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.a == nil {
		return "", nil, errors.New("raccoon: 凭证为空")
	}
	raw, err := MarshalAuthFile(f.a)
	if err != nil {
		return "", nil, err
	}
	name := FileName(f.a)
	if name == "" {
		return "", nil, errors.New("raccoon: 凭证文件名为空")
	}
	return name, raw, nil
}
