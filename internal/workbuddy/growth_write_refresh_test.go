// RED 测试：写操作后必须**就地刷新该账号快照**。
//
// 现场：写操作（claim/accept/...）只改上游状态，不更新服务端的 GrowthSnapshot。
// 于是前端只能用 ?refresh=1 全量重探 —— 为了更新 1 个账号，把 N 个账号全探一遍
// （实测 3 账号 7.2s，1 账号 2.3s）。这正是"点了要等 7 秒"的后端根因。
//
// 本测试钉死：六个 *For 在**成功**后都刷新自己那个账号的快照，
// 且**只**刷新自己那个账号（不能顺手把别人的也探一遍 —— 那等于白做）。
package workbuddy

import (
	"strings"
	"testing"

	"workbuddy2api/internal/checkinlog"
)

// countProbes 统计 stub 上 tasks 接口被调了几次 —— 每次 probeGrowth 必调它一次。
func probeCount(g *growthStub) int32 { return g.tasksCalls.Load() }

// TestGrowthClaimRefreshesOwnSnapshot 领奖成功后快照应已更新为最新。
//
// 用「stub 状态会演进」来验证：第一次 /tasks 返回 completed（可领），
// 领奖后第二次 /tasks 返回 claimed（已领）。若实现不刷新快照，
// 前端读到的仍是第一次那份 completed —— 那正是"界面不更新"的根因。
func TestGrowthClaimRefreshesOwnSnapshot(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"done","accept_status":"completed","reward_credit":300}]}`
	// 领奖后切到 claimed 状态（模拟上游真实行为）
	g.tasksJSONAfterClaim.Store(`{"tasks":[{"task_code":"done","accept_status":"claimed","reward_credit":300}]}`)
	s, _ := newGrowthHarness(t, g)

	s.RefreshGrowth(true, false)
	sn0 := s.GrowthSnapshots()[0]
	if sn0.ClaimableCount != 1 {
		t.Fatalf("前置：初次应看到 1 个可领，得到 %d", sn0.ClaimableCount)
	}

	res := s.GrowthClaimFor("u1", "", triggerManual)
	if res.Status != checkinlog.StatusOK {
		t.Fatalf("前置：领奖应成功，得到 %s（%s）", res.Status, res.Detail)
	}

	// 快照必须已经被就地刷新成 claimed
	sn := s.GrowthSnapshots()[0]
	if sn.ClaimableCount != 0 {
		t.Errorf("领奖后快照未刷新：ClaimableCount 仍为 %d（期望 0）", sn.ClaimableCount)
	}
}

// TestGrowthWriteRefreshesOnlyTargetAccount 只刷新目标账号，不碰其它账号。
//
// 计数口径：一次 probeGrowth = 1 次 /tasks；GrowthClaimFor 自己还要拉一次
// /tasks 来找可领任务。所以「领 u1 一次」的总量是：
//
//	1（claim 自己拉任务） + 1（刷新 u1 快照） = 2 次
//
// 若实现里写成全量重探，就会变成 1 + N（N=账号数）。用 2 个账号做对照，
// 期望值 2 与全量的 3 能区分开。
func TestGrowthWriteRefreshesOnlyTargetAccount(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"done","accept_status":"completed","reward_credit":300}]}`
	s := newGrowthHarness2(t, g, "u1", "u2")

	s.RefreshGrowth(true, false) // 全量建快照：2 个账号 → 2 次
	before := probeCount(g)

	res := s.GrowthClaimFor("u1", "", triggerManual)
	if res.Status != checkinlog.StatusOK {
		t.Fatalf("领奖失败: %s", res.Detail)
	}
	delta := probeCount(g) - before
	if delta != 2 {
		t.Errorf("期望 2 次（claim 自己拉任务 1 次 + 只重探 u1 1 次），实际 %d；"+
			"若为 3 说明实现做成了全量重探", delta)
	}
}

// TestGrowthAcceptRefreshesOwnSnapshot 接单成功后同样刷新。
func TestGrowthAcceptRefreshesOwnSnapshot(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"a","accept_status":"not_accepted","reward_credit":100}]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)
	before := probeCount(g)

	res := s.GrowthAcceptFor("u1", "", triggerManual)
	if res.Status != checkinlog.StatusOK {
		t.Fatalf("接单应成功，得到 %s（%s）", res.Status, res.Detail)
	}
	if probeCount(g) <= before {
		t.Error("接单成功后未刷新快照")
	}
}

// TestGrowthSkipRefreshesOwnSnapshot 跳过路径也要刷新快照。
//
// 关键设计决定：用户点「领取」却没领到，往往正是因为**快照旧了**
// （守卫轮刚替我们领过、或任务已被领走）。这时若不刷新，界面会一直显示
// "待领取"，用户会反复点 —— 所以 skip 路径必须刷新。
//
// 计数口径：GrowthClaimFor 无条件先拉一次 /tasks（找可领任务），
// 刷新快照再调一次。所以 skip 路径的正常开销是 **+2**，不是 +1。
// 这里同时守住「不能是全量」：用 2 个账号做对照，全量会是 +3。
func TestGrowthSkipRefreshesOwnSnapshot(t *testing.T) {
	g := defaultGrowthStub()
	// 没有可领任务
	g.tasksJSON = `{"tasks":[{"task_code":"a","accept_status":"accepted","reward_credit":100}]}`
	s := newGrowthHarness2(t, g, "u1", "u2")
	s.RefreshGrowth(true, false)
	before := probeCount(g)

	res := s.GrowthClaimFor("u1", "", triggerManual)
	if res.Status != checkinlog.StatusSkip {
		t.Fatalf("前置：应无可领，得到 %s（%s）", res.Status, res.Detail)
	}
	delta := probeCount(g) - before
	if delta != 2 {
		t.Errorf("跳过路径应 +2（拉任务 1 次 + 刷新自己 1 次），实际 +%d；"+
			"若为 1 说明没刷新（界面会一直显示待领取），若为 3 说明做成了全量", delta)
	}
}

// TestGrowthFailDoesNotProbe 上游报错时不该再重探 —— 那只会把错误信息覆盖掉。
func TestGrowthFailDoesNotProbe(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"a","accept_status":"completed","reward_credit":100}]}`
	g.claimFail.Store(true) // 领奖接口返回 400
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)
	before := probeCount(g)

	res := s.GrowthClaimFor("u1", "", triggerManual)
	if res.Status != checkinlog.StatusFail {
		t.Fatalf("前置：应失败，得到 %s（%s）", res.Status, res.Detail)
	}
	delta := probeCount(g) - before
	if delta != 1 {
		t.Errorf("失败路径应恰好 +1（只为找可领任务），实际 +%d", delta)
	}
}

// TestGrowthDetailUnchangedByRefresh 兜底：确认 Detail 文案没被刷新逻辑改坏。
func TestGrowthDetailUnchangedByRefresh(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"done","accept_status":"completed","reward_credit":300}]}`
	s, _ := newGrowthHarness(t, g)
	res := s.GrowthClaimFor("u1", "", triggerManual)
	if !strings.Contains(res.Detail, "领取") {
		t.Errorf("详情文案应仍含「领取」，得到 %q", res.Detail)
	}
}
