package scheduler

import (
	"testing"
	"time"
)

// 本文件是**排程框架**的测试：时点计算与槽位唤醒。
//
// 用例名沿用改造前（那时只有签到/保活两个内置任务），断言逐字保留 ——
// 它们是"搬运没有改行为"的证据。唯一的改动是构造方式：
// 原先写 CheckinHours/KeepaliveHours/CheckinDisabled/KeepaliveDisabled，
// 现在写等价的 Slots 定义（同一个时点、同一个启停，只是不再由核心命名）。

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// 两个测试槽位名。核心不认识它们的业务含义 —— 这里刻意用中性名字，
// 避免测试把"核心知道签到"这件事固化回来。
const (
	slotA = "slot-a"
	slotB = "slot-b"
)

// twoSlots 造一份"A 配 9 点、B 配 22 点"的槽位定义。
func twoSlots() []Slot {
	return []Slot{
		{Name: slotA, Hours: []int{9}},
		{Name: slotB, Hours: []int{22}},
	}
}

// TestNextWakeKeepaliveOnly A 已过点时按 B 的整点唤醒。
func TestNextWakeKeepaliveOnly(t *testing.T) {
	s := New(Config{Slots: twoSlots()})
	at, slots := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(slots) != 1 || slots[0].name != slotB {
		t.Errorf("slots=%v want [%s]", slots, slotB)
	}
}

// TestNextWakeSameInstantFiresAll A 与 B 配到同一整点时两个都要执行。
func TestNextWakeSameInstantFiresAll(t *testing.T) {
	s := New(Config{Slots: []Slot{
		{Name: slotA, Hours: []int{9, 22}},
		{Name: slotB, Hours: []int{22}},
	}})
	at, slots := s.nextWake(time.Date(2026, 9, 11, 21, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasSlot(slots, slotA) || !hasSlot(slots, slotB) {
		t.Errorf("slots=%v want A+B（同一时刻两任务）", slots)
	}

	// 只剩 A：22 点过后下一次是次日 09:00，且只含 A。
	at, slots = s.nextWake(time.Date(2026, 9, 11, 22, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(slots) != 1 || slots[0].name != slotA {
		t.Errorf("slots=%v want [%s]", slots, slotA)
	}
}

// TestNextWakeNothingScheduled 无槽位时时点为空，Run 只等退出信号。
func TestNextWakeNothingScheduled(t *testing.T) {
	s := &Scheduler{cfg: Config{}}
	at, slots := s.nextWake(time.Now())
	if !at.IsZero() || len(slots) != 0 {
		t.Errorf("at=%v slots=%v want zero/nil", at, slots)
	}
}

// TestNextWakeCheckinDisabled 显式停用 A 后，排程里不再有它的时点（B 照常）。
func TestNextWakeCheckinDisabled(t *testing.T) {
	s := New(Config{Slots: []Slot{
		{Name: slotA, Hours: []int{9, 21}, Disabled: true},
		{Name: slotB, Hours: []int{22}},
	}})
	at, slots := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 21 点的 A）", at, want)
	}
	if len(slots) != 1 || slots[0].name != slotB {
		t.Errorf("slots=%v want [%s]", slots, slotB)
	}
}

// TestNextWakeKeepaliveDisabled 显式停用 B 后，排程里不再有它的时点（A 照常）。
func TestNextWakeKeepaliveDisabled(t *testing.T) {
	s := New(Config{Slots: []Slot{
		{Name: slotA, Hours: []int{9, 21}},
		{Name: slotB, Hours: []int{22}, Disabled: true},
	}})
	at, slots := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（不应再有 22 点的 B）", at, want)
	}
	if len(slots) != 1 || slots[0].name != slotA {
		t.Errorf("slots=%v want [%s]", slots, slotA)
	}
}

// TestNextWakeBothDisabledNothingScheduled 全部停用 → 无可唤醒时点。
func TestNextWakeBothDisabledNothingScheduled(t *testing.T) {
	s := New(Config{Slots: []Slot{
		{Name: slotA, Hours: []int{9, 21}, Disabled: true},
		{Name: slotB, Hours: []int{22}, Disabled: true},
	}})
	at, slots := s.nextWake(time.Now())
	if !at.IsZero() || len(slots) != 0 {
		t.Errorf("at=%v slots=%v want zero/nil", at, slots)
	}
}

// TestRunAllDisabledNoSpinNoCalls 全部停用：Run 不空转（只等退出信号），
// 且不触发任何槽位执行。
func TestRunAllDisabledNoSpinNoCalls(t *testing.T) {
	spy := &slotSpy{}
	s := New(Config{
		Slots: []Slot{
			{Name: slotA, Hours: []int{9, 21}, Disabled: true},
			{Name: slotB, Hours: []int{22}, Disabled: true},
		},
		Runner: spy,
	})

	ctx, cancel := contextWithTimeout(t, 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Run(ctx) // 阻塞到 ctx 取消为止（无时点可等，不构造 timer）
	elapsed := time.Since(start)

	if n := spy.count.Load(); n != 0 {
		t.Errorf("slot runs=%d want 0（全部停用 → 不执行）", n)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("Run returned after %v, before ctx done（不应提前返回）", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v（不应空转/忙等）", elapsed)
	}
}

// hasSlot 判定某个槽位名在不在唤醒列表里。
func hasSlot(slots []slotAt, name string) bool {
	for _, v := range slots {
		if v.name == name {
			return true
		}
	}
	return false
}
