// gate_test.go 管理面门禁的守卫。
//
// # 这组测试是七件事的判据（用户报障「填错 admin token 居然能进 UI」）
//
//	① 无凭据（**含本机直连**）  → 401（改造前本机无条件放行，那就是报障的根因）
//	② 拿 **api_key**            → 401（权限倒挂：对话钥匙不得解锁管理面）
//	③ 拿 admin_token            → 放行（Bearer 与 ?token= 两条）
//	④ 浏览器会话                → 放行；★ 非管理员的历史记录**不构成身份**（401）
//	⑤ 会话失效（改口令/被撤）    → 401
//	⑥ 一个凭据都没配            → 一律 401（不是"回落用 api_key"）
//	⑦ 轮换令牌                  → 立即生效，旧令牌立刻失效
//
// # 为什么每条都要有
//
// 这类缺陷的形态是"**静默放行**"——它不报错、不影响正常使用，
// 只在有人恶意访问时才显形。少一条断言就少一层可被静默绕过的前提。
//
// ⚠ 2026-10-19 用户新口径：登录系统收成单账号管理员，"成员"角色已取消。
// 原先的 ④ 是"按角色分流（管理员全部 / 成员白名单 memberRules）"，
// 那张白名单连同它的测试**已删**（见 admin.go 的 canAccess 注释）——
// 换成了更强的 ④：盘上的成员历史记录连**身份**都拿不到。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/account"
)

// gateFixture 门禁测试夹具：一个最小 Handler + 两把不同的钥匙 + 可选账号体系。
type gateFixture struct {
	h          *Handler
	apiKey     string
	adminToken string
	accounts   *account.Store
	sessions   *account.SessionStore
	admin      *account.User // 唯一那个管理员
	legacy     *account.User // 盘上的**成员历史记录**（改造前留下的，不构成身份）
}

// newGateFixture 造夹具。withAccounts=true 时另建账号体系。
//
// ⚠ 单账号口径：管理员走 CreateBootstrap（唯一建号路径），而那条
// "成员历史记录"是**手写进账号文件的** —— 没有 API 能造出它（那正是目标态 1），
// 而目标态 3 要测的恰恰是"盘上有这么一条"的旧数据形态。
func newGateFixture(t *testing.T, withAccounts bool) *gateFixture {
	t.Helper()
	const (
		apiKey     = "API-KEY-for-chat-only"
		adminToken = "ADMIN-TOKEN-for-console"
	)
	cfg := Config{
		ListensLoopback: true,
		APIKey:          apiKey,
		AdminToken:      adminToken,
	}
	f := &gateFixture{apiKey: apiKey, adminToken: adminToken}
	if withAccounts {
		dir := t.TempDir()
		var err error
		accPath := filepath.Join(dir, "accounts.json")
		seedLegacyMemberFile(t, accPath, "boss", "boss-password-001", "staff", "staff-password-002")
		f.accounts, err = account.New(accPath)
		if err != nil {
			t.Fatalf("账号库: %v", err)
		}
		f.sessions, err = account.NewSessionStore(filepath.Join(dir, "sessions.json"))
		if err != nil {
			t.Fatalf("会话库: %v", err)
		}
		if f.admin = f.accounts.ByUsername("boss"); f.admin == nil {
			t.Fatal("前置：管理员记录应当被加载")
		}
		if f.legacy = f.accounts.ByUsername("staff"); f.legacy == nil {
			t.Fatal("前置：成员历史记录应当被加载")
		}
		cfg.Accounts = f.accounts
		cfg.Sessions = f.sessions
	}
	f.h = New(cfg)
	// 测试专用端点：把「门禁判据」与「端点内部实现」解耦。
	//
	// /admin/accounts 这类真实端点在夹具里会因为没有 Pool 而 panic ——
	// 那是**端点**的前置条件（它要读账号池），不是门禁的判据。
	// 挂一个最小处理器后，200/403 只反映门禁的判定结果，
	// 于是角色分流的断言不会被端点内部的接线问题干扰。
	f.h.mux.HandleFunc("/admin/testonly", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return f
}

// seedLegacyMemberFile 手写一个账号文件：一个管理员 + 一条**成员**历史记录。
//
// 口令用真 bcrypt 哈希（account.HashPassword）—— 这样"口令对但角色不是
// 管理员"这条反例在 HTTP 层也真的走到角色判定，而不是被口令校验挡下。
func seedLegacyMemberFile(t *testing.T, path, adminName, adminPass, memberName, memberPass string) {
	t.Helper()
	row := func(name, plain string, role int) map[string]any {
		h, err := account.HashPassword(plain)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		return map[string]any{
			"id":            "u-" + name,
			"username":      name,
			"display_name":  name,
			"password_hash": h,
			"role":          role,
			"enabled":       true,
			"auth_version":  1,
			"created_at":    "2026-01-01T00:00:00Z",
		}
	}
	raw, err := json.MarshalIndent([]map[string]any{
		row(adminName, adminPass, account.RoleAdmin),
		row(memberName, memberPass, account.RoleMember),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写账号库: %v", err)
	}
}

// probe 打一个**只读**的管理端点，返回状态码与响应体。
//
// 传 path="" 时用 /admin/ui/manifest（成员白名单内的端点，不需要任何接线）；
// 要测"角色不够"就用 /admin/accounts（上游账号池，管理员专属）。
func (f *gateFixture) probe(path, remoteAddr, bearer string, extraHeaders map[string]string) (int, string) {
	if path == "" {
		path = "/admin/ui/manifest"
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remoteAddr
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// probeAs 用某个账号的会话 cookie 打请求。
func (f *gateFixture) probeAs(t *testing.T, u *account.User, path string) (int, string) {
	t.Helper()
	sess, err := f.sessions.Issue(u, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("签发会话: %v", err)
	}
	return f.probeWithSID(path, sess.SID)
}

func (f *gateFixture) probeWithSID(path, sid string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.AddCookie(&http.Cookie{Name: account.CookieName, Value: sid})
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ① ★ 无凭据 ⇒ 401 —— **包括来自回环地址的请求**。
//
// 这是用户报障「填错 admin token 居然能进 UI」的正解：改造前
// `localExempt()` 对"监听回环 + 上一跳回环 + 无转发头"的请求**无条件放行**，
// 于是本机任何进程（以及浏览器里任意页面发起的请求）都是管理员，
// 连 /admin/apikeys 都能读到钥匙（掩码）。
func TestGate_NoCredentialDenied(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remoteAddr string
		headers    map[string]string
	}{
		{"本机直连", "127.0.0.1:5555", nil},
		{"本机 IPv6", "[::1]:5555", nil},
		{"远程", "203.0.113.9:5555", nil},
		{"经反代转发", "127.0.0.1:5555", map[string]string{"X-Forwarded-For": "203.0.113.9"}},
	} {
		f := newGateFixture(t, false)
		code, body := f.probe("", tc.remoteAddr, "", tc.headers)
		if code != http.StatusUnauthorized {
			t.Errorf("%s 无凭据应当 401，实际 %d ——\n"+
				"回环地址**不是凭据**。改造前这里 200（免令牌），"+
				"于是「能连上来」被当成了「有权限」。", tc.name, code)
		}
		if !containsStr(body, "auth_required") {
			t.Errorf("%s 的 401 应当带 code=auth_required（前端据此跳登录页），实际: %s", tc.name, body)
		}
	}
}

// ② 拿 **api_key** ⇒ 仍然拒绝（权限倒挂不得复活）。
func TestGate_APIKeyDenied(t *testing.T) {
	f := newGateFixture(t, false)
	code, body := f.probe("", "203.0.113.9:5555", f.apiKey, nil)
	if code != http.StatusUnauthorized {
		t.Errorf("拿 api_key 应当 401，实际 %d ——\n"+
			"api_key 是**对话接口**的凭据（可能发给第三方客户端/机器人），"+
			"它绝不能解锁管理台。改造前它们共用一把钥匙，那正是用户报的"+
			"「没授权却能改设置」的根因之一。", code)
	}
	if containsStr(body, f.apiKey) {
		t.Error("★ 拒绝响应里不能回显任何凭据")
	}
}

// ③ 拿 admin_token ⇒ 放行（远程也一样：它是机器凭据，与来源地址无关）。
func TestGate_AdminTokenAllowed(t *testing.T) {
	f := newGateFixture(t, false)
	if code, _ := f.probe("", "203.0.113.9:5555", f.adminToken, nil); code != http.StatusOK {
		t.Errorf("拿 admin_token 应当放行，实际 %d ——"+
			"它是脚本/CI 用的机器凭据，坏了等于没有远程管理面。", code)
	}
}

// ③b `?token=` 查询参数同样可用（浏览器地址栏无法设请求头）。
func TestGate_QueryTokenAccepted(t *testing.T) {
	f := newGateFixture(t, false)
	req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest?token="+f.adminToken, nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("?token=<admin_token> 应当放行，实际 %d ——"+
			"浏览器地址栏没办法自定义请求头，查询参数是它唯一能带凭据的地方。", rec.Code)
	}
}

// ④ 浏览器会话 ⇒ 放行；★ 成员历史记录**不构成身份**（401，不是 403）。
//
// ⚠ 语义变更（2026-10-19 单账号口径）：这条测试原先叫 TestGate_SessionRoleSplit，
// 断言"管理员全部放行 / 成员按 memberRules 白名单分流（白名单内的 200、
// 管理员专属的 403 forbidden_role）"。
//
// 那个面**已经不存在**：用户新口径是"只要一个账号，用户其实可以不用"，
// memberRules 白名单连同 forbidden_role 的**可达路径**一起删了
// （见 admin.go 的 canAccess）⇒ 成员既不能进白名单，也不会撞 403，
// 它在**身份**这一层就结束了（401 auth_required）。
//
// 所以断言换成了语义相反、但更强的一组：
//
//	① 管理员会话：白名单内与管理员专属端点**都**能进（不再有"成员能进的子集"）
//	② 盘上的成员历史记录：连一条有效会话都换不到身份 ⇒ 401 auth_required
//	③ 那条会话对**任何**管理端点都是 401（包括改造前白名单里的那几个）
func TestGate_SingleAdminSession(t *testing.T) {
	f := newGateFixture(t, true)

	// ① 管理员：所有管理端点都能进
	for _, path := range []string{"/admin/ui/manifest", "/admin/stats", "/admin/testonly"} {
		if code, body := f.probeAs(t, f.admin, path); code != http.StatusOK {
			t.Errorf("管理员访问 %s 应当 200，实际 %d: %s", path, code, body)
		}
	}

	// ②③ 成员历史记录：会话有效也换不到身份 ⇒ 一律 401
	for _, path := range []string{
		"/admin/ui/manifest", // 改造前它在这个白名单里
		"/admin/apikeys",     // 同上
		"/admin/stats",
		"/admin/accounts",
		"/admin/testonly",
	} {
		code, body := f.probeAs(t, f.legacy, path)
		if code != http.StatusUnauthorized {
			t.Errorf("★ 成员历史记录访问 %s 应当 401（成员账号已取消，它不构成身份），实际 %d: %s",
				path, code, body)
		}
		if !containsStr(body, "auth_required") {
			t.Errorf("★ 应当是 auth_required（=「没有有效身份」），不是角色不足；实际: %s", body)
		}
	}
}

// ④b 非管理员身份的会话**默认拒绝**：任何路径都进不去（不是"只挡白名单外的"）。
//
// ⚠ 语义变更：这条原先叫 TestGate_MemberWhitelistIsDenyByDefault，钉的是
// "memberRules 是白名单不是黑名单（漏登记只会让成员进不去，不会越权）"。
// 白名单没了，但**默认拒绝**这条判据必须留下来 —— 它现在是"盘上的成员记录
// 在任何路径上都不构成身份"，覆盖面比原版更宽（原版只查白名单外的路径）。
func TestGate_NonAdminSessionDeniedEverywhere(t *testing.T) {
	f := newGateFixture(t, true)
	for _, path := range []string{
		"/admin/ui/manifest",
		"/admin/apikeys",
		"/admin/accounts/reload",
		"/admin/accounts/checkin/all",
		"/admin/settings/save",
		"/admin/some/future/endpoint",
	} {
		if code, _ := f.probeAs(t, f.legacy, path); code != http.StatusUnauthorized {
			t.Errorf("非管理员身份访问 %s 应当 401（默认拒绝），实际 %d", path, code)
		}
	}
}

// ⑤ 会话失效（口令变更/被撤销）⇒ 401，不能拿旧 cookie 进管理面。
func TestGate_StaleSessionRejected(t *testing.T) {
	f := newGateFixture(t, true)
	sess, err := f.sessions.Issue(f.admin, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.probeWithSID("/admin/ui/manifest", sess.SID); code != http.StatusOK {
		t.Fatalf("前置条件不成立：刚签发的会话应当能进，实际 %d", code)
	}
	// 改口令 ⇒ 账号 AuthVersion +1 ⇒ 所有旧会话失效
	if _, err := f.accounts.SetPassword(f.admin.ID, "admin-new-password-003"); err != nil {
		t.Fatal(err)
	}
	if code, body := f.probeWithSID("/admin/ui/manifest", sess.SID); code != http.StatusUnauthorized {
		t.Errorf("★ 口令变更后旧会话必须 401，实际 %d: %s ——\n"+
			"门禁必须走 SessionStore.Lookup 的四道校验（过期/撤销/禁用/AuthVersion），"+
			"自己读 cookie 查表会漏掉全部四道。", code, body)
	}
	// 伪造 SID
	if code, _ := f.probeWithSID("/admin/ui/manifest", "deadbeef"); code != http.StatusUnauthorized {
		t.Errorf("伪造 SID 应当 401，实际 %d", code)
	}
}

// ⑥ 一个凭据都没配 ⇒ 一律 401（安全默认），不得回落到 api_key。
func TestGate_NothingConfiguredDeniesAll(t *testing.T) {
	h := New(Config{ListensLoopback: true, APIKey: "some-api-key"})
	for _, addr := range []string{"127.0.0.1:5555", "203.0.113.9:5555"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("没配任何凭据时 %s 应当 401，实际 %d ——\n"+
				"没配就是**没有管理面**（用户可以去 /ui 建第一个管理员），"+
				"而不是回落到 api_key（那正是要修掉的权限倒挂）。", addr, rec.Code)
		}
	}
	// 拿 api_key 冒充门禁令牌也要被拒（不能有隐式回落）
	req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Authorization", "Bearer some-api-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未配任何凭据时拿 api_key 应当 401，实际 %d", rec.Code)
	}
	// 提示要指向"去 /ui 建管理员"这个可执行动作
	if !containsStr(rec.Body.String(), "/ui") {
		t.Errorf("一个凭据都没配时，拒绝提示应当指向 /ui（去创建第一个管理员），实际: %s", rec.Body.String())
	}
}

// ⑦ ⚠ 轮换门禁令牌后**立即生效**（用户报障：轮换之后还要重启才有用）。
//
// 根因是内存里有两份副本：server.Handler.apiKey（有 SetAPIKey）与
// admin.Handler.cfg.APIKey（构造期快照）。门禁读后者 ⇒ 永不更新。
// 现在门禁读 SetAdminToken 维护的可变字段 —— 这条钉住那个行为。
func TestGate_RotationTakesEffectImmediately(t *testing.T) {
	f := newGateFixture(t, false)
	const newToken = "ROTATED-ADMIN-TOKEN"

	if code, _ := f.probe("", "203.0.113.9:5555", newToken, nil); code != http.StatusUnauthorized {
		t.Fatalf("前置条件不成立：新令牌在轮换前不该能用（实际 %d）", code)
	}
	f.h.SetAdminToken(newToken)
	if code, _ := f.probe("", "203.0.113.9:5555", newToken, nil); code != http.StatusOK {
		t.Errorf("轮换后新令牌应当**立即**可用，实际 %d ——\n"+
			"这正是用户报的「轮换之后还需要重启整个项目才有用」：\n"+
			"门禁若读构造期快照（cfg.AdminToken），SetAdminToken 就白调了。", code)
	}
	if code, _ := f.probe("", "203.0.113.9:5555", f.adminToken, nil); code != http.StatusUnauthorized {
		t.Errorf("轮换后**旧**令牌应当立即失效，实际 %d ——"+
			"旧令牌还能进意味着「轮换」没有真正吊销凭据。", code)
	}
}

// 门禁拒绝时返回的提示必须**可执行**（指出下一步做什么），而不是空话。
func TestGate_DenyHintIsActionable(t *testing.T) {
	// 情形一：配了令牌没带 ⇒ 提示要说清怎么带令牌、浏览器怎么登录
	f := newGateFixture(t, false)
	_, body := f.probe("", "203.0.113.9:5555", "", nil)
	for _, want := range []string{"admin_token", "/ui"} {
		if !containsStr(body, want) {
			t.Errorf("拒绝提示里应当说明「%s」（用户才知道下一步做什么），实际: %s", want, body)
		}
	}

	// 情形二：配了账号 ⇒ 提示要指向 /ui 登录
	f2 := newGateFixture(t, true)
	_, body2 := f2.probe("", "203.0.113.9:5555", "", nil)
	if !containsStr(body2, "/ui") {
		t.Errorf("有控制台账号时，提示应当指向 /ui 登录，实际: %s", body2)
	}

	// ⚠ 任何情形都不得再出现"本机免令牌"这类已经不成立的说明
	for _, body := range []string{body, body2} {
		if containsStr(body, "本机免令牌") || containsStr(body, "loopback") {
			t.Errorf("提示里不该再提「本机免令牌」（该路径已删除），实际: %s", body)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
