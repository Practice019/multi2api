// harness_test.go 搬运过来的测试公共装置。
//
// # 搬运说明（Task 3b）
//
// 这些测试原先在 internal/scheduler，与它们覆盖的业务一起搬进本包。
// 断言与用例名**逐字保留** —— 它们是"搬运没有改行为"的唯一证据。
//
// 改动仅限构造方式（原来是 scheduler.New(scheduler.Config{...})，
// 现在是 workbuddy.NewWithConfig(workbuddy.Config{...})），语义一一对应。
package workbuddy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// testPoolAdapter 把 *pool.Pool 适配成 AccountPool（仅测试用）。
//
// 生产路径的等价适配器在 cmd/server/upstream_business.go —— 之所以两边各有一份，
// 是因为本包（含测试）不得 import pool 之外的……准确说：生产代码不得依赖 pool，
// 但本包的测试可以（测试不是被架构约束检查的对象，且它需要真池来验证行为）。
// 为了让测试与生产走**同一条接口**，这里仍用适配器而不是绕过接口。
type testPoolAdapter struct{ p *pool.Pool }

func (a testPoolAdapter) List() []Account {
	src := a.p.List()
	out := make([]Account, 0, len(src))
	for _, st := range src {
		out = append(out, Account{
			UID: st.UID, Nickname: st.Nickname,
			Disabled: st.Disabled, Cooling: st.Cooling,
		})
	}
	return out
}

func (a testPoolAdapter) AuthByUID(uid string) *auth.Auth { return a.p.AuthByUID(uid) }
func (a testPoolAdapter) Has(uid string) bool             { _, ok := a.p.Status(uid); return ok }
func (a testPoolAdapter) SetCredits(uid string, c int64)  { a.p.SetCredits(uid, c) }
func (a testPoolAdapter) ReenableIfCredits(uid string, remain int64) {
	a.p.ReenableIfCredits(uid, remain)
}
func (a testPoolAdapter) Disable(uid, reason string) { a.p.Disable(uid, reason) }

var _ AccountPool = testPoolAdapter{}

// newTestProvider 用给定的账号与桩服务器构造一个完整接线的 Provider。
func newTestProvider(t *testing.T, srv *httptest.Server, accounts ...*auth.Auth) (*Provider, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up}), p
}

// newTestProviderWithLog 同上，另带一份历史记录。
func newTestProviderWithLog(t *testing.T, srv *httptest.Server, accounts ...*auth.Auth) (*Provider, *pool.Pool, *checkinlog.Log) {
	t.Helper()
	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	log := checkinlog.New(filepath.Join(t.TempDir(), "checkin-log.json"), 30)
	return NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up, Log: log}), p, log
}

// testAuth 造一个可用凭证。
func testAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
}

// testAuthNamed 造一个带昵称的可用凭证。
func testAuthNamed(uid, nick string) *auth.Auth {
	a := testAuth(uid)
	a.Nickname = nick
	return a
}

// stubServer 起一个 httptest 服务器，返回 (server, 关闭函数)。
func stubServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
