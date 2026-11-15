// job_status_note_test.go 调度状态里「为什么没跑」的判据。
//
// # 它修的是什么
//
// 状态页那两行不含原因：
//
//	workbuddy-activity    「还没有执行记录」   ← 其实它是默认关闭的（未配时点就不跑）
//	workbuddy-travel-watch「约已过 11 分」      ← 其实它在等账号到期（按需触发）
//
// 两句都会让人去做一次没有必要的排查。所以 `Status` 加了 `Due` 与 `Note`。
package scheduler

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// ★ ① Note 必须区分六种情形（不许一句话糊过去）。
func TestStatusNoteDistinguishes(t *testing.T) {
	onDemand := gateway.Job{Name: "x", Due: func(time.Time) bool { return false }}
	interval := gateway.Job{Name: "y", Interval: time.Minute}

	cases := []struct {
		lastRun string
		due     bool
		job     gateway.Job
		want    string
	}{
		{"", true, onDemand, "当前已到执行条件"},
		{"", false, onDemand, "按需触发，当前条件未满足"},
		{"", false, interval, "还没有执行过"},
		{"2026-01-01T00:00:00Z", true, onDemand, "当前已到执行条件"},
		{"2026-01-01T00:00:00Z", false, onDemand, "按需触发，当前条件未满足"},
		{"2026-01-01T00:00:00Z", false, interval, "间隔任务，等待下一轮"},
	}
	for _, c := range cases {
		got := statusNote(Status{LastRun: c.lastRun, Due: c.due}, c.job)
		if !strings.Contains(got, c.want) {
			t.Fatalf("lastRun=%q due=%v onDemand=%v ⇒ %q，want 含 %q",
				c.lastRun, c.due, c.job.Due != nil, got, c.want)
		}
	}
}

// ★ ② 从未执行 + 按需 + 未到条件 ⇒ 必须点出"未启用或未到窗口"。
//
// 这条正是 workbuddy-activity 的形状：默认关闭（未配时点），
// 界面显示"还没有执行记录"。不解释的话运维会以为坏了。
func TestStatusNoteExplainsNeverRanOnDemand(t *testing.T) {
	job := gateway.Job{Name: "workbuddy-activity", Due: func(time.Time) bool { return false }}
	got := statusNote(Status{LastRun: "", Due: false}, job)
	if !strings.Contains(got, "未启用") && !strings.Contains(got, "未到窗口") {
		t.Fatalf("★ 应点出「未启用或未到窗口」，实际 %q —— "+
			"只有「还没有执行记录」会让人以为坏了", got)
	}
}

// ★ ③ 已跑过 + 按需 + 未到条件 ⇒ 必须点出"等待账号到期"。
//
// 这条是 workbuddy-travel-watch 的形状：按 arrive_at 错峰，可能几小时没账号到期。
func TestStatusNoteExplainsOnDemandWaiting(t *testing.T) {
	job := gateway.Job{Name: "workbuddy-travel-watch", Due: func(time.Time) bool { return false }}
	got := statusNote(Status{LastRun: "2026-01-01T00:00:00Z", Due: false}, job)
	if !strings.Contains(got, "等待账号到期") && !strings.Contains(got, "时刻窗口") {
		t.Fatalf("★ 应点出「等待账号到期或时刻窗口」，实际 %q —— "+
			"只有「已过 N 分」会让人以为卡住了", got)
	}
}

// ★ ④ safeDue 吞掉 panic（一个上游的 bug 不该让整页状态 500）。
func TestSafeDueSwallowsPanic(t *testing.T) {
	job := gateway.Job{Name: "boom", Due: func(time.Time) bool { panic("nil map") }}
	if got := safeDue(job, time.Now()); got {
		t.Fatal("panic 时应返回 false")
	}
}

// ⑤ 没有 Due 的固定间隔任务 ⇒ safeDue 报 true（不存在"到不到条件"）。
func TestSafeDueWithoutDueFunc(t *testing.T) {
	job := gateway.Job{Name: "plain", Interval: time.Minute}
	if !safeDue(job, time.Now()) {
		t.Fatal("固定间隔任务应返回 true")
	}
}

// ⑥ Due 现算，不是缓存的。
func TestSafeDueEvaluatesLive(t *testing.T) {
	n := 0
	job := gateway.Job{Name: "counter", Due: func(time.Time) bool { n++; return n%2 == 1 }}
	if !safeDue(job, time.Now()) {
		t.Fatal("第 1 次应为 true")
	}
	if safeDue(job, time.Now()) {
		t.Fatal("★ 第 2 次仍为 true —— Due 没有现算")
	}
}
