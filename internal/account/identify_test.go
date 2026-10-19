// identify_test.go 身份解析的守卫。
//
// # 为什么这几条值得单独钉住
//
// 判据本身很薄（"谁的凭据"），但它是**两个入口共用**的：`/ui`（internal/server）
// 与 `/admin/*`（internal/admin）。一处写错的后果是全局的，而"取凭据的优先级"
// 又恰好是最容易被后人无意改坏的地方（谁都会觉得"顺手换个顺序没差别"）。
package account

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// idFixture 最小夹具：一个管理员账号 + 一条会话。
type idFixture struct {
	store    *Store
	sessions *SessionStore
	admin    *User
}

func newIDFixture(t *testing.T) *idFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := New(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sessions, err := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	admin, err := store.Create("boss", "boss-password-001", RoleAdmin)
	if err != nil {
		t.Fatalf("建管理员: %v", err)
	}
	return &idFixture{store: store, sessions: sessions, admin: admin}
}

// onlyToken 造一个"只认这一把令牌"的回调（模拟各包的 adminTokenOK）。
func onlyToken(want string) func(string) bool {
	return func(cred string) bool { return cred == want }
}

func reqWith(t *testing.T, target string, mutate func(*http.Request)) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if mutate != nil {
		mutate(r)
	}
	return r
}

// ★ 取凭据的**优先级**：Bearer → ?token= → 令牌 cookie。
//
// 顺序不是审美问题：脚本带对了头、URL 里却残留一个旧参数（别人贴给他的
// 残缺长链接）时，必须**以头为准**。反过来会让"头是对的却进不去"。
func TestTokenFromRequestPrecedence(t *testing.T) {
	const (
		fromHeader = "TOKEN-FROM-HEADER"
		fromQuery  = "TOKEN-FROM-QUERY"
		fromCookie = "TOKEN-FROM-COOKIE"
	)
	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{
			"只有 Bearer",
			reqWith(t, "/ui", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+fromHeader) }),
			fromHeader,
		},
		{
			"只有 ?token=",
			reqWith(t, "/ui?token="+fromQuery, nil),
			fromQuery,
		},
		{
			"只有令牌 cookie",
			reqWith(t, "/ui", func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: fromCookie})
			}),
			fromCookie,
		},
		{
			"★ 三者都有 ⇒ Bearer 优先",
			reqWith(t, "/ui?token="+fromQuery, func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+fromHeader)
				r.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: fromCookie})
			}),
			fromHeader,
		},
		{
			"★ 查询参数优先于 cookie",
			reqWith(t, "/ui?token="+fromQuery, func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: fromCookie})
			}),
			fromQuery,
		},
		{
			"Bearer 大小写不敏感（HTTP 头本就如此）",
			reqWith(t, "/ui", func(r *http.Request) { r.Header.Set("Authorization", "bEaReR "+fromHeader) }),
			fromHeader,
		},
		{
			"Authorization 不是 Bearer ⇒ 空",
			reqWith(t, "/ui", func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwdw==") }),
			"",
		},
		{
			"什么都没有 ⇒ 空",
			reqWith(t, "/ui", nil),
			"",
		},
	}
	for _, tc := range cases {
		if got := TokenFromRequest(tc.req); got != tc.want {
			t.Errorf("%s：TokenFromRequest = %q，want %q", tc.name, got, tc.want)
		}
	}
}

// ★ 令牌赢过会话（两条路都在时以令牌为准），且令牌来源恒为管理员。
func TestIdentifyTokenWinsOverSession(t *testing.T) {
	f := newIDFixture(t)
	member, err := f.store.Create("staff", "staff-password-002", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.sessions.Issue(member, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatal(err)
	}
	const tok = "ADMIN-TOKEN-XYZ"
	r := reqWith(t, "/ui", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: sess.SID})
		r.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: tok})
	})
	p, ok := Identify(r, onlyToken(tok), f.store, f.sessions)
	if !ok {
		t.Fatal("两条路都在时应当认得出身份")
	}
	if p.Kind != KindToken {
		t.Errorf("Kind = %q，want %q（令牌优先）", p.Kind, KindToken)
	}
	if !p.IsAdmin() {
		t.Error("★ 令牌来源必须等价管理员 —— 机器凭据没有「成员」这种身份")
	}
	if p.User != nil {
		t.Error("令牌来源不该带账号（User 应为 nil）")
	}
}

// 会话来源带出账号与角色（成员就是成员，不得被抬成管理员）。
func TestIdentifySessionCarriesRole(t *testing.T) {
	f := newIDFixture(t)
	member, err := f.store.Create("staff", "staff-password-002", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.sessions.Issue(member, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatal(err)
	}
	r := reqWith(t, "/ui", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: sess.SID})
	})
	p, ok := Identify(r, onlyToken("nobody"), f.store, f.sessions)
	if !ok {
		t.Fatal("有效会话应当被认出来")
	}
	if p.Kind != KindSession || p.UserID != member.ID || p.Role != RoleMember {
		t.Errorf("会话身份 = %+v，want Kind=%s UserID=%s Role=%d", p, KindSession, member.ID, RoleMember)
	}
	if p.IsAdmin() {
		t.Error("★ 成员不得被判成管理员")
	}
}

// ★ 失效路径：错误令牌、无效/已撤销会话、账号禁用 —— 全部未认证。
//
// 这里逐条钉住"必须走 SessionStore.Lookup"这件事：绕过它（自己读 cookie
// 查表）会让最后两条静默放行。
func TestIdentifyRejectsInvalidCredentials(t *testing.T) {
	f := newIDFixture(t)
	sess, err := f.sessions.Issue(f.admin, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("错误令牌", func(t *testing.T) {
		r := reqWith(t, "/ui", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: "wrong"})
		})
		if _, ok := Identify(r, onlyToken("right"), f.store, f.sessions); ok {
			t.Error("错误令牌不得通过")
		}
	})
	t.Run("无效会话 ID", func(t *testing.T) {
		r := reqWith(t, "/ui", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: "deadbeef"})
		})
		if _, ok := Identify(r, nil, f.store, f.sessions); ok {
			t.Error("伪造的会话 ID 不得通过")
		}
	})
	t.Run("★ 已撤销会话", func(t *testing.T) {
		f.sessions.Revoke(sess.SID)
		r := reqWith(t, "/ui", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: sess.SID})
		})
		if _, ok := Identify(r, nil, f.store, f.sessions); ok {
			t.Error("登出后旧 cookie 不得继续通过（Lookup 的第一道校验）")
		}
	})
	t.Run("★ 账号被禁用", func(t *testing.T) {
		s2, err := f.sessions.Issue(f.admin, "127.0.0.1", "probe/1.0")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.SetEnabled(f.admin.ID, false); err != nil {
			t.Fatal(err)
		}
		r := reqWith(t, "/ui", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: s2.SID})
		})
		if _, ok := Identify(r, nil, f.store, f.sessions); ok {
			t.Error("账号已禁用 ⇒ 会话必须失效（否则「禁用」只挡新登录，挡不住已在线的）")
		}
	})
	t.Run("★ 改口令后旧会话（AuthVersion）", func(t *testing.T) {
		s3, err := f.sessions.Issue(f.admin, "127.0.0.1", "probe/1.0")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.SetPassword(f.admin.ID, "brand-new-password-003"); err != nil {
			t.Fatal(err)
		}
		r := reqWith(t, "/ui", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: s3.SID})
		})
		if _, ok := Identify(r, nil, f.store, f.sessions); ok {
			t.Error("改口令后旧会话必须失效（否则用户报障 ② 换个形态复发）")
		}
	})
	t.Run("没接账号体系（nil）", func(t *testing.T) {
		r := reqWith(t, "/ui", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: CookieName, Value: sess.SID})
		})
		if _, ok := Identify(r, nil, nil, nil); ok {
			t.Error("Accounts/Sessions 为 nil 时不得凭 cookie 通过")
		}
	})
}

// 令牌 cookie 的写入/清除：HttpOnly 与 SameSite 缺一不可。
func TestAdminTokenCookieAttributes(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ui?token=x", nil)

	for _, mode := range []string{CookieSecureAuto, CookieSecureAlways, CookieSecureNever} {
		rec := httptest.NewRecorder()
		SetAdminTokenCookie(rec, r, mode, "THE-TOKEN")
		c := rec.Result().Cookies()
		if len(c) != 1 || c[0].Name != AdminTokenCookie {
			t.Fatalf("mode=%s：应当只下发一个 %s cookie", mode, AdminTokenCookie)
		}
		if !c[0].HttpOnly {
			t.Errorf("mode=%s：★ 必须 HttpOnly（否则 XSS 能读走管理令牌）", mode)
		}
		if c[0].SameSite != http.SameSiteLaxMode {
			t.Errorf("mode=%s：SameSite 应为 Lax，实际 %v", mode, c[0].SameSite)
		}
		if c[0].Path != "/" {
			t.Errorf("mode=%s：Path 应为 /，实际 %q", mode, c[0].Path)
		}
		if c[0].MaxAge <= 0 {
			t.Errorf("mode=%s：MaxAge 应为正（有界），实际 %d", mode, c[0].MaxAge)
		}
		wantSecure := mode == CookieSecureAlways
		if c[0].Secure != wantSecure {
			t.Errorf("mode=%s：Secure = %v，want %v", mode, c[0].Secure, wantSecure)
		}
	}

	// 清除：MaxAge<0（浏览器立刻删）
	rec := httptest.NewRecorder()
	ClearAdminTokenCookie(rec, r, CookieSecureNever)
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Value != "" || c[0].MaxAge >= 0 {
		t.Errorf("ClearAdminTokenCookie 应当下发空值且 MaxAge<0，实际 %+v", c)
	}
}

// 令牌 cookie 里的空白不该被当成有效凭据（复制的链接常带空格/换行）。
func TestAdminTokenCookieTrimsWhitespace(t *testing.T) {
	r := reqWith(t, "/ui", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: AdminTokenCookie, Value: "  TOKEN  "})
	})
	if got := TokenFromRequest(r); got != "TOKEN" {
		t.Errorf("cookie 值应当去掉首尾空白，实际 %q", got)
	}
	// 反过来：判定必须拿到**去过空白**的串（否则"看起来对但差一个空格"进不去）
	p, ok := Identify(r, onlyToken("TOKEN"), nil, nil)
	if !ok || p.Kind != KindToken {
		t.Errorf("去空白后的值应当能通过判定，实际 ok=%v kind=%q", ok, p.Kind)
	}
}
