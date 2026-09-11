package scheduler

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// watchHarness 旅行守卫用例的公共装置。
type watchHarness struct {
	stub *travelStub
	s    *Scheduler
	p    *pool.Pool
	log  *checkinlog.Log
}

// newWatchHarness 起一个同时模拟 growth + billing 的桩，构造带历史记录的调度器。
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

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	log := checkinlog.New(filepath.Join(t.TempDir(), "checkin-log.json"), 30)

	return &watchHarness{
		stub: stub,
		s:    New(Config{Pool: p, Upstream: up, Log: log}),
		p:    p,
		log:  log,
	}
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
	if h.s.isDue("u1", time.Now()) {
		t.Error("刚排完计划，此刻不应到期")
	}
	if !h.s.isDue("u1", time.Now().Add(time.Hour+time.Minute)) {
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
