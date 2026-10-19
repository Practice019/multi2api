package account

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 端点路径。集中一处：前端、探针、文档都照这里写，避免三处各写一遍写岔。
//
// 都挂在 `/admin/auth/` 下，因为它们是**获取凭据**的地方，必须挂在
// 管理端门禁（`/admin/` 的那个 handler）**之外** —— 否则"没登录所以打不开
// 登录接口"这种死锁。挂在同一个 mux 上时 Go 的 ServeMux 会优先匹配更长的
// 前缀，所以 /admin/auth/ 会先于 /admin/ 命中（见 cmd/server 的挂载处）。
const (
	PathRegister = "/admin/auth/register"
	PathLogin    = "/admin/auth/login"
	PathLogout   = "/admin/auth/logout"
	PathSession  = "/admin/auth/session"
)

// maxAuthBody 匿名端点的请求体上限（8KB：注册/登录的 JSON 用不到更多）。
//
// ⚠ 这里只做"别把内存读爆"的兜底；真正的反批量上限（413 + code）
// 在 T10 接上 internal/authlimit.EnforceBodyLimit 后统一（同一数值）。
const maxAuthBody = 8 << 10

// Limiter 限速接缝。
//
// 为什么是接口而不是直接 import 具体的限速器：T3 只把**接缝**留好，
// 具体实现（滑动窗口 / 内存占用 / 清理策略）在 T10 接上，两者可以各自
// 演进而不互相牵动。nil 表示不限速（本期默认），这样 T3 的功能与反滥用
// 能分开验证 —— 反滥用一旦混进来，登录本身坏了会看不出来是谁的问题。
type Limiter interface {
	// Allow 报告这一次调用是否放行；被拒时给出建议的 retry-after。
	Allow(key string, now time.Time) (ok bool, retryAfter time.Duration)
}

// 会话 Cookie 的 Secure 策略。
const (
	// CookieSecureAuto 只在请求本身走 TLS 时加 Secure（默认）。
	CookieSecureAuto = "auto"
	// CookieSecureAlways 一律加 Secure。
	//
	// 用途：反代/隧道（浏览器 https → CF → 本机 http）场景。此时请求到
	// 达本进程时是明文，auto 判不出来，但**浏览器看到的页面是 https**，
	// 所以加了 Secure 之后 cookie 照常发送，同时杜绝了"同一域名下误走
	// http 也带 cookie"。
	CookieSecureAlways = "always"
	// CookieSecureNever 一律不加（本机 http 调试用）。
	CookieSecureNever = "never"
)

// HTTPConfig 组装控制台登录端点所需的一切。
type HTTPConfig struct {
	Accounts *Store
	Sessions *SessionStore
	// RegisterEnabled 是否开放注册（对应 new-api 的 common.RegisterEnabled）。
	//
	// ⚠ 关掉它**不会**关掉"首启建管理员"：库里一个账号都没有时，
	// 注册接口仍会建出管理员。否则"部署时忘了先建号 + 关闭注册"会让控制台
	// 永久进不去（用户只能删 data/accounts.json 重来）。两种语义分开：
	// 关闭的是"继续注册新成员"，不是"让自己进不去"。
	RegisterEnabled bool
	// CookieSecure 取 CookieSecure* 之一，空值按 auto。
	CookieSecure string
	// Limiter 可空（空 = 不限速）。
	Limiter Limiter
}

// HTTP 是控制台登录端点的 http.Handler。
type HTTP struct {
	cfg HTTPConfig
}

// NewHTTP 构造登录端点处理器。缺账号库/会话库属于装配错误，直接返回 error
// （比"跑起来之后每个请求 500"早得多地暴露问题）。
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	if cfg.Accounts == nil {
		return nil, errors.New("account: HTTPConfig.Accounts 不能为空")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("account: HTTPConfig.Sessions 不能为空")
	}
	switch cfg.CookieSecure {
	case "", CookieSecureAuto, CookieSecureAlways, CookieSecureNever:
	default:
		return nil, errors.New("account: CookieSecure 只能是 auto/always/never")
	}
	return &HTTP{cfg: cfg}, nil
}

// ServeHTTP 按路径分派。所有响应都带 no-store —— 登录/会话响应里有凭据
// 与身份信息，被任何一层缓存住都是泄露（new-api 的 DisableCache() 同义）。
func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	switch strings.TrimSuffix(r.URL.Path, "/") {
	case PathRegister:
		h.register(w, r)
	case PathLogin:
		h.login(w, r)
	case PathLogout:
		h.logout(w, r)
	case PathSession:
		h.session(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "没有这个接口")
	}
}

// ---------------------------------------------------------------------------
// 请求/响应
// ---------------------------------------------------------------------------

// credsBody 注册与登录共用的请求体。
//
// ⚠ 这里**不**包含 role 字段，是刻意的：注册接口不接受调用方指定角色
// （new-api controller/user.go:283 也是服务端写死普通用户）。少一个字段
// 就少一条"第一个请求把自己注册成管理员"的路。
type credsBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// apiError 统一错误体：`{"error":{"code":"...","message":"..."}}`。
//
// code 是**稳定**的机器判据（前端按它决定"跳登录页"还是"提示一下"），
// message 只给人看。new-api 的做法同此（middleware/auth.go 的
// writeDashboardAuthError 就是按稳定码返回）。前端靠 message 字符串判断
// 会随文案微调而失效。
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: apiError{Code: code, Message: message}})
}

// decodeCreds 解析请求体；失败时自己写响应并返回 ok=false。
func decodeCreds(w http.ResponseWriter, r *http.Request) (credsBody, bool) {
	var body credsBody
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		// 区分"太大"与"格式错"：前者是攻击/误用，后者是调用方写错了，
		// 给人看的信息不同，但都别把解析错误原文回吐（可能含请求内容片段）。
		if strings.Contains(err.Error(), "http: request body too large") {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体过大")
			return credsBody{}, false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 JSON")
		return credsBody{}, false
	}
	// 只读一次：多给的字节意味着请求体里有别的东西，不接受。
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体应当是单个 JSON 对象")
		return credsBody{}, false
	}
	return body, true
}

// clientIP 取请求来源 IP（限速的 key、登录审计）。
//
// ⚠ 刻意**不**看 X-Forwarded-For：那是调用方可以随便写的头，拿它当限速键
// 等于"每个请求自带一个新身份"，限速形同不存在。反代场景下所有人共享
// 反代的 IP（限速更严而不是更松），这是安全方向上可以接受的偏差 ——
// 要按真实客户端 IP 限速，得先有"只信任哪几跳代理"的配置，那是另一件事。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

func (h *HTTP) secureCookie(r *http.Request) bool {
	switch h.cfg.CookieSecure {
	case CookieSecureAlways:
		return true
	case CookieSecureNever:
		return false
	default:
		return r.TLS != nil
	}
}

// setSIDCookie 下发会话 Cookie。
//
// 三个属性缺一不可：
//
//	HttpOnly 前端 JS 读不到 ⇒ XSS 也偷不走会话（这正是替换"凭据存
//	         localStorage"的关键差别：localStorage 里的东西任何脚本都能读）
//	SameSite=Lax 跨站 POST 不带 ⇒ 挡住 CSRF 的绝大多数形态
//	Path=/   整站可用（/ui 与 /admin/* 都要）
func (h *HTTP) setSIDCookie(w http.ResponseWriter, r *http.Request, sid string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sid,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookie(r),
	})
}

// clearSIDCookie 清 Cookie（登出）。MaxAge<0 让浏览器立刻删掉。
func (h *HTTP) clearSIDCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secureCookie(r),
	})
}

// SIDFromRequest 读会话 Cookie 值（秘密常量 CookieName 的唯一读取点）。
//
// 导出给 internal/admin 的管理面门禁用：门禁要"会话或 admin_token"两条路，
// 而会话这条路必须与登录端点读**同一个** Cookie —— 抄一份常量或自己拼
// Cookie 名，将来改名就会变成"登录成功但管理面永远说你没登录"。
// 没带/空值都返回空串，调用方据此走"未登录"分支。
func SIDFromRequest(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil || c == nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

// allow 限速判定；未接限速器时一律放行。
//
// 返回 false 时已经写好 429 与 Retry-After（HTTP 标准头，客户端据此退避，
// 比让人猜"过一会儿再试"友好）。⚠ 429 的响应体也带 code=rate_limited，
// 与冻结契约一致。
func (h *HTTP) allow(w http.ResponseWriter, r *http.Request, scope string) bool {
	if h.cfg.Limiter == nil {
		return true
	}
	key := scope + "|" + clientIP(r)
	ok, retry := h.cfg.Limiter.Allow(key, time.Now())
	if ok {
		return true
	}
	secs := int(retry.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", itoa(secs))
	writeError(w, http.StatusTooManyRequests, "rate_limited",
		"请求过于频繁，请 "+itoa(secs)+" 秒后再试")
	return false
}

// itoa 小工具：避免为了一个整数引入 strconv 到本文件的所有调用点。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// 注册
// ---------------------------------------------------------------------------

// register 建号。
//
// # 首启建管理员（bootstrap）与普通注册是**两个**语义
//
//	库里一个账号都没有 → 建管理员（CreateBootstrap，原子，只成功一次）
//	已有账号            → 只建 RoleMember（哪怕调用方在请求体里写了 role）
//
// 判定顺序必须是"先看库空不空"，而不是"先看开放注册开关"：否则部署时
// 关了注册 + 没建号 = 永远进不去（详见 HTTPConfig.RegisterEnabled 的注释）。
func (h *HTTP) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !h.allow(w, r, "register") {
		return
	}
	body, ok := decodeCreds(w, r)
	if !ok {
		return
	}
	username := strings.TrimSpace(body.Username)

	// ① 首启：建管理员（只可能成功一次）
	if h.cfg.Accounts.Count() == 0 {
		u, err := h.cfg.Accounts.CreateBootstrap(username, body.Password)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":        true,
				"bootstrap": true,
				"user":      u.view(),
				"message":   "已创建管理员账号 " + u.Username + "，请用它与口令登录",
			})
			return
		}
		if !errors.Is(err, ErrBootstrapDone) {
			writeAccountError(w, err)
			return
		}
		// 竞态：刚好被别人抢先建了 ⇒ 落到下面的普通注册路径
	}

	// ② 普通注册：只在开放注册时
	if !h.cfg.RegisterEnabled {
		writeError(w, http.StatusForbidden, "register_disabled", "本机已关闭注册，请联系管理员建号")
		return
	}
	u, err := h.cfg.Accounts.Create(username, body.Password, RoleMember)
	if err != nil {
		writeAccountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u.view()})
}

// writeAccountError 把 account 包的哨兵错误映射成状态码 + 稳定 code。
//
// 映射集中一处：注册/登录/改密各写一遍的话，很容易出现"登录时说用户名错、
// 注册时说被占用"这种前后不一致（既难用也泄露信息）。
func writeAccountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrBadUsername):
		writeError(w, http.StatusBadRequest, "bad_username", "用户名不合法：3–32 位，只允许字母数字与 _ - . @")
	case errors.Is(err, ErrBadPassword):
		writeError(w, http.StatusBadRequest, "bad_password", "口令不合法：长度需在 8–128 之间")
	case errors.Is(err, ErrUsernameTaken):
		writeError(w, http.StatusConflict, "username_taken", "用户名已被占用")
	case errors.Is(err, ErrBootstrapDone):
		writeError(w, http.StatusConflict, "bootstrap_done", "管理员已存在")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "账号不存在")
	case errors.Is(err, ErrBadRole):
		writeError(w, http.StatusBadRequest, "bad_role", "角色值不合法")
	case errors.Is(err, ErrSessionLimit):
		writeError(w, http.StatusTooManyRequests, "rate_limited", "登录过于频繁，请稍后再试")
	default:
		// 未预期的错误：不把原文回吐（可能是路径/内部结构），但服务端要能查。
		writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法不支持，请用 "+allow)
}

// ---------------------------------------------------------------------------
// 登录 / 登出 / 会话
// ---------------------------------------------------------------------------

// login 校验口令并签发会话。
func (h *HTTP) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !h.allow(w, r, "login") {
		return
	}
	body, ok := decodeCreds(w, r)
	if !ok {
		return
	}

	u, err := h.cfg.Accounts.Authenticate(strings.TrimSpace(body.Username), body.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrCredentials):
			// 同一个 code、同一句话：不区分"用户不存在"与"口令错"，
			// 否则登录接口本身就变成用户名枚举器。
			writeError(w, http.StatusUnauthorized, "bad_credentials", "用户名或口令不正确")
		case errors.Is(err, ErrDisabled):
			writeError(w, http.StatusForbidden, "disabled", "账号已被禁用，请联系管理员")
		default:
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
		}
		return
	}

	sess, err := h.cfg.Sessions.Issue(u, clientIP(r), r.UserAgent())
	if err != nil {
		writeAccountError(w, err)
		return
	}
	h.setSIDCookie(w, r, sess.SID, int(SessionTTL/time.Second))
	// 审计信息，失败不影响登录（TouchLogin 内部吞掉写盘错误）
	h.cfg.Accounts.TouchLogin(u.ID, clientIP(r))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"user":       u.view(),
		"expires_at": stampOf(sess.ExpiresAt),
	})
}

// logout 撤销当前会话并清 Cookie。幂等：没有会话也回 200。
func (h *HTTP) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if sid := SIDFromRequest(r); sid != "" {
		h.cfg.Sessions.Revoke(sid)
	}
	h.clearSIDCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// session 返回当前登录态（前端启动时调一次，决定显示登录页还是控制台）。
func (h *HTTP) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	sid := SIDFromRequest(r)
	if sid == "" {
		// 没带 Cookie 不等于出错：这是"没登录"，前端据此显示登录页。
		writeError(w, http.StatusUnauthorized, "auth_required", "请先登录")
		return
	}
	sess, u, err := h.cfg.Sessions.Lookup(sid, h.cfg.Accounts)
	if err != nil {
		switch {
		case errors.Is(err, ErrSessionExpired):
			writeError(w, http.StatusUnauthorized, "session_expired", "登录已过期，请重新登录")
		case errors.Is(err, ErrSessionRevoked):
			writeError(w, http.StatusUnauthorized, "session_revoked", "登录已失效（口令变更或被下线），请重新登录")
		case errors.Is(err, ErrDisabled):
			writeError(w, http.StatusForbidden, "disabled", "账号已被禁用，请联系管理员")
		default:
			// 含 ErrSessionNotFound：Cookie 是我们不认识的（伪造/已清理）
			writeError(w, http.StatusUnauthorized, "auth_required", "请先登录")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"user": u.view(),
		"session": map[string]any{
			"expires_at": stampOf(sess.ExpiresAt),
			"ip":         sess.IP,
			"ua":         sess.UA,
		},
	})
}
