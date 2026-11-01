package server

import (
	"testing"
	"time"
)

// TestBudgetStopsRotatingWhenTimeIsGone 行为的核心判据：
// 时间预算用尽后**不得**再换号（用户实测：单次尝试 40 秒，逐号重试会把总等待放大）。
//
// 用计数循环模拟池循环的判据形态（与 handler.go 里那段同构）：
//
//	for i := 0; i < maxAttempts; i++ {
//	    if i > 0 && !budget.ShouldTryAgain(maxAttempts-i) { break }
//	    attempts = i + 1
//	}
func TestBudgetStopsRotatingWhenTimeIsGone(t *testing.T) {
	// 干跑：预算充裕 ⇒ 用满 maxAttempts
	fresh := newAttemptBudget(time.Hour)
	attempts := 0
	for i := 0; i < 5; i++ {
		if i > 0 && !fresh.ShouldTryAgain(5-i, 0) {
			break
		}
		attempts = i + 1
	}
	if attempts != 5 {
		t.Errorf("预算充裕时应当用满 5 次尝试，实际 %d", attempts)
	}

	// 预算已耗尽 ⇒ 只允许第一次（第一次必须允许，否则小预算会让所有请求直接失败）
	tight := newAttemptBudget(1 * time.Millisecond)
	time.Sleep(3 * time.Millisecond)
	attempts2 := 0
	for i := 0; i < 5; i++ {
		if i > 0 && !tight.ShouldTryAgain(5-i, 0) {
			break
		}
		attempts2 = i + 1
	}
	if attempts2 != 1 {
		t.Errorf("预算耗尽后应当只试 1 次就停（不再逐号重试），实际 %d", attempts2)
	}
}
