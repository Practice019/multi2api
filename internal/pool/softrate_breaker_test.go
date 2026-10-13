// softrate_breaker_test.go 钉住「软限流（429）不触发熔断」这条契约。
//
// # 这是一条真 bug 的回归钉（注释与实现不一致）
//
// pool.go 的注释一直声称 softStreak 与 fails 是"两条并行的升级线，计数器独立"，
// 但实现里 cooldownUntil / CooldownSoftForModel 都无条件调用
// recordBreakerFailureLocked —— 于是 3 次零散 429（横跨几小时、账号健康）
// 就能凑满 breakerThreshold 把号封 30 分钟（retryCount>0 时倍增到 6h）。
// 用户侧症状：余额充足、响应正常的号突然从池子里消失半小时。
//
// 本文件把"429 只走软限流退避、不动熔断"钉成契约 —— 谁把那一行加回去，这里先红。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestSoftRateDoesNotFeedBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 默认 breakerThreshold=3：旧实现三次软冷却即触发熔断。
	for i := 0; i < 3; i++ {
		p.Cooldown("u1", CoolSoft, 60*time.Second, "429 rate limited")
	}
	p.mu.RLock()
	e := p.byUID["u1"]
	fails, breakerUntil, softStreak := e.fails, e.breakerUntil, e.softStreak
	p.mu.RUnlock()

	if fails != 0 {
		t.Errorf("429 不得污染熔断失败计数：fails=%d want 0", fails)
	}
	if !breakerUntil.IsZero() {
		t.Errorf("429 不得触发熔断：breakerUntil=%v want 零值", breakerUntil)
	}
	if softStreak != 3 {
		t.Errorf("软限流自己的计数器应照常累计：softStreak=%d want 3（指数退避仍生效）", softStreak)
	}
}

// TestModelSoftRateDoesNotFeedBreaker 模型级软限流（6004）同样不喂熔断。
func TestModelSoftRateDoesNotFeedBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Second)
	for i := 0; i < 3; i++ {
		p.CooldownSoftForModel("u1", 60*time.Second, reset, "gpt-5.5", "6004 模型级限流")
	}
	p.mu.RLock()
	e := p.byUID["u1"]
	fails, breakerUntil := e.fails, e.breakerUntil
	p.mu.RUnlock()
	if fails != 0 || !breakerUntil.IsZero() {
		t.Errorf("模型级软限流不得喂熔断：fails=%d breakerUntil=%v", fails, breakerUntil)
	}
}

// TestServerErrorStillFeedsBreaker 反向守护：真正的 5xx/网络错误**必须**仍然
// 喂熔断（别把解耦改成"谁都不喂"）。
func TestServerErrorStillFeedsBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Minute, time.Minute) // 阈值 1：一次即熔断
	p.NoteError("u1")
	p.mu.RLock()
	until := p.byUID["u1"].breakerUntil
	p.mu.RUnlock()
	if until.IsZero() {
		t.Error("5xx/网络错误必须仍然触发熔断（阈值 1）")
	}
}
