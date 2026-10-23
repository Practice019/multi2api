// groups_ui_live_test.go 真的把 /ui/groups 走一遍 HTTP（不只是"文件里有没有那段字"）。
//
// # 为什么还要这一条
//
// groups_ui_test.go 里的断言全是**读文件**：它证明源文件里有该有的东西，
// 证明不了"请求进来能拿到页面"。中间还隔着 embed、路由注册、身份判据三跳 ——
// 任一跳漏了，文件断言照样全绿，而用户打开 /ui/groups 是 404 或登录页循环。
//
// 这条用**带身份**的请求真发一次，断言 200 + text/html + 页面里有分组页的标记。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGroupsUIServesHTMLWithAuth 已认证 ⇒ 200 + 分组页 HTML。
//
// 身份用 admin_token 那条路（Config.AdminToken）：它是 uiIdentify 认的凭据之一，
// 不需要建账号库/会话库 —— 这条测试要验的是**页面能不能出来**，不是登录流程
// （后者有 verify_login_flow_e2e.js 那套）。
func TestGroupsUIServesHTMLWithAuth(t *testing.T) {
	const tok = "test-admin-token-groups-ui"
	h := NewHandler(Config{AdminToken: tok})

	req := httptest.NewRequest(http.MethodGet, "/ui/groups", nil)
	// 令牌 cookie 的形式由 account 包定义；这里用最直接的一条：
	// ?token= 会让 /ui 换 cookie 后 302 —— 但 /ui/groups 不做那次换手，
	// 所以直接带 Authorization 头（account.Identify 认它）。
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		body := rec.Body.String()
		if len(body) > 200 {
			body = body[:200]
		}
		t.Fatalf("已认证时 /ui/groups 应 200，实际 %d（body: %s）\n"+
			"⚠ 若这里是 302，说明身份判据没认 Authorization 头 —— "+
			"那会让「用令牌打开分组页」变成登录页循环", rec.Code, body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type 应为 text/html，实际 %q", ct)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("管理面不该被缓存，Cache-Control 实际 %q", got)
	}
	body := rec.Body.String()
	for _, needle := range []string{"分组管理", "id=\"rows\"", "checkConsistency"} {
		if !strings.Contains(body, needle) {
			t.Errorf("返回的 HTML 里找不到 %q —— 页面可能没被完整内嵌", needle)
		}
	}
}
