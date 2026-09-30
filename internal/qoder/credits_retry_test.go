package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestGetJSONRetriesTransient5xx 5xx 必须**重试**，不能一次定生死。
//
// # 为什么这条是本轮的核心判据（用户报「签到 失败」）
//
// 实测（同一凭据、同一时刻交替打 10 次）：
//
//	不带 machine 头  200×7  503×3
//	带 machine 头    200×8  503×2
//
// `campaign service is temporarily unavailable` 是**上游间歇性**故障
//（与请求头无关，`/usage` 端点同时刻恒 200）。而签到每 30 分钟扫一次，
// 撞上 503 就记一次 "fail" —— 用户看到的「今日签到 失败」有相当比例
// 就是这么来的。旧实现不重试，**一次 503 直接判失败**。
func TestGetJSONRetriesTransient5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			// 前两次模拟上游"临时不可用"（真实错误体就是 JSON）
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errorCode":"DEPENDENCY_UNAVAILABLE",` +
				`"errorMessage":"campaign service is temporarily unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"campaigns":[]}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	disableLiveMachineIdentity(t)

	var out map[string]any
	ok, reason := c.getJSON(context.Background(), testAuth(), campaignsPath, &out)
	if !ok {
		t.Fatalf("前两次 503、第三次 200 —— 应当重试后成功，实际失败：%s", reason)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("应发 3 次（2 次 503 + 1 次成功），实际 %d 次", got)
	}
}

// TestGetJSONDoesNotRetry4xx 4xx 是确定性的，**不该**重试。
//
// 401 重试三次只是白等 600ms 且同样失败 —— 而且会让"凭据失效"这个
// 需要用户动作的结论被延迟暴露。
func TestGetJSONDoesNotRetry4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errorCode":"UNAUTHORIZED"}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	disableLiveMachineIdentity(t)

	var out map[string]any
	ok, reason := c.getJSON(context.Background(), testAuth(), campaignsPath, &out)
	if ok {
		t.Fatal("401 不该成功")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("401 不该重试，实际发了 %d 次", got)
	}
	if !strings.Contains(reason, "凭据已失效") {
		t.Errorf("401 的文案应告诉用户要重新登录，实际 %q", reason)
	}
}

// TestGetJSONGivesUpAfterMaxAttempts 一直 5xx 时**用尽重试后如实失败**。
//
// 反面对照：不能因为"加了重试"就把永久故障吞掉。
func TestGetJSONGivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"errorCode":"DEPENDENCY_UNAVAILABLE"}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	disableLiveMachineIdentity(t)

	var out map[string]any
	ok, reason := c.getJSON(context.Background(), testAuth(), campaignsPath, &out)
	if ok {
		t.Fatal("一直 503 时必须失败")
	}
	if got := atomic.LoadInt32(&calls); got != creditsMaxAttempts {
		t.Errorf("应尝试 %d 次，实际 %d 次", creditsMaxAttempts, got)
	}
	if !strings.Contains(reason, "暂时不可用") {
		t.Errorf("文案应说清是上游临时故障，实际 %q", reason)
	}
}

// TestDescribeNonJSONDoesNotMislableJSON JSON 错误体**不能**被说成"非 JSON"。
//
// # 这是本轮修的一个小缺陷，但它害我多查了一轮
//
// 旧文案对被调用的**所有**非 2xx 都写「服务端返回了非 JSON 响应」，
// 而 503 的错误体本身就是 JSON：
//
//	{"errorCode":"DEPENDENCY_UNAVAILABLE","errorMessage":"campaign service…"}
//
// 这句文案把人往"解析器坏了"的方向带，而真相是"上游服务临时不可用"。
// 判据应当是**看 body 到底是不是 JSON**，而不是假设。
func TestDescribeNonJSONDoesNotMislableJSON(t *testing.T) {
	body := `{"errorCode":"DEPENDENCY_UNAVAILABLE","errorMessage":"campaign service is temporarily unavailable"}`
	got := describeNonJSON(503, body)
	if strings.Contains(got, "非 JSON") {
		t.Errorf("JSON 错误体不该被说成「非 JSON 响应」：%q", got)
	}
	// 应当把服务端给的两个字段抠出来（那才是有诊断价值的）
	if !strings.Contains(got, "DEPENDENCY_UNAVAILABLE") {
		t.Errorf("应带出 errorCode，实际 %q", got)
	}
	if !strings.Contains(got, "temporarily unavailable") {
		t.Errorf("应带出 errorMessage，实际 %q", got)
	}

	// HTML（真正非 JSON 的情形）仍应如实描述
	html := describeNonJSON(502, "<html><body>502 Bad Gateway</body></html>")
	if !strings.Contains(html, "无法识别") {
		t.Errorf("HTML 响应应说『无法识别』，实际 %q", html)
	}
}

// TestLoadCampaignsRecoversFromTransient503 端到端：瞬时 503 不该变成签到失败。
//
// 走**真实业务路径**（FetchCheckinStatus），而不是只测 getJSON ——
// 因为用户看到的是"签到失败"，判据必须落在那一层。
func TestLoadCampaignsRecoversFromTransient503(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errorCode":"DEPENDENCY_UNAVAILABLE"}`))
			return
		}
		// 第二次：可领的每日 100 积分
		_, _ = w.Write([]byte(`{"campaigns":[{"campaignId":"c-1","campaignKey":"act-1",` +
			`"actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",` +
			`"benefit":{"amount":100}}]}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	disableLiveMachineIdentity(t)

	st, ok := c.FetchCheckinStatus(context.Background(), testAuth())
	if !ok {
		t.Fatal("第一次 503、第二次成功 —— 应当重试后拿到状态，" +
			"而不是让用户看到「签到失败」")
	}
	if !st.Active {
		t.Error("拿到响应即 active=true")
	}
	if st.TodayCheckedIn {
		t.Error("有 CLAIMABLE 的活动时不该判成「今天已领」")
	}
	if st.DailyCredit != 100 {
		t.Errorf("可领积分为 100，实际 %v", st.DailyCredit)
	}
}
