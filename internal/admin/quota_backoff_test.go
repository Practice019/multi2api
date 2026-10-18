package admin

import (
	"testing"
	"time"
)

// 本文件：自适应降速判据（`quotaBackoffStep`）的守门用例。
//
// ⚠ 为什么值得单独测：判错的方向有两个，都不报错 ——
//   该退不退（猛撞上游）、不该退却退（额度悄悄变慢，而用户看到的只是"数据旧"）。

// TestQuotaBackoffSingleBadRunDoesNotBackoff 单轮异常不退让。
//
// ⚠ 这是最关键的一条：实测 76 个号里失败 1 个（约 1.3%）是**正常抖动**，
// 若一次抖动就让间隔翻倍，这条循环会自己把自己弄得越来越慢 ——
// 那正是在制造用户报的"额度是过时的"。
func TestQuotaBackoffSingleBadRunDoesNotBackoff(t *testing.T) {
	badRuns, every := quotaBackoffStep(true, 0, quotaAutoRefreshEvery)
	if badRuns != 1 {
		t.Errorf("连续坏轮数 = %d，want 1", badRuns)
	}
	if every != quotaAutoRefreshEvery {
		t.Errorf("单轮异常**不该**退让（want %v，实际 %v）—— "+
			"一次偶发抖动就降速会让这条循环自己越跑越慢", quotaAutoRefreshEvery, every)
	}
}

// TestQuotaBackoffDoublesAfterConsecutiveRuns 连续两轮异常才翻倍。
func TestQuotaBackoffDoublesAfterConsecutiveRuns(t *testing.T) {
	_, e1 := quotaBackoffStep(true, 0, quotaAutoRefreshEvery)
	badRuns2, e2 := quotaBackoffStep(true, 1, e1)
	if badRuns2 != quotaAutoRefreshFailRun {
		t.Errorf("连续坏轮数 = %d，want %d", badRuns2, quotaAutoRefreshFailRun)
	}
	if e2 != quotaAutoRefreshEvery*2 {
		t.Errorf("连续 %d 轮异常后退让到 %v，want %v",
			quotaAutoRefreshFailRun, e2, quotaAutoRefreshEvery*2)
	}
}

// TestQuotaBackoffCapsAtMax 封顶：不无限退让。
func TestQuotaBackoffCapsAtMax(t *testing.T) {
	every := quotaAutoRefreshEvery
	badRuns := 0
	for i := 0; i < 40; i++ {
		badRuns, every = quotaBackoffStep(true, badRuns, every)
	}
	if every != quotaAutoRefreshMaxEvery {
		t.Errorf("反复退让应当封顶在 %v，实际 %v —— "+
			"无限退让会让「上游恢复了、额度却再也不更新」变成一个没有任何信号的状态",
			quotaAutoRefreshMaxEvery, every)
	}
	if every > quotaAutoRefreshMaxEvery {
		t.Errorf("间隔越过了上限：%v > %v", every, quotaAutoRefreshMaxEvery)
	}
}

// TestQuotaBackoffRecoversImmediatelyOnHealthyRun 一恢复就**立刻**回到基准。
func TestQuotaBackoffRecoversImmediatelyOnHealthyRun(t *testing.T) {
	// 先退让两档。
	_, e1 := quotaBackoffStep(true, 0, quotaAutoRefreshEvery)
	_, e2 := quotaBackoffStep(true, 1, e1)
	_, e3 := quotaBackoffStep(true, 2, e2)
	if e3 <= quotaAutoRefreshEvery {
		t.Fatalf("前置条件不满足：应当已经退让（%v）", e3)
	}
	badRuns, recovered := quotaBackoffStep(false, 3, e3)
	if badRuns != 0 {
		t.Errorf("正常轮必须把连续计数归零，实际 %d", badRuns)
	}
	if recovered != quotaAutoRefreshEvery {
		t.Errorf("恢复后应当**立刻**回到基准间隔 %v（而不是慢慢恢复），实际 %v —— "+
			"退让是为了让路，不是为了惩罚上游；额度是选号权重的输入，慢了直接影响选号",
			quotaAutoRefreshEvery, recovered)
	}
}

// TestQuotaBackoffStaysAtBaseWhenAlwaysHealthy 一直正常就一直保持基准。
func TestQuotaBackoffStaysAtBaseWhenAlwaysHealthy(t *testing.T) {
	badRuns, every := 0, quotaAutoRefreshEvery
	for i := 0; i < 100; i++ {
		badRuns, every = quotaBackoffStep(false, badRuns, every)
	}
	if badRuns != 0 || every != quotaAutoRefreshEvery {
		t.Errorf("一直正常时不该发生任何变化，实际 badRuns=%d every=%v", badRuns, every)
	}
}

// TestQuotaBackoffMaxIsAboveBase 上限必须严格大于基准，否则"退让"是空操作。
func TestQuotaBackoffMaxIsAboveBase(t *testing.T) {
	if quotaAutoRefreshMaxEvery <= quotaAutoRefreshEvery {
		t.Errorf("上限 %v 必须大于基准 %v，否则退让没有任何余地",
			quotaAutoRefreshMaxEvery, quotaAutoRefreshEvery)
	}
	if quotaAutoRefreshFailRun < 2 {
		t.Errorf("quotaAutoRefreshFailRun = %d，必须 ≥ 2 —— "+
			"等于 1 的话**单轮**异常就退让，那正是上面那条测试要防的", quotaAutoRefreshFailRun)
	}
	_ = time.Second
}
