package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// stubScheduler 起一个假上游（按 handler 分流）与只含 u1 的调度器。
// 与本包既有测试（scheduler_test.go / growthwatch_test.go）同一套构造方式。
func stubScheduler(t *testing.T, h http.HandlerFunc) *Scheduler {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Nickname: "测试号"})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	return New(Config{Pool: p, Upstream: up})
}

// TestRefreshCreditsIncludesDosageHint 余额查询失败时，必须把上游的额度告警
// 一并写进 Detail —— 这是 Task C2 的全部价值所在。
//
// 背景（实测）：该端点正常时完全静默（3 个真实账号都是 notifyCode=0、文案为空），
// 所以它只在**故障路径**上被调用。这条测试钉住"故障时确实会去问、且答案进了历史"。
func TestRefreshCreditsIncludesDosageHint(t *testing.T) {
	var dosageCalls atomic.Int32
	s := stubScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			http.Error(w, `{"code":500,"msg":"resource unavailable"}`, 500)
		case strings.HasSuffix(r.URL.Path, "/get-dosage-notify"):
			dosageCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{
				"dosageNotifyCode":2,"dosageNotifyZh":"额度已用尽，请充值","dosageNotifyEn":"exhausted"}}`))
		default:
			http.NotFound(w, r)
		}
	})

	res, _ := s.RefreshCredits("u1", triggerManual)
	if res.Status != "fail" {
		t.Fatalf("前置：余额查询应失败，得到 %s（%s）", res.Status, res.Detail)
	}
	if dosageCalls.Load() == 0 {
		t.Fatal("故障路径应查询额度告警（实测该端点平时静默，只在故障时才有意义）")
	}
	if !strings.Contains(res.Detail, "额度已用尽，请充值") {
		t.Errorf("Detail 应含上游提示，得到 %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "上游提示") {
		t.Errorf("提示应有可辨识前缀，得到 %q", res.Detail)
	}
}

// TestRefreshCreditsNoHintWhenDosageSilent 上游没有告警时，Detail 不该多出噪音。
//
// 正常情况（实测）：dosageNotifyCode=0 且文案为空。此时不该追加
// "上游提示：（空）"这种空话。
func TestRefreshCreditsNoHintWhenDosageSilent(t *testing.T) {
	s := stubScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			http.Error(w, `{"code":500,"msg":"boom"}`, 500)
		case strings.HasSuffix(r.URL.Path, "/get-dosage-notify"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"dosageNotifyCode":0,"dosageNotifyZh":"","dosageNotifyEn":""}}`))
		default:
			http.NotFound(w, r)
		}
	})

	res, _ := s.RefreshCredits("u1", triggerManual)
	if res.Status != "fail" {
		t.Fatalf("前置：应失败，得到 %s", res.Status)
	}
	if strings.Contains(res.Detail, "上游提示") {
		t.Errorf("无告警时不该追加空提示，得到 %q", res.Detail)
	}
}

// TestRefreshCreditsSurvivesDosageFailure 告警端点自己挂了，也不能影响主流程。
//
// 这是"诊断路径"的基本要求：它只是锦上添花，任何失败都必须静默降级。
func TestRefreshCreditsSurvivesDosageFailure(t *testing.T) {
	s := stubScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			http.Error(w, `{"code":500,"msg":"boom"}`, 500)
		case strings.HasSuffix(r.URL.Path, "/get-dosage-notify"):
			// 告警端点也炸（含返回 HTML 的形态，与实测的 401 页一致）
			http.Error(w, `<html>500</html>`, 500)
		default:
			http.NotFound(w, r)
		}
	})

	res, _ := s.RefreshCredits("u1", triggerManual)
	if res.Status != "fail" {
		t.Fatalf("主流程结论不该被改变，得到 %s", res.Status)
	}
	if res.Detail == "" {
		t.Error("原始失败原因仍要保留")
	}
	if strings.Contains(res.Detail, "<html>") {
		t.Errorf("不该把 HTML 错误页塞进 Detail，得到 %q", res.Detail)
	}
}

// TestRefreshCreditsNoDosageOnSuccess 成功路径**不该**去问告警端点。
//
// 实测该端点比余额查询还慢（约 690ms vs 290ms），成功时去问纯属浪费。
func TestRefreshCreditsNoDosageOnSuccess(t *testing.T) {
	var dosageCalls atomic.Int32
	s := stubScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[
				{"PackageName":"p","CapacitySize":100,"CapacityRemain":42,"CapacityUsed":58}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/get-dosage-notify"):
			dosageCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"dosageNotifyCode":0}}`))
		default:
			http.NotFound(w, r)
		}
	})

	res, _ := s.RefreshCredits("u1", triggerManual)
	if res.Status != "ok" {
		t.Fatalf("前置：应成功，得到 %s（%s）", res.Status, res.Detail)
	}
	if dosageCalls.Load() != 0 {
		t.Errorf("成功路径不该查询告警端点（它更慢且平时静默），实际调用 %d 次", dosageCalls.Load())
	}
}

// TestRefreshCreditsDosageHintIsTruncated 超长文案要被压行 —— 它进历史文件。
func TestRefreshCreditsDosageHintIsTruncated(t *testing.T) {
	long := strings.Repeat("额度异常", 200) // 800 字
	s := stubScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			http.Error(w, `{"code":500,"msg":"boom"}`, 500)
		case strings.HasSuffix(r.URL.Path, "/get-dosage-notify"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"dosageNotifyCode":9,"dosageNotifyZh":"` + long + `"}}`))
		default:
			http.NotFound(w, r)
		}
	})

	res, _ := s.RefreshCredits("u1", triggerManual)
	if len(res.Detail) > 400 {
		t.Errorf("Detail 过长（%d 字符），应被 shortErr 压行", len(res.Detail))
	}
}
