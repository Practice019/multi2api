// slots_test.go 槽位框架的测试：执行、钩子、运行时覆盖。
//
// 本文件覆盖的是**核心的框架行为**（到点喊谁、喊完喊钩子、覆盖值怎么生效），
// 不涉及任何具体上游业务 —— 那部分测试在 internal/workbuddy。
package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// slotSpy 一个记录调用的假 SlotRunner。
type slotSpy struct {
	count  atomic.Int32
	names  []string
	lastAt string
}

func (s *slotSpy) RunSlot(name, trigger string) {
	s.count.Add(1)
	s.names = append(s.names, name)
	s.lastAt = trigger
}

func (s *slotSpy) RunSlotFor(name, uid, trigger string) (TaskResult, bool) {
	s.count.Add(1)
	s.names = append(s.names, name)
	return TaskResult{UID: uid, Status: "ok"}, true
}

// contextWithTimeout 是 context.WithTimeout 的薄封装（测试里少写一个 import）。
func contextWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), d)
}

// TestRunSlotForwardsToRunner 到点执行的行为：核心把槽位名**原样**交给 Runner。
//
// 这条钉住"核心不认识槽位名"：核心做的只有转发，没有 switch。
func TestRunSlotForwardsToRunner(t *testing.T) {
	spy := &slotSpy{}
	s := New(Config{Slots: twoSlots(), Runner: spy})
	s.RunSlot(slotB, triggerSchedule)

	if spy.count.Load() != 1 {
		t.Fatalf("runner 应被调用 1 次，实际 %d", spy.count.Load())
	}
	if len(spy.names) != 1 || spy.names[0] != slotB {
		t.Errorf("核心应原样转发槽位名 %q，实际 %v", slotB, spy.names)
	}
	if spy.lastAt != triggerSchedule {
		t.Errorf("trigger=%q want %q", spy.lastAt, triggerSchedule)
	}
}

// TestRunSlotForUnknownNameStillForwards 核心不认识的槽位名也照传 ——
// 认名字是上游的事（它自己的任务清单由它自己认）。
func TestRunSlotForUnknownNameStillForwards(t *testing.T) {
	spy := &slotSpy{}
	s := New(Config{Slots: twoSlots(), Runner: spy})
	s.RunSlot("something-core-never-heard-of", triggerManual)

	if spy.count.Load() != 1 {
		t.Fatalf("未知槽位名也必须转发（核心不做判断），实际调用 %d 次", spy.count.Load())
	}
	if spy.names[0] != "something-core-never-heard-of" {
		t.Errorf("名字应原样传递，实际 %q", spy.names[0])
	}
}

// TestRunSlotWithoutRunnerDoesNotPanic 未接 Runner 时到点只记录，不 panic。
func TestRunSlotWithoutRunnerDoesNotPanic(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	s.RunSlot(slotA, triggerSchedule) // 不应 panic
}

// TestRunSlotForReportsMissingAccount RunSlotFor 把上游的 ok 原样返回。
func TestRunSlotForReportsMissingAccount(t *testing.T) {
	spy := &slotSpy{}
	s := New(Config{Slots: twoSlots(), Runner: spy})
	res, ok := s.RunSlotFor(slotA, "u1", triggerManual)
	if !ok || res.UID != "u1" {
		t.Errorf("res=%+v ok=%v want u1/true", res, ok)
	}
	if spy.count.Load() != 1 {
		t.Errorf("runner 调用次数=%d want 1", spy.count.Load())
	}
}

// TestRunSlotForWithoutRunner 未接 Runner 时 ok=false（管理台据此回 404）。
func TestRunSlotForWithoutRunner(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	if _, ok := s.RunSlotFor(slotA, "u1", triggerManual); ok {
		t.Error("未接 Runner 时应 ok=false")
	}
}

// ---------------------------------------------------------------------------
// 搭车钩子
// ---------------------------------------------------------------------------

// stubSlotHook 记录被调用的假钩子。
type stubSlotHook struct {
	n       atomic.Int32
	slots   []string
	name    string
	panicOn bool
}

func (h *stubSlotHook) AfterSlot(slot string) {
	h.n.Add(1)
	h.slots = append(h.slots, slot)
	if h.panicOn {
		panic("钩子炸了")
	}
}

func (h *stubSlotHook) HookName() string { return h.name }

// TestSlotRunsUpstreamHook 核心跑完槽位后必须喊一次钩子，并把槽位名告诉它。
//
// 「签到时顺带旅行」这半条链路（核心确实会在槽位收尾时通知上游）由本用例覆盖；
// 另一半（上游收到通知后确实只在签到那次推进旅行）在
// internal/workbuddy 的 TestAfterSlotOnlyFiresOnCheckin 里。
func TestSlotRunsUpstreamHook(t *testing.T) {
	spy := &slotSpy{}
	s := New(Config{Slots: twoSlots(), Runner: spy})
	h := &stubSlotHook{name: "travel"}
	s.AddSlotHook(h)

	s.RunSlot(slotA, triggerSchedule)

	if h.n.Load() != 1 {
		t.Errorf("槽位收尾应恰好喊一次钩子，实际 %d 次", h.n.Load())
	}
	if len(h.slots) != 1 || h.slots[0] != slotA {
		t.Errorf("钩子应拿到槽位名 %q，实际 %v", slotA, h.slots)
	}
	if spy.count.Load() != 1 {
		t.Errorf("槽位本身仍要执行，实际 %d 次", spy.count.Load())
	}
}

// TestSlotHookPanicDoesNotAbort 钩子 panic 不影响槽位主流程，也不影响其它钩子。
func TestSlotHookPanicDoesNotAbort(t *testing.T) {
	spy := &slotSpy{}
	s := New(Config{Slots: twoSlots(), Runner: spy})
	bad := &stubSlotHook{name: "bad", panicOn: true}
	good := &stubSlotHook{name: "good"}
	s.AddSlotHook(bad)
	s.AddSlotHook(good)

	s.RunSlot(slotA, triggerSchedule) // 不应 panic

	if good.n.Load() != 1 {
		t.Errorf("前一个钩子 panic 不该阻止后续钩子，实际 %d 次", good.n.Load())
	}
	if spy.count.Load() != 1 {
		t.Errorf("钩子 panic 不该影响槽位执行，实际 %d 次", spy.count.Load())
	}
}

// TestAddSlotHookIgnoresNil nil 钩子被静默忽略（不会在收尾时 panic）。
func TestAddSlotHookIgnoresNil(t *testing.T) {
	s := New(Config{Slots: twoSlots(), Runner: &slotSpy{}})
	s.AddSlotHook(nil)
	s.RunSlot(slotA, triggerSchedule) // 不应 panic

	if len(s.hooks) != 0 {
		t.Errorf("nil 钩子不该被登记，实际 %d 个", len(s.hooks))
	}
}

// ---------------------------------------------------------------------------
// 运行时覆盖（设置页用）
// ---------------------------------------------------------------------------

// TestSlotEnabledOverride 运行时开关立即影响排程。
func TestSlotEnabledOverride(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	if !s.SlotEnabled(slotA) || !s.SlotEnabled(slotB) {
		t.Fatal("前置：两个槽位初始都应启用")
	}
	s.SetSlotEnabled(slotA, false)
	if s.SlotEnabled(slotA) {
		t.Error("停用后应为 false")
	}
	if !s.SlotEnabled(slotB) {
		t.Error("停用 A 不该影响 B")
	}
	at, slots := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（A 已停用）", at, want)
	}
	if len(slots) != 1 || slots[0].name != slotB {
		t.Errorf("slots=%v want [%s]", slots, slotB)
	}
}

// TestSetSlotHoursRejectsOutOfRange 非法小时返回错误且不生效。
func TestSetSlotHoursRejectsOutOfRange(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	if err := s.SetSlotHours(slotA, []int{25}); err == nil {
		t.Error("25 点应被拒绝")
	}
	if err := s.SetSlotHours(slotA, []int{-1}); err == nil {
		t.Error("-1 点应被拒绝")
	}
	if got := s.SlotHours(slotA); len(got) != 1 || got[0] != 9 {
		t.Errorf("被拒绝的写入不该生效，实际 %v", got)
	}
	// 边界值必须接受
	if err := s.SetSlotHours(slotA, []int{0, 23}); err != nil {
		t.Errorf("0 与 23 都应合法，得到 %v", err)
	}
}

// TestSetSlotHoursEmptyIsNoop 空切片视为「不改」（管理台不传就是不动它）。
func TestSetSlotHoursEmptyIsNoop(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	if err := s.SetSlotHours(slotA, nil); err != nil {
		t.Fatalf("空切片不该报错: %v", err)
	}
	if got := s.SlotHours(slotA); len(got) != 1 || got[0] != 9 {
		t.Errorf("空切片不该改动时点，实际 %v", got)
	}
}

// TestUnknownSlotIsIgnored 未定义的槽位名：开关/改时点静默忽略（没有对象可改）。
func TestUnknownSlotIsIgnored(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	if s.SlotEnabled("nope") {
		t.Error("未定义的槽位不该报告为启用")
	}
	s.SetSlotEnabled("nope", true) // 不应 panic
	if err := s.SetSlotHours("nope", []int{25}); err == nil {
		t.Error("越界校验应独立于槽位是否存在")
	}
	if got := s.SlotHours("nope"); got != nil {
		t.Errorf("未定义槽位的时点应为 nil，实际 %v", got)
	}
}

// TestSlotsStatusReflectsOverrides 管理台读到的状态反映运行时覆盖。
func TestSlotsStatusReflectsOverrides(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	s.SetSlotEnabled(slotA, false)
	if err := s.SetSlotHours(slotB, []int{3, 4}); err != nil {
		t.Fatal(err)
	}
	st := s.SlotsStatus()
	if len(st) != 2 {
		t.Fatalf("槽位数=%d want 2", len(st))
	}
	// 顺序 = 装配顺序（稳定）
	if st[0].Name != slotA || st[1].Name != slotB {
		t.Fatalf("顺序应稳定，实际 %v/%v", st[0].Name, st[1].Name)
	}
	if st[0].Enabled {
		t.Error("A 应显示为停用")
	}
	if len(st[1].Hours) != 2 || st[1].Hours[0] != 3 {
		t.Errorf("B 的时点应为覆盖后的 [3 4]，实际 %v", st[1].Hours)
	}
}

// TestSlotHoursReturnsCopy 读到的时点是拷贝，改它不影响内部状态。
func TestSlotHoursReturnsCopy(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	got := s.SlotHours(slotA)
	got[0] = 99
	if again := s.SlotHours(slotA); again[0] != 9 {
		t.Errorf("外部改动泄漏进内部状态：%v", again)
	}
}
