// login.go LobsterAI 的登录流程（本地回调服务器 + authCode 换 token）。
//
// # 与参照项目的差异（本网关是纯后端，没有内嵌登录页）
//
// 参照项目在**单个进程内**起一个回调服务器，收到 code 后立即完成 exchange ——
// 它明确说明这是为了取代 Go 侧那套"两个进程 + /tmp 状态文件 + 人在环确认"
// 的脆弱编排（参照 lobsterai-oauth.ts 的头部注释）。
//
// 本网关沿用它更稳的那部分（单进程内完成 exchange，无状态文件），
// 但**回调地址必须指回本机监听端口** —— 这是 LobsterAI 的硬约束：
// portal 只会跳到 `redirect_uri`，而它必须是 `http://127.0.0.1:{port}/auth/callback`。
//
// 因此本实现**需要绑一个本地端口**。这与 Cline/Raccoon 不同（那两个不起端口）。
//
// # 服务器部署的已知限制（诚实写明）
//
// 回调指向**浏览器所在机器**的回环地址。若网关跑在服务器上而浏览器在本地，
// 回调打不进服务器的端口。参照项目用"本机登录页"（页面与回调同机）绕开了它，
// 本网关没有那个页面 —— 故这条路径**只适用于网关与浏览器同机的部署**。
//
// 这不是本实现引入的新问题：它是 OAuth 回环回调的固有形态，
// codearts/trae 也有同样的限制（它们靠 manual 模式缓解）。
// LobsterAI **没有** manual 模式可退（服务端只支持 redirect_uri 回跳）。
package lobsterai

import (
	"context"
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
	state    string
	port     int
	session  LoginSession
	issuedAt time.Time
	ready    bool
	cred     gateway.Credential
	failRsn  string
	mu       sync.Mutex
}

// loginFlow 实现 gateway.LoginFlow。
type loginFlow struct {
	p   *Provider
	mu  sync.Mutex
	ses map[string]*loginEntry
}

// Configured 报告这份部署真的能走登录流程吗。
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
		return "", "", errors.New("lobsterai: 登录流程未初始化")
	}
	return lf.Start()
}

// Poll 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("lobsterai: 登录流程未初始化")
	}
	return lf.Poll(state)
}

// Configured 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Configured() bool {
	lf, ok := p.LoginFlow()
	return ok && lf.Configured()
}

// Start 发起一次登录：起回调服务器 + 返回 portal 授权 URL。
//
// 返回 (state, authURL, error)。
func (f *loginFlow) Start() (string, string, error) {
	if f == nil || f.p == nil {
		return "", "", errors.New("lobsterai: 登录流程未配置")
	}
	sess, err := NewLoginSession()
	if err != nil {
		return "", "", fmt.Errorf("lobsterai: 生成登录会话失败: %w", err)
	}
	state, err := NewUUID()
	if err != nil {
		return "", "", fmt.Errorf("lobsterai: 生成 state 失败: %w", err)
	}

	// 绑随机端口（只绑 127.0.0.1）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("lobsterai: 回调端口监听失败: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	e := &loginEntry{state: state, port: port, session: sess, issuedAt: time.Now()}
	f.mu.Lock()
	f.ses[state] = e
	f.mu.Unlock()

	go f.serveCallback(ln, e)
	return state, f.p.client.LoginURL(port, state), nil
}

// serveCallback 跑回调服务器并在拿到 code 后立即完成 exchange。
func (f *loginFlow) serveCallback(ln net.Listener, e *loginEntry) {
	mux := http.NewServeMux()
	done := make(chan struct{})
	var closeOnce sync.Once

	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		gotState := r.URL.Query().Get("state")
		code := r.URL.Query().Get("code")
		// ⚠ state 必须匹配（防跨会话串号）
		if gotState != e.state {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "state 不匹配，请重新发起登录")
			return
		}
		if strings.TrimSpace(code) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "缺少授权码")
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(requestTimeoutMS)*time.Millisecond)
		a, err := f.p.client.ExchangeAuthCode(ctx, code, e.session)
		cancel()
		if err != nil {
			f.markFailed(e, err.Error())
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "换取凭据失败："+err.Error())
			closeOnce.Do(func() { close(done) })
			return
		}
		f.markReady(e, a)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<h2>登录成功</h2><p>可以关闭此页面，回到控制台。</p>")
		closeOnce.Do(func() { close(done) })
	})
	// 浏览器常请求 /favicon.ico：给个 204 避免它落进 404 分支被误读成错误
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	// 超时或完成后关服务器
	timeout := time.NewTimer(time.Duration(loginTimeoutMS) * time.Millisecond)
	defer timeout.Stop()
	select {
	case <-done:
	case <-timeout.C:
		f.markFailed(e, "lobsterai: 登录超时（600 秒内未完成）")
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// Poll 查询授权结果。未完成时返回 gateway.ErrLoginPending。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	if f == nil {
		return gateway.Credential{}, errors.New("lobsterai: 登录流程未配置")
	}
	f.mu.Lock()
	e, ok := f.ses[state]
	f.mu.Unlock()
	if !ok {
		return gateway.Credential{}, errors.New("lobsterai: 登录会话不存在或已结束，请重新发起")
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

// markReady 标记就绪。
func (f *loginFlow) markReady(e *loginEntry, a *Auth) {
	cred := gateway.Credential{
		Provider: providerID,
		UID:      a.UIDValue(),
		Nickname: a.Nickname,
		// ⚠ 必须是 authFile 包装，不能直接放 `a` —— 核心落盘要求
		// Secret 实现 MarshalAuthFile（只有上游知道自己凭证的文件名与形状）。
		Secret: &authFile{a: a},
	}
	if ms := a.ExpiresAtMS(); ms > 0 {
		cred.ExpiresAt = time.UnixMilli(ms)
	}
	e.mu.Lock()
	e.ready = true
	e.cred = cred
	e.mu.Unlock()
}

// markFailed 标记失败。
func (f *loginFlow) markFailed(e *loginEntry, reason string) {
	e.mu.Lock()
	if e.failRsn == "" {
		e.failRsn = reason
	}
	e.mu.Unlock()
}

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
// 也与 trae / mimo / loomy / codearts 的既有做法一致。
type authFile struct{ a *Auth }

// MarshalAuthFile 返回 (文件名, 内容) —— 核心 pollViaFlow 的 authFileWriter 契约。
//
// ⚠ 不实现它就等于"登录不了"：浏览器授权会成功，但核心落盘那一步
// 会以 501 拒绝（「该上游的凭证结构尚未接入落盘」）。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.a == nil {
		return "", nil, errors.New("lobsterai: 凭证为空")
	}
	raw, err := MarshalAuthFile(f.a)
	if err != nil {
		return "", nil, err
	}
	name := FileName(f.a)
	if name == "" {
		return "", nil, errors.New("lobsterai: 凭证文件名为空")
	}
	return name, raw, nil
}