package account

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// httpFixture 起一套端点（账号库 + 会话库都在临时目录）。
func httpFixture(t *testing.T, registerEnabled bool) (*Store, *SessionStore, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	accounts, err := New(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sessions, err := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	h, err := NewHTTP(HTTPConfig{Accounts: accounts, Sessions: sessions, RegisterEnabled: registerEnabled})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return accounts, sessions, srv
}

// post 发一个 JSON POST，返回状态码、响应体与响应头。
func post(t *testing.T, srv *httptest.Server, path, body string, cookies ...*http.Cookie) (int, map[string]any, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	if parsed == nil {
		parsed = map[string]any{}
	}
	parsed["__raw"] = string(raw)
	return resp.StatusCode, parsed, resp.Header
}

// get 发一个带 cookie 的 GET。
func get(t *testing.T, srv *httptest.Server, path string, cookies ...*http.Cookie) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	if parsed == nil {
		parsed = map[string]any{}
	}
	parsed["__raw"] = string(raw)
	return resp.StatusCode, parsed, resp.Header
}

// setCookies 从响应头里取出 Set-Cookie 列表。
func setCookies(h http.Header) []*http.Cookie {
	return (&http.Response{Header: h}).Cookies()
}

// errorCode 取响应体里 error.code。
func errorCode(body map[string]any) string {
	e, ok := body["error"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := e["code"].(string)
	return s
}

// roleOf 取响应体里 user.role（JSON 数字解成 float64）。
func roleOf(t *testing.T, body map[string]any) int {
	t.Helper()
	u, ok := body["user"].(map[string]any)
	if !ok {
		return -1
	}
	f, _ := u["role"].(float64)
	return int(f)
}

// TestRegisterBootstrapThenMember 首启建管理员只成功一次；后续注册只给成员。
func TestRegisterBootstrapThenMember(t *testing.T) {
	accounts, _, srv := httpFixture(t, true)

	// ① 库空 ⇒ 首启建管理员
	code, body, _ := post(t, srv, PathRegister, `{"username":"boss","password":"boss-password-001"}`)
	if code != http.StatusOK {
		t.Fatalf("首启注册应当成功，得到 %d %v", code, body)
	}
	if body["bootstrap"] != true {
		t.Errorf("首启注册应当标记 bootstrap，得到 %v", body["bootstrap"])
	}
	if got := roleOf(t, body); got != RoleAdmin {
		t.Errorf("★ 首启账号应当是管理员(%d)，得到 %d", RoleAdmin, got)
	}

	// ② 库非空 ⇒ 只建成员
	code, body, _ = post(t, srv, PathRegister, `{"username":"staff","password":"staff-password-002"}`)
	if code != http.StatusOK {
		t.Fatalf("第二个注册应当成功，得到 %d %v", code, body)
	}
	if got := roleOf(t, body); got != RoleMember {
		t.Errorf("★ 普通注册只能是成员(%d)，得到 %d", RoleMember, got)
	}
	if body["bootstrap"] == true {
		t.Error("第二次注册不该再标记 bootstrap")
	}

	// ③ 客户端硬塞 role 无效（本实现直接拒收未知字段）
	code, body, _ = post(t, srv, PathRegister,
		`{"username":"sneaky","password":"sneaky-password-003","role":"admin"}`)
	if code != http.StatusBadRequest {
		t.Errorf("★ 请求体里带 role 应当被拒（400），得到 %d %v", code, body)
	}
	if errorCode(body) != "bad_request" {
		t.Errorf("code 应为 bad_request，得到 %q", errorCode(body))
	}
	if accounts.Count() != 2 {
		t.Errorf("★ 不该多出账号，得到 %d 个", accounts.Count())
	}
	if u := accounts.ByUsername("sneaky"); u != nil {
		t.Errorf("★ 硬塞 role 的请求不能建出账号: %+v", u)
	}

	// ④ 重复用户名 / 非法用户名 / 短口令
	code, body, _ = post(t, srv, PathRegister, `{"username":"staff","password":"staff-password-002"}`)
	if code != http.StatusConflict || errorCode(body) != "username_taken" {
		t.Errorf("重复用户名应得 409 username_taken，得到 %d %q", code, errorCode(body))
	}
	code, body, _ = post(t, srv, PathRegister, `{"username":"ab","password":"staff-password-002"}`)
	if code != http.StatusBadRequest || errorCode(body) != "bad_username" {
		t.Errorf("短用户名应得 400 bad_username，得到 %d %q", code, errorCode(body))
	}
	code, body, _ = post(t, srv, PathRegister, `{"username":"okname","password":"short"}`)
	if code != http.StatusBadRequest || errorCode(body) != "bad_password" {
		t.Errorf("短口令应得 400 bad_password，得到 %d %q", code, errorCode(body))
	}
}

// TestRegisterDisabledStillBootstrap 关闭注册不挡首启（否则永远进不去）。
func TestRegisterDisabledStillBootstrap(t *testing.T) {
	accounts, _, srv := httpFixture(t, false)

	// 库空：仍允许建管理员
	code, body, _ := post(t, srv, PathRegister, `{"username":"owner","password":"owner-password-001"}`)
	if code != http.StatusOK || body["bootstrap"] != true {
		t.Fatalf("★ 关闭注册时首启建管理员仍必须可用，得到 %d %v", code, body)
	}
	if accounts.Count() != 1 {
		t.Fatalf("应当建出 1 个账号，得到 %d", accounts.Count())
	}
	// 库非空：普通注册被拒
	code, body, _ = post(t, srv, PathRegister, `{"username":"later","password":"later-password-002"}`)
	if code != http.StatusForbidden || errorCode(body) != "register_disabled" {
		t.Errorf("关闭注册后应得 403 register_disabled，得到 %d %q", code, errorCode(body))
	}
	if accounts.Count() != 1 {
		t.Errorf("被拒的注册不该建号，得到 %d 个", accounts.Count())
	}
}

// TestLoginCookieAndCredentialErrors 登录取 cookie（HttpOnly/SameSite），错口令不泄露用户是否存在。
func TestLoginCookieAndCredentialErrors(t *testing.T) {
	accounts, _, srv := httpFixture(t, true)
	if _, err := accounts.Create("alice", "alice-password-001", RoleMember); err != nil {
		t.Fatal(err)
	}

	// 错口令 / 不存在的用户：同一个 code、同一句话
	code, body, _ := post(t, srv, PathLogin, `{"username":"alice","password":"wrong-password-x"}`)
	if code != http.StatusUnauthorized || errorCode(body) != "bad_credentials" {
		t.Fatalf("错口令应得 401 bad_credentials，得到 %d %q", code, errorCode(body))
	}
	msgWrong := body["__raw"]
	code, body, _ = post(t, srv, PathLogin, `{"username":"nobody-here","password":"wrong-password-x"}`)
	if code != http.StatusUnauthorized || errorCode(body) != "bad_credentials" {
		t.Errorf("不存在的用户应得 401 bad_credentials，得到 %d %q", code, errorCode(body))
	}
	if body["__raw"] != msgWrong {
		t.Errorf("★ 两种失败必须返回完全一样的响应体（否则可枚举用户名）:\n  %v\n  %v", msgWrong, body["__raw"])
	}

	// 正确口令：拿到 HttpOnly 会话 cookie
	code, body, hdr := post(t, srv, PathLogin, `{"username":"alice","password":"alice-password-001"}`)
	if code != http.StatusOK {
		t.Fatalf("正确口令应登录成功，得到 %d %v", code, body)
	}
	if !strings.Contains(hdr.Get("Set-Cookie"), "HttpOnly") {
		t.Errorf("★ 会话 cookie 必须带 HttpOnly，得到 %q", hdr.Get("Set-Cookie"))
	}
	if !strings.Contains(hdr.Get("Set-Cookie"), "SameSite=Lax") {
		t.Errorf("会话 cookie 应当带 SameSite=Lax，得到 %q", hdr.Get("Set-Cookie"))
	}
	cs := setCookies(hdr)
	if len(cs) != 1 || cs[0].Name != CookieName || len(cs[0].Value) != 64 {
		t.Fatalf("Set-Cookie 形态不对: %+v", cs)
	}
	if got := roleOf(t, body); got != RoleMember {
		t.Errorf("登录响应里的角色应当是成员，得到 %d", got)
	}

	// 禁用账号
	if err := accounts.SetEnabled(accounts.ByUsername("alice").ID, false); err != nil {
		t.Fatal(err)
	}
	code, body, _ = post(t, srv, PathLogin, `{"username":"alice","password":"alice-password-001"}`)
	if code != http.StatusForbidden || errorCode(body) != "disabled" {
		t.Errorf("禁用账号应得 403 disabled，得到 %d %q", code, errorCode(body))
	}
}

// TestSessionLogoutFlow 会话读取 → 登出 → 旧 cookie 失效（计划 T3 的第三条验收）。
func TestSessionLogoutFlow(t *testing.T) {
	accounts, _, srv := httpFixture(t, true)
	if _, err := accounts.Create("bob", "bob-password-001", RoleMember); err != nil {
		t.Fatal(err)
	}

	// 没 cookie：401 auth_required（不是错误，是"没登录"）
	code, body, _ := get(t, srv, PathSession)
	if code != http.StatusUnauthorized || errorCode(body) != "auth_required" {
		t.Fatalf("未登录取会话应得 401 auth_required，得到 %d %q", code, errorCode(body))
	}
	// 伪造 cookie：同样是 401 auth_required（不区分"伪造"与"没带"，不给探测口）
	code, body, _ = get(t, srv, PathSession, &http.Cookie{Name: CookieName, Value: strings.Repeat("a", 64)})
	if code != http.StatusUnauthorized || errorCode(body) != "auth_required" {
		t.Errorf("伪造 cookie 应得 401 auth_required，得到 %d %q", code, errorCode(body))
	}

	// 登录 → 带 cookie 取会话 200
	_, _, hdr := post(t, srv, PathLogin, `{"username":"bob","password":"bob-password-001"}`)
	sid := setCookies(hdr)[0]
	code, body, _ = get(t, srv, PathSession, sid)
	if code != http.StatusOK {
		t.Fatalf("带 cookie 取会话应为 200，得到 %d %v", code, body)
	}
	if body["ok"] != true {
		t.Errorf("会话响应应当 ok=true，得到 %v", body)
	}
	if _, hasUser := body["user"]; !hasUser {
		t.Error("会话响应应当带 user")
	}
	if _, hasSess := body["session"]; !hasSess {
		t.Error("会话响应应当带 session")
	}

	// 登出：清 cookie，且旧 SID 立刻失效
	code, _, hdr = post(t, srv, PathLogout, `{}`, sid)
	if code != http.StatusOK {
		t.Fatalf("登出应当 200，得到 %d", code)
	}
	cleared := setCookies(hdr)
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Errorf("★ 登出必须下发删除用的 Set-Cookie（MaxAge<0），得到 %+v", cleared)
	}
	code, body, _ = get(t, srv, PathSession, sid)
	if code != http.StatusUnauthorized {
		t.Errorf("★ 登出后同一个 cookie 再取会话必须 401，得到 %d %v", code, body)
	}
	// 登出幂等：已经登出再登出仍 200
	if code, _, _ = post(t, srv, PathLogout, `{}`, sid); code != http.StatusOK {
		t.Errorf("重复登出应当仍是 200，得到 %d", code)
	}
}

// TestSessionRejectsStaleAndExpired 口令变更/过期两种失效路径的 code 不同。
func TestSessionRejectsStaleAndExpired(t *testing.T) {
	accounts, sessions, srv := httpFixture(t, true)
	u, err := accounts.Create("carol", "carol-password-001", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	_, _, hdr := post(t, srv, PathLogin, `{"username":"carol","password":"carol-password-001"}`)
	sid := setCookies(hdr)[0]

	// 改口令 ⇒ 旧会话失效（前端据此提示"请重新登录"）
	if _, err := accounts.SetPassword(u.ID, "carol-password-002"); err != nil {
		t.Fatal(err)
	}
	code, body, _ := get(t, srv, PathSession, sid)
	if code != http.StatusUnauthorized || errorCode(body) != "session_revoked" {
		t.Errorf("口令变更后应得 401 session_revoked，得到 %d %q", code, errorCode(body))
	}

	// 过期：重新登录后把时间往前挪
	_, _, hdr = post(t, srv, PathLogin, `{"username":"carol","password":"carol-password-002"}`)
	sid = setCookies(hdr)[0]
	rewind(t, sessions, sid.Value, SessionTTL+time.Minute)
	code, body, _ = get(t, srv, PathSession, sid)
	if code != http.StatusUnauthorized || errorCode(body) != "session_expired" {
		t.Errorf("过期应得 401 session_expired，得到 %d %q", code, errorCode(body))
	}
}

// TestNoSecretsInAuthResponses 认证响应里不能出现口令明文。
func TestNoSecretsInAuthResponses(t *testing.T) {
	accounts, _, srv := httpFixture(t, true)
	const plain = "super-secret-password-777"
	if _, err := accounts.Create("dave", plain, RoleMember); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		run  func() (int, map[string]any, http.Header)
	}{
		{"注册（首启）", func() (int, map[string]any, http.Header) {
			return post(t, srv, PathRegister, `{"username":"newbie","password":"`+plain+`"}`)
		}},
		{"登录成功", func() (int, map[string]any, http.Header) {
			return post(t, srv, PathLogin, `{"username":"dave","password":"`+plain+`"}`)
		}},
		{"登录失败", func() (int, map[string]any, http.Header) {
			return post(t, srv, PathLogin, `{"username":"dave","password":"wrong-password-x"}`)
		}},
	}
	for _, tc := range cases {
		_, body, hdr := tc.run()
		if raw, _ := body["__raw"].(string); strings.Contains(raw, plain) {
			t.Errorf("★ %s 的响应体里出现了口令明文: %s", tc.name, raw)
		}
		if strings.Contains(hdr.Get("Set-Cookie"), plain) {
			t.Errorf("★ %s 的 Set-Cookie 里出现了口令明文: %s", tc.name, hdr.Get("Set-Cookie"))
		}
	}
}

// TestMethodAndRouting 方法与路由的边界。
func TestMethodAndRouting(t *testing.T) {
	_, _, srv := httpFixture(t, true)

	if code, _, hdr := get(t, srv, PathLogin); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "POST" {
		t.Errorf("GET 登录应当 405 + Allow: POST，得到 %d Allow=%q", code, hdr.Get("Allow"))
	}
	if code, _, hdr := post(t, srv, PathSession, `{}`); code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "GET" {
		t.Errorf("POST 会话应当 405 + Allow: GET，得到 %d Allow=%q", code, hdr.Get("Allow"))
	}
	codeReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/auth/nope", nil)
	resp, err := srv.Client().Do(codeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("未知路径应当 404，得到 %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("认证响应必须 no-store，得到 %q", cc)
	}

	// 尾部斜杠与不带斜杠等价
	if code, _, _ := post(t, srv, PathLogin+"/", `{"username":"x","password":"y"}`); code == http.StatusNotFound {
		t.Error("带尾斜杠的路径也应当命中（否则前端多一个斜杠就 404）")
	}
}

// TestBodyTooLarge 超大请求体得到 413（不是 500，也不是把它读进内存）。
func TestBodyTooLarge(t *testing.T) {
	_, _, srv := httpFixture(t, true)
	huge := `{"username":"a","password":"` + strings.Repeat("x", maxAuthBody+1024) + `"}`
	code, body, _ := post(t, srv, PathRegister, huge)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应得 413，得到 %d %v", code, errorCode(body))
	}
	if errorCode(body) != "body_too_large" {
		t.Errorf("code 应为 body_too_large，得到 %q", errorCode(body))
	}
	// 非 JSON 也是 400 而不是崩
	code, body, _ = post(t, srv, PathRegister, `not json at all`)
	if code != http.StatusBadRequest || errorCode(body) != "bad_request" {
		t.Errorf("非 JSON 应得 400 bad_request，得到 %d %q", code, errorCode(body))
	}
}

// denyAllLimiter 固定拒绝的限速器（验证 T3 留的接缝在 T10 能接上）。
type denyAllLimiter struct{ calls int }

func (d *denyAllLimiter) Allow(string, time.Time) (bool, time.Duration) {
	d.calls++
	return false, 42 * time.Second
}

// TestLimiterSeam 接上限速器后，被拒的请求得到 429 + Retry-After。
func TestLimiterSeam(t *testing.T) {
	accounts, sessions, _ := httpFixture(t, true)
	if _, err := accounts.Create("erin", "erin-password-001", RoleMember); err != nil {
		t.Fatal(err)
	}
	lim := &denyAllLimiter{}
	h, err := NewHTTP(HTTPConfig{Accounts: accounts, Sessions: sessions, RegisterEnabled: true, Limiter: lim})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, body, hdr := post(t, srv, PathLogin, `{"username":"erin","password":"erin-password-001"}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("被限速时应得 429，得到 %d %v", code, body)
	}
	if errorCode(body) != "rate_limited" {
		t.Errorf("code 应为 rate_limited，得到 %q", errorCode(body))
	}
	if hdr.Get("Retry-After") != "42" {
		t.Errorf("应当回 Retry-After: 42，得到 %q", hdr.Get("Retry-After"))
	}
	if lim.calls != 1 {
		t.Errorf("限速器应当被调用 1 次，得到 %d", lim.calls)
	}
	// 被限速的请求不该真的去校验口令（否则限速只是"多一步"）
	if sessions.Count() != 0 {
		t.Error("被限速的登录不该签发会话")
	}
}

// TestCookieSecureModes Secure 属性的三种策略。
func TestCookieSecureModes(t *testing.T) {
	dir := t.TempDir()
	accounts, _ := New(filepath.Join(dir, "accounts.json"))
	sessions, _ := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if _, err := accounts.Create("frank", "frank-password-001", RoleMember); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		mode string
		tls  bool
		want bool
	}{
		{CookieSecureAlways, false, true},
		{CookieSecureAlways, true, true},
		{CookieSecureNever, true, false},
		{CookieSecureAuto, true, true},
		{CookieSecureAuto, false, false},
		{"", true, true}, // 空 = auto
	} {
		h, err := NewHTTP(HTTPConfig{Accounts: accounts, Sessions: sessions, CookieSecure: tc.mode})
		if err != nil {
			t.Fatalf("NewHTTP(%q): %v", tc.mode, err)
		}
		var srv *httptest.Server
		if tc.tls {
			srv = httptest.NewTLSServer(h)
		} else {
			srv = httptest.NewServer(h)
		}
		_, _, hdr := post(t, srv, PathLogin, `{"username":"frank","password":"frank-password-001"}`)
		got := strings.Contains(hdr.Get("Set-Cookie"), "Secure")
		if got != tc.want {
			t.Errorf("模式 %q（TLS=%v）应当 Secure=%v，得到 %v（%q）", tc.mode, tc.tls, tc.want, got, hdr.Get("Set-Cookie"))
		}
		srv.Close()
	}

	// 非法策略要在构造时报错，而不是跑到运行时才发现
	if _, err := NewHTTP(HTTPConfig{Accounts: accounts, Sessions: sessions, CookieSecure: "sometimes"}); err == nil {
		t.Error("非法 CookieSecure 应当在 NewHTTP 报错")
	}
	if _, err := NewHTTP(HTTPConfig{Sessions: sessions}); err == nil {
		t.Error("缺账号库应当在 NewHTTP 报错")
	}
	if _, err := NewHTTP(HTTPConfig{Accounts: accounts}); err == nil {
		t.Error("缺会话库应当在 NewHTTP 报错")
	}
}
