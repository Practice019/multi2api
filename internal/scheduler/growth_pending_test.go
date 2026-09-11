// RED 测试：验证「待完成」应包含进行中的任务。
//
// 用户报告：一个真实账号的状态是 claimed=13 / accepted=4 / in_progress=1，
// 界面上「待完成」却显示 0 —— 因为只统计了 not_accepted。
// 正确的「待完成」= not_accepted + accepted + in_progress（不论是否已接单，
// 只要还没做成都算待完成），「完成后可得」= 这些任务的 reward_credit 之和。
package scheduler

import (
	"testing"

	"workbuddy2api/internal/upstream"
)

// TestGrowthPendingCountIncludesAcceptedAndInProgress 钉死「待完成」的口径。
func TestGrowthSnapshotPendingSemantics(t *testing.T) {
	g := defaultGrowthStub()
	// 覆盖全部五种状态，逐个断言是否计入「待完成」。
	g.tasksJSON = `{"tasks":[
      {"task_code":"a","title":"未接单","accept_status":"not_accepted","reward_credit":100},
      {"task_code":"b","title":"已接单","accept_status":"accepted","reward_credit":200,
       "progress":{"current":0,"target":5}},
      {"task_code":"c","title":"进行中","accept_status":"in_progress","reward_credit":300,
       "progress":{"current":2,"target":5}},
      {"task_code":"d","title":"待领取","accept_status":"completed","reward_credit":400,
       "progress":{"current":5,"target":5}},
      {"task_code":"e","title":"已领取","accept_status":"claimed","reward_credit":500,
       "progress":{"current":5,"target":5}}
    ]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)

	sn := s.GrowthSnapshots()[0]

	// 待完成 = not_accepted + accepted + in_progress = 3 个
	if sn.PendingCount != 3 {
		t.Errorf("PendingCount 期望 3（未接单+已接单+进行中），得到 %d", sn.PendingCount)
	}
	// 完成后可得 = 100 + 200 + 300 = 600（completed/claimed 的奖励已经到手或即将到手，不算"完成后可得"）
	if sn.PendingCredit != 600 {
		t.Errorf("PendingCredit 期望 600，得到 %d", sn.PendingCredit)
	}
	// 并行验证：completed 仍单独算「可领」
	if sn.ClaimableCount != 1 || sn.ClaimableCredit != 400 {
		t.Errorf("Claimable 期望 1 个 / 400 分，得到 %d 个 / %d 分", sn.ClaimableCount, sn.ClaimableCredit)
	}
	// 旧的 acceptable 口径保持不变（只有 not_accepted 才可接单）
	if sn.AcceptableCount != 1 || sn.AcceptableCredit != 100 {
		t.Errorf("Acceptable 期望 1 个 / 100 分，得到 %d 个 / %d 分", sn.AcceptableCount, sn.AcceptableCredit)
	}
}

// TestGrowthPendingOnlyUserVisibleTasks 已领完的账号「待完成」必须是 0，
// 不能把所有任务都算进去。
func TestGrowthPendingWhenAllClaimed(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"a","accept_status":"claimed","reward_credit":100},
      {"task_code":"b","accept_status":"claimed","reward_credit":200}
    ]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)

	sn := s.GrowthSnapshots()[0]
	if sn.PendingCount != 0 {
		t.Errorf("全部 claimed 时 PendingCount 应为 0，得到 %d", sn.PendingCount)
	}
	if sn.PendingCredit != 0 {
		t.Errorf("全部 claimed 时 PendingCredit 应为 0，得到 %d", sn.PendingCredit)
	}
}

// TestGrowthPendingDoesNotDoubleCountCompleted completed 属于「待领取」，
// 不该同时计入「待完成」—— 否则用户会看到同一笔奖励被算两次。
func TestGrowthPendingExcludesCompleted(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"only","accept_status":"completed","reward_credit":400}
    ]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)

	sn := s.GrowthSnapshots()[0]
	if sn.PendingCount != 0 {
		t.Errorf("completed 不应计入待完成，得到 PendingCount=%d", sn.PendingCount)
	}
	if sn.ClaimableCount != 1 {
		t.Errorf("completed 应计入可领，得到 ClaimableCount=%d", sn.ClaimableCount)
	}
}

// 用 upstream 包直接验证 Pending() 判定，避免只测聚合层。
func TestGrowthTaskPendingPredicate(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{upstream.GrowthStatusNotAccepted, true},
		{upstream.GrowthStatusAccepted, true},
		{upstream.GrowthStatusInProgress, true},
		{upstream.GrowthStatusCompleted, false},
		{upstream.GrowthStatusClaimed, false},
		{"", false},
	}
	for _, c := range cases {
		task := upstream.GrowthTask{AcceptStatus: c.status}
		if got := task.Pending(); got != c.want {
			t.Errorf("status=%q Pending()=%v，期望 %v", c.status, got, c.want)
		}
	}
}
