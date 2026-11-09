package scheduler

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
)

// retryFloor 的定义在**产品文件** dailycheckin.go 里（测试文件里定义会让产品构建失败）。
// 它 = 30 分钟，理由见那处的注释。
// 本文件的测试只**读**它（断言"下限之内不重试"）。

// TestDailyCheckinJobRespectsRetryFloor **两次尝试之间的时间下限**（T-435）。
//
// # 为什么必须有这条
//
// T-430 的 TestDailyCheckinJobRetriesFailedUpstream 只断言了「第二轮要重试失败的」，
// **从没断言「两轮之间隔多久」** ⇒ 于是"每 30 秒重试一次"也能通过 ⇒ 线上事故就是这么漏的。
// 这条补的是**时间维度**，不是重复。
func TestDailyCheckinJobRespectsRetryFloor(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local)
	bad := &fakeCheckinProvider{id: "up-bad", status: checkinlog.StatusFail}
	j := NewDailyCheckinJob(fakeReg(t, bad), nil, nil)
	advance := withClock(t, t0)

	run := func(when time.Time, label string) int {
		t.Helper()
		advance(when)
		if err := j.Run(context.Background()); err != nil {
			t.Fatalf("%s Run 报错: %v", label, err)
		}
		return bad.calls
	}

	base := run(t0, "t0")
	if base != 1 {
		t.Fatalf("t0 首轮调用数 = %d，want 1", base)
	}
	// ★ 三道断言说的是同一件事：**太近就不许重试**
	if n := run(t0.Add(1*time.Minute), "t0+1min"); n != base {
		t.Errorf("t0+1min 又扫了一次（累计 %d）—— 被 retryFloor(%v) 挡住才对。"+
			" ★ 这就是 2026-11-09 的线上事故：失败的上游被每 40 秒重扫，"+
			"3 个失效号每天约 4300 次请求，而上游有风控", n, retryFloor)
	}
	if n := run(t0.Add(29*time.Minute), "t0+29min"); n != base {
		t.Errorf("t0+29min 又扫了一次（累计 %d）—— 仍在下限之内", n)
	}
	// 过了下限 ⇒ 必须重试（否则失败的上游当天再也不签，闸门就变成了"永不再试"）
	if n := run(t0.Add(31*time.Minute), "t0+31min"); n != base+1 {
		t.Errorf("t0+31min 调用数 = %d，want %d —— 过了 retryFloor 必须重试，"+
			"否则失败的上游当天再也不签", n, base+1)
	}
	// 再近一点又该被挡住（连续两轮的行为一致，不是"只挡一次"）
	if n := run(t0.Add(32*time.Minute), "t0+32min"); n != base+1 {
		t.Errorf("t0+32min 又扫了一次（累计 %d）—— 上一轮刚试过，应被挡住", n)
	}
}
