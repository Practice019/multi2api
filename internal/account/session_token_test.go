package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// session_token_test.go —— `session` 端点必须能回答"我是谁"的**两种**身份。
//
// # 这组测试挡的是一个真 bug（T12 回归里挖出来的）
//
// `?token=<admin_token>` 经 `/ui` 换成令牌 Cookie 后 302 到干净的 /ui（见
// internal/server/webui.go）。那条路**能**进管理面，但控制台首屏问的是
// `GET /admin/auth/session` —— 它原先只认会话 Cookie ⇒ 令牌用户拿到 401
// ⇒ 前端判"会话失效" ⇒ `location.replace('/ui')` ⇒ 又拿到控制台外壳 ⇒
// **无限重载**。症状："用带令牌的链接打开控制台，页面一直在闪"。
//
// 60 多个开 `/ui` 的既有探针也一起红，而它们绝大多数与登录无关 ——
// 表面看像"控制台坏了"，根因却是这一个端点的判据少了一条。

// tokenFixture 带令牌校验的端点夹具。
func tokenFixture(t *testing.T, adminToken string) *HTTP {
	t.Helper()
	dir := t.TempDir()
	acc, err := New(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sess, err := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	// 用可变变量模拟"运行时轮换"：回调每次读当前值（与 admin.Handler 同一形状）。
	live := adminToken
	h, err := NewHTTP(HTTPConfig{
		Accounts:        acc,
		Sessions:        sess,
		RegisterEnabled: true,
		AdminTokenOK: func(cred string) bool {
			if live == "" {
				return false
			}
			return cred == live
		},
	})
	if err != nil {
		t.Fatalf("端点: %v", err)
	}
	return h
}

// doReq 发一个请求并解出回执（返回状态码 + 解析后的对象）。
//
// ⚠ 是**普通函数**而不是 `func (h *HTTP) ...`：给被测类型挂方法会让"测试辅助"
// 看起来像产品 API 的一部分（本包已有 `HTTP.ServeHTTP` 这类真接口）。
func doReq(t *testing.T, h *HTTP, method, path string, cookies map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// ★ 令牌 Cookie ⇒ 200 且 kind=token、user=null（不再把令牌用户踢去登录页）。
func TestSessionAcceptsAdminTokenCookie(t *testing.T) {
	h := tokenFixture(t, "tok-abc")

	code, body := doReq(t, h, http.MethodGet, PathSession, map[string]string{AdminTokenCookie: "tok-abc"})
	if code != http.StatusOK {
		t.Fatalf("★ 带有效令牌 Cookie 问 /admin/auth/session 必须 200（403/401 会让控制台把令牌用户踢回登录页并无限重载），实际 %d", code)
	}
	if got := body["kind"]; got != "token" {
		t.Errorf("kind 应当是 token（前端据此显示「管理令牌」并隐藏改密码），实际 %v", got)
	}
	if u, ok := body["user"]; !ok || u != nil {
		t.Errorf("令牌身份没有账号主体 ⇒ user 必须是 null（给个假用户名会让前端去调改密码之类），实际 %v", u)
	}
}

// 令牌可以走三种载体（Bearer / 查询参数 / Cookie），session 端点三条都要认
// —— 只认 Cookie 的话，脚本用 Bearer 问"我是谁"仍会 401，而管理面明明放行。
func TestSessionAcceptsTokenFromAllCarriers(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		hdr  bool
	}{
		{"Bearer 头", PathSession, true},
		{"查询参数", PathSession + "?token=tok-abc", false},
	} {
		h := tokenFixture(t, "tok-abc")
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.hdr {
			req.Header.Set("Authorization", "Bearer tok-abc")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s：应当 200，实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// 无效令牌 / 空令牌 / 没有 AdminTokenOK 接缝 ⇒ 仍是 401（不能因为加了这条路
// 就把"没登录"也放进来）。
func TestSessionRejectsBadOrAbsentToken(t *testing.T) {
	h := tokenFixture(t, "tok-abc")
	cases := []struct {
		name    string
		cookies map[string]string
	}{
		{"错令牌", map[string]string{AdminTokenCookie: "tok-WRONG"}},
		{"啥都不带", nil},
		{"空值令牌", map[string]string{AdminTokenCookie: ""}},
	}
	for _, tc := range cases {
		code, body := doReq(t, h, http.MethodGet, PathSession, tc.cookies)
		if code != http.StatusUnauthorized {
			t.Errorf("%s：应当 401，实际 %d", tc.name, code)
			continue
		}
		got := body["code"]
		if got == nil {
			// 嵌套信封
			if e, ok := body["error"].(map[string]any); ok {
				got = e["code"]
			}
		}
		if got != "auth_required" {
			t.Errorf("%s：401 的 code 应当是 auth_required（前端据此'显示登录页'而不是'会话过期'），实际 %v", tc.name, got)
		}
	}

	// 没有接缝的部署（老配置：只有会话）⇒ 令牌不算身份。零值行为必须与 T3 时一致。
	dir := t.TempDir()
	acc, _ := New(filepath.Join(dir, "accounts.json"))
	sess, _ := NewSessionStore(filepath.Join(dir, "sessions.json"))
	h2, err := NewHTTP(HTTPConfig{Accounts: acc, Sessions: sess, RegisterEnabled: true})
	if err != nil {
		t.Fatalf("端点: %v", err)
	}
	code, _ := doReq(t, h2, http.MethodGet, PathSession, map[string]string{AdminTokenCookie: "tok-abc"})
	if code != http.StatusUnauthorized {
		t.Errorf("没接 AdminTokenOK 时不认令牌身份（应当 401），实际 %d", code)
	}
}

// 令牌轮换立即生效（session 端点走的必须是**当前**值，不能是构造期快照）。
func TestSessionTokenFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	acc, _ := New(filepath.Join(dir, "accounts.json"))
	sess, _ := NewSessionStore(filepath.Join(dir, "sessions.json"))
	var live = "old-token"
	h, err := NewHTTP(HTTPConfig{
		Accounts: acc, Sessions: sess, RegisterEnabled: true,
		AdminTokenOK: func(cred string) bool { return cred != "" && cred == live },
	})
	if err != nil {
		t.Fatalf("端点: %v", err)
	}
	if code, _ := doReq(t, h, http.MethodGet, PathSession, map[string]string{AdminTokenCookie: "old-token"}); code != 200 {
		t.Fatalf("轮换前旧令牌应当 200，实际 %d", code)
	}
	live = "new-token" // 运行时轮换
	if code, _ := doReq(t, h, http.MethodGet, PathSession, map[string]string{AdminTokenCookie: "new-token"}); code != 200 {
		t.Errorf("轮换后新令牌应当立即 200（快照实现会在这里 401 ⇒「轮换要重启」复发），实际 %d", code)
	}
	if code, _ := doReq(t, h, http.MethodGet, PathSession, map[string]string{AdminTokenCookie: "old-token"}); code != 401 {
		t.Errorf("轮换后旧令牌应当立即 401，实际 %d", code)
	}
}

// 登出要**同时**清两个 Cookie：令牌用户没有会话可撤销，只清会话 Cookie 的话
// 点「登出」等于什么都没发生（页面仍是登录态）。
func TestLogoutClearsBothCookies(t *testing.T) {
	h := tokenFixture(t, "tok-abc")
	req := httptest.NewRequest(http.MethodPost, PathLogout, nil)
	req.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: "tok-abc"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登出应当 200，实际 %d", rec.Code)
	}
	var clearedSID, clearedTok bool
	for _, c := range rec.Result().Cookies() {
		if c.Value == "" || c.MaxAge < 0 {
			switch c.Name {
			case CookieName:
				clearedSID = true
			case AdminTokenCookie:
				clearedTok = true
			}
		}
	}
	if !clearedSID {
		t.Error("登出必须清会话 Cookie")
	}
	if !clearedTok {
		t.Error("★ 登出必须也清令牌 Cookie（用 ?token= 进来的浏览器没有会话，只清会话 Cookie 等于登出无效）")
	}
}

// 会话身份的回执要带 kind=session（前端靠 kind 分流，缺了就当令牌处理会隐藏改密码）。
func TestSessionReportsKindSession(t *testing.T) {
	dir := t.TempDir()
	acc, err := New(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sess, err := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	h, err := NewHTTP(HTTPConfig{Accounts: acc, Sessions: sess, RegisterEnabled: true})
	if err != nil {
		t.Fatalf("端点: %v", err)
	}
	u, err := acc.CreateBootstrap("alice", "alice-password-001")
	if err != nil {
		t.Fatalf("建号: %v", err)
	}
	s, err := sess.Issue(u, "127.0.0.1", "probe")
	if err != nil {
		t.Fatalf("签发: %v", err)
	}
	code, body := doReq(t, h, http.MethodGet, PathSession, map[string]string{CookieName: s.SID})
	if code != http.StatusOK {
		t.Fatalf("会话应当 200，实际 %d", code)
	}
	if got := body["kind"]; got != "session" {
		t.Errorf("kind 应当是 session，实际 %v", got)
	}
	um, _ := body["user"].(map[string]any)
	if um == nil || um["username"] != "alice" {
		t.Errorf("会话身份要带 user（前端显示用户名），实际 %v", body["user"])
	}
	if strings.Contains(recBody(body), "password") {
		t.Error("回执里不得出现任何口令相关字段")
	}
}

// recBody 把回执再序列化一次，用于"里面不该有 X"这类粗判。
func recBody(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}
