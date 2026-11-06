// renew.go MonkeyCode 的**自动续期**：用 邮箱 + 密码 + PoW 换一个新 session。
//
// # 为什么这个文件存在（它推翻了一条旧结论）
//
// 本仓以前的结论是「session 30 天硬过期，**无续期接口**，过期只能重登/重导」
// （见 expiry.go 与 credential.go 的旧注释）。那个结论**是错的**：
// 2026-11-01 在注册机侧实测跑通了续期链路，本文件把它移植过来。
//
// # 链路（三步，全程不需要有效 session）
//
//	① POST /api/v1/public/captcha/challenge   → {challenge:{c,s,d}, token}
//	     公开端点。解 c 个子挑战（PoW，纯 CPU）—— 复用 captcha.go 的实现，不重写。
//	② POST /api/v1/public/captcha/redeem      → {success, token}
//	     这个 token 就是 captcha_token。
//	③ POST /api/v1/users/password-login       {email, password, captcha_token}
//	     → 200 {"code":0,…} + Set-Cookie: monkeycode_ai_session=<新 uuid>; Max-Age=2592000
//
// # 实测关键事实（每条都值得写进注释，否则下一个人会以为是笔误）
//
//  1. **Max-Age=2592000 = 30 天**。这是服务端亲口给的真实有效期，
//     以前只能按 AddedAt+30 天估算 —— 现在续期后可以拿真值（TTLSource="max-age"）。
//  2. **新旧 session 并存**（旧的不会被吊销）。所以续期是**安全操作**：
//     失败不影响现有凭据，成功了也只是多一把可用的 session。
//  3. **challenge 与 password-login 都不要求登录态** ⇒ session 完全失效时也能救回来。
//     只有 password 本身失效（改密/被封）才需要人工重登。
//  4. 单号耗时 4~20 秒（PoW 是 CPU，网络占大头）；8 并发下 35 个号约 16~20 秒。
//
// # ⚠⚠ 不要"为了安全"给这两步加前置校验
//
// 本文件里 challenge/redeem/password-login 三枪**都不带 Cookie、不带 Authorization**。
// 这不是遗漏，而是**必须**：它们就是用来救"session 已经死了"的账号的。
// 一旦有人加一句"没 session 就别请求"，**唯一能自救的路径就没了** ——
// 而现象是"过期号再也换不回来"，不会报错，极难查。同理也不要把它们
// 挪进 sessionHeaders() 那条路（那会给空 session 拼出 `Cookie: monkeycode_ai_session=`）。
package monkeycode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// pathPasswordLogin 邮箱密码登录（续期的第三步）。
//
// 注册机侧的对照实现把它写在 login.go 的注释里（`choices.password`
// → `POST /api/v1/users/password-login`），但本仓从未实现；这里是第一版实现。
const pathPasswordLogin = "/api/v1/users/password-login"

// sessionCookieName 上游下发的 session cookie 名（与 MONKEYCODE 的域名无关，固定串）。
const sessionCookieName = "monkeycode_ai_session"

// renewAttempts 网络层偶发失败时的**总尝试次数**（1 次首发 + 1 次重试）。
//
// 为什么是 2 而不是 3：与 netretry.go 的 getAttempts 同口径 —— 死出口下
// 每次尝试都要等满 TLS 握手超时（20s），而续期是**并发批量**动作，
// 3 次尝试会把"失败"拖成"卡住"，比失败更糟。
const renewAttempts = 2

// renewPost 发一次 POST 并把**响应头**一起带回来。
//
// 为什么不复用 doJSONOnce：它只回 (状态码, body)，而续期必须看 `Set-Cookie`
// （session 只在那里下发）。改 doJSONOnce 的签名会波及它所有调用点，
// 而这里只需要一个"连响应头一起拿"的极薄版本 —— 所以单独一个函数，
// 保持 client.go 那三个 doJSON* 的契约不变。
//
// 行为与 doJSONOnce 对齐：非 2xx **不**当错误（交调用方按业务语义判），
// 只有传输层失败才返回 error。
func (c *Client) renewPost(ctx context.Context, a *Auth, rawURL string, headers http.Header, body []byte) (int, []byte, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpFor(a).Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, nil, err
	}
	return resp.StatusCode, raw, resp.Header.Values("Set-Cookie"), nil
}

// RenewResult 单个账号的续期结果。
type RenewResult struct {
	// Session 换回来的新 session（旧的不受任何影响，仍然可用）。
	Session string
	// ExpiresInSec 有效期（秒）。有 Max-Age 时是上游给的**真值**，否则是 SessionLifetime。
	ExpiresInSec int64
	// TTLSource 有效期来源：`max-age`（上游亲口给的） / `fallback`（按 30 天推算）。
	TTLSource string
}

// 有效期来源取值。
const (
	ttlSourceMaxAge   = "max-age"
	ttlSourceFallback = "fallback"
)

// renewHost 续期时用的站点基址。
//
// **两个注入点为什么都留着**（2026-11-01 逐个调用点核对过，不是照抄旧注释）：
//
//	① `Client.Host`（优先）—— 给**持有 *Client 的测试**：把假上游地址挂到那个 Client 实例上，
//	   作用域**限于该实例**，不污染同包其它测试，测试结束也无需清理。
//	② `renewHostOverride`（兜底）—— 给**拿不到 *Client 的场合**：包级变量，任何位置都能设；
//	   也在 `c == nil` 时兜底（`renewHost` 是方法，但入口处显式做了 nil 判断）。
//
//	现状核对（谁是当前入口）：本文件内 `renewHost` 的调用点都在 `*Client` 的方法上
//	（`renewSession` 与探针路径）⇒ **两个注入点当前都可用**；在跑的测试（renew_test.go）选的是 ②。
//	**保留 ①** 是为"每个测试一个 Client"的写法留口，且删它要连带改调用点 —— 收益不抵风险。
//
// ⚠ `renewHostOverride` 是**包级变量**，用它就必须**禁止 `t.Parallel()`**（见 renew_test.go 文件头的维护提示）。
// ⚠ 不改 `Auth.Host()` 本身：那条路径被 egress 自检/模型目录/签到共用，
// 动它会把"所有 host 请求"一起改道 —— 影响面远大于续期。
func (c *Client) renewHost(a *Auth) string {
	if c != nil {
		if s := strings.TrimSpace(c.Host); s != "" {
			return strings.TrimRight(s, "/")
		}
	}
	if s := strings.TrimSpace(renewHostOverride); s != "" {
		return strings.TrimRight(s, "/")
	}
	return a.Host()
}

// renewHostOverride 仅测试用；生产恒为空。
var renewHostOverride string

// renewSession 用 email+password+PoW 换一个新 session。
//
// attempts: 网络层偶发重试次数（<=0 用默认 renewAttempts）。
//
// 重试范围**只有传输层**（transientNetErr 判定的那种）；`code!=0`（403 密码错等）
// 是确定性结论，重试只是白打一次上游。
func (c *Client) renewSession(ctx context.Context, region, email, password string, attempts int) (*RenewResult, error) {
	if strings.TrimSpace(email) == "" || strings.TrimSpace(password) == "" {
		return nil, fmt.Errorf("monkeycode: 续期需要 email 与 password（缺任一个都不发请求）")
	}
	// 只有 region 的 Auth：challenge/redeem/password-login 都不需要 session，
	// 而 solveCaptcha 内部只用它取 host（以及拼一个空 Cookie，见下）。
	probe := &Auth{Region: region}

	captchaToken, err := c.solveCaptcha(ctx, probe)
	if err != nil {
		return nil, fmt.Errorf("monkeycode: 续期取 captcha_token 失败：%w", err)
	}

	body, err := json.Marshal(map[string]string{
		"email":         strings.TrimSpace(email),
		"password":      password,
		"captcha_token": captchaToken,
	})
	if err != nil {
		return nil, err
	}

	host := c.renewHost(probe)
	// ⚠ 不带 Cookie / Authorization（见文件头警告）。
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")

	// POST 不在 doJSON 的自动重试范围内（那条路只重试 GET，理由是"重复提交
	// 对签到是错误码"）。续期正相反 —— 它**天然可重放**（新旧 session 并存），
	// 所以这里自己做一次有限重试。
	tries := attempts
	if tries <= 0 {
		tries = renewAttempts
	}
	var lastErr error
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, c.wrapNetErr(probe, http.MethodPost, host+pathPasswordLogin, ctx.Err())
			case <-time.After(retryDelay(attempt)):
			}
		}
		code, raw, setCookies, err := c.renewPost(ctx, probe, host+pathPasswordLogin, h, body)
		if err != nil {
			lastErr = err
			if !transientNetErr(err) {
				break
			}
			continue
		}
		if code != http.StatusOK {
			// 业务错：不重试（403 密码错 / 401 / 429 重试也一样）。
			// 把上游原文带出来 —— 只说"HTTP 403"没法判断是密码错还是 PoW 被拒。
			return nil, fmt.Errorf("monkeycode: 续期失败（password-login HTTP %d）：%s", code, gateway.Truncate(string(raw), 200))
		}
		return parsePasswordLogin(raw, setCookies)
	}
	if lastErr != nil {
		return nil, c.wrapNetErr(probe, http.MethodPost, host+pathPasswordLogin, lastErr)
	}
	// 理论上不可达（循环里要么 return 要么置 lastErr）。
	return nil, fmt.Errorf("monkeycode: 续期失败（未拿到任何响应）")
}

// cookiesOf 取一次响应里的全部 Set-Cookie 原文。
//
// （保留这个小工具给测试用：测试直接构造 []string 更省事，
// 但需要"从真响应拿原文"时用得上。）
func cookiesOf(resp *http.Response) []string {
	if resp == nil {
		return nil
	}
	return resp.Header.Values("Set-Cookie")
}

// passwordLoginPayload 只取我们需要的那一点点（成功与否 + 有没有显式报错）。
//
// 用 json.RawMessage 接 data：它可能没有、可能是 null、也可能是对象，
// 而**这里对它的类型没有任何要求** —— 我们只要 status 干净、cookie 拿到。
// 硬解成结构体会因为一个字段类型不符把"续期其实成功了"判成失败
// （regmachine.go 的 balance 就是同一类坑）。
type passwordLoginPayload struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// parsePasswordLogin 从响应体 + Set-Cookie 里取新 session。
//
// ⚠ 响应体与 cookie 的**判据来源不同**：
//   - 业务是否成功看 code（`code!=0` 且有 message → 报错）
//   - session 只能从 Set-Cookie 拿（body 里**没有** session 字段，实测确认）
//
// 所以"HTTP 200 且 code=0 但没下发 cookie"是一种**必须单独报出来**的失败形态：
// 它意味着上游改了行为，而不是密码错。分开报，排障时才有方向。
func parsePasswordLogin(raw []byte, setCookies []string) (*RenewResult, error) {
	var p passwordLoginPayload
	if err := json.Unmarshal(raw, &p); err == nil {
		if p.Code != 0 && p.Message != "" {
			return nil, fmt.Errorf("monkeycode: 续期被上游拒绝（code=%d）：%s", p.Code, p.Message)
		}
	}
	sess, ttl, source := parseSessionCookie(setCookies)
	if sess == "" {
		return nil, fmt.Errorf("monkeycode: 续期返回 200 但没有下发 %s（上游行为变了？）：%s",
			sessionCookieName, gateway.Truncate(string(raw), 160))
	}
	return &RenewResult{Session: sess, ExpiresInSec: ttl, TTLSource: source}, nil
}

// parseSessionCookie 从一批 Set-Cookie 里挑出 session 与它的 Max-Age。
//
// 判据（三条都要对，任一条错都会静默拿到错值）：
//
//	① 只认**名为 sessionCookieName 的**那条 cookie。
//	   GitHub OAuth 那条链路会下发别的 cookie（_gh_sess 等），挑错就整条续期白做。
//	② Max-Age 只在 >0 时采信：`Max-Age=0` 在 RFC 6265 里是**删除**语义，
//	   不是"零秒有效" —— 把它当 0 秒会算出"立刻过期"，界面天天报警。
//	③ 没写 Max-Age（或写的是负数/非数字）→ 回退 SessionLifetime，
//	   并把来源标成 fallback（诚实说明"这是我们推算的"）。
//
// @return session、有效期秒数、来源；session 为空 = 没找到（调用方报错）。
func parseSessionCookie(cookies []string) (session string, expiresInSec int64, source string) {
	source = ttlSourceFallback
	expiresInSec = int64(SessionLifetime / time.Second)
	for _, c := range cookies {
		name, value, attrs := splitSetCookie(c)
		if !strings.EqualFold(name, sessionCookieName) {
			continue
		}
		if v := strings.TrimSpace(value); v != "" {
			session = v
		}
		if sec, ok := maxAgeOf(attrs); ok && sec > 0 {
			expiresInSec = sec
			source = ttlSourceMaxAge
		}
		break // 同名 cookie 只认第一条（上游不会下发两条 session）
	}
	return session, expiresInSec, source
}

// splitSetCookie 拆一条 Set-Cookie：`name=value; Attr; Attr=…`。
//
// 不用 net/http 的 cookie 解析器：它会把 `Max-Age` 塞进 Cookie.MaxAge
// **但丢掉了"有没有写"这个事实**（没写时是 0，写了 Max-Age=0 时也是 0）——
// 而这两者在本文件里语义完全相反（推算 vs 删除）。
func splitSetCookie(raw string) (name, value string, attrs map[string]string) {
	parts := strings.Split(raw, ";")
	attrs = map[string]string{}
	if len(parts) == 0 {
		return "", "", attrs
	}
	head := parts[0]
	if i := strings.IndexByte(head, '='); i >= 0 {
		name = strings.TrimSpace(head[:i])
		value = head[i+1:]
	} else {
		name = strings.TrimSpace(head)
	}
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.IndexByte(p, '='); i >= 0 {
			attrs[strings.ToLower(strings.TrimSpace(p[:i]))] = strings.TrimSpace(p[i+1:])
		} else {
			attrs[strings.ToLower(p)] = ""
		}
	}
	return name, value, attrs
}

// maxAgeOf 取 Max-Age（秒）。第二个返回值 = 这条属性**确实存在且能解析成数字**。
//
// 负 Max-Age（会话 cookie 的标记）也算"存在"，但值 <=0 ⇒ 调用方会用回退值
// （见 parseSessionCookie 的第②③条）。
func maxAgeOf(attrs map[string]string) (int64, bool) {
	raw, ok := attrs["max-age"]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// HasRenewCreds 这份凭证有没有续期所需的材料（邮箱 + 密码）。
//
// ⚠ 只判"材料在不在"，**不判 session 是否有效** —— 续期恰恰是给
// session 已经失效的账号用的（见文件头第 3 条）。
func (a *Auth) HasRenewCreds() bool {
	return a != nil &&
		strings.TrimSpace(a.Email) != "" &&
		strings.TrimSpace(a.Password) != ""
}

// RenewSession 续期一个账号并把结果写回磁盘。
//
// 返回 (更新后的 Auth, 续期结果, error)。失败时前两个为 nil。
//
// # 写回的边界（只动这三样）
//
//	Session           换回来的新 session（旧的不吊销）
//	SessionRenewedAt  本次续期时刻（让 expiry.go 用"真续期时刻 + 30 天"而不是估算）
//	其他               Email/Password/APIKey/SigningSecret/Region/UserID/Plan/… 全部原样保留
//
// 之所以能"全部保留"：写盘走的是 MarshalAuthFile(*Auth)，而 Auth 是**结构化字段集**，
// 不存在"把不认识的字段丢掉"那种白名单裁剪（accountimport.go 的 credOf 也是同一个序列化器）。
func (p *Provider) RenewSession(ctx context.Context, uid string) (*Auth, *RenewResult, error) {
	a := p.cred(uid)
	if a == nil {
		return nil, nil, fmt.Errorf("monkeycode: 找不到账号 %s", uid)
	}
	// ⚠ 材料不齐 → 明确报错且**不发任何网络请求**。
	//   现象上这很重要：不做前置检查的话，用户会看到一条"PoW 失败/captcha 错误"，
	//   而真正的原因是这份凭证根本没存过密码。
	if !a.HasRenewCreds() {
		return nil, nil, fmt.Errorf("monkeycode: 账号 %s 缺 email 或 password，无法自动续期"+
			"（需要重新导入一份含 email/password 的凭证）", a.DisplayUID())
	}

	res, err := p.client.renewSession(ctx, a.RegionOrDefault(), a.Email, a.Password, renewAttempts)
	if err != nil {
		return nil, nil, err
	}

	// 拷贝一份再改：a 可能是池里的共享对象，就地改会让并发请求读到半写状态
	// （signCredentials 的注释里踩过同一个坑）。
	out := *a
	out.Session = res.Session
	out.SessionRenewedAt = time.Now()

	// 写盘。SaveFile 是**原子写**（先 .tmp 再 rename，0600），见 credential.go。
	if _, serr := SaveFile(p.authDir, &out); serr != nil {
		// 与 signCredentials 同口径：写盘失败**不阻断**本次结果
		// （新 session 已经在手里、本次可用），但必须记日志 ——
		// 否则重启后 session 又变回旧的，而没人知道原因。
		logf("monkeycode: 续期成功但写盘失败（本次可用，重启后需重续）：%v", serr)
	}
	// 写回池内缓存（下次请求直接拿到新 session）。
	p.mu.Lock()
	if p.creds == nil {
		p.creds = map[string]*Auth{}
	}
	p.creds[out.UID()] = &out
	p.mu.Unlock()

	logf("monkeycode: 已续期 %s（新 session 有效期 %d 天，来源 %s）",
		out.DisplayName(), res.ExpiresInSec/86400, res.TTLSource)
	return &out, res, nil
}
