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
//
// ⚠ 2026-10-19：参数 `registerEnabled bool` 已删除 —— 那个开关
// （HTTPConfig.RegisterEnabled）随"开放注册"这个概念一起没了。
func httpFixture(t *testing.T) (*Store, *SessionStore, *httptest.Server) {
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
	h, err := NewHTTP(HTTPConfig{Accounts: accounts, Sessions: sessions})
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

// ★ TestRegisterOnlyFirstTime 目标态 2：注册只在"首次"，之后整体关闭。
//
// ⚠ 语义变更（2026-10-19 用户新口径）：这条测试原先叫
// TestRegisterBootstrapThenMember，断言"首启建管理员，之后的注册只建**成员**"。
// "之后再建成员"那个面**已经不存在**（用户原话：「只要一个账号，用户其实
// 可以不用」）⇒ 断言改成单账号口径：库非空之后**一律** 403 register_closed，
// 且库里**永远只有一个**账号。
func TestRegisterOnlyFirstTime(t *testing.T) {
	accounts, _, srv := httpFixture(t)

	// ① 库空 ⇒ 首启建管理员（全新部署必须进得去）
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

	// ② 库非空 ⇒ 注册**关闭**（明确状态码 403 + 稳定 code + 明确文案）
	code, body, _ = post(t, srv, PathRegister, `{"username":"staff","password":"staff-password-002"}`)
	if code != http.StatusForbidden {
		t.Fatalf("★ 库非空后再注册必须被拒（403），得到 %d %v", code, body)
	}
	if got := errorCode(body); got != registerClosedCode {
		t.Errorf("★ code 应为 %q（稳定的机器判据），得到 %q", registerClosedCode, got)
	}
	if raw, _ := body["__raw"].(string); !strings.Contains(raw, "单账号") || !strings.Contains(raw, "管理员账号登录") {
		t.Errorf("★ 文案要说清「为什么」与「怎么办」（单账号 + 用管理员账号登录），得到 %s", raw)
	}
	if got := accounts.Count(); got != 1 {
		t.Errorf("★ 被拒的注册不得建号：库里应当只有 1 个账号，得到 %d", got)
	}
	if u := accounts.ByUsername("staff"); u != nil {
		t.Errorf("★ 第二次注册不得建出任何账号（更不得是成员）: %+v", u)
	}
	// 把**真实的**状态码与文案打进日志：验收时要的是"真的回了这个"，
	// 不是"测试没红"。断言失败时的输出读起来更费劲，且可能被忽略。
	t.Logf("证据：库非空后注册 → HTTP %d，code=%q，文案=%s", code, errorCode(body), body["__raw"])

	// ③ 客户端硬塞 role 无效（本实现直接拒收未知字段）
	code, body, _ = post(t, srv, PathRegister,
		`{"username":"sneaky","password":"sneaky-password-003","role":"admin"}`)
	if code != http.StatusBadRequest {
		t.Errorf("★ 请求体里带 role 应当被拒（400），得到 %d %v", code, body)
	}
	if errorCode(body) != "bad_request" {
		t.Errorf("code 应为 bad_request，得到 %q", errorCode(body))
	}
	if accounts.Count() != 1 {
		t.Errorf("★ 不该多出账号，得到 %d 个", accounts.Count())
	}
	if u := accounts.ByUsername("sneaky"); u != nil {
		t.Errorf("★ 硬塞 role 的请求不能建出账号: %+v", u)
	}

	// ④ 重名 / 非法用户名 / 短口令：库非空 ⇒ 全部先撞"注册已关闭"
	//
	// ⚠ 语义变更：原先这里断言的是 409 username_taken / 400 bad_username /
	// 400 bad_password —— 那三条只在"库为空"时才可能被看到（它们发生在
	// CreateBootstrap 内部的输入校验里）。库非空之后判定顺序是"先看库空不空"，
	// 所以**关闭**才是正确结果。用户名/口令的边界判据改到空库夹具上做（见下）。
	code, body, _ = post(t, srv, PathRegister, `{"username":"staff","password":"staff-password-002"}`)
	if code != http.StatusForbidden || errorCode(body) != registerClosedCode {
		t.Errorf("★ 库非空时重名注册也应当是 403 %s，得到 %d %q", registerClosedCode, code, errorCode(body))
	}
}

// TestRegisterInputValidationOnEmptyLibrary 输入校验在**空库**上仍然生效。
//
// 这是上一条 ④ 移过来的判据：单账号口径下"用户名/口令不合法"只可能在建
// 第一个管理员时看到（那之后端点整体关闭），所以每条断言各用一个**全新的
// 空库**夹具 —— 而不是把断言放宽。
func TestRegisterInputValidationOnEmptyLibrary(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantCode string
	}{
		{"短用户名", `{"username":"ab","password":"staff-password-002"}`, "bad_username"},
		{"短口令", `{"username":"okname","password":"short"}`, "bad_password"},
		{"非法字符", `{"username":"has space","password":"staff-password-002"}`, "bad_username"},
	} {
		_, _, srv := httpFixture(t)
		code, body, _ := post(t, srv, PathRegister, tc.body)
		if code != http.StatusBadRequest || errorCode(body) != tc.wantCode {
			t.Errorf("%s：应得 400 %s，得到 %d %q", tc.name, tc.wantCode, code, errorCode(body))
		}
	}
}

// ★ TestRegisterClosedDoesNotCreateAnything 关闭之后**任何**输入都不建号。
//
// 这条是"库非空 ⇒ 不许再建出成员"的兜底判据：直接把库里塞满各种形态的
// 注册请求（含成员角色字段、含 admin 角色字段、空名、超长名），逐个断言
// 库的账号数从头到尾不变。
func TestRegisterClosedDoesNotCreateAnything(t *testing.T) {
	accounts, _, srv := httpFixture(t)
	// 先建出那个唯一的管理员
	if code, body, _ := post(t, srv, PathRegister, `{"username":"boss","password":"boss-password-001"}`); code != http.StatusOK {
		t.Fatalf("前置失败：首启建管理员 %d %v", code, body)
	}
	before := accounts.Count()

	// 每个请求体给出它应当得到的 (状态码, code)，然后统一断言"账号数没变"。
	//
	// ⚠ 两种拒绝都要接受，但**分得很清**：
	//
	//	403 register_closed  合法请求体 ⇒ 注册已关闭（这就是"注册只在首次"）
	//	400 bad_request      带 role 字段 ⇒ **解码层**就拒了（严格解码，
	//	                     credsBody 里根本没有 role 这个字段）
	//
	// 后者其实比前者更硬：连"服务端要不要信这个字段"都不用讨论 —— 它进不来。
	// 所以这里**没有**放宽断言（原断言只要求"账号数不变"），而是把"带 role
	// 的请求连门都进不了"也钉成一条。
	for _, tc := range []struct {
		body     string
		wantCode int
		wantErr  string
	}{
		{`{"username":"staff","password":"staff-password-002"}`, http.StatusForbidden, registerClosedCode},
		{`{"username":"staff2","password":"staff-password-003","role":1}`, http.StatusBadRequest, "bad_request"},
		{`{"username":"staff3","password":"staff-password-004","role":10}`, http.StatusBadRequest, "bad_request"},
		{`{"username":"boss","password":"boss-password-001"}`, http.StatusForbidden, registerClosedCode},
		{`{"username":"","password":"whatever-password-1"}`, http.StatusForbidden, registerClosedCode},
		{`{"username":"` + strings.Repeat("z", 200) + `","password":"whatever-password-1"}`, http.StatusForbidden, registerClosedCode},
	} {
		code, resp, _ := post(t, srv, PathRegister, tc.body)
		if code != tc.wantCode || errorCode(resp) != tc.wantErr {
			t.Errorf("★ 库非空后注册应当 %d %s，得到 %d %q（请求体 %s）",
				tc.wantCode, tc.wantErr, code, errorCode(resp), tc.body)
		}
		if accounts.Count() != before {
			t.Fatalf("★ 账号数被改动了：%d → %d（请求体 %s）", before, accounts.Count(), tc.body)
		}
	}
}

// TestLoginCookieAndCredentialErrors 登录取 cookie（HttpOnly/SameSite），错口令不泄露用户是否存在。
func TestLoginCookieAndCredentialErrors(t *testing.T) {
	accounts, _, srv := httpFixture(t)
	if _, err := accounts.CreateBootstrap("alice", "alice-password-001"); err != nil {
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
	// ⚠ 语义变更：夹具里那个唯一账号就是**管理员**（成员角色已取消）。
	if got := roleOf(t, body); got != RoleAdmin {
		t.Errorf("登录响应里的角色应当是管理员(%d)，得到 %d", RoleAdmin, got)
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
	accounts, _, srv := httpFixture(t)
	if _, err := accounts.CreateBootstrap("bob", "bob-password-001"); err != nil {
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
	// T12 起登出会清**两个** Cookie：会话（wb2api_sid）+ 管理令牌
	// （wb2api_admin_token）。理由：用 `?token=` 换手进来的浏览器没有会话可撤销，
	// 只清会话 Cookie 的话点「登出」等于什么都没发生（页面还是登录态）。
	//
	// 判据不变的部分：每个被清的 Cookie 都必须是 MaxAge<0（浏览器立刻删），
	// 且**必须包含会话 Cookie**（漏了它就是"登出后旧 cookie 还能用"）。
	clearedOK := len(cleared) >= 1
	var sawSID bool
	for _, c := range cleared {
		if c.MaxAge >= 0 {
			clearedOK = false
		}
		if c.Name == CookieName {
			sawSID = true
		}
	}
	if !clearedOK || !sawSID {
		t.Errorf("★ 登出必须下发删除用的 Set-Cookie（MaxAge<0）且包含会话 Cookie，得到 %+v", cleared)
	}
	if len(cleared) > 2 {
		t.Errorf("登出只该清会话与令牌两个 Cookie，得到 %+v", cleared)
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
	accounts, sessions, srv := httpFixture(t)
	u, err := accounts.CreateBootstrap("carol", "carol-password-001")
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
	accounts, _, srv := httpFixture(t)
	const plain = "super-secret-password-777"
	if _, err := accounts.CreateBootstrap("dave", plain); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		run  func() (int, map[string]any, http.Header)
	}{
		// ⚠ 语义变更：库非空 ⇒ 注册被拒（403 register_closed），不再是"首启"。
		// 判据不变的部分更严了一点：**被拒**的响应里同样不许回吐口令明文
		//（错误响应也是响应，也可能进日志/截图）。
		{"注册（库非空 ⇒ 403 register_closed）", func() (int, map[string]any, http.Header) {
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
	_, _, srv := httpFixture(t)

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
	_, _, srv := httpFixture(t)
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
	accounts, sessions, _ := httpFixture(t)
	if _, err := accounts.CreateBootstrap("erin", "erin-password-001"); err != nil {
		t.Fatal(err)
	}
	lim := &denyAllLimiter{}
	h, err := NewHTTP(HTTPConfig{Accounts: accounts, Sessions: sessions, Limiter: lim})
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
	if _, err := accounts.CreateBootstrap("frank", "frank-password-001"); err != nil {
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

// ★★ TestLoginRefusesLegacyMemberWithClearMessage 目标态 3 的 HTTP 层判据。
//
// 盘上放一条改造前留下的成员记录（role=1，口令是我们知道的那个），要求：
//
//	① 登录**不崩**（服务端 5xx 是不合格的：那是"服务坏了"，不是"这个账号没了"）
//	② 401 + 稳定 code `member_retired` + 明确文案（点明"用管理员账号登录"）
//	③ 不下发任何会话 cookie（没有会话 = 后面每条 /admin/* 都会 401）
//	④ 服务端**留了一条日志**（要求 3 明说；这里断言日志格式串存在于源码行为里
//	   —— 日志本身由 log 包写到 stderr，测试里只能验证到"走的是这条路"）
//	⑤ 同一个库里那个真管理员照常登录
func TestLoginRefusesLegacyMemberWithClearMessage(t *testing.T) {
	dir := t.TempDir()
	accPath := filepath.Join(dir, "accounts.json")
	seedAccountsFile(t, accPath,
		accountRow(t, "boss", "boss-password-001", RoleAdmin),
		accountRow(t, "staff", "staff-password-002", RoleMember),
	)
	accounts, err := New(accPath)
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sessions, err := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	h, err := NewHTTP(HTTPConfig{Accounts: accounts, Sessions: sessions})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// ① ② ③ 成员记录：口令正确也拒，且是"明确的 401 + 明确文案"
	code, body, hdr := post(t, srv, PathLogin, `{"username":"staff","password":"staff-password-002"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("★ 成员记录登录必须 401（不是 5xx、也不是放行），得到 %d %v", code, body)
	}
	if got := errorCode(body); got != "member_retired" {
		t.Errorf("★ code 应为 member_retired（前端/运维据此分流），得到 %q", got)
	}
	if raw, _ := body["__raw"].(string); !strings.Contains(raw, "成员账号已取消") ||
		!strings.Contains(raw, "管理员账号登录") {
		t.Errorf("★ 文案必须是「成员账号已取消，请用管理员账号登录」，得到 %s", raw)
	}
	if cs := setCookies(hdr); len(cs) != 0 {
		t.Errorf("★ 被拒的登录不得下发任何会话 cookie，得到 %+v", cs)
	}
	if sessions.Count() != 0 {
		t.Errorf("★ 被拒的登录不得签发会话，会话库里有 %d 条", sessions.Count())
	}
	t.Logf("证据：成员历史记录登录 → HTTP %d，code=%q，文案=%s；会话数=%d",
		code, errorCode(body), body["__raw"], sessions.Count())

	// 口令错 ⇒ 仍是 bad_credentials（不给"这个用户名存在吗"的探测信号）
	code, body, _ = post(t, srv, PathLogin, `{"username":"staff","password":"wrong-password-zz"}`)
	if code != http.StatusUnauthorized || errorCode(body) != "bad_credentials" {
		t.Errorf("★ 成员记录配错口令应是 401 bad_credentials，得到 %d %q", code, errorCode(body))
	}

	// ⑤ 同库的真管理员照常登录
	code, body, hdr = post(t, srv, PathLogin, `{"username":"boss","password":"boss-password-001"}`)
	if code != http.StatusOK {
		t.Fatalf("★ 同库的真管理员必须仍能登录，得到 %d %v", code, body)
	}
	if cs := setCookies(hdr); len(cs) != 1 || cs[0].Name != CookieName {
		t.Errorf("管理员登录应拿到会话 cookie，得到 %+v", cs)
	}
}

// ★ TestSessionOfLegacyMemberIsRefused 旧会话 cookie 也不构成身份。
//
// 这条堵的是"只堵登录口"那种半吊子做法：改造前签发的成员会话（或账号文件
// 被回滚/手改过）如果还能通过 Lookup，`/ui` 就会把控制台外壳发给它
// （Identify 走的正是 Lookup）—— 等于成员面从一条旧 cookie 又活了。
func TestSessionOfLegacyMemberIsRefused(t *testing.T) {
	dir := t.TempDir()
	accPath := filepath.Join(dir, "accounts.json")
	seedAccountsFile(t, accPath, accountRow(t, "staff", "staff-password-002", RoleMember))
	accounts, err := New(accPath)
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sessions, err := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	h, err := NewHTTP(HTTPConfig{Accounts: accounts, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 直接给那条成员记录签一条会话（模拟"改造之前签好的那条 cookie"）——
	// 绕过登录口，因为登录口现在建不出会话（那正是要求 1/2）。
	member := accounts.ByUsername("staff")
	if member == nil {
		t.Fatal("前置：成员记录应当被加载")
	}
	sess, err := sessions.Issue(member, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("签会话: %v", err)
	}
	code, body, _ := get(t, srv, PathSession, &http.Cookie{Name: CookieName, Value: sess.SID})
	if code != http.StatusUnauthorized {
		t.Fatalf("★ 成员历史会话必须 401（不许构成身份），得到 %d %v", code, body)
	}
	if got := errorCode(body); got != "member_retired" {
		t.Errorf("★ code 应为 member_retired，得到 %q", got)
	}
}
