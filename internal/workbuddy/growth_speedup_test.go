// 效果验证（不启进程版）：
// 用 httptest 起一个"可控延迟"的假上游，直接调 RefreshGrowth 测耗时。
//
// 为什么不用真机实例：用户明确要求不要动项目进程。
// httptest 是进程内的假服务器，不占端口、不影响任何在跑的东西。
package workbuddy

import (
	"testing"
	"time"
)

// TestGrowthRefreshSpeedupMeasured 量化「账号并发 + 调用并发 + 共享预算」的叠加效果。
//
// 用「与真实上游同量级的延迟」建模：
//
//	/tasks   1500ms（实测 /tasks 约 1590ms，占单账号探针 66%）
//	其余 4 个 各 200ms（实测 travel/config 约 257ms）
//
// 串行 + 账号串行的理论值（3 账号）：
//
//	3 × (1500 + 4×200) = 3 × 2300 = 6900ms   ← 与改动前实测 7157ms 吻合
//
// 改动后（账号并发 + 调用并发 + 共享预算 5）应显著低于此值。
func TestGrowthRefreshSpeedupMeasured(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过耗时用例（-short）")
	}

	const tasksDelay = 1500 * time.Millisecond
	const otherDelay = 200 * time.Millisecond

	sb := &mixedStub{tasksDelay: tasksDelay, otherDelay: otherDelay}
	s := newMixedHarness(t, sb, 3)

	t0 := time.Now()
	s.RefreshGrowth(true, false)
	elapsed := time.Since(t0)

	serial := 3 * (tasksDelay + 4*otherDelay) // 6900ms
	t.Logf("3 账号刷新耗时 %v（串行理论值 %v，提速 %.1fx）",
		elapsed.Round(time.Millisecond), serial,
		float64(serial)/float64(elapsed))

	if elapsed > serial/2 {
		t.Errorf("耗时 %v 未显著快于串行理论值 %v", elapsed, serial)
	}
	// 共享预算：峰值不得超过 5
	if p := sb.peak.Load(); p > 5 {
		t.Errorf("上游并发峰值 %d 超过共享预算 5", p)
	}
}

// TestGrowthRefreshSharedBudgetAcrossBothLevels 钉死「两层共用一个预算」。
//
// 这是引入账号层并发时引入的真回归：账号层限 5、调用层又各开 4 个并发，
// 若不共享预算，峰值会变成 5×4=20（实测就是这么被 TestRefreshGrowthConcurrencyCap
// 抓出来的）。这里用 8 个账号放大观察，断言峰值恒 <= 5。
func TestGrowthRefreshSharedBudgetAcrossBothLevels(t *testing.T) {
	sb := &mixedStub{tasksDelay: 30 * time.Millisecond, otherDelay: 20 * time.Millisecond}
	s := newMixedHarness(t, sb, 8)

	s.RefreshGrowth(true, false)

	if p := sb.peak.Load(); p > 5 {
		t.Errorf("8 账号下上游并发峰值 %d —— 两层并发必须共享预算（上限 5），"+
			"否则实际会达到账号数×每账号并发数", p)
	}
	if sb.tasks.Load() != 8 {
		t.Errorf("8 个账号都应被探测，实际 tasks 调用 %d 次", sb.tasks.Load())
	}
}
