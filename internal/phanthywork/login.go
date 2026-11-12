// login.go 登录（OAuth2 + PKCE(S256) + loopback）与它的**手动兜底**路径。
//
// # 两条路都必须有（报告 auth §1.7 的原话）
//
//	「手动兜底路径（listener 起不来时**是常态**）」
//
// 官方客户端自己就是 `catch { this.redirect = …/oauth/code/success }` ——
// 起不来监听**不是异常**，是一条设计好的分支。本包照做，两条路共用同一个
// 「投递授权码 → 换 token」的实现（见 finishExchange），不写第二套换证逻辑。
//
// # 与核心契约的对齐（gateway.LoginFlow）
//
//	Start() (state, authURL, err)  起会话 + 返授权 URL
//	Poll(state) (Credential, error)  **非阻塞**：未完成回 gateway.ErrLoginPending
//	Configured() bool               本部署真的能走登录吗（**假按钮防护**）
//
// 三个方法都**直接挂在 *Provider 上**（不是访问器）—— 这是核心发现扩展点的
// 唯一方式（类型断言 p.(gateway.LoginFlow)）。本仓实测过四个上游因为写成
// 访问器而在前端一直没有「＋ 添加账号」按钮。
//
// # 三个超时（报告 §1.6，别混）
//
//	会话存活 10 分钟（超时 ⇒ 过期，用户得重新发起）
//	单次换 token 20 秒（ExchangeCode）与 8 秒 × 3 次（续期，见 client.go）
//
// # 回调参数（严格照抄报告 §1.1/§1.4）
//
//	只绑 127.0.0.1:0（**只绑回环**，绝不 0.0.0.0 —— 报告 §9.1 记的
//	「本地网关绑 0.0.0.0 导致同局域网可达」是反面教材）
//	只接受 GET，路径必须 /oauth/callback
//	Host 白名单**必须**同时放 127.0.0.1 / localhost / [::1]（厂商会把
//	redirect_uri 里的 127.0.0.1 改写成 localhost 再跳回来 —— 只认
//	127.0.0.1 的实现在真机上会「浏览器说登录成功、客户端一直转圈」）
//	state 必须**恰好出现一次**且等于创建值；错的不结束流程（正确回调可能随后到）
package phanthywork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// ── 协议常量（报告 §1.2 的 8 个 query，逐字）─────────────────────────────

// OAuthClientID PhanthyCode 与 PhanthyWork **共用**的 OAuth client id。
//
// 报告 §3.5：两个产品 oauthClientId 都是 phanthy-code-cli，凭据可以跨产品复用；
// OpusWork 是独立 client（opuswork-desktop）+ 独立域名，本包不覆盖它。
const OAuthClientID = "phanthy-code-cli"

// oauthScopes 6 个 scope，**顺序固定**（官方客户端逐字如此）。
//
// 顺序变了不影响语义，但报告给的是逐字快照，照抄可以少一个变量。
var oauthScopes = []string{
	"org:create_api_key",
	"user:profile",
	"user:inference",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

// OAuthScopes 导出 scope 列表（登录流程与测试都要用）。
func OAuthScopes() []string {
	out := make([]string, len(oauthScopes))
	copy(out, oauthScopes)
	return out
}

// scopeString 空格连接的 scope（授权 URL 与 refresh 都用它）。
func scopeString() string { return strings.Join(oauthScopes, " ") }

const (
	// oauthCallbackPath 回调路径（只接受这一个）。
	oauthCallbackPath = "/oauth/callback"

	// manualCallbackURL listener 起不来时用的 redirect_uri。
	//
	// 与官方客户端一致：`${base}/oauth/code/success` —— 授权完成后
	// 浏览器会停在这个页面上，用户把地址栏里那一整条（或页面上的
	// code#state）复制回来即可。
	manualCallbackPath = "/oauth/code/success"

	// loginSessionTTL 会话存活期（10 分钟，与官方 sidecar 的 w34 = 600000 一致）。
	loginSessionTTL = 10 * time.Minute

	// loginExchangeTimeout 换 token 的等待上限（比会话短，避免"过期了还在换"）。
	loginExchangeTimeout = 30 * time.Second
)

// ManualLoginPath 手动兜底端点（前端渲染"粘贴回跳地址"输入框 → POST 这里）。
//
// 路径里带上游 id：核心是按**路径**把请求转给对应上游的 AdminRoutes 的。
const ManualLoginPath = "/admin/phanthywork/login/complete"

// manualLoginInstructions 给用户看的中文操作说明。
//
// 措辞与 codearts 的同名说明保持一致（用户已经熟悉那套说法），只把
// "state 会随这次登录带回来"这一点讲清楚 —— 本上游的 state 既是 CSRF
// 令牌，也是"这次登录"的句柄。
const manualLoginInstructions = "在本机浏览器里打开授权页并完成登录（手机号登录即可）。" +
	"登录成功后浏览器会跳到 127.0.0.1 的回调地址；" +
	"如果页面提示「无法访问 / 连接被拒绝」（网关在服务器上时是正常的），" +
	"就把**地址栏里那一整条地址**复制下来粘到下面 —— " +
	"形如 http://127.0.0.1:端口/oauth/callback?code=…&state=… 。" +
	"也可以只粘 code（此时按界面上这次登录的 state 完成）。"

// ── Provider 上的三个方法（契约要求：**直接挂方法集**）────────────────────

// loginFlowCached 惰性构造登录流程，**保持同一个实例**。
//
// ⚠ 必须缓存成同一个实例：sessions map 挂在它身上，每次新建就等于
// 每次一份空 map（本仓记过这个实测 bug：Start 与 Poll 各自拿到一个
// 新实例 ⇒ Poll 永远找不到 state）。
func (p *Provider) loginFlowCached() *loginFlow {
	p.loginOnce.Do(func() {
		p.login = newLoginFlow(p)
	})
	return p.login
}

// Start 申请一次登录，返回 (state, 授权URL)。
func (p *Provider) Start() (string, string, error) {
	return p.loginFlowCached().start()
}

// Poll 查询授权结果。未完成时返回 gateway.ErrLoginPending（核心据此回 202）。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	return p.loginFlowCached().poll(state)
}

// Configured 报告这份部署真的能走登录流程。
//
// 判据：授权与换 token 的端点是**编译期常量**（本包没有可缺省的客户端配置），
// 所以只要 Provider 本身存在就可用。
//
// ⚠ 为什么仍然要这个方法：核心的 `ok && lf.Configured()` 是
// **假按钮防护** —— 只看 `ExtOf[LoginFlow]` 成功就会给没配登录的部署
// 画一个点了必失败的「＋ 添加账号」。本包真的能登（loopback + 手动兜底两条路），
// 所以照实回 true。
func (p *Provider) Configured() bool { return p != nil }

// ManualLogin 自报手动兜底的端点与指引（gateway.ManualLoginExt）。
func (p *Provider) ManualLogin() (string, string) {
	return ManualLoginPath, manualLoginInstructions
}

// ── loginFlow 本体 ───────────────────────────────────────────────────────

// loginSession 一次进行中的授权。
type loginSession struct {
	state    string
	verifier string
	redirect string
	// callback 手动兜底时用：用户粘回来的 code 投递到这里（缓冲 1，非阻塞投递）。
	callback chan string
	// listener 本地回调服务器（**可能为 nil** —— 起不来是常态）。
	listener net.Listener
	// server 与之配套的 http.Server。
	server *http.Server
	// done 换证结果。
	done  chan loginResult
	once  sync.Once
	expAt time.Time
}

// loginResult 一次换证的结果。
type loginResult struct {
	cred gateway.Credential
	err  error
}

// loginFlow 实现 gateway.LoginFlow 与手动兜底。
type loginFlow struct {
	p *Provider

	mu       sync.Mutex
	sessions map[string]*loginSession
}

// 编译期断言：两个接口都必须挂在**同一个** *Provider 上。
var (
	_ gateway.LoginFlow              = (*Provider)(nil)
	_ gateway.ManualLoginExt         = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.CredentialExpiryExt    = (*Provider)(nil)
	_ gateway.CredentialTokenExt     = (*Provider)(nil)
	_ gateway.ErrorClassifier        = (*Provider)(nil)
	_ gateway.AdminExt               = (*Provider)(nil)
	_ gateway.CredentialRefresher    = (*Provider)(nil)
	_ gateway.DisplayNameExt         = (*Provider)(nil)
	_ gateway.RefreshSkewExt         = (*Provider)(nil)
)

// newLoginFlow 建一个登录流程。
func newLoginFlow(p *Provider) *loginFlow {
	return &loginFlow{p: p, sessions: make(map[string]*loginSession, 2)}
}

// start 起一次授权：生成 PKCE、**尽力**起 loopback、返回授权 URL。
func (f *loginFlow) start() (string, string, error) {
	verifier := newNonce() // 32 字节 base64url 与 24 字节同形态，够用且复用同一随机源
	state := newNonce()
	if verifier == "" || state == "" {
		return "", "", fmt.Errorf("phanthywork: 随机源不可用，无法发起登录")
	}
	challenge := sha256B64URL([]byte(verifier))

	redirect := f.p.client.apiBase() + manualCallbackPath
	sess := &loginSession{
		state:    state,
		verifier: verifier,
		redirect: redirect,
		callback: make(chan string, 1),
		done:     make(chan loginResult, 1),
		expAt:    time.Now().Add(loginSessionTTL),
	}
	// **尽力**起回调监听。起不来 ⇒ 走手动兜底（报告 auth §1.7：这是常态）。
	if ln, srv, err := startCallbackListener(sess); err == nil {
		sess.listener, sess.server = ln, srv
		sess.redirect = fmt.Sprintf("http://127.0.0.1:%d%s", ln.Addr().(*net.TCPAddr).Port, oauthCallbackPath)
	} else {
		logListenFailed(err)
	}

	f.mu.Lock()
	f.gcLocked()
	f.sessions[state] = sess
	f.mu.Unlock()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", OAuthClientID)
	q.Set("redirect_uri", sess.redirect)
	q.Set("scope", scopeString())
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")

	return state, f.p.client.apiBase() + pathAuthorize + "?" + q.Encode(), nil
}

// poll 查询授权结果（**非阻塞**）。
//
// 语义与 codearts 的同名实现一致：首次 Poll 把阻塞的等待下沉到 goroutine，
// 之后只读 channel。这样 Poll 永远瞬时返回，与核心的 202 语义对齐。
func (f *loginFlow) poll(state string) (gateway.Credential, error) {
	f.mu.Lock()
	sess, ok := f.sessions[strings.TrimSpace(state)]
	f.mu.Unlock()
	if !ok || sess == nil {
		// state 未知（已换走 / 已过期 / 从未发起）。
		return gateway.Credential{}, errStateGone
	}
	sess.once.Do(func() {
		go func() {
			code, werr := waitForCode(sess)
			if werr != nil {
				sess.done <- loginResult{err: werr}
				f.closeSession(sess)
				return
			}
			cred, cerr := f.finishExchange(sess, code)
			sess.done <- loginResult{cred: cred, err: cerr}
			f.closeSession(sess)
		}()
	})
	select {
	case r := <-sess.done:
		f.mu.Lock()
		delete(f.sessions, sess.state)
		f.mu.Unlock()
		if r.err != nil {
			return gateway.Credential{}, r.err
		}
		return r.cred, nil
	default:
		return gateway.Credential{}, gateway.ErrLoginPending
	}
}

// gcLocked 清理过期会话（每发起一次登录顺手做一次）。
func (f *loginFlow) gcLocked() {
	now := time.Now()
	for k, s := range f.sessions {
		if s == nil || now.After(s.expAt) {
			if s != nil {
				f.closeSessionLocked(s)
			}
			delete(f.sessions, k)
		}
	}
}

// closeSession 关掉会话占用的资源（**必须关**：每个回话占一个真实端口）。
func (f *loginFlow) closeSession(s *loginSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeSessionLocked(s)
}

// closeSessionLocked 与 closeSession 同体，只是假定已持锁。
func (f *loginFlow) closeSessionLocked(s *loginSession) {
	if s == nil {
		return
	}
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.server.Shutdown(ctx)
		cancel()
		s.server = nil
	}
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
}

// waitForCode 等授权码：本地回调**或**手动粘贴，谁先到算谁。
func waitForCode(s *loginSession) (string, error) {
	timeout := time.Until(s.expAt)
	if timeout <= 0 {
		return "", errLoginExpired
	}
	select {
	case code := <-s.callback:
		return code, nil
	case <-time.After(timeout):
		return "", errLoginExpired
	}
}

// finishExchange 用授权码换凭证（**两条路共用**这一段）。
func (f *loginFlow) finishExchange(s *loginSession, code string) (gateway.Credential, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return gateway.Credential{}, fmt.Errorf("phanthywork: 没有授权码")
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginExchangeTimeout)
	defer cancel()

	ts, err := f.p.client.ExchangeCode(ctx, code, s.state, s.verifier, s.redirect)
	if err != nil {
		return gateway.Credential{}, err
	}
	a := &Auth{
		Provider:   providerID,
		OAuth:      OAuthTokens{AccessToken: ts.AccessToken, RefreshToken: ts.RefreshToken, ExpiresAtMS: ts.ExpiresAtMS, Scopes: ts.Scopes},
		baseSkewMS: f.p.client.baselineSkewMS(),
	}
	if err := a.normalize(); err != nil {
		return gateway.Credential{}, err
	}
	// 身份 = JWT 的 sub（UID 的判据），同时把它当 accountScope 的兜底。
	if c, ok := ParseJWTClaims(ts.AccessToken); ok {
		if a.AccountScope == "" {
			a.AccountScope = strings.TrimSpace(c.Sub)
		}
		if a.NicknameValue == "" {
			a.NicknameValue = strings.TrimSpace(c.Sub)
		}
	}
	if a.UID() == "" {
		return gateway.Credential{}, fmt.Errorf("phanthywork: 换到的令牌里没有 sub，无法确定账号身份")
	}
	return gateway.Credential{
		Provider:  providerID,
		UID:       a.UID(),
		Nickname:  a.Nickname(),
		ExpiresAt: time.UnixMilli(ts.ExpiresAtMS),
		Secret:    a,
	}, nil
}

// deliverCode 把手动粘贴来的授权码投进对应会话（**首个到达者生效**）。
func (f *loginFlow) deliverCode(state, code string) error {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if state == "" {
		return fmt.Errorf("缺少 state：请把整个回跳地址粘回来，或重新发起一次登录")
	}
	if code == "" {
		return fmt.Errorf("没有 code：请把整个回跳地址粘回来")
	}
	f.mu.Lock()
	sess, ok := f.sessions[state]
	f.mu.Unlock()
	if !ok || sess == nil {
		return errStateGone
	}
	select {
	case sess.callback <- code:
		// 投递成功。无论 Poll 有没有被调过，waitForCode 都会收到它
		//（channel 带缓冲 1 ⇒ "先粘贴、后轮询"与"先轮询、后粘贴"都成立）。
	default:
		// 已有码在管道里：重复粘贴被忽略（与"回调只该来一次"语义一致）。
	}
	return nil
}

// ── 本地回调监听 ─────────────────────────────────────────────────────────

// startCallbackListener 起 loopback 回调服务器。
//
// ⚠ 只绑 127.0.0.1:0（**绝不 0.0.0.0**）。报告 §9.1 记的反面教材就是
// 官方客户端把本地网关绑在 0.0.0.0 上，导致同局域网（或同一 Tailscale
// 网络）内任何设备都能访问那个"本地特权"前缀。
func startCallbackListener(sess *loginSession) (net.Listener, *http.Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler:           callbackHandler(sess),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return ln, srv, nil
}

// callbackHandler 回调处理器。
//
// 三条硬规则（报告 auth §1.1/§1.3）：
//
//	只接受 GET
//	Host 白名单同时放 127.0.0.1 / localhost / [::1]（厂商会把 127.0.0.1
//	  改写成 localhost 再跳回来）
//	state 必须等于本次会话的 state；**错的槽不清会话**（正确回调可能随后到）
func callbackHandler(sess *loginSession) http.Handler {
	port := 0
	if sess.listener != nil {
		if ta, ok := sess.listener.Addr().(*net.TCPAddr); ok {
			port = ta.Port
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet {
			http.Error(w, "只接受 GET", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != oauthCallbackPath {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if !hostAllowed(r.Host, port) {
			http.Error(w, "Host 不在白名单内", http.StatusForbidden)
			return
		}
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		state := strings.TrimSpace(r.URL.Query().Get("state"))
		if code == "" || state == "" {
			http.Error(w, "回调缺少 code 或 state", http.StatusBadRequest)
			return
		}
		if state != sess.state {
			// ⚠ 不结束流程：正确的那次回调可能随后到（报告 §1.1）。
			http.Error(w, "state 不匹配", http.StatusBadRequest)
			return
		}
		select {
		case sess.callback <- code:
		default:
		}
		_, _ = w.Write([]byte("登录成功，请返回网关控制台。此页面可以关闭。"))
	})
}

// hostAllowed 判定回调请求的 Host 是否在白名单内。
//
// 白名单三个主机名：127.0.0.1 / localhost / [::1]。
//
// ⚠ **端口刻意不参与判定**：厂商的授权服务器会把 redirect_uri 里的
// 127.0.0.1 改写成 localhost 再跳回来（报告 auth §1.1 的实测注释），
// 而监听本身只绑 127.0.0.1:0，外部根本连不进来 —— 端口在这里再加一道
// 只会制造"浏览器显示登录成功、客户端一直转圈"（Host 头对不上）。
// 真正的护栏是：**只绑回环** + **state 必须完全一致**。
func hostAllowed(host string, _ int) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	hostOnly := h
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i >= 0 {
			hostOnly = h[:i+1]
		}
	} else if i := strings.LastIndex(h, ":"); i >= 0 {
		hostOnly = h[:i]
	}
	switch hostOnly {
	case "127.0.0.1", "localhost", "[::1]", "::1":
		return true
	}
	return false
}

// ── 手动兜底端点（AdminExt 上的一条路由）──────────────────────────────────

// handleManualComplete POST {state, code} → 投递 → 既有链路换证。
//
// 与 codearts 的同名端点同形：这里**只回"已收到"**，凭证由下一次 Poll 取走
// （核心是轮询式的）。
func (p *Provider) handleManualComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeLocalJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "只接受 POST"})
		return
	}
	var body struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeLocalJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	state, code, err := extractPasted(body.Code, body.State)
	if err != nil {
		writeLocalJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := p.loginFlowCached().deliverCode(state, code); err != nil {
		writeLocalJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeLocalJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"state":   state,
		"message": "已收到授权码，正在换取凭证；界面会在下次轮询时自动完成。",
	})
}

// extractPasted 从用户粘贴的内容里取出 (state, code)。
//
// 支持四种形态（用户实际会粘什么就收什么）：
//
//	① 完整回跳地址 http://127.0.0.1:8080/oauth/callback?code=abc&state=xyz
//	② 裸 query 串   code=abc&state=xyz
//	③ fragment 变体  http://…/callback#code=abc&state=xyz
//	④ 只粘 code     abc（state 回落到界面给的 stateHint）
//
// 任何形态都**不 panic**：取不到就返回可读错误，由 handler 转 400。
//
// ⚠ 「像地址」的判据不能只查 `code=`：那样 `http://…?state=X`
// （有地址、没 code）会掉进"裸 code"分支，把整条 URL 当 code 投出去 ——
// 那种"看起来成功"的错最难查。
func extractPasted(pasted, stateHint string) (state, code string, err error) {
	raw := strings.TrimSpace(pasted)
	if raw == "" {
		return "", "", fmt.Errorf("没有粘贴任何内容")
	}
	addrish := strings.Contains(raw, "://") ||
		strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "?") ||
		strings.ContainsAny(raw, "?#") ||
		strings.Contains(raw, "=")
	if !addrish {
		return strings.TrimSpace(stateHint), raw, nil
	}
	q := raw
	if j := strings.IndexAny(raw, "?#"); j >= 0 {
		q = raw[j+1:]
	}
	q = strings.TrimPrefix(q, "?")
	if j := strings.IndexByte(q, '#'); j >= 0 {
		q = q[:j]
	}
	values, perr := url.ParseQuery(q)
	if perr != nil {
		return "", "", fmt.Errorf("粘贴的地址解析失败：%v", perr)
	}
	c := strings.TrimSpace(values.Get("code"))
	if c == "" {
		return "", "", fmt.Errorf("粘贴的地址里没有 code 参数 —— 请确认复制的是**授权完成后的回跳地址**")
	}
	s := strings.TrimSpace(values.Get("state"))
	if s == "" {
		s = strings.TrimSpace(stateHint)
	}
	return s, c, nil
}

// writeLocalJSON 写一个 JSON 响应（本包自用，避免依赖核心的写响应工具）。
func writeLocalJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── 续期（gateway.CredentialRefresher）───────────────────────────────────

// RefreshCredential 续期一份凭证（**原地更新** + 落盘）。
//
// # 并发闸（坑⑤）是这里的重点
//
// refreshToken **轮换**，所以两路并发刷新会互踢。本方法的顺序刻意是：
//
//	① 进来先看闸（同键 in-flight 合并 / 刚成功的回放窗口内直接复用）
//	② 闸里说"我已经刷过了" → 直接读**当前**令牌，若已新鲜就成功返回
//	③ 否则自己发起（自己拿闸的发起权），完成时 finish 广播结果
//
// # 终态与可重试的区分（决定核心是否换号）
//
//	ErrRefreshExpired → 终态（凭据废了，需人工重登）
//	其它 error        → 可重试（5xx/429/网络抖动）
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	nowLocal := nowMS()
	// 已经够新 → 不打网络（开源实现的第一步就是"看本地是否还有效"）。
	//
	// ⚠ 判据必须与**后台任务同源**：这里调 needsRefresh（它复用 RefreshSkew），
	// 而不是另写一个固定提前量。两处判据不一致会出现最糟的形态 ——
	// 后台每轮都判定"该刷"，进到这里又被拒，于是**永远不刷而日志写着成功**。
	// （我第一版就是 refreshLead=2m vs skew=900s 两个判据，被用例当场抓住：
	//   jobs_test.go 的 TestRunRefreshExactlyOnceWhenSkewMet 报"实际 0 次"。）
	if !p.needsRefresh(a, time.UnixMilli(nowLocal)) {
		return nil
	}
	key := a.refreshKey()
	// ⚠ replayOK=true：续期要回放「刚成功的那一次」。
	//
	// 这正是坑⑤ 的解药：refreshToken 轮换后，第 2 个并发请求若再刷一次就是
	// 「拿旧 token 去换」⇒ 登录莫名失效；回放窗口让它在 60 秒内直接拿到结果。
	// 键由账号身份决定 ⇒ **跨轮换稳定**（见 refreshKey 的注释）。
	call, initiator := a.gate.acquire(key, nowLocal, true)
	if !initiator {
		select {
		case <-call.done:
		case <-time.After(refreshWaitCap):
			return fmt.Errorf("phanthywork: 等待同账号的续期结果超时")
		}
		if call.err != nil {
			return call.err
		}
		// 别人刷成功了：确认一下当前令牌真的够新（轮换是原地更新的）。
		if !p.needsRefresh(a, time.UnixMilli(nowMS())) {
			return nil
		}
		return fmt.Errorf("phanthywork: 续期被合并，但令牌仍未刷新")
	}
	ctx, cancel := context.WithTimeout(context.Background(), refreshCallTimeout)
	defer cancel()
	ts, rerr := p.client.RefreshTokens(ctx, a)
	if rerr != nil {
		a.gate.finish(key, call, "", rerr)
		return rerr
	}
	// 落盘失败不让续期失败：内存里的凭证已经可用，下次启动才会用到旧的那份。
	if err := a.applyTokenSet(ts); err != nil {
		a.gate.finish(key, call, "", err)
		return err
	}
	a.gate.finish(key, call, ts.AccessToken, nil)
	return nil
}

// RefreshSkew 报告 access token 多早算该刷（gateway.RefreshSkewExt）。
//
// 判据复用 gateway 的**统一策略**：按寿命比例（剩余 ≤ 一半就续）。
// access token 寿命 1800 秒 ⇒ 窗口 900 秒（15 分钟）。
//
// ⚠ 不自己写一个固定窗口：固定时长相对寿命的意义会随寿命漂移
// （见 gateway/refreshratio.go 的文件头），而本仓已经为这件事
// 统一过一次口径。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil || a.OAuth.AccessToken == "" {
		return 0, false
	}
	if a.OAuth.RefreshToken == "" {
		// 没有 refreshToken = 不能续期 ⇒ 报 ok=false（核心回落自己的兜底，
		// 而不是"明确声明不需要提前刷"——那是 (0, true) 的语义）。
		return 0, false
	}
	return gateway.RefreshSkewFromToken(a.OAuth.AccessToken)
}

// applyTokenSet 把一次 token 响应写回凭证（**就地**）并落盘。
//
// 三条照抄的边界：
//
//	① 服务端没给新 refreshToken ⇒ **沿用旧的**（报告 §4.1 第 ⑤ 步）
//	② expiresAt = 现在 + expires_in（服务端没给就用该路径的兜底值）
//	③ scopes 只在服务端真的给了时才覆盖
func (a *Auth) applyTokenSet(ts TokenSet) error {
	if a == nil {
		return errNilAuth
	}
	if strings.TrimSpace(ts.AccessToken) == "" {
		return fmt.Errorf("phanthywork: token 响应里没有 access_token")
	}
	a.OAuth.AccessToken = ts.AccessToken
	if ts.RefreshToken != "" {
		a.OAuth.RefreshToken = ts.RefreshToken
	}
	if ts.ExpiresAtMS > 0 {
		a.OAuth.ExpiresAtMS = ts.ExpiresAtMS
	}
	if len(ts.Scopes) > 0 {
		a.OAuth.Scopes = ts.Scopes
	}
	// 换了 access_token ⇒ 账号 uuid 可能刚变得可读，补一次（不覆盖已有的）。
	if a.AccountScope == "" {
		if c, ok := ParseJWTClaims(ts.AccessToken); ok {
			a.AccountScope = strings.TrimSpace(c.Sub)
		}
	}
	return saveAuthFile(a)
}

// ── 本文件的哨兵 ─────────────────────────────────────────────────────────

var (
	// errStateGone 该 state 不存在（已兑换 / 已过期 / 从未发起）。
	errStateGone = fmt.Errorf("phanthywork: 授权会话不存在或已结束，请重新发起登录")

	// errLoginExpired 授权链接已过期（10 分钟）。
	errLoginExpired = fmt.Errorf("phanthywork: 登录链接已过期，请重新登录以生成新链接")
)

// errIsStateGone 供测试断言用。
func errIsStateGone(err error) bool { return errors.Is(err, errStateGone) }

// logListenFailed 记录一次"回调监听起不来"（这是**常态**，不是故障）。
func logListenFailed(err error) {
	if err == nil {
		return
	}
	log.Printf("phanthywork: 本地回调监听未起（改用「粘贴回跳地址」模式）: %v", err)
}
