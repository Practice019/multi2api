// refresh_test.go 续期路径的守卫。
//
// # 这个文件是补一个**真实的用户报障**
//
// 用户报：控制台的 Token 列显示「已过期 10/10 17:21」，账号一直不能用 ——
// 而它本应自动续期。
//
// 根因：`RefreshSkewExt`（多早该刷）实现了，`CredentialRefresher`（怎么刷）
// **漏了**。两者必须成对，而漏掉后者的后果不是「不续期」这么简单：
//
//	核心判「该刷了」→ 找续期实现 → 找不到 →
//	语义被解释成「该上游的凭证不需要刷新」→ **静默跳过**
//
// 所以漏实现**不报任何错**。下面几条就是钉住这条链的每一环。
package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 假令牌端点：记录收到的 form，按脚本返回。
//
// resp 传 map ⇒ 编成 JSON 回；传 string ⇒ **原样**回
// （后者用于测「响应体是原始 JSON 文本」的路径）。
func newFakeTokenServer(t *testing.T, resp any, status int, got *url.Values) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got != nil {
			*got = r.PostForm
		}
		if r.URL.Path != pathToken {
			t.Errorf("打到了错误的端点: %s（应 %s）", r.URL.Path, pathToken)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/x-www-form-urlencoded") {
			t.Errorf("Content-Type 应为 form-urlencoded，实际 %q", ct)
		}
		w.WriteHeader(status)
		switch v := resp.(type) {
		case string:
			_, _ = w.Write([]byte(v))
		default:
			_ = json.NewEncoder(w).Encode(v)
		}
	}))
}

// 续期必须打 `/oauth2/token`，且带**refresh_token grant** 与产品常量。
//
// 漏掉 `client_id`/`scope`/`audience` 里的任何一个，服务端会拒绝 ——
// 所以逐个钉住。
func TestRefreshTokenSendsCorrectGrant(t *testing.T) {
	var got url.Values
	srv := newFakeTokenServer(t, map[string]any{
		"access_token":  "mmoat_new",
		"refresh_token": "mmort_new",
		"token_type":    "Bearer",
		"expires_in":    3600,
		"scope":         "agent.default",
	}, http.StatusOK, &got)
	defer srv.Close()

	c := NewClient(Config{AccountBase: srv.URL, APIBase: srv.URL})
	next, err := c.refreshToken(context.Background(), &Auth{
		AccessToken: "mmoat_old", RefreshToken: "mmort_old",
	})
	if err != nil {
		t.Fatalf("续期应成功: %v", err)
	}
	for k, want := range map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": "mmort_old",
		"client_id":     "mcode-public",
		"scope":         "agent.default",
		"audience":      "agent-backend",
	} {
		if got.Get(k) != want {
			t.Errorf("form 的 %s = %q，期望 %q", k, got.Get(k), want)
		}
	}
	if next.AccessToken != "mmoat_new" {
		t.Errorf("access_token 应更新，实际 %q", next.AccessToken)
	}
	if next.IssuedAt == "" || next.ExpiresAt == "" {
		t.Error("续期后必须重算 IssuedAt / ExpiresAt（否则窗口会按旧寿命算）")
	}
}

// ⚠ refresh_token 的轮换：服务端**下发新值就用新的**。
//
// 硬沿用旧值会让轮换型上游在第一次续期后就作废。
func TestRefreshTokenAdoptsRotatedToken(t *testing.T) {
	srv := newFakeTokenServer(t, map[string]any{
		"access_token": "mmoat_new", "refresh_token": "mmort_rotated",
		"token_type": "Bearer", "expires_in": 3600, "scope": "agent.default",
	}, http.StatusOK, nil)
	defer srv.Close()

	c := NewClient(Config{AccountBase: srv.URL, APIBase: srv.URL})
	next, err := c.refreshToken(context.Background(), &Auth{AccessToken: "a", RefreshToken: "mmort_old"})
	if err != nil {
		t.Fatal(err)
	}
	if next.RefreshToken != "mmort_rotated" {
		t.Errorf("服务端下发了新 refresh_token 就该采用，实际 %q", next.RefreshToken)
	}
}

// ⚠ 反过来：服务端**不下发**就沿用旧的。
//
// 硬要求新值会让非轮换型上游每次续期都失败。
func TestRefreshTokenKeepsTokenWhenNotRotated(t *testing.T) {
	srv := newFakeTokenServer(t, map[string]any{
		"access_token": "mmoat_new",
		"token_type":   "Bearer", "expires_in": 3600, "scope": "agent.default",
	}, http.StatusOK, nil)
	defer srv.Close()

	c := NewClient(Config{AccountBase: srv.URL, APIBase: srv.URL})
	next, err := c.refreshToken(context.Background(), &Auth{AccessToken: "a", RefreshToken: "mmort_old"})
	if err != nil {
		t.Fatal(err)
	}
	if next.RefreshToken != "mmort_old" {
		t.Errorf("服务端没下发新值就该沿用旧的，实际 %q", next.RefreshToken)
	}
}

// refresh_token 死掉时错误必须**可判别**（便于核心禁用账号而不是反复重试）。
func TestRefreshTokenTerminalErrors(t *testing.T) {
	for _, c := range []struct {
		body string
		want string
	}{
		{`{"error":"invalid_grant"}`, "已失效"},
		{`{"error":"refresh_token_expired"}`, "已失效"},
		{`{"error":"token revoked"}`, "已失效"},
	} {
		t.Run(c.body, func(t *testing.T) {
			srv := newFakeTokenServer(t, c.body, http.StatusBadRequest, nil)
			defer srv.Close()
			cl := NewClient(Config{AccountBase: srv.URL, APIBase: srv.URL})
			_, err := cl.refreshToken(context.Background(), &Auth{AccessToken: "a", RefreshToken: "r"})
			if err == nil {
				t.Fatal("终态错误必须报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应含 %q（便于判终态），实际 %q", c.want, err.Error())
			}
		})
	}
}

// 没有 refresh_token ⇒ 报错（不是静默跳过）。
//
// ⚠ 这两件事必须分开：
//
//	ok=true, skew=0   上游明确声明「不需要提前刷」
//	刷不了            是**能力缺失** → 报错，让核心记失败并最终禁用账号
func TestRefreshCredentialRequiresRefreshToken(t *testing.T) {
	p := New(Config{})
	err := p.RefreshCredential(gatewayCred(&Auth{AccessToken: "mmoat_only"}))
	if err == nil {
		t.Fatal("没有 refresh_token 时应报错（不能静默成功）")
	}
	if !strings.Contains(err.Error(), "重新登录") {
		t.Errorf("错误信息应告诉用户怎么办（重新登录），实际 %q", err.Error())
	}
}

// RefreshCredential **原地更新**池里那份凭证，而不是换一个新对象。
//
// # 为什么这条最重要
//
// 账号池里存的是**同一个 `*Auth` 指针**。若实现返回新对象而不写回原对象，
// 池里那份会永远停在作废的旧 token 上 —— 表现为「续期明明成功，
// 下一个请求还是 401」，极难查。
//
// 所以断言的是**同一个对象的字段变了**。
func TestRefreshCredentialUpdatesInPlace(t *testing.T) {
	srv := newFakeTokenServer(t, map[string]any{
		"access_token": "mmoat_fresh", "refresh_token": "mmort_new",
		"token_type": "Bearer", "expires_in": 3600, "scope": "agent.default",
	}, http.StatusOK, nil)
	defer srv.Close()

	a := &Auth{
		AccessToken: "mmoat_stale", RefreshToken: "mmort_old",
		Nickname: "某号",
	}
	p := New(Config{AccountBase: srv.URL, APIBase: srv.URL})
	if err := p.RefreshCredential(gatewayCred(a)); err != nil {
		t.Fatalf("续期应成功: %v", err)
	}
	if a.AccessToken != "mmoat_fresh" {
		t.Errorf("原对象必须被就地更新（池里存的就是它），实际 access_token=%q", a.AccessToken)
	}
	if a.Nickname != "某号" {
		t.Errorf("服务端不重发的字段必须保留，昵称变成了 %q", a.Nickname)
	}
	if a.ExpiresAtMS() <= time.Now().UnixMilli() {
		t.Error("续期后过期时刻必须是未来")
	}
}

// RefreshSkew 按**寿命的一半**算（用户明确期望的语义）。
func TestRefreshSkewIsHalfLifetime(t *testing.T) {
	now := time.Now()
	p := New(Config{})

	// 1 小时寿命 ⇒ 30 分钟窗口。
	a := &Auth{
		AccessToken: "t", RefreshToken: "r",
		IssuedAt:  msString(now.UnixMilli()),
		ExpiresAt: msString(now.Add(time.Hour).UnixMilli()),
	}
	skew, ok := p.RefreshSkew(gatewayCred(a))
	if !ok {
		t.Fatal("可续期的凭证应报 ok=true")
	}
	if skew != 30*time.Minute {
		t.Errorf("1 小时寿命的窗口应为 30m（一半），实际 %v", skew)
	}

	// 10 分钟寿命 ⇒ 5 分钟窗口（下限 2m 不触发）。
	b := &Auth{
		AccessToken: "t", RefreshToken: "r",
		IssuedAt:  msString(now.UnixMilli()),
		ExpiresAt: msString(now.Add(10 * time.Minute).UnixMilli()),
	}
	if skew2, _ := p.RefreshSkew(gatewayCred(b)); skew2 != 5*time.Minute {
		t.Errorf("10 分钟寿命的窗口应为 5m，实际 %v", skew2)
	}

	// 极短寿命 ⇒ 夹到下限 2m（否则窗口小到会被调度间隔跳过）。
	d := &Auth{
		AccessToken: "t", RefreshToken: "r",
		IssuedAt:  msString(now.UnixMilli()),
		ExpiresAt: msString(now.Add(60 * time.Second).UnixMilli()),
	}
	if skew3, _ := p.RefreshSkew(gatewayCred(d)); skew3 != 2*time.Minute {
		t.Errorf("极短寿命应夹到下限 2m，实际 %v", skew3)
	}

	// 超长寿命 ⇒ 夹到上限 30m。
	e := &Auth{
		AccessToken: "t", RefreshToken: "r",
		IssuedAt:  msString(now.UnixMilli()),
		ExpiresAt: msString(now.Add(24 * time.Hour).UnixMilli()),
	}
	if skew4, _ := p.RefreshSkew(gatewayCred(e)); skew4 != 30*time.Minute {
		t.Errorf("超长寿命应夹到上限 30m，实际 %v", skew4)
	}

	// 寿命未知（手工导入，无 issued_at）⇒ 固定 10m 兜底。
	f := &Auth{AccessToken: "t", RefreshToken: "r", ExpiresAt: msString(now.Add(time.Hour).UnixMilli())}
	if skew5, _ := p.RefreshSkew(gatewayCred(f)); skew5 != 10*time.Minute {
		t.Errorf("寿命未知应回落 10m，实际 %v", skew5)
	}

	// 不可续期 ⇒ ok=false（**不是** skew=0 —— 那是"明确不需要"的语义）。
	if _, ok := p.RefreshSkew(gatewayCred(&Auth{AccessToken: "t"})); ok {
		t.Error("没有 refresh_token 应报 ok=false（提供不了信息），不是 skew=0")
	}
}

// LifetimeMillis 的边界。
func TestLifetimeMillis(t *testing.T) {
	now := time.Now()
	good := &Auth{
		IssuedAt:  msString(now.UnixMilli()),
		ExpiresAt: msString(now.Add(time.Hour).UnixMilli()),
	}
	if got := good.LifetimeMillis(); got != int64(time.Hour/time.Millisecond) {
		t.Errorf("寿命应为 3600000ms，实际 %d", got)
	}
	// 缺 issued_at ⇒ 未知（0 哨兵）。
	if got := (&Auth{ExpiresAt: msString(now.UnixMilli())}).LifetimeMillis(); got != 0 {
		t.Errorf("缺 issued_at 应报未知(0)，实际 %d", got)
	}
	// 过期早于发证（数据坏了）⇒ 未知，不是负数。
	bad := &Auth{
		IssuedAt:  msString(now.UnixMilli()),
		ExpiresAt: msString(now.Add(-time.Hour).UnixMilli()),
	}
	if got := bad.LifetimeMillis(); got != 0 {
		t.Errorf("过期早于发证应报未知(0)，实际 %d", got)
	}
}

func msString(ms int64) string {
	return fmt.Sprintf("%d", ms)
}
