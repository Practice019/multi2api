package workbuddy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/pool"
)

// watchHarness 旅行守卫用例的公共装置。
type watchHarness struct {
	stub *travelStub
	s    *Provider
	p    *pool.Pool
	log  *checkinlog.Log
}

// newWatchHarness 起一个同时模拟 growth + billing 的桩，构造带历史记录的 Provider。
// statusJSON 是 travel/status 的 data 原文。
func newWatchHarness(t *testing.T, statusJSON string) *watchHarness {
	t.Helper()
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"instance_id":7437787,"name":"设计喵","rarity":"SR","personality":"爱钻研","soul_desc":"设计师"}`,
		state: statusJSON,
	}
	srv := billingAndGrowthServer(stub)
	t.Cleanup(srv.Close)

	s, p, log := newTestProviderWithLog(t, srv, testAuth("u1"))
	return &watchHarness{stub: stub, s: s, p: p, log: log}
}

// travelStateJSON 拼 travel/status 的 data 原文。arriveIn<=0 表示已到站。
func travelStateJSON(state string, recordID, reward int64, arriveIn time.Duration) string {
	now := time.Now().Unix()
	arrive := now + int64(arriveIn.Seconds())
	return fmt.Sprintf(
		`{"state":%q,"record_id":%d,"reward_credit":%d,"daily_limit_reached":true,`+
			`"buddy_id":7437787,"depart_at":%d,"arrive_at":%d,"server_now":%d,"duration_hours":3,`+
			`"location":{"id":4,"code":"ancient_town","name":"古镇客栈","duration_hours":3}}`,
		state, recordID, reward, arrive-10800, arrive, now)
}

// hasAutoRecord 判断历史里是否出现了一条 trigger=auto 的指定状态记录。
func (h *watchHarness) hasAutoRecord(kind, status string) bool {
	for _, r := range h.log.Recent(0) {
		if r.Kind == checkinlog.KindTravel && r.Trigger == "auto" && r.Status == status {
			return true
		}
	}
	return false
}

// TestTravelAutoClaimOnArrival 到站即自动领奖：守卫轮发现 arrived 必须直接 claim，
// 且历史里留下 trigger=auto 的成功记录。
func TestTravelAutoClaimOnArrival(t *testing.T) {
	h := newWatchHarness(t, travelStateJSON("arrived", 42, 7, -2*time.Minute))

	if !h.s.TravelAutoClaimEnabled() {
		t.Fatal("零值 Config 应默认开启自动领奖")
	}
	h.s.RefreshTravel(true, h.s.TravelAutoClaimEnabled())

	if n := h.stub.claimCalls.Load(); n != 1 {
		t.Fatalf("claim 调用次数 = %d，期望 1（到站应自动领）", n)
	}
	if got := h.stub.record.Load(); got != 42 {
		t.Errorf("claim 携带的 record_id = %d，期望 42", got)
	}
	if !h.hasAutoRecord(checkinlog.KindTravel, checkinlog.StatusOK) {
		t.Errorf("缺少 trigger=auto 的旅行成功历史: %+v", h.log.Recent(0))
	}
	// 领奖后把余额同步进池（桩返回 500 分）。
	if st, ok := h.p.Status("u1"); !ok || st.Credits != 500 {
		t.Errorf("领奖后余额未同步: %+v ok=%v", st, ok)
	}
}

// TestTravelAutoClaimDisabled 关掉开关后只探测、不领奖。
func TestTravelAutoClaimDisabled(t *testing.T) {
	h := newWatchHarness(t, travelStateJSON("arrived", 42, 7, -2*time.Minute))
	h.s.SetTravelAutoClaim(false)
	if h.s.TravelAutoClaimEnabled() {
		t.Fatal("SetTravelAutoClaim(false) 未生效")
	}
	h.s.RefreshTravel(true, h.s.TravelAutoClaimEnabled())

	if n := h.stub.claimCalls.Load(); n != 0 {
		t.Fatalf("关闭后不应领奖，claim 调用 %d 次", n)
	}
	// 但仍要探测到状态并写进快照，界面才看得见。
	snaps := h.s.TravelSnapshots()
	if len(snaps) != 1 || snaps[0].State != "arrived" {
		t.Errorf("快照未更新: %+v", snaps)
	}
}

// TestTravelWatchSchedulesNextCheckAtArrival 在途时把下次检查排到 arrive_at 之后，
// 中间的守卫轮不得再打扰上游 —— 这是「不盲轮询」的核心保证。
func TestTravelWatchSchedulesNextCheckAtArrival(t *testing.T) {
	h := newWatchHarness(t, travelStateJSON("traveling", 4373480, 6, time.Hour))

	h.s.RefreshTravel(true, h.s.TravelAutoClaimEnabled())
	if n := h.stub.claimCalls.Load(); n != 0 {
		t.Fatalf("在途不该领奖，claim 调用 %d 次", n)
	}
	if h.s.IsDue("u1", time.Now()) {
		t.Error("刚排完计划，此刻不应到期")
	}
	if !h.s.IsDue("u1", time.Now().Add(time.Hour+time.Minute)) {
		t.Error("到站时刻之后应到期")
	}

	// 非强制扫描：未到期 → 不得产生新的上游请求。
	before := h.stub.statusCalls.Load()
	h.s.RefreshTravel(false, h.s.TravelAutoClaimEnabled())
	if after := h.stub.statusCalls.Load(); after != before {
		t.Errorf("未到期却重复探测上游: statusCalls %d -> %d", before, after)
	}

	// 强制刷新（用户点「刷新」）则必须回源。
	h.s.RefreshTravel(true, h.s.TravelAutoClaimEnabled())
	if after := h.stub.statusCalls.Load(); after <= before {
		t.Errorf("force 刷新未回源: statusCalls 仍为 %d", after)
	}
}

// TestTravelSnapshotParsesUpstreamFields 快照要吃到上游的真实字段
// （instance_id / rarity / arrive_at / server_now 时钟校正）。
func TestTravelSnapshotParsesUpstreamFields(t *testing.T) {
	h := newWatchHarness(t, travelStateJSON("traveling", 4373480, 6, 90*time.Minute))
	h.s.RefreshTravel(true, false)

	snaps := h.s.TravelSnapshots()
	if len(snaps) != 1 {
		t.Fatalf("快照数 = %d，期望 1", len(snaps))
	}
	s := snaps[0]
	if !s.HasBuddy || s.BuddyName != "设计喵" || s.BuddyRarity != "SR" {
		t.Errorf("猫档案解析错误: %+v", s)
	}
	if s.State != "traveling" || s.RecordID != 4373480 || s.RewardCredit != 6 {
		t.Errorf("旅行状态解析错误: %+v", s)
	}
	if s.LocationName != "古镇客栈" || s.DurationHours != 3 {
		t.Errorf("目的地解析错误: %+v", s)
	}
	// arriveIn=90min，允许几秒误差。
	if s.RemainingSec < 89*60 || s.RemainingSec > 91*60 {
		t.Errorf("剩余时间 = %ds，期望约 90 分钟", s.RemainingSec)
	}
}

// TestTravelDepartSkipsWhenAlreadyDeparted 已派出时派猫返回 skip（不是 fail），
// 且不产生 depart 上游调用。
func TestTravelDepartSkipsWhenAlreadyDeparted(t *testing.T) {
	h := newWatchHarness(t, travelStateJSON("traveling", 4373480, 6, time.Hour))
	res := h.s.TravelDepartFor("u1", triggerManual)

	if res.Status != checkinlog.StatusSkip {
		t.Fatalf("status = %q，期望 skip（今日已派出）", res.Status)
	}
	if n := h.stub.departCalls.Load(); n != 0 {
		t.Errorf("不该真的调 depart，调用 %d 次", n)
	}
	if res.State != "traveling" || res.ArriveAt == 0 {
		t.Errorf("结果里应带上状态与到站时间: %+v", res)
	}
}

// TestTravelClaimSkipsWhenNotArrived 未到站时领奖返回 skip，且不产生 claim 调用。
func TestTravelClaimSkipsWhenNotArrived(t *testing.T) {
	h := newWatchHarness(t, travelStateJSON("traveling", 4373480, 6, time.Hour))
	res := h.s.TravelClaimFor("u1", triggerManual)

	if res.Status != checkinlog.StatusSkip {
		t.Fatalf("status = %q，期望 skip（猫未到站）", res.Status)
	}
	if n := h.stub.claimCalls.Load(); n != 0 {
		t.Errorf("不该真的调 claim，调用 %d 次", n)
	}
}

// TestTravelSnapshotFillsRowWithoutBuddy 无猫时也要有一行（待领养），而不是整行消失。
func TestTravelSnapshotFillsRowWithoutBuddy(t *testing.T) {
	h := newWatchHarness(t, travelStateJSON("idle", 0, 0, 0))
	h.stub.buddy = "null"
	h.s.RefreshTravel(true, false)

	snaps := h.s.TravelSnapshots()
	if len(snaps) != 1 || snaps[0].HasBuddy {
		t.Fatalf("无猫快照不符: %+v", snaps)
	}
}

// TestTravelJobDueIsFalseRightAfterRun 守卫轮的 Due 在刚跑完一轮后必须为假。
//
// 这条守住"不盲轮询"：核心的轮询粒度是 30 秒，而守卫间隔是 1 分钟起。
// 若 Due 不考虑间隔下限，把缓存填满之后的每一轮都会立刻返回真，
// 于是守卫轮被抬到 30 秒一轮 —— 空转频率翻倍。
func TestTravelJobDueIsFalseRightAfterRun(t *testing.T) {
	h := newWatchHarness(t, travelStateJSON("traveling", 4373480, 6, time.Hour))
	h.s.cfg.TravelWatchInterval = time.Minute

	// 首轮：无快照 → 到期，跑一趟填满缓存。
	if !h.s.travelDue(time.Now()) {
		t.Fatal("首轮（无快照）应到期，等价于改造前的启动首扫")
	}
	if err := h.s.runTravelJob(context.Background()); err != nil {
		t.Fatalf("runTravelJob: %v", err)
	}
	// 刚跑完：既受间隔下限约束，账号也都没到期。
	if h.s.travelDue(time.Now()) {
		t.Error("刚跑完一轮后 Due 应为假（否则守卫轮被抬到核心的 30 秒粒度）")
	}
	// 过了守卫间隔、且账号到期（在途账号的 next 排在 arrive_at+20s，这里给足时间）。
	h.s.travelLastRun = time.Now().Add(-2 * time.Minute)
	h.s.travel.mu.Lock()
	h.s.travel.due["u1"] = time.Now().Add(-time.Second)
	h.s.travel.mu.Unlock()
	if !h.s.travelDue(time.Now()) {
		t.Error("过了守卫间隔且账号到期时 Due 应为真")
	}
}

// TestGrowthJobDueGatesOnEmptySnapshots 成长守卫轮的 Due：无快照时到期（启动首扫），
// 刚跑完且账号都未到期时为假。
func TestGrowthJobDueGatesOnEmptySnapshots(t *testing.T) {
	g := defaultGrowthStub()
	srv := stubServer(t, g.handler())
	s, _ := newTestProvider(t, srv, testAuth("u1"))

	if !s.growthDueJob(time.Now()) {
		t.Fatal("无快照时应到期（启动首扫）")
	}
	if err := s.runGrowthJob(context.Background()); err != nil {
		t.Fatalf("runGrowthJob: %v", err)
	}
	if s.growthDueJob(time.Now()) {
		t.Error("刚跑完一轮后 Due 应为假")
	}
	s.growthLastRun = time.Now().Add(-time.Hour)
	// 把账号的下次检查时刻推到过去，否则 scheduleGrowthNext 刚把它排到 10 分钟后。
	s.growth.mu.Lock()
	s.growth.due["u1"] = time.Now().Add(-time.Second)
	s.growth.mu.Unlock()
	if !s.growthDueJob(time.Now()) {
		t.Error("过了守卫间隔且账号到期时 Due 应为真")
	}
}

// TestJobsReturnsBothWatchTasks Provider 必须把守卫轮都注册出去。
//
// # 数量从 2 变成 3 是本版本的有意变更
//
// 新增了 JobActivity（对话活跃上报）。它与两个守卫轮**形状不同**：
// 守卫轮按"每个账号自己的到期时刻"错峰，活跃上报按"整点窗口 + 当日是否已跑"。
// 但三者都通过同一个 gateway.Job 契约注册，因此这里的结构断言（名字非空、
// Run/Due/Interval 齐备）对三者同样成立。
func TestJobsReturnsBothWatchTasks(t *testing.T) {
	s, _ := newTestProvider(t, stubServer(t, defaultGrowthStub().handler()), testAuth("u1"))
	jobs := s.Jobs()
	if len(jobs) != 3 {
		t.Fatalf("应注册 3 个任务（旅行守卫 / 成长守卫 / 活跃上报），得到 %d", len(jobs))
	}
	names := map[string]bool{}
	for _, j := range jobs {
		if j.Name == "" {
			t.Error("任务名不能为空（调度器用它做去重与日志）")
		}
		if j.Run == nil {
			t.Errorf("任务 %s 缺 Run", j.Name)
		}
		if j.Due == nil {
			t.Errorf("任务 %s 缺 Due —— 守卫轮按账号到期错峰、活跃上报按整点窗口，都不是纯固定间隔", j.Name)
		}
		if j.Interval <= 0 {
			t.Errorf("任务 %s 的 Interval 应 > 0（作为轮询下限）", j.Name)
		}
		names[j.Name] = true
	}
	for _, want := range []string{JobTravelWatch, JobGrowthWatch, JobActivity} {
		if !names[want] {
			t.Errorf("缺少任务 %s", want)
		}
	}
}
