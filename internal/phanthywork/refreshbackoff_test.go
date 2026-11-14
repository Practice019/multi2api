package phanthywork

// refreshbackoff_test.go 续期退避表的判据（官方 `Pd3` 的 `w` map）。
//
// 这个表存在的**第二层理由**必须被测到，否则它会被当成"可选优化"删掉：
//
//	本轮把 invalid_grant 从"终态"改成"可重试、不计账"之后，
//	**计数熔断就没有了** —— 一个真废掉的凭据会被每一个进来的对话请求
//	各触发一次上游续期。退避表是移除计数器之后**必须**补上的那一半。
//
// 所以下面第一条判据钉的不是"省了几次调用"，而是"不计账之后仍然不会打爆上游"。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// httpTime 造一个 HTTP 日期形态的 Retry-After（官方那段 `Date.parse(M) - now` 的输入）。
func httpTime(unixSec int64) string {
	return time.Unix(unixSec, 0).UTC().Format(http.TimeFormat)
}

// TestRefreshBackoffSuppressesRepeatedUpstreamCalls 被拒之后，退避窗口内**不再打上游**。
//
// 这是"不计账"的配套安全网：没有它，移除 refreshFails 计数就只是把
// "账号被误禁"换成了"上游被打"。
func TestRefreshBackoffSuppressesRepeatedUpstreamCalls(t *testing.T) {
	hits := 0
	srv := rejectSrv(t, &hits, nil)
	c := NewWithBase(srv.URL)
	a := newTestAuth("u-bo", "rt-x")

	if _, err := c.RefreshTokens(context.Background(), a); err == nil {
		t.Fatal("上游回 invalid_grant，第一次应失败")
	}
	if hits != 1 {
		t.Fatalf("第一次应打 1 次上游，实际 %d", hits)
	}

	// 第二次：同一把 refresh_token ⇒ 同一个退避键 ⇒ 应当被挡住
	_, err2 := c.RefreshTokens(context.Background(), a)
	if hits != 1 {
		t.Errorf("退避窗口内**不该**再打上游，实际打了 %d 次 —— 退避表没生效。\n"+
			"后果：退避表缺失时，一个真废掉的凭据会被每个进来的对话请求各打一次上游\n"+
			"（因为 invalid_grant 已经不计入 refreshFails、不再触发熔断）。", hits)
	}
	// 被挡住时对外仍必须是"可重试、不计账"那一档
	if !IsRefreshRejected(err2) {
		t.Errorf("退避中的错误也必须是「可重试、不计账」那一档：%v", err2)
	}
	if IsRefreshExpired(err2) {
		t.Errorf("退避中的错误不得判成终态：%v", err2)
	}
	if !strings.Contains(err2.Error(), "未打上游") {
		t.Errorf("错误文本应说清本次连网络都没出（便于区分「被拒」与「被我们挡下」）：%v", err2)
	}
}

// TestRefreshBackoffAppliesAfterServerErrors 5xx 放弃之后同样进表。
//
// 对应官方：
//
//	if (X === 2 || Z > 2000) {
//	  if (w.size >= 128) w.delete(w.keys().next().value);
//	  throw w.set(H, K() + Math.min(Math.max(Z, 5000), 300000)), new Po;
//	}
func TestRefreshBackoffAppliesAfterServerErrors(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream_unavailable"}}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := newTestAuth("u-5xx-bo", "rt-x")

	// 第一次：重试 3 次后放弃（后退避会睡一小会儿）
	if _, err := c.RefreshTokens(context.Background(), a); err == nil {
		t.Fatal("上游一直 502，应失败")
	}
	first := hits
	if first < 2 {
		t.Fatalf("5xx 应重试，实际只打了 %d 次", first)
	}

	// 第二次：退避窗口内 ⇒ 零新增
	_, _ = c.RefreshTokens(context.Background(), a)
	if hits != first {
		t.Errorf("5xx 放弃之后应进退避表，但第二次又打了 %d 次（累计 %d）——\n"+
			"上游抖动时每个对话请求都会各触发一轮 3 次重试，1 个请求 = 3 次上游调用",
			hits-first, hits)
	}
}

// TestBackoffWindowIsClamped 官方那对 clamp：`Math.min(Math.max(Z, 5000), 300000)`。
func TestBackoffWindowIsClamped(t *testing.T) {
	cases := []struct {
		in, want int64
		why      string
	}{
		{0, 5000, "下界 5 秒"},
		{1, 5000, "下界 5 秒"},
		{5000, 5000, "恰好下界"},
		{60_000, 60_000, "区间内原样"},
		{299_999, 299_999, "区间内原样"},
		{300_000, 300_000, "恰好上界"},
		{900_000, 300_000, "上界 5 分钟"},
	}
	for _, c := range cases {
		if got := backoffWindowMS(c.in); got != c.want {
			t.Errorf("backoffWindowMS(%d) = %d, want %d（%s）", c.in, got, c.want, c.why)
		}
	}
}

// TestParseRetryAfterMS 解 Retry-After：秒数或 HTTP 日期。
func TestParseRetryAfterMS(t *testing.T) {
	const now = int64(1794658000000) // 2026-11-14 20:06:40 (+08)
	cases := []struct {
		in   string
		want int64
		why  string
	}{
		{"", 0, "空 ⇒ 没有"},
		{"30", 30_000, "秒数"},
		{"0", 0, "0 秒"},
		{"-5", 0, "负数当没有"},
		{"not-a-date", 0, "解不出当没有"},
	}
	for _, c := range cases {
		if got := parseRetryAfterMS(c.in, now); got != c.want {
			t.Errorf("parseRetryAfterMS(%q) = %d, want %d（%s）", c.in, got, c.want, c.why)
		}
	}
	// HTTP 日期形态：取"之后 60 秒"
	future := now/1000 + 60
	if got := parseRetryAfterMS(httpTime(future), now); got != 60_000 {
		t.Errorf("HTTP 日期应解成 60 秒，实际 %d ms", got)
	}
	// 已经过去的日期 ⇒ 0（不能是负数，否则退避窗口会变成"立刻解禁"）
	if got := parseRetryAfterMS(httpTime(now/1000-60), now); got != 0 {
		t.Errorf("过去的日期应解成 0，实际 %d ms", got)
	}
}

// TestRefreshBackoffTableSemantics 表本身的语义：挡 / 到点放行 / 放行时自清 / 容量上限。
func TestRefreshBackoffTableSemantics(t *testing.T) {
	var b refreshBackoff
	b.set("k", 10_000)

	if _, ok := b.blocked("k", 9_000); !ok {
		t.Error("窗口内应挡住")
	}
	if _, ok := b.blocked("k", 10_000); ok {
		t.Error("到点应放行（官方：`(w.get(H) ?? 0) > K()` —— 恰好等于也放行）")
	}
	// 放行时应把条目清掉（官方紧随其后的 `w.delete(H)`）
	if _, ok := b.blocked("k", 9_000); ok {
		t.Error("放行之后应已自清，否则这个键永远留在表里")
	}
	if _, ok := b.blocked("never-set", 0); ok {
		t.Error("没设过的键不该被挡")
	}
	if _, ok := b.blocked("never-set", 0); ok {
		t.Error("没设过的键第二次也不该被挡")
	}

	// 容量上限：超了必须丢旧的，不能无限增长
	for i := 0; i < refreshBackoffMaxKeys+32; i++ {
		b.set(fmt.Sprintf("key-%d", i), 10_000_000)
	}
	b.mu.Lock()
	n := len(b.until)
	b.mu.Unlock()
	if n > refreshBackoffMaxKeys {
		t.Errorf("表容量应 <= %d，实际 %d —— 键由 url+body 派生，\n"+
			"凭据一多就会无界增长（官方是 `if (w.size >= 128) w.delete(...)`）",
			refreshBackoffMaxKeys, n)
	}
}

// TestRefreshBackoffKeySeparatesRequests 键必须由 url 与 body 共同决定。
//
// 只想钉住"不会因为拼接巧合而让两个不同请求共用一个退避窗口"。
func TestRefreshBackoffKeySeparatesRequests(t *testing.T) {
	a := refreshBackoffKey("https://x/a", []byte(`{"b":1}`))
	if a == refreshBackoffKey("https://x/ab", []byte(`{"b":1}`)) {
		t.Error("url 不同不该同键")
	}
	if a == refreshBackoffKey("https://x/a", []byte(`{"b":2}`)) {
		t.Error("body 不同不该同键")
	}
	if a != refreshBackoffKey("https://x/a", []byte(`{"b":1}`)) {
		t.Error("同样的 url+body 必须同键（否则退避挡不住同一把旧 token 的重复尝试）")
	}
}
