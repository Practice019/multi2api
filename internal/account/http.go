package account

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/authlimit"
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
	// PathPassword 自助改口令（T9）。
	//
	// 与上面几条同一批、同一理由挂在门禁之外：它得先能回答"我是谁"才谈得上
	// 改口令，而"回答我是谁"正是这一批端点的职责（放在门禁里会变成
	// "没身份就不许问自己有没有身份"）。
	//
	// ⚠ 它**只认会话 cookie**，不认 admin_token —— 令牌没有"账号"这个主体，
	// 见 change_password.go 的长注释。
	PathPassword = "/admin/auth/password"
)

// maxAuthBody 匿名端点的请求体上限。
//
// ⚠ **唯一来源在 `internal/authlimit.MaxAnonymousBody`**，这里只是引用。
//
// 原先这里是独立的 `8 << 10`，与 authlimit 那份**各写一遍**。两份独立常量总有
// "改了一处忘了另一处"的风险，而这两处**必须相等**：
//
//	· 限流层（`authlimit.EnforceBodyLimit`）先判 413 —— 它若**小于**读取层，
//	  就会出现"读取层已经把更大的体读进来了、限流层才说超限"，白读一遍；
//	· 它若**大于**读取层，`http.MaxBytesReader` 会先返回 `MaxBytesError`，
//	  而那个错误不是限流层的 413 信封（code `body_too_large`）⇒ 同一个越界
//	  请求会因为"谁先撞上"而得到两种不同回执。
//
// 所以不是"加一条钉住两份常量的测试"（那只是把漂移变成红灯），而是**让漂移
// 结构上不可能**：只有一处能改，改了两边一起变。
const maxAuthBody = authlimit.MaxAnonymousBody

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
	// ⚠ 2026-10-19 用户新口径：`RegisterEnabled` 字段**已删除**。
	//
	// 它曾经表示"是否开放注册"（库非空时还允许继续注册出成员）。单账号
	// 口径下这个概念不存在了：建号**只有**"库为空时建第一个管理员"这一次，
	// 库非空之后注册接口整体关闭（见 register）。留一个开关字段会让读者
	// 以为"打开它能再注册"，而那正是本次要删掉的东西。
	//
	// 配置里的 `console.disables_register` 因此不再影响本端点行为
	// （cmd/server 启动日志会如实说明这一点，不做静默忽略）。
	CookieSecure string
	// Limiter 可空（空 = 不限速）。
	Limiter Limiter
	// AdminTokenOK 判"这个凭据是不是当前有效的管理令牌"。可空。
	//
	// 唯一用途是让 `session` 端点能回答"我是谁" —— 令牌身份要能看到控制台
	// （见 session 里那段"无限重载"的注释）。空 = 不认令牌身份。
	//
	// ⚠ 是**回调**而不是令牌值：令牌支持运行时轮换，传值就把"轮换后要重启"
	// 那个 bug 原样复制过来了（同一理由见 Identify 的长注释）。
	AdminTokenOK func(cred string) bool
	// Captcha 可空（空 = 不启用人机校验）。
	//
	// ⚠ 与 Limiter 一样是**接口**而不是具体实现：本包不认识 authlimit。
	// 见 captcha.go 的长注释（"接线错了 / 题算错了 / 端点忘了判"三件事分开可测）。
	Captcha Captcha
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
	case PathPassword:
		h.changePassword(w, r)
	case PathCaptcha:
		h.issueCaptcha(w, r)
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
	// 人机校验（T10）。两个字段都**可选**：未启用人机校验的部署上它们缺省
	// 就是空的，端点行为与 T3 时逐字一致（不启用 = 这条支路根本不被走到）。
	//
	// ⚠ 解码是严格模式（DisallowUnknownFields）⇒ 字段名必须与这里一致。
	// 自创别名（captcha_param 之类）会得到 400 bad_request 而不是
	// captcha_required，排查时会以为是"题不对"。
	CaptchaID     string `json:"captcha_id"`
	CaptchaAnswer string `json:"captcha_answer"`
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
// decodeCreds 注册与登录共用的请求体解码。
//
// ⚠ 解码纪律本体已抽到 httpbody.go 的 decodeBody（T9 起有第三个调用方
// `changePassword`）：四个细节（大小上限 / 拒未知字段 / 单个 JSON 对象 /
// 区分"太大"与"格式错"）必须只有一份实现，否则新端点会慢慢漏掉其中一条。
func decodeCreds(w http.ResponseWriter, r *http.Request) (credsBody, bool) {
	var body credsBody
	if !decodeBody(w, r, &body) {
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

// register 建号：**只有首启那一次**。
//
// # 2026-10-19 用户新口径：注册只在"首次"
//
// 用户原话：「我们那个用户和管理员，我觉得可以只要一个管理员就行了，
// 只要一个账号。用户其实可以不用。」
//
//	库里一个账号都没有  → 建出**唯一**的管理员（CreateBootstrap，原子，只得手一次）
//	库非空              → 接口**整体关闭**（403 register_closed），没有第二种结果
//
// 判定顺序仍然是"**先看库空不空**"而不是"先看某个开关"：否则部署时
// 关了注册 + 没建号 = 永远进不去（用户只能删 data/accounts.json 重来）。
//
// 竞态：Count()==0 与 CreateBootstrap 之间可能有人抢先（两个请求同时到）。
// CreateBootstrap 自己在锁里做"检查空 + 建号"，第二个到达者得到
// ErrBootstrapDone ⇒ **落到关闭分支**（回 403），不会被当成成功。
// 这正是"只有一个账号"在并发下也成立的原因。
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
	// 人机校验（T10）。⚠ 放在"首启建管理员"**之前**：
	//
	//	· 这条路是**唯一**能把调用方变成管理员的入口，把它放在题后面，
	//	  是"加一道题"这件事唯一真正要紧的地方
	//	· 反批量五层属于**保留不动**的管理员面（见计划 §目标态 5）
	//
	// 代价：部署后第一次打开控制台的人要答一道算术题。那道题就在他眼前
	// （本地 SVG），而他本来就要想一个口令 —— 可以接受。
	// 确实不想要的话：配置里 `console.disable_captcha: true`。
	if !h.checkCaptcha(w, r, body.CaptchaID, body.CaptchaAnswer) {
		return
	}
	username := strings.TrimSpace(body.Username)

	// ① 首启：建这**唯一**一个管理员（只可能成功一次）
	if h.cfg.Accounts.Count() == 0 {
		u, err := h.cfg.Accounts.CreateBootstrap(username, body.Password)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":        true,
				"bootstrap": true,
				"user":      u.view(),
				"message":   "已创建管理员账号 " + u.Username + "，请用它与口令登录（本部署只此一个账号）",
			})
			return
		}
		if !errors.Is(err, ErrBootstrapDone) {
			writeAccountError(w, err)
			return
		}
		// 竞态：刚好被别人抢先建了 ⇒ 落到下面"关闭"那条路
	}

	// ② 库非空 ⇒ 注册关闭。**没有第三个分支** —— 尤其没有"建成员"。
	//    状态码与 code 都是稳定判据：403 + register_closed。
	writeRegisterClosed(w)
}

// registerClosedCode 注册接口关闭时的稳定错误码。
//
// ⚠ 刻意**不沿用**旧的 `register_disabled`：那个 code 的语义是"管理员在配置里
// 关掉了公开注册"，而那时库非空仍可能继续建成员 —— 与现在的"规矩本身：
// 单账号部署不存在继续注册"不是同一件事。沿用旧 code 会让前端、探针与
// 后续排查把两种语义混成一条。
const registerClosedCode = "register_closed"

// writeRegisterClosed 注册接口的关闭回执（403 + 明确文案）。
//
// 文案要能让"全新部署的人"与"想再注册一个的人"都看懂：前者需要知道
// 什么时候这条路是开的（库为空时），后者需要知道该用什么进控制台。
func writeRegisterClosed(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, registerClosedCode,
		"本部署为单账号管理员模式：注册只在账号库为空时开放（用于建第一个管理员），"+
			"账号已存在则注册入口关闭。请用管理员账号登录。")
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
	// 人机校验（T10）。放在口令校验**之前**：反过来的话"答错题"与"口令错"
	// 都能靠响应时间区分出账号是否存在（口令校验要跑一次 bcrypt，题不用）。
	if !h.checkCaptcha(w, r, body.CaptchaID, body.CaptchaAnswer) {
		return
	}

	u, err := h.cfg.Accounts.Authenticate(strings.TrimSpace(body.Username), body.Password)
	if err != nil {
		switch {
		case errors.Is(err, ErrCredentials):
			// 同一个 code、同一句话：不区分"用户不存在"与"口令错"，
			// 否则登录接口本身就变成用户名枚举器。
			writeError(w, http.StatusUnauthorized, "bad_credentials", "用户名或口令不正确")
		case errors.Is(err, ErrMemberRetired):
			// ★ 2026-10-19 单账号口径（要求 3）：盘上的成员历史记录。
			//
			// 不是"口令错"（口令是对的，我们在 Authenticate 里验过才会走到这），
			// 所以给一句能照做的文案，并**留一条日志** —— 运维要能发现
			// "这条旧记录还有人在用"，否则只会收到一句"我登不上了"。
			//
			// ⚠ 401 而不是 403：前端对 401 的统一处置是"留在/回到登录页"
			// 并显示服务端文案 —— 这正是我们要的（他没有可用的账号）。
			// 403 会被某些前端当成"已登录但无权"，反而不提示重新登录。
			//
			// 日志只打用户名与来源 IP（用户名不是口令）；%q 引号包裹，
			// 免得手写文件里的怪字符串把日志排版搅乱。
			log.Printf("account: 拒绝非管理员历史记录登录 username=%q ip=%s —— "+
				"成员账号已取消，请用管理员账号", strings.TrimSpace(body.Username), clientIP(r))
			writeError(w, http.StatusUnauthorized, "member_retired", "成员账号已取消，请用管理员账号登录")
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
	// 令牌 Cookie 一并清掉：用 `?token=` 进来的浏览器没有会话可撤销，
	// 只清会话 Cookie 的话点「登出」**什么都没发生**（页面还是登录态）——
	// 用户会以为登出坏了。
	//
	// ⚠ 清 Cookie **不等于**令牌作废：那一串令牌本身仍然有效（别人手里的
	// 副本、脚本里的副本都还能用）。要真正作废请去 API 管理页轮换它。
	// 这里做的是"把这个浏览器上的那把钥匙拿走"。
	ClearAdminTokenCookie(w, r, h.cfg.CookieSecure)
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
		// 没带会话 Cookie 时还有**第二条路**要回答：管理令牌身份。
		//
		// # 为什么必须答（T12 的回归里挖出来的真 bug）
		//
		// `?token=<admin_token>` 会在 `/ui` 换成令牌 Cookie 并 302 到干净的 /ui
		// （见 internal/server/webui.go）。那条路**能**进管理面（`/admin/*` 认它），
		// 但控制台首屏问的正是本端点 —— 只认会话 Cookie ⇒ 令牌用户拿到 401 ⇒
		// 前端判"会话失效" ⇒ `location.replace('/ui')` ⇒ 又拿到控制台外壳 ⇒
		// **无限重载**。症状是"用带令牌的链接打开控制台，页面一直在闪"。
		//
		// 判据交给调用方（`AdminTokenOK` 回调），本包不持有令牌：令牌支持运行时
		// 轮换，存快照就会重演"轮换后要重启"那个 bug（见 Identify 的长注释）。
		if h.cfg.AdminTokenOK != nil {
			if cred := TokenFromRequest(r); cred != "" && h.cfg.AdminTokenOK(cred) {
				// user 为 null 是**有意的**：令牌没有"账号"这个主体
				// （它不属于某个人、也没有口令可改）。前端据此显示「管理令牌」
				// 而不是某个用户名，并隐藏「改密码」。
				writeJSON(w, http.StatusOK, map[string]any{
					"ok":          true,
					"kind":        "token",
					"user":        nil,
					"admin_token": true,
				})
				return
			}
		}
		// 既没有会话也没有有效令牌 ⇒ 这才叫"没登录"，前端据此显示登录页。
		writeError(w, http.StatusUnauthorized, "auth_required", "请先登录")
		return
	}
	sess, u, err := h.cfg.Sessions.Lookup(sid, h.cfg.Accounts)
	if err != nil {
		// 映射本体抽在 autherr.go（T9 起改口令端点共用同一份）：
		// 两处各写一份会漂移，前端对同一个失效原因就有了两种说法。
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"kind": "session",
		"user": u.view(),
		"session": map[string]any{
			"expires_at": stampOf(sess.ExpiresAt),
			"ip":         sess.IP,
			"ua":         sess.UA,
		},
	})
}
