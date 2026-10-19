package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// change_password_test.go T9 验收（用户报障 ②：「轮换/改密之后必须重启」）。
//
// # 要挡的三种失效形态
//
//	① 改完口令**自己**被踢下线 —— 用户看到的就是"网页加载失败，必须重启"
//	   （报障原文）。这条最要紧，也是 new-api 专门有个
//	   AdvanceUserSessionAuthVersion 的原因。
//	② 改完口令**别人**还进得来 —— 安全侧的反面：报障修好了但洞开了。
//	③ 不验旧口令也能改 —— 拿到会话 cookie 的人可以直接把账号口令改掉，
//	   完成永久接管（会话是有有效期的，改口令没有）。
//
// 另外两条边界：新口令规则与注册同一份（不能在这里放宽）、新旧相同要明说
// （否则用户以为"把别处踢掉了"其实没有）。

// cpFixture 一套"两个浏览器会话 + 一个账号"的夹具。
type cpFixture struct {
	h    *HTTP
	acc  *Store
	sess *SessionStore
	user *User
	sidA string // 浏览器 A（执行改口令的那条）
	sidB string // 浏览器 B（其它设备）
}

func newCPFixture(t *testing.T) *cpFixture {
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
	h, err := NewHTTP(HTTPConfig{Accounts: acc, Sessions: sess, RegisterEnabled: true})
	if err != nil {
		t.Fatalf("端点: %v", err)
	}
	u, err := acc.Create("boss", "boss-password-001", RoleAdmin)
	if err != nil {
		t.Fatalf("建号: %v", err)
	}
	sa, err := sess.Issue(u, "127.0.0.1", "browser-A/1.0")
	if err != nil {
		t.Fatalf("签发 A: %v", err)
	}
	sb, err := sess.Issue(u, "10.0.0.9", "browser-B/1.0")
	if err != nil {
		t.Fatalf("签发 B: %v", err)
	}
	return &cpFixture{h: h, acc: acc, sess: sess, user: u, sidA: sa.SID, sidB: sb.SID}
}

func (f *cpFixture) post(t *testing.T, sid, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, PathPassword, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: sid})
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// live 用某个 SID 打一次 /admin/auth/session，报告"这条会话现在还活着吗"。
func (f *cpFixture) live(t *testing.T, sid string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, PathSession, nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: sid})
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ① ★ 改口令后：当前会话仍活，另一台设备失效。
func TestChangePasswordKeepsCurrentSession(t *testing.T) {
	f := newCPFixture(t)

	// 改之前两条都活
	if code, _ := f.live(t, f.sidA); code != http.StatusOK {
		t.Fatalf("改之前 A 会话应当有效，实际 %d", code)
	}
	if code, _ := f.live(t, f.sidB); code != http.StatusOK {
		t.Fatalf("改之前 B 会话应当有效，实际 %d", code)
	}

	rec := f.post(t, f.sidA,
		`{"current_password":"boss-password-001","new_password":"boss-password-002"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("改口令 = %d: %s", rec.Code, rec.Body.String())
	}
	var ok struct {
		OK            bool `json:"ok"`
		RevokedOthers int  `json:"revoked_others"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("回执不是 JSON: %v", err)
	}
	if !ok.OK {
		t.Errorf("回执 ok 应当为 true，实际 %s", rec.Body.String())
	}
	if ok.RevokedOthers != 1 {
		t.Errorf("应当报告 1 条其它会话失效，实际 %d", ok.RevokedOthers)
	}

	// ★★ 本条的核心：**改口令的那个人不掉线**
	code, body := f.live(t, f.sidA)
	if code != http.StatusOK {
		t.Errorf("★★ 改完口令后**当前**会话必须仍然有效（用户报障 ②：轮换/改密后要重启才能用），\n"+
			"实际 %d: %s\n"+
			"修法是 SetPassword 之后把**执行改动的这条**会话 AdvanceAuthVersion 到新版本"+
			"（抄 new-api model/user_session.go）。", code, body)
	}
	// ★ 另一台设备按设计失效
	codeB, bodyB := f.live(t, f.sidB)
	if codeB != http.StatusUnauthorized {
		t.Errorf("★ 改口令后**其它**设备的会话必须失效（否则改口令等于没改），实际 %d: %s",
			codeB, bodyB)
	}
	if !strings.Contains(bodyB, "session_revoked") {
		t.Errorf("★ 其它设备应当拿到 session_revoked（点明原因，用户才知道该重登），实际 %s", bodyB)
	}

	// 旧口令登不进来、新口令能登进来
	if _, err := f.acc.Authenticate("boss", "boss-password-001"); err == nil {
		t.Error("★ 旧口令还能登录 —— 改口令没真正生效")
	}
	if _, err := f.acc.Authenticate("boss", "boss-password-002"); err != nil {
		t.Errorf("新口令必须能登录，实际 %v", err)
	}
}

// ③ ★ 不验旧口令 / 验错旧口令 ⇒ 拒绝，且口令不变。
func TestChangePasswordRequiresCurrentPassword(t *testing.T) {
	f := newCPFixture(t)

	for _, c := range []struct {
		desc, body, wantCode string
	}{
		{"旧口令错误", `{"current_password":"wrong-password-999","new_password":"boss-password-002"}`, "bad_current_password"},
		{"旧口令为空", `{"current_password":"","new_password":"boss-password-002"}`, "bad_request"},
		{"旧口令缺失", `{"new_password":"boss-password-002"}`, "bad_request"},
	} {
		rec := f.post(t, f.sidA, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d，want 400: %s", c.desc, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), c.wantCode) {
			t.Errorf("%s: 应当报 code=%s，实际 %s", c.desc, c.wantCode, rec.Body.String())
		}
		// 口令没被改动
		if _, err := f.acc.Authenticate("boss", "boss-password-001"); err != nil {
			t.Errorf("%s: 口令不应当被改动（旧口令仍应能登录），实际 %v", c.desc, err)
		}
		// 两条会话都还活着（失败的尝试不该踢人）
		if code, _ := f.live(t, f.sidA); code != http.StatusOK {
			t.Errorf("%s: 失败的改口令尝试不该影响会话，A 实际 %d", c.desc, code)
		}
	}
}

// 未登录（无 cookie / 伪造 cookie）⇒ 401，且**不**回吐任何口令相关信息。
func TestChangePasswordRequiresSession(t *testing.T) {
	f := newCPFixture(t)

	for _, c := range []struct{ desc, sid string }{
		{"没带 cookie", ""},
		{"伪造 cookie", "deadbeef-not-a-session"},
	} {
		rec := f.post(t, c.sid, `{"current_password":"x","new_password":"boss-password-002"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d，want 401: %s", c.desc, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "auth_required") {
			t.Errorf("%s: 应当报 auth_required，实际 %s", c.desc, rec.Body.String())
		}
	}
}

// 新口令规则必须与注册**同一份**；新旧相同要明说。
func TestChangePasswordPolicyMatchesRegister(t *testing.T) {
	f := newCPFixture(t)

	// 太短：注册时也拦（validatePassword 是同一份实现）
	rec := f.post(t, f.sidA, `{"current_password":"boss-password-001","new_password":"short"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad_password") {
		t.Errorf("过短的新口令应当 400 bad_password，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 新旧相同：明说"未做改动"，而不是假报成功
	rec = f.post(t, f.sidA, `{"current_password":"boss-password-001","new_password":"boss-password-001"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "same_password") {
		t.Errorf("新旧相同应当 400 same_password，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 多余字段：解码纪律（DisallowUnknownFields）必须生效，
	// 否则前端把 new_password 写成 newPassword 会静默按"字段缺失"处理。
	rec = f.post(t, f.sidA,
		`{"current_password":"boss-password-001","new_password":"boss-password-002","role":10}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知字段应当 400（否则字段名写错会静默走默认值），实际 %d: %s", rec.Code, rec.Body.String())
	}
}
