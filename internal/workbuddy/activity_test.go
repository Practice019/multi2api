// activity_test.go 活跃上报：顺序、去重、排程门控。
//
// # 本文件最重要的一条是 TestAdoptReportsBeforeBuddyFirst
//
// 它钉住的是"领养恒失败"这个真 bug 的修复：
//
//	改造前 travelAdopt = 同意协议 + buddy/first（**没有上报**）
//	  → first_buddy 门槛永远不满足 → 400 → 当日跳过 → 明日再试 → 永远循环
//
// 只断言"上报被调用过"是不够的 —— 顺序也必须是上报在前，
// 因为 buddy/first 的门槛判定读的就是那个事件。放在后面等于没做。
package workbuddy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// activityStub 记录请求顺序与 /v2/report 的请求体。
type activityStub struct {
	mu       sync.Mutex
	order    []string // 依次收到的 "report" / "agreement" / "first"
	reports  []string // 每次 /v2/report 的 body
	failRepo bool     // 让 /v2/report 返回 500（测"上报失败不阻塞领养"）
}

func (s *activityStub) note(what string) {
	s.mu.Lock()
	s.order = append(s.order, what)
	s.mu.Unlock()
}

func (s *activityStub) sequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *activityStub) reportBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reports...)
}

func (s *activityStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/report"):
			s.note("report")
			s.mu.Lock()
			s.reports = append(s.reports, string(body))
			fail := s.failRepo
			s.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case strings.HasSuffix(r.URL.Path, "/buddy/agreement"):
			s.note("agreement")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case strings.HasSuffix(r.URL.Path, "/buddy/first"):
			s.note("first")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/buddy/info"):
			// 无猫 → 触发 travelAdopt。
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	})
}

// TestAdoptReportsBeforeBuddyFirst 领养必须先补活跃上报，再走协议与领养。
//
// # 这是"领养恒失败"的真因与修复
//
// 反向判别力：把 travelAdopt 里的 `p.ensureAdoptPrereq(a)` 删掉
// （即回到改造前的形态），顺序会变成 [agreement, first]，本用例立刻红。
func TestAdoptReportsBeforeBuddyFirst(t *testing.T) {
	stub := &activityStub{}
	s, _ := newTestProvider(t, stubServer(t, stub.handler()), testAuth("u1"))

	s.RunTravelNow()

	got := stub.sequence()
	want := []string{"report", "agreement", "first"}
	if len(got) != len(want) {
		t.Fatalf("请求序列 = %v，期望 %v\n"+
			"  ← 缺 report 说明领养前置没补上（first_buddy 门槛将永远不满足，"+
			"领养恒 400，且 first_buddy 是其余 17 个任务的前置）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 步 = %q，期望 %q（全部序列 %v）—— "+
				"顺序必须是「上报 → 协议 → 领养」：buddy/first 的门槛判定读的就是那个上报事件",
				i, got[i], want[i], got)
		}
	}
}

// TestAdoptReportCarriesUserID 上报必须带 userId（缺它上游 200 静默丢弃）。
//
// 这条是"上报了但没效果"的唯一防线：静默丢弃没有任何错误信号，
// 只能靠请求体形状本身来钉。
func TestAdoptReportCarriesUserID(t *testing.T) {
	stub := &activityStub{}
	s, _ := newTestProvider(t, stubServer(t, stub.handler()), testAuth("u-adopt"))

	s.RunTravelNow()

	bodies := stub.reportBodies()
	if len(bodies) == 0 {
		t.Fatal("没有发出任何 /v2/report 请求")
	}
	for i, b := range bodies {
		var events []map[string]any
		if err := json.Unmarshal([]byte(b), &events); err != nil {
			t.Fatalf("第 %d 次上报不是数组（上游收数组）：%v; body=%s", i, err, b)
		}
		if len(events) != 1 {
			t.Fatalf("第 %d 次上报含 %d 个事件，期望 1", i, len(events))
		}
		if got, _ := events[0]["userId"].(string); got != "u-adopt" {
			t.Errorf("第 %d 次上报 userId=%q，期望 u-adopt —— "+
				"缺它上游返回 200 但**静默丢弃**（上报成功、连登不涨、领养不解锁，"+
				"而日志上什么都看不出来）", i, got)
		}
		if got, _ := events[0]["eventCode"].(string); got != "chat_request_send" {
			t.Errorf("第 %d 次上报 eventCode=%q，期望 chat_request_send", i, got)
		}
	}
}

// TestAdoptContinuesWhenReportFails 上报失败**不阻塞**领养。
//
// 若该账号的门槛此前已被其它途径满足（用户在客户端里聊过），
// 领养仍会成功。"上报失败就不领养"会白白浪费一次机会。
func TestAdoptContinuesWhenReportFails(t *testing.T) {
	stub := &activityStub{failRepo: true}
	s, _ := newTestProvider(t, stubServer(t, stub.handler()), testAuth("u1"))

	s.RunTravelNow()

	got := stub.sequence()
	sawFirst := false
	for _, x := range got {
		if x == "first" {
			sawFirst = true
		}
	}
	if !sawFirst {
		t.Fatalf("上报失败时仍应继续尝试领养，实际序列 = %v", got)
	}
}

// ── 排程门控 ────────────────────────────────────────────────────────────

// TestActivityDisabledByDefault 未配置时点 → 不启用。
//
// 这是"新增能力必须 opt-in"的载体：老部署升级后不能自动开始发上游请求。
func TestActivityDisabledByDefault(t *testing.T) {
	s, _ := newTestProvider(t, stubServer(t, (&activityStub{}).handler()), testAuth("u1"))

	if s.ActivityEnabled() {
		t.Error("未配置 activity_hours 时不该启用（新增能力必须 opt-in，" +
			"否则老部署升级后会对每个账号自动发请求）")
	}
	if s.activityDueJob(time.Now()) {
		t.Error("未启用时 Due 必须为 false")
	}
}

// TestActivityDueOnlyInsideWindow 只在配置的整点窗口内到期。
func TestActivityDueOnlyInsideWindow(t *testing.T) {
	s, _ := newTestProvider(t, stubServer(t, (&activityStub{}).handler()), testAuth("u1"))
	s.SetActivitySchedule([]int{10}, true)

	if !s.ActivityEnabled() {
		t.Fatal("配了时点应启用")
	}
	// 窗口内。
	in := time.Date(2026, 9, 14, 10, 30, 0, 0, time.Local)
	if !s.activityDueJob(in) {
		t.Error("10 点窗口内应到期")
	}
	// 窗口外。
	out := time.Date(2026, 9, 14, 11, 30, 0, 0, time.Local)
	if s.activityDueJob(out) {
		t.Error("11 点不在配置的窗口内，不该到期")
	}
}

// TestActivityDueOncePerDay 同一自然日只跑一次。
func TestActivityDueOncePerDay(t *testing.T) {
	s, _ := newTestProvider(t, stubServer(t, (&activityStub{}).handler()), testAuth("u1"))
	s.SetActivitySchedule([]int{10}, true)

	day1 := time.Date(2026, 9, 14, 10, 5, 0, 0, time.Local)
	if !s.activityDueJob(day1) {
		t.Fatal("首次应到期")
	}
	s.markActivityRan(day1)
	if s.activityDueJob(day1.Add(20 * time.Minute)) {
		t.Error("同一自然日（窗口内）不该重复到期 —— " +
			"日活跃奖励按天去重，重复上报没有额外收益")
	}
	// 次日窗口内应再次到期。
	day2 := time.Date(2026, 9, 15, 10, 5, 0, 0, time.Local)
	if !s.activityDueJob(day2) {
		t.Error("换到次日应再次到期")
	}
}

// TestActivityExplicitDisableKeepsHours 显式关闭不擦除时点。
//
// 与签到的开关语义一致：改回 true 即恢复原时点，无需补配。
func TestActivityExplicitDisableKeepsHours(t *testing.T) {
	s, _ := newTestProvider(t, stubServer(t, (&activityStub{}).handler()), testAuth("u1"))
	s.SetActivitySchedule([]int{10, 18}, true)
	s.SetActivitySchedule(nil, false) // 只关开关，不动时点

	if s.ActivityEnabled() {
		t.Error("显式关闭后不该启用")
	}
	if got := s.ActivityHours(); len(got) != 2 {
		t.Errorf("关闭不该擦除时点，得到 %v", got)
	}
	s.SetActivitySchedule(nil, true)
	if !s.ActivityEnabled() {
		t.Error("重新打开应恢复原时点")
	}
}

// TestActivityPerAccountDedup 同一账号当日只上报一次。
func TestActivityPerAccountDedup(t *testing.T) {
	stub := &activityStub{}
	s, _ := newTestProvider(t, stubServer(t, stub.handler()), testAuth("u1"))

	s.RunActivityNow()
	s.RunActivityNow() // 第二轮应全部跳过

	if n := len(stub.reportBodies()); n != 1 {
		t.Errorf("同账号同日上报 %d 次，期望 1 —— 重复上报没有额外收益，只增加风控画像", n)
	}
}

// TestActivityDisabledAccountsSkipped 禁用账号不上报。
func TestActivityDisabledAccountsSkipped(t *testing.T) {
	stub := &activityStub{}
	s, p := newTestProvider(t, stubServer(t, stub.handler()), testAuth("u1"))
	p.Disable("u1", "test")

	s.RunActivityNow()

	if n := len(stub.reportBodies()); n != 0 {
		t.Errorf("禁用账号不该被上报，实际 %d 次", n)
	}
}
