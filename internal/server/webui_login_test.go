// webui_login_test.go `/ui` 的 T5 验收：不再注入密钥 + 未认证给登录页 +
// `?token=` 一次性换 cookie。
//
// # 为什么这些判据必须在 **HTTP 层**（而不是只测 renderUI 纯函数）
//
// 用户报障 ③ 的形态是"打开页面就拿到钥匙"，而钥匙是**handler 决定**要不要
// 塞进响应体的（旧 renderUI 的注入参数由 ui() 传）。只测纯函数等于只测了
// "替换函数工作正常"，测不到"该不该替换" —— 那正是出问题的地方。
//
// 所以这一组拿**真实配置密钥**（SECRET-…）去比对**真实响应体**：只要任何
// 一条路径把配置里的 api_key / admin_token 漏进 HTML，它就红。
package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"workbuddy2api/internal/account"
	"workbuddy2api/internal/pool"
)

// 配置里的两把钥匙。故意做成一眼能认的形状：出现在响应体里就是泄露。
const (
	testAPIKey     = "SECRET-API-KEY-AAA111"
	testAdminToken = "SECRET-ADMIN-TOKEN-BBB222"
)

// 登录页与外壳各自的稳定标记（页面里真实存在的结构，不是文案猜测）。
const (
	loginPageMarker   = `id="form"`                 // 登录表单
	consolePageMarker = `window.__WB2API_KEY__ = "` // 控制台外壳里的"未注入"基准
)

type uiFixture struct {
	h        *Handler
	accounts *account.Store
	sessions *account.SessionStore
	user     *account.User
}

// newUIFixture 造一个最小 /ui 夹具。withAccounts=false 模拟"没接账号体系"。
func newUIFixture(t *testing.T, withAccounts, registerOpen bool) *uiFixture {
	t.Helper()
	f := &uiFixture{}
	cfg := Config{
		// 真池子（空池）：/status 与 /admin/* 的处理器会读它，给 nil 会 panic
		// 成 500 —— 那样断言就成了"没被门禁挡住"而不是"真的通了"，弱得多。
		Pool:         pool.New(filepath.Join(t.TempDir(), "pool-state.json")),
		APIKey:       testAPIKey,
		AdminToken:   testAdminToken,
		CookieSecure: account.CookieSecureNever, // 测试走明文 http
		RegisterOpen: func() bool { return registerOpen },
	}
	if withAccounts {
		dir := t.TempDir()
		var err error
		if f.accounts, err = account.New(filepath.Join(dir, "accounts.json")); err != nil {
			t.Fatalf("账号库: %v", err)
		}
		if f.sessions, err = account.NewSessionStore(filepath.Join(dir, "sessions.json")); err != nil {
			t.Fatalf("会话库: %v", err)
		}
		if f.user, err = f.accounts.Create("boss", "boss-password-001", account.RoleAdmin); err != nil {
			t.Fatalf("建管理员: %v", err)
		}
		cfg.Accounts = f.accounts
		cfg.Sessions = f.sessions
	}
	f.h = NewHandler(cfg)
	return f
}

// get 打一次 /ui。
func (f *uiFixture) get(t *testing.T, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// sessionCookie 给夹具里那个账号签一条会话并返回 cookie。
func (f *uiFixture) sessionCookie(t *testing.T) *http.Cookie {
	t.Helper()
	sess, err := f.sessions.Issue(f.user, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("签发会话: %v", err)
	}
	return &http.Cookie{Name: account.CookieName, Value: sess.SID}
}

// cookieValue 从响应里挑出指定名字的 Set-Cookie。
func cookieValue(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ★ TestUIHTTPNeverLeaksSecrets 四类访问者的响应体里都**不得**出现配置密钥。
//
// 这条是用户报障 ③ 的直接判据：改造前"本机访问 /ui"会把 api_key 明文注入
// 页面（`window.__WB2API_KEY__ = "mk-…"`），于是"打开页面"就等于拿到钥匙。
//
// ⚠ 唯一允许携带 admin_token 的地方是 `?token=` 那条 302 的 **Set-Cookie 头**
// （浏览器必须拿到它才能继续用管理台）。所以这里只查**响应体**，不查头 ——
// 而"头部与体部的区别"正是设计的一部分：body 会进缓存、进日志、进截图。
func TestUIHTTPNeverLeaksSecrets(t *testing.T) {
	f := newUIFixture(t, true, true)
	good := f.sessionCookie(t)

	cases := []struct {
		name    string
		target  string
		cookies []*http.Cookie
	}{
		{"未认证", "/ui", nil},
		{"令牌查询参数（合法）", "/ui?token=" + url.QueryEscape(testAdminToken), nil},
		{"令牌查询参数（错误）", "/ui?token=wrong-token-xyz", nil},
		{"令牌 cookie（合法）", "/ui", []*http.Cookie{{Name: account.AdminTokenCookie, Value: testAdminToken}}},
		{"令牌 cookie（陈旧）", "/ui", []*http.Cookie{{Name: account.AdminTokenCookie, Value: "stale-token"}}},
		{"会话 cookie（合法）", "/ui", []*http.Cookie{good}},
	}
	for _, tc := range cases {
		rec := f.get(t, tc.target, tc.cookies...)
		body := rec.Body.String()
		if strings.Contains(body, testAPIKey) {
			t.Errorf("★ %s：响应体里出现了配置的 **api_key** ——\n"+
				"改造前正是这样注入的（window.__WB2API_KEY__ = \"…\"），"+
				"于是打开页面就等于拿到 /v1 钥匙。", tc.name)
		}
		if strings.Contains(body, testAdminToken) {
			t.Errorf("★ %s：响应体里出现了配置的 **admin_token** ——\n"+
				"令牌只应存在于 Set-Cookie 头（以及 302 之后的浏览器 cookie 里），"+
				"不得写进 HTML（HTML 会进缓存/日志/截图）。", tc.name)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s：Cache-Control 应为 no-store（页面按身份分叉），实际 %q", tc.name, cc)
		}
	}
}

// 未认证 ⇒ 拿到**登录页**，而不是控制台外壳。
func TestUILoginPageWhenUnauthenticated(t *testing.T) {
	f := newUIFixture(t, true, false)
	rec := f.get(t, "/ui")
	if rec.Code != http.StatusOK {
		t.Fatalf("未认证访问 /ui 应当 200（给一张能填账号口令的页面），实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, loginPageMarker) {
		t.Errorf("未认证时应当返回登录页（找标记 %s），实际响应里没有它", loginPageMarker)
	}
	if !strings.Contains(body, account.PathLogin) {
		t.Errorf("登录页里应当有 %s 这个端点（表单要打它）", account.PathLogin)
	}
	if strings.Contains(body, consolePageMarker) {
		t.Error("未认证时**不该**返回控制台外壳 —— 外壳会立刻发一堆取数请求，用户看到的是一屏 401")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type 应为 text/html，实际 %q", ct)
	}
}

// 会话说得通 ⇒ 控制台外壳。
func TestUIConsoleWhenSessionValid(t *testing.T) {
	f := newUIFixture(t, true, false)
	rec := f.get(t, "/ui", f.sessionCookie(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("带有效会话访问 /ui 应当 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, consolePageMarker) {
		t.Error("带有效会话时应当返回控制台外壳（找 window.__WB2API_KEY__ 那处基准）")
	}
	if strings.Contains(body, loginPageMarker) {
		t.Error("已登录不该再看到登录页")
	}
}

// ★ 会话被撤销（登出/改口令/禁用）⇒ 退回登录页。
//
// 判据必须走 SessionStore.Lookup（四道校验），自己读 cookie 查表会漏掉它们
// —— 表现成"登出之后旧 cookie 还能打开控制台"。
func TestUILoginPageWhenSessionRevoked(t *testing.T) {
	f := newUIFixture(t, true, false)
	c := f.sessionCookie(t)
	if rec := f.get(t, "/ui", c); !strings.Contains(rec.Body.String(), consolePageMarker) {
		t.Fatal("前置条件不成立：刚签发的会话应当能打开控制台")
	}
	f.sessions.Revoke(c.Value)
	rec := f.get(t, "/ui", c)
	if !strings.Contains(rec.Body.String(), loginPageMarker) {
		t.Error("会话已撤销 ⇒ 必须退回登录页（旧 cookie 不得继续打开控制台）")
	}
}

// ★ `?token=<admin_token>` ⇒ 302 回干净的 /ui + 下发 HttpOnly 令牌 cookie。
//
// 换手的三个理由：① 地址栏没法设请求头，令牌只能走查询参数；
// ② 令牌留在 URL 会顺着历史/Referer/代理日志漏出去；
// ③ 换到 cookie 之后才可能是 HttpOnly（JS 读不到 ⇒ XSS 偷不走）。
func TestUITokenQuerySetsCookieAndRedirects(t *testing.T) {
	f := newUIFixture(t, true, false)
	rec := f.get(t, "/ui?token="+url.QueryEscape(testAdminToken))

	if rec.Code != http.StatusFound {
		t.Fatalf("?token= 应当 302（换成干净 URL），实际 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui" {
		t.Errorf("302 目标应当是 /ui（不带令牌），实际 %q", loc)
	}
	c := cookieValue(rec, account.AdminTokenCookie)
	if c == nil {
		t.Fatal("没有下发令牌 cookie —— 换手没完成，页面此后每个请求都会 401")
	}
	if c.Value != testAdminToken {
		t.Error("cookie 值应当是刚校验通过的那把令牌")
	}
	if !c.HttpOnly {
		t.Error("★ 令牌 cookie 必须 HttpOnly（否则 XSS 能读走它）")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite 应为 Lax（挡 CSRF），实际 %v", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path 应为 /（/ui 与 /admin/* 都要带），实际 %q", c.Path)
	}
	if c.MaxAge <= 0 {
		t.Errorf("MaxAge 应为正数（有界而不是会话级），实际 %d", c.MaxAge)
	}
	if strings.Contains(rec.Body.String(), testAdminToken) {
		t.Error("302 的响应体里不该出现令牌")
	}
}

// ★ 错误令牌**不是**判定 oracle：与"没带令牌"的响应**完全相同**。
//
// 否则这个端点就成了"给我一个令牌，我告诉你对不对"的离线试错口。
func TestUIBadTokenQueryIsNotAnOracle(t *testing.T) {
	f := newUIFixture(t, true, false)
	plain := f.get(t, "/ui")
	bad := f.get(t, "/ui?token=wrong-token-xyz")

	if bad.Code != http.StatusOK {
		t.Errorf("错误令牌应当走「未认证」那条路（200 登录页），实际 %d", bad.Code)
	}
	if cookieValue(bad, account.AdminTokenCookie) != nil {
		t.Error("错误令牌不该下发任何 cookie")
	}
	if plain.Body.String() != bad.Body.String() {
		t.Error("★ 带错误令牌与不带令牌的响应必须**逐字节相同** ——\n" +
			"任何差异都会让攻击者能用它离线试令牌（判定 oracle）。")
	}
}

// 令牌 cookie 能直接进控制台（换手之后的正常状态）。
func TestUITokenCookieAuthorizesConsole(t *testing.T) {
	f := newUIFixture(t, true, false)
	rec := f.get(t, "/ui", &http.Cookie{Name: account.AdminTokenCookie, Value: testAdminToken})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), consolePageMarker) {
		t.Errorf("带合法令牌 cookie 应当直接进控制台，实际 %d", rec.Code)
	}
}

// ★ 轮换令牌后，浏览器里那份旧 cookie 立即失效并被清掉。
//
// 这条同时守住"轮换立即生效"（用户报障 ②）：判定每次都读**当前**令牌，
// 所以旧副本不需要任何额外清理动作就不匹配了。
func TestUIStaleTokenCookieIsRejectedAndCleared(t *testing.T) {
	f := newUIFixture(t, true, false)
	const rotated = "ROTATED-ADMIN-TOKEN-CCC333"
	f.h.SetAdminToken(rotated)

	old := f.get(t, "/ui", &http.Cookie{Name: account.AdminTokenCookie, Value: testAdminToken})
	if !strings.Contains(old.Body.String(), loginPageMarker) {
		t.Error("轮换后旧令牌 cookie 不得再打开控制台")
	}
	if c := cookieValue(old, account.AdminTokenCookie); c == nil || c.MaxAge >= 0 {
		t.Error("应当顺手清掉失效的令牌 cookie（MaxAge<0），否则浏览器每次请求都白带一个旧副本")
	}
	// 新令牌照常可用
	fresh := f.get(t, "/ui", &http.Cookie{Name: account.AdminTokenCookie, Value: rotated})
	if !strings.Contains(fresh.Body.String(), consolePageMarker) {
		t.Error("轮换后的新令牌 cookie 应当立刻可用")
	}
}

// 登录页的两个标志位按真实状态渲染（bootstrap / 注册开关）。
func TestUILoginPageFlags(t *testing.T) {
	boolFlag := func(rec *httptest.ResponseRecorder, key string) string {
		re := regexp.MustCompile(key + `:\s*(true|false)`)
		if m := re.FindStringSubmatch(rec.Body.String()); m != nil {
			return m[1]
		}
		return ""
	}

	// ① 账号库**有账号** ⇒ bootstrap=false
	f1 := newUIFixture(t, true, false)
	if got := boolFlag(f1.get(t, "/ui"), "bootstrap"); got != "false" {
		t.Errorf("账号库非空时 bootstrap 应为 false，实际 %q", got)
	}

	// ② ★ 账号体系已接线但**一个账号都没有**（首次部署）⇒ bootstrap=true
	f2 := newUIEmptyAccountsFixture(t, false)
	rec := f2.get(t, "/ui")
	if got := boolFlag(rec, "bootstrap"); got != "true" {
		t.Errorf("★ 账号库为空时 bootstrap 应为 true（页面给出「创建第一个管理员」），实际 %q", got)
	}
	if got := boolFlag(rec, "register"); got != "false" {
		t.Errorf("注册开关关闭时 register 应为 false，实际 %q", got)
	}

	// ③ 开放注册 ⇒ register=true
	f3 := newUIFixture(t, true, true)
	if got := boolFlag(f3.get(t, "/ui"), "register"); got != "true" {
		t.Errorf("开放注册时 register 应为 true，实际 %q", got)
	}

	// ④ 没接账号体系（Accounts=nil）⇒ bootstrap **必须**为 false。
	//
	// 这不是"没账号所以该显示创建入口"：没接线时 `/admin/auth/register`
	// 根本没挂载，页面给一个"创建第一个管理员"的按钮点了必然 404。
	// 宁可不显示入口，也不要显示一个点了就错的按钮。
	f4 := newUIFixture(t, false, false)
	if got := boolFlag(f4.get(t, "/ui"), "bootstrap"); got != "false" {
		t.Errorf("没接账号体系时 bootstrap 应为 false（创建入口此时不可用），实际 %q", got)
	}

	// ⑤ 两个占位符都不得残留（否则页面上的标志位是字符串而非布尔）
	body := rec.Body.String()
	for _, ph := range []string{loginBootstrapPlaceholder, loginRegisterPlaceholder} {
		if strings.Contains(body, `"`+ph+`"`) {
			t.Errorf("登录页里仍残留占位符字面量 %s —— 标志位没被替换成布尔", ph)
		}
	}
}

// newUIEmptyAccountsFixture 账号体系已接线但账号库为空（首次部署的真实形态）。
//
// 与 newUIFixture(t, false, …) 的区别：那个是"压根没接线"（Accounts=nil），
// 两者在登录页上应给出**不同**的标志位 —— 前者能创建管理员，后者不能。
func newUIEmptyAccountsFixture(t *testing.T, registerOpen bool) *uiFixture {
	t.Helper()
	dir := t.TempDir()
	accounts, err := account.New(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sessions, err := account.NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	return &uiFixture{
		h: NewHandler(Config{
			Pool:         pool.New(filepath.Join(dir, "pool-state.json")),
			APIKey:       testAPIKey,
			AdminToken:   testAdminToken,
			CookieSecure: account.CookieSecureNever,
			RegisterOpen: func() bool { return registerOpen },
			Accounts:     accounts,
			Sessions:     sessions,
		}),
		accounts: accounts,
		sessions: sessions,
	}
}
