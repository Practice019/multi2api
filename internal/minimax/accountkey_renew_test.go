// accountkey_renew_test.go 判重**必须能处理"旧凭证已过期"**。
//
// # 这是用户第二次报障的直接教训
//
// 第一次修完（存 identity + 比指纹）我测过、全绿、还端到端跑通了 ——
// 但那个端到端里，**已有凭证是新鲜的**。
//
// 用户的真实场景是：旧凭证 21:41 过期 → 他 21:47 重登。
// 判重要拿已有凭证去取指纹，而它的令牌已死 ⇒ 积分端点 401（实测）⇒
// 按"取不到就不算命中"的规则放弃 ⇒ **又新增一条**。
//
// 也就是说：我第一版判重**恰好在它被需要的那个场景里注定失效**，
// 而我的测试全都在"旧凭证还活着"的假设下写的 —— 那种假设让测试
// 看起来覆盖了，实则一条也没碰到真正的路径。
//
// 所以本文件的两条用例都以**过期凭证**为主角。
package minimax

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 判重必须"先把已过期的已有凭证续上，再取指纹"，从而命中。
func TestDedupRenewsExpiredExistingCredential(t *testing.T) {
	var creditCalls atomic.Int32
	var refreshCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathToken:
			n := refreshCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"mmoat_after_renew_` + dec(int64(n)) +
				`","refresh_token":"mmort_r` + dec(int64(n)) +
				`","token_type":"Bearer","expires_in":3600,"scope":"agent.default"}`))
		case pathCreditDetails:
			n := creditCalls.Add(1)
			auth := r.Header.Get("Authorization")
			// 旧令牌（已过期）⇒ 401；续期后的新令牌 ⇒ 正常返回同一个桶。
			if auth == "Bearer mmoat_old" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = n
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(bucketA))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := New(Config{AccountBase: srv.URL, APIBase: srv.URL})
	// 已有账号：令牌**已过期**、但有 refresh_token。
	expired := &Auth{
		AccessToken:  "mmoat_old",
		RefreshToken: "mmort_old",
		ExpiresAt:    dec(time.Now().Add(-time.Hour).UnixMilli()),
	}
	p.creds["token-old"] = expired

	// 新登录得到的凭证（令牌不同，指纹相同）。
	fresh := &Auth{AccessToken: "mmoat_new_login", RefreshToken: "mmort_new"}

	got, ok := p.dedupIdentity(fresh)
	if !ok {
		t.Errorf("已有凭证过期时必须**先续期再取指纹**并命中，实际判为不命中。\n"+
			"这正是用户第二次报障的现场：旧号 21:41 过期、21:47 重登 ⇒ "+
			"用死令牌取不到指纹 ⇒ 按旧实现放弃 ⇒ 又变成两条账号。\n"+
			"refresh_calls=%d credit_calls=%d", refreshCalls.Load(), creditCalls.Load())
	}
	if got != "token-old" {
		t.Errorf("应命中已有账号 token-old，实际 %q", got)
	}
	if refreshCalls.Load() == 0 {
		t.Error("判重过程里没有对已有凭证发起续期 ⇒ 用户看到的「凭据没刷新」不会被修好")
	}
	// 顺带确认：旧凭证真的被续上了（这就是"凭据没刷新"的解）。
	if expired.AccessToken != "mmoat_after_renew_1" {
		t.Errorf("已有凭证应被就地续期，实际 access_token=%q", expired.AccessToken)
	}
	// 指纹要缓存下来 —— 下次比对不再打网络，也不再依赖令牌生死。
	if expired.AccountKey == "" || fresh.AccountKey == "" {
		t.Errorf("两侧都应缓存指纹；已有=%q 新=%q", expired.AccountKey, fresh.AccountKey)
	}
}

// 缓存命中 ⇒ 不再打积分端点。
//
// 这条钉住"判重不会每次都 N+1 次请求"：AccountKey 有了就直接用。
func TestAccountKeyCacheAvoidsExtraCalls(t *testing.T) {
	var creditCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathCreditDetails {
			creditCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(bucketA))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := New(Config{AccountBase: srv.URL, APIBase: srv.URL})
	a := &Auth{AccessToken: "mmoat_x", AccountKey: "cached-key"}
	if got := p.accountKey(a); got != "cached-key" {
		t.Errorf("应直接返回缓存，实际 %q", got)
	}
	if creditCalls.Load() != 0 {
		t.Errorf("有缓存时不该再打积分端点，实际打了 %d 次", creditCalls.Load())
	}
}

// 既取不到指纹、又不可续期 ⇒ 放弃合并（不猜）。
func TestAccountKeyGivesUpWhenUnrenewable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // 令牌死了，且没有 refresh_token
	}))
	defer srv.Close()

	p := New(Config{AccountBase: srv.URL, APIBase: srv.URL})
	a := &Auth{AccessToken: "mmoat_dead_only", ExpiresAt: dec(time.Now().Add(-time.Hour).UnixMilli())}
	if got := p.accountKey(a); got != "" {
		t.Errorf("取不到又续不了时必须返回空（放弃合并），实际 %q", got)
	}
}
