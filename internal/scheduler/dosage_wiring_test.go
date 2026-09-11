package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

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
//
// 除了长度，还必须断言**截断后仍是合法 UTF-8**。
// 原实现 `s[:120]` 按字节切，中文 3 字节/字符极易切在中间，
// 产生非法 UTF-8，落进历史后再序列化就变成 \ufffd（"�"）。
// 当初这条测试只查长度、不查编码，所以缺陷溜过去了 —— 由独立评审复现（6 组样本 3 组中招）。
func TestRefreshCreditsDosageHintIsTruncated(t *testing.T) {
	long := strings.Repeat("额度异常", 200) // 2400 字节
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
		t.Errorf("Detail 过长（%d 字节），应被 shortErr 压行", len(res.Detail))
	}
	if !utf8.ValidString(res.Detail) {
		t.Errorf("Detail 不是合法 UTF-8（截断切在字符中间）: %q", res.Detail)
	}
	// 落盘要经 JSON 序列化，那里最能暴露非法编码
	b, err := json.Marshal(map[string]string{"detail": res.Detail})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `\ufffd`) {
		t.Errorf("序列化后出现替换字符（非法 UTF-8 的典型症状）: %s", string(b))
	}
}

// shortErr 直接覆盖：各种字节对齐下的中文截断都必须产出合法 UTF-8。
//
// 这是上面那条的"纯函数版"，覆盖 120 字节边界上的所有偏移（UTF-8 单字符最多 3 字节，
// 所以偏移 0/1/2 三种对齐都要试）。
func TestShortErrTruncatesOnRuneBoundary(t *testing.T) {
	for pad := 0; pad < 4; pad++ {
		// 用 ASCII 前缀把中文推到不同的字节对齐位置
		s := strings.Repeat("a", pad) + strings.Repeat("额度异常", 100)
		got := shortErr(fmt.Errorf("%s", s))
		if !utf8.ValidString(got) {
			t.Errorf("pad=%d: 截断结果非法 UTF-8: %q", pad, got)
		}
		if len(got) > 120 {
			t.Errorf("pad=%d: 长度 %d 超过上限 120", pad, len(got))
		}
		if len(got) < 100 {
			t.Errorf("pad=%d: 只保留 %d 字节，退让过多（应尽量接近 120）", pad, len(got))
		}
	}
	// 换行仍要被压平
	if got := shortErr(fmt.Errorf("a\nb\r\nc")); strings.ContainsAny(got, "\n\r") {
		t.Errorf("换行应被替换为空格: %q", got)
	}
	// nil 与短串
	if got := shortErr(nil); got != "" {
		t.Errorf("nil 应返回空串，得到 %q", got)
	}
	if got := shortErr(fmt.Errorf("short")); got != "short" {
		t.Errorf("短串不该被改: %q", got)
	}
}
