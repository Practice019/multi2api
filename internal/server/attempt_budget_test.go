package server

import (
	"testing"
	"time"
)

// TestAttemptBudgetDefaults 预算默认值与非正输入。
func TestAttemptBudgetDefaults(t *testing.T) {
	if got := newAttemptBudget(0).Limit(); got != DefaultAttemptBudget {
		t.Errorf("limit<=0 应当回落到默认值，得到 %v", got)
	}
	if got := newAttemptBudget(-5 * time.Second).Limit(); got != DefaultAttemptBudget {
		t.Errorf("负数也该回落，得到 %v", got)
	}
	if got := newAttemptBudget(3 * time.Second).Limit(); got != 3*time.Second {
		t.Errorf("显式值应当被尊重，得到 %v", got)
	}
}

// TestAttemptBudgetExhaustsByTime 时间一到就必须报告用尽（这是本文件的核心判据）。
func TestAttemptBudgetExhaustsByTime(t *testing.T) {
	b := newAttemptBudget(1 * time.Millisecond)
	if b.Exhausted() {
		t.Error("刚构造就不该判定用尽（除非预算为 0）")
	}
	time.Sleep(3 * time.Millisecond)
	if !b.Exhausted() {
		t.Error("超过预算后必须报告用尽 —— 否则上游整体超时会被逐号重试放大")
	}
}

// TestAttemptBudgetShouldTryAgain 次数与时间是**与**关系：任一用尽就不该再试。
func TestAttemptBudgetShouldTryAgain(t *testing.T) {
	fresh := newAttemptBudget(time.Hour)
	if !fresh.ShouldTryAgain(3, 0) {
		t.Error("时间充裕 + 还有次数 ⇒ 应该再试")
	}
	if fresh.ShouldTryAgain(0, 0) {
		t.Error("次数用尽 ⇒ 不该再试（哪怕时间充裕）")
	}
	tight := newAttemptBudget(1 * time.Millisecond)
	time.Sleep(3 * time.Millisecond)
	if tight.ShouldTryAgain(99, 0) {
		t.Error("时间用尽 ⇒ 不该再试（哪怕次数很多）")
	}
}

// TestAttemptBudgetNilSafe nil 接收者不得 panic（池循环里可能没装配）。
func TestAttemptBudgetNilSafe(t *testing.T) {
	var b *attemptBudget
	if b.Exhausted() {
		t.Error("nil 应当视为不限制")
	}
	if b.ShouldTryAgain(1, 0) != true {
		t.Error("nil + 有次数 ⇒ 继续")
	}
	if b.Spent() != 0 || b.Limit() != 0 {
		t.Error("nil 的观测值应为 0")
	}
}
