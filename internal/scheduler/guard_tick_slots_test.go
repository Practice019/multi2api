// guard_tick_slots_test.go 回归测试：**守卫线的轮询唤醒不得顺带跑整点槽位**。
//
// # 这条守的是一个实测事故（2026-09-13，用户在生产实例上看到）
//
// Run 的循环把两条时间线合成一次睡眠（见 scheduler.go 的 Run/onWake）：
//
//	next, slots := s.nextWake(now)   // next = 下一次整点；slots = 那一刻要跑的槽位
//	wake := min(next, now+jobTickInterval)
//
// 只要注册了任何守卫任务，wake 就会被 tick（30 秒）提前 —— 于是循环每 30 秒
// 醒一次。旧实现醒来后**无条件**执行 slots，而 slots 是"下一次整点要跑的槽位"
// （只要有槽位配置就永远非空，与"现在是不是整点"无关）。
//
// 后果（用户贴的日志逐字）：
//
//	checkin <uid>: upstream client (http 400): {"code":10001,"msg":"今天已签到，请明天再来"}
//	checkin ... 每 34 秒 × 3 个账号（一天约 2880 次/账号）
//	travel  <uid>: skip (daily limit reached)   ← 槽位钩子触发的，绕过它自己的 60s 下限
//
// 也就是说签到与猫猫旅行被 30 秒一次地打了一整天，而当日早就做完了。
package scheduler

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// registryWithAlwaysDueJob 造一个"注册了一个恒到期任务"的注册表。
//
// 为什么必须有它：只有 `s.jobs.Len() > 0` 时 Run 才会把唤醒点提前到 tick
// （见 runLoop 里的 wake 计算）—— 没有守卫任务时循环只等整点，
// 这条回归就永远复现不出被 tick 唤醒的那种形态。
func registryWithAlwaysDueJob(t *testing.T) *gateway.Registry {
	t.Helper()
	reg := gateway.NewRegistry()
	err := reg.Register(&stubProvider{id: "stub", jobs: []gateway.Job{{
		Name: "always-due",
		// Due 恒真：让守卫线每一轮都"有活干"。这里要的是"循环确实按 tick 醒"，
		// 判到期本身由 job_test.go 覆盖。
		Due: func(time.Time) bool { return true },
		Run: func(context.Context) error { return nil },
	}}})
	if err != nil {
		t.Fatalf("注册假上游失败: %v", err)
	}
	return reg
}

// nextHourFarEnough 返回一个"下一次到点至少还有 1 小时"的小时值。
//
// 为什么不写死一个数字：写死的话，测试恰好在那个整点前后 220 毫秒内运行就会
// 变成偶发红。取 (当前小时+2)%24 保证距离 >= 1 小时 + 1 秒 —— 远超下面的观测窗口，
// 于是"这一轮的唤醒只可能来自 tick"是**确定的**，不是碰运气。
func nextHourFarEnough(now time.Time) int {
	return (now.Hour() + 2) % 24
}

// TestGuardTickDoesNotRunSlots 被 tick 提前的唤醒**一个槽位都不许跑**。
//
// 变异验证（必须做）：把 onWake 里的 `if now.Before(next) { return }` 删掉，
// 本用例立刻变红（一轮 220ms 内会被调用约 10 次）。若删掉后仍绿，
// 说明这条用例没有真正盯住那个 bug。
func TestGuardTickDoesNotRunSlots(t *testing.T) {
	spy := &slotSpy{}
	s := New(Config{
		Slots:    []Slot{{Name: slotA, Hours: []int{nextHourFarEnough(time.Now())}}},
		Runner:   spy,
		Registry: registryWithAlwaysDueJob(t),
	})

	const window = 220 * time.Millisecond
	ctx, cancel := contextWithTimeout(t, window)
	defer cancel()

	start := time.Now()
	s.runLoop(ctx, 20*time.Millisecond) // tick 缩到 20ms，让"被提前唤醒"毫秒级复现
	elapsed := time.Since(start)

	if n := spy.count.Load(); n != 0 {
		t.Errorf("守卫线轮询触发了 %d 次槽位执行（期望 0）。\n"+
			"  后果：签到 + 搭车的猫猫旅行每 30 秒打一次上游，一天约 2880 次；\n"+
			"        上游只会回「今天已签到，请明天再来」，任务历史也被灌满。\n"+
			"  根因：slots 是「下一次整点要跑的槽位」，不是「现在该跑的槽位」 ——\n"+
			"        唤醒是被守卫线的 tick 提前的，此刻离 next 还远。", n)
	}
	if elapsed < window-40*time.Millisecond {
		t.Errorf("runLoop 只跑了 %v（窗口 %v）—— 它应当一直循环到 ctx 取消", elapsed, window)
	}
}

// TestOnWakeRunsSlotsOnlyAtTheirTime 唤醒处理的两个分支：
// 没到点什么都不做；到了点（含恰好等于）原样转发槽位名。
//
// 后一半同样重要：只把守卫加严而不跑槽位，等于把签到彻底关掉 ——
// 那是一条更严重的回归。
func TestOnWakeRunsSlotsOnlyAtTheirTime(t *testing.T) {
	spy := &slotSpy{}
	s := New(Config{Slots: twoSlots(), Runner: spy})
	next := time.Now().Add(time.Hour)
	slots := []slotAt{{at: next, name: slotB}}

	s.onWake(time.Now(), next, slots) // 守卫线轮询
	if n := spy.count.Load(); n != 0 {
		t.Errorf("未到整点就跑了 %d 次：会变成「每 tick 一次签到」", n)
	}

	s.onWake(next, next, slots) // 恰好到点也算到
	if n := spy.count.Load(); n != 1 {
		t.Fatalf("到点应执行 1 次，实际 %d 次 —— 签到会被彻底跑不到", n)
	}
	if len(spy.names) != 1 || spy.names[0] != slotB {
		t.Errorf("执行的槽位名 = %v，期望 [%s]（核心只做原样转发）", spy.names, slotB)
	}
	if spy.lastAt != triggerSchedule {
		t.Errorf("trigger=%q want %q", spy.lastAt, triggerSchedule)
	}
}
