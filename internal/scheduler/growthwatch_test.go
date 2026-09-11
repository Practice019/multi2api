package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// growthStub 模拟成长中心全部端点，记录写操作次数与参数。
type growthStub struct {
	tasksJSON   string
	streakJSON  string
	energyJSON  string
	quotaJSON   string
	lotteryJSON string

	acceptCalls, redeemCalls, makeupCalls, openCalls, drawCalls, claimCalls atomic.Int32
	// tasksCalls 统计 /tasks 被调用次数 —— 每次 probeGrowth 必调它一次，
	// 因此可以用它数「探针跑了几次」（用于验证写操作后是否就地刷新）。
	tasksCalls atomic.Int32
	// tasksJSONAfterClaim 非空时：一旦发生过领奖调用，/tasks 改回这个 JSON。
	// 用于模拟上游真实行为 —— 领奖后任务状态从 completed 变 claimed，
	// 从而可以验证「写操作后快照是否被就地刷新」。
	tasksJSONAfterClaim atomic.Value
	lastCodes                                                               atomic.Value // []string
	lastTier, lastDate                                                      atomic.Value
	lastOpenCount                                                           atomic.Int32
	// lastClaimCode 记录最后一次领奖请求里的 task_code（从 URL 路径解析）。
	lastClaimCode atomic.Value

	// claimJSON 是领奖接口的 data 体；为空时回一个「真的领到了 100 分」。
	claimJSON atomic.Value
	// claimFail 为真时领奖接口返回 400，用于验证失败路径。
	claimFail atomic.Bool

	// acceptStatus/acceptMessage 控制接单接口逐条返回的 status/message。
	acceptStatus  atomic.Value
	acceptMessage atomic.Value
	// acceptPerCode 按 task_code 给不同的 result 对象（用于验证"预期拒绝"的分类）。
	// map[string]string: task_code -> 完整 result JSON。
	acceptPerCode atomic.Value
}

func (g *growthStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/v2/activity/growth"
		ok := func(body string) { fmt.Fprintf(w, `{"code":0,"msg":"OK","data":%s}`, body) }
		switch {
		case r.URL.Path == base+"/tasks":
			g.tasksCalls.Add(1)
			// 领奖发生过后，任务状态演进（completed → claimed）
			if body, _ := g.tasksJSONAfterClaim.Load().(string); body != "" && g.claimCalls.Load() > 0 {
				ok(body)
				return
			}
			ok(g.tasksJSON)
		case r.URL.Path == base+"/streak":
			ok(g.streakJSON)
		case r.URL.Path == base+"/energy":
			ok(g.energyJSON)
		case r.URL.Path == base+"/buddy/quota":
			ok(g.quotaJSON)
		case r.URL.Path == base+"/lottery/chances":
			ok(g.lotteryJSON)
		// 领奖：路径是 /tasks/{code}/claim，必须逐任务调用。
		// 用 HasSuffix 匹配而不是相等，才能验证实现真的把 task_code 拼进了路径。
		case strings.HasPrefix(r.URL.Path, base+"/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			g.claimCalls.Add(1)
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/tasks/"), "/claim")
			g.lastClaimCode.Store(code)
			if g.claimFail.Load() {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"invalid request"}`))
				return
			}
			if body, _ := g.claimJSON.Load().(string); body != "" {
				ok(body)
				return
			}
			// 默认：第一次调用算真领到，第二次起返回 already_claimed（幂等）。
			if g.claimCalls.Load() > 1 {
				ok(`{"already_claimed":true,"credit":0,"energy":0}`)
				return
			}
			ok(`{"already_claimed":false,"credit":100,"energy":5}`)
		case r.URL.Path == base+"/tasks/accept":
			g.acceptCalls.Add(1)
			// 关键契约：上游只认数组形状 {"task_codes":[...]}。
			// 收到单数 task_code 会返回 400 invalid request —— 这里照做，
			// 于是「实现里用错形状」会被测试立刻抓住。
			var b struct {
				TaskCodes []string `json:"task_codes"`
				TaskCode  string   `json:"task_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.TaskCode != "" || len(b.TaskCodes) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"invalid request"}`))
				return
			}
			g.lastCodes.Store(append([]string(nil), b.TaskCodes...))
			st, _ := g.acceptStatus.Load().(string)
			msg, _ := g.acceptMessage.Load().(string)
			if st == "" {
				st = "accepted"
			}
			rs := make([]string, 0, len(b.TaskCodes))
			for _, c := range b.TaskCodes {
				rs = append(rs, fmt.Sprintf(`{"task_code":%q,"status":%q,"message":%q}`, c, st, msg))
			}
			ok(`{"results":[` + strings.Join(rs, ",") + `]}`)
		case r.URL.Path == base+"/redeem":
			g.redeemCalls.Add(1)
			var b struct {
				Tier string `json:"tier"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			g.lastTier.Store(b.Tier)
			ok(`{}`)
		case r.URL.Path == base+"/makeup":
			g.makeupCalls.Add(1)
			var b struct {
				TargetDate string `json:"target_date"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			g.lastDate.Store(b.TargetDate)
			ok(`{}`)
		case r.URL.Path == base+"/buddy/open":
			g.openCalls.Add(1)
			var b struct {
				Count int `json:"count"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			g.lastOpenCount.Store(int32(b.Count))
			ok(`{}`)
		case r.URL.Path == base+"/lottery/draw":
			g.drawCalls.Add(1)
			ok(`{}`)
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func newGrowthHarness(t *testing.T, g *growthStub) (*Scheduler, *checkinlog.Log) {
	t.Helper()
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Nickname: "测试号"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	log := checkinlog.New(filepath.Join(t.TempDir(), "checkin-log.json"), 30)
	return New(Config{Pool: p, Upstream: up, Log: log}), log
}

// newGrowthHarness2 池里放两个账号，用于验证「写操作只重探自己那个账号」。
func newGrowthHarness2(t *testing.T, g *growthStub, uid1, uid2 string) *Scheduler {
	t.Helper()
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: uid1, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Nickname: "甲"})
	p.Add(&auth.Auth{UID: uid2, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Nickname: "乙"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	log := checkinlog.New(filepath.Join(t.TempDir(), "checkin-log.json"), 30)
	return New(Config{Pool: p, Upstream: up, Log: log})
}

func defaultGrowthStub() *growthStub {
	return &growthStub{
		tasksJSON:   `{"tasks":[]}`,
		streakJSON:  `{"streak":{"days":3,"next_tier":"7d","next_tier_remaining":4,"makeup_dates":[]},"makeup_cards":{"balance":0,"max":4},"redemption_status":{"remaining_days":3,"tiers":[]}}`,
		energyJSON:  `{"balance":0,"total_consumed":0,"total_earned":0}`,
		quotaJSON:   `{"affordable":0,"balance":0,"cost_per_open":10,"max_open_count":5}`,
		lotteryJSON: `{"balance":0}`,
	}
}

func hasGrowthRecord(log *checkinlog.Log, want, contains string) bool {
	for _, r := range log.Recent(0) {
		if r.Kind == checkinlog.KindGrowth && r.Status == want && strings.Contains(r.Detail, contains) {
			return true
		}
	}
	return false
}

// TestGrowthAcceptableSemantics 钉死核心语义修正：
// 只有 not_accepted 可接单；completed 是「已完成且奖励已发放」，不是「待领取」。
func TestGrowthAcceptableSemantics(t *testing.T) {
	cases := []struct {
		name string
		task upstream.GrowthTask
		want bool
	}{
		{"未接单 → 可接", upstream.GrowthTask{AcceptStatus: upstream.GrowthStatusNotAccepted}, true},
		{"已接单 → 不可再接", upstream.GrowthTask{AcceptStatus: upstream.GrowthStatusAccepted}, false},
		// 这两条是之前实现的错误来源：曾把 completed + 进度达标当成「可领奖」。
		{"已完成（奖励已发放）→ 不可接", upstream.GrowthTask{AcceptStatus: upstream.GrowthStatusCompleted,
			HasReward: true, Progress: &upstream.GrowthProgress{Current: 1, Target: 1}}, false},
		{"无需接单 → 不可接", upstream.GrowthTask{AcceptStatus: upstream.GrowthStatusClaimed,
			HasReward: true, Progress: &upstream.GrowthProgress{Current: 1, Target: 1}}, false},
		{"锁定 → 不可接", upstream.GrowthTask{AcceptStatus: upstream.GrowthStatusNotAccepted, Locked: true}, false},
	}
	for _, c := range cases {
		task := c.task
		if got := task.Acceptable(); got != c.want {
			t.Errorf("%s: Acceptable()=%v want %v", c.name, got, c.want)
		}
	}
}

// TestGrowthSnapshotCountsByStatus 快照按状态分类统计，「可接单」只算未接单的。
func TestGrowthSnapshotCountsByStatus(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"a","title":"未接单","accept_status":"not_accepted","has_reward":true,"reward_credit":100,"reward_energy":5},
      {"task_code":"b","title":"已接单","accept_status":"accepted","has_reward":true,"progress":{"current":0,"target":1}},
      {"task_code":"c","title":"已完成","accept_status":"completed","has_reward":true,"reward_credit":300,"progress":{"current":1,"target":1}},
      {"task_code":"d","title":"无需接单","accept_status":"claimed","has_reward":true,"progress":{"current":1,"target":1}}
    ]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)

	sn := s.GrowthSnapshots()[0]
	if sn.TasksTotal != 4 {
		t.Fatalf("total=%d", sn.TasksTotal)
	}
	if sn.TasksCompleted != 1 || sn.TasksAccepted != 1 {
		t.Errorf("completed=%d accepted=%d want 1/1", sn.TasksCompleted, sn.TasksAccepted)
	}
	if sn.AcceptableCount != 1 || sn.AcceptableCredit != 100 {
		t.Errorf("acceptable: count=%d credit=%d want 1/100", sn.AcceptableCount, sn.AcceptableCredit)
	}
	// 快照携带全部任务（不只待接单队列），由调用方按状态筛选；
	// 这里同时验证「全部」与「只要待接单」两种读法。
	if len(sn.Tasks) != 4 {
		t.Fatalf("Tasks 应含全部 4 条，得到 %d", len(sn.Tasks))
	}
	// 可接单的判定与 UI 一致：只有未接单且未锁定的任务才可操作。
	var acceptable []GrowthTaskView
	for _, tv := range sn.Tasks {
		if tv.Status == "not_accepted" && !tv.Locked {
			acceptable = append(acceptable, tv)
		}
	}
	if len(acceptable) != 1 || acceptable[0].TaskCode != "a" {
		t.Errorf("可接单明细: %+v", acceptable)
	}
}

// TestGrowthAcceptUsesArrayShape 接单请求必须是数组形状。
func TestGrowthAcceptUsesArrayShape(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"a","title":"A","accept_status":"not_accepted","reward_credit":100},
      {"task_code":"b","title":"B","accept_status":"completed","reward_credit":300,"progress":{"current":1,"target":1}}
    ]}`
	s, log := newGrowthHarness(t, g)
	res := s.GrowthAcceptFor("u1", "", triggerManual)

	if res.Status != checkinlog.StatusOK {
		t.Fatalf("status=%s detail=%s（形状错误会以 400 体现在这里）", res.Status, res.Detail)
	}
	codes, _ := g.lastCodes.Load().([]string)
	if len(codes) != 1 || codes[0] != "a" {
		t.Errorf("请求的 task_codes=%v，期望只有 [a]", codes)
	}
	if res.Count != 1 {
		t.Errorf("count=%d want 1", res.Count)
	}
	if !hasGrowthRecord(log, checkinlog.StatusOK, "[accept]") {
		t.Errorf("缺少接单历史: %+v", log.Recent(2))
	}
}

// TestGrowthAcceptSkipWhenNothingToAccept 没有未接单任务时记 skip。
func TestGrowthAcceptSkipWhenNothingToAccept(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"c","accept_status":"completed","progress":{"current":1,"target":1}}]}`
	s, log := newGrowthHarness(t, g)
	res := s.GrowthAcceptFor("u1", "", triggerManual)

	if res.Status != checkinlog.StatusSkip {
		t.Fatalf("status=%s，期望 skip", res.Status)
	}
	if g.acceptCalls.Load() != 0 {
		t.Error("不该发接单请求")
	}
	if !hasGrowthRecord(log, checkinlog.StatusSkip, "[accept]") {
		t.Error("缺少 skip 历史")
	}
}

// TestGrowthAcceptPerTaskError 逐条返回 error 且全失败时记 fail 并带上原因。
func TestGrowthAcceptPerTaskError(t *testing.T) {
	g := defaultGrowthStub()
	g.acceptStatus.Store("error")
	g.acceptMessage.Store("task already completed")
	g.tasksJSON = `{"tasks":[{"task_code":"c","accept_status":"not_accepted"}]}`
	s, log := newGrowthHarness(t, g)
	res := s.GrowthAcceptFor("u1", "", triggerManual)

	if res.Status != checkinlog.StatusFail {
		t.Fatalf("status=%s，期望 fail", res.Status)
	}
	if !strings.Contains(res.Detail, "task already completed") {
		t.Errorf("未带上上游原因: %s", res.Detail)
	}
	if !hasGrowthRecord(log, checkinlog.StatusFail, "[accept]") {
		t.Error("缺少 fail 历史")
	}
}

// TestGrowthTogglesDefaults 默认领奖、接单、补签开；三个花资源的关。
//
// 接单从「默认关」改成「默认开」的理由：实测 not_accepted 只在
// 「还没领到第一只 Buddy」的窄窗口存在，官方前端连按钮都不给。
// 默认关的实际后果是新账号那批任务连进度都不显示（用户不知道该做什么），
// 而接单本身是纯登记动作、不消耗任何资源，默认开的代价为零。
func TestGrowthTogglesDefaults(t *testing.T) {
	g := defaultGrowthStub()
	s, _ := newGrowthHarness(t, g)
	accept, makeup, redeem, open, draw, claim := s.GrowthToggles()
	if !accept || !makeup || redeem || open || draw || !claim {
		t.Fatalf("默认开关不符: claim=%v accept=%v makeup=%v redeem=%v open=%v draw=%v",
			claim, accept, makeup, redeem, open, draw)
	}
}

// TestGrowthAutoActionsRespectToggles 默认执行接单与补签；三个花资源的一次都不能发生。
func TestGrowthAutoActionsRespectToggles(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"a","accept_status":"not_accepted","reward_credit":100}]}`
	g.streakJSON = `{"streak":{"days":1,"next_tier":"7d","next_tier_remaining":6,"makeup_dates":["2026-09-01"]},
      "makeup_cards":{"balance":2,"max":4},
      "redemption_status":{"remaining_days":30,"tiers":[{"tier":"7d","days":7,"credit":0,"energy":2}]}}`
	g.quotaJSON = `{"affordable":3,"balance":40,"cost_per_open":10,"max_open_count":5}`
	g.lotteryJSON = `{"balance":2}`

	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, true)

	if g.makeupCalls.Load() != 1 {
		t.Errorf("默认应自动补签 1 次，实际 %d", g.makeupCalls.Load())
	}
	if got := g.lastDate.Load(); got != "2026-09-01" {
		t.Errorf("补签日期=%v，期望取第一个可补签日期", got)
	}
	// 接单默认开：没有可接任务时才不会调用，本用例有 1 个 not_accepted，应当被接单。
	if g.acceptCalls.Load() == 0 {
		t.Error("接单默认开，却一次都没调用")
	}
	for name, n := range map[string]int32{
		"兑换":  g.redeemCalls.Load(),
		"开盲盒": g.openCalls.Load(),
		"抽奖":  g.drawCalls.Load(),
	} {
		if n != 0 {
			t.Errorf("%s 默认关闭，却调用了 %d 次", name, n)
		}
	}
}

// TestGrowthAutoActionsWhenEnabled 显式开启后各动作按额度执行。
func TestGrowthAutoActionsWhenEnabled(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"a","accept_status":"not_accepted","reward_credit":100}]}`
	g.streakJSON = `{"streak":{"days":30,"makeup_dates":[]},"makeup_cards":{"balance":0,"max":4},
      "redemption_status":{"remaining_days":30,"tiers":[
        {"tier":"7d","days":7,"credit":0,"energy":2},
        {"tier":"14d","days":14,"credit":50,"energy":3},
        {"tier":"28d","days":28,"credit":150,"energy":5}]}}`
	g.quotaJSON = `{"affordable":3,"balance":40,"cost_per_open":10,"max_open_count":5}`
	g.lotteryJSON = `{"balance":1}`

	s, _ := newGrowthHarness(t, g)
	// 参数顺序：accept, makeup, redeem, open, draw, claim。
	// claim 这里给 false，因为本用例的存根没有可领任务，开了只会多打一次任务列表。
	s.SetGrowthToggles(true, true, true, true, true, false)
	s.RefreshGrowth(true, true)

	if g.acceptCalls.Load() == 0 {
		t.Error("开启后未自动接单")
	}
	if g.redeemCalls.Load() != 1 {
		t.Errorf("兑换 %d 次，期望 1", g.redeemCalls.Load())
	}
	if got := g.lastTier.Load(); got != "28d" {
		t.Errorf("兑换档位=%v，期望 28d（取可兑最高档）", got)
	}
	if g.openCalls.Load() != 1 {
		t.Errorf("开盲盒 %d 次，期望 1", g.openCalls.Load())
	}
	if n := g.lastOpenCount.Load(); n != 3 {
		t.Errorf("开盲盒数量=%d，期望 3（=affordable 且不超 max_open_count）", n)
	}
	if g.drawCalls.Load() != 1 {
		t.Errorf("抽奖 %d 次，期望 1", g.drawCalls.Load())
	}
}

// TestGrowthRedeemSkipsWhenDaysInsufficient 天数不够时明确 skip 并给出原因。
func TestGrowthRedeemSkipsWhenDaysInsufficient(t *testing.T) {
	g := defaultGrowthStub()
	g.streakJSON = `{"streak":{"days":1,"makeup_dates":[]},"makeup_cards":{"balance":0,"max":4},
      "redemption_status":{"remaining_days":3,"tiers":[{"tier":"7d","days":7}]}}`
	s, log := newGrowthHarness(t, g)
	res := s.GrowthRedeemFor("u1", "", triggerManual)

	if res.Status != checkinlog.StatusSkip {
		t.Fatalf("status=%s，期望 skip", res.Status)
	}
	if !strings.Contains(res.Detail, "不足以兑换") {
		t.Errorf("原因描述不清: %s", res.Detail)
	}
	if g.redeemCalls.Load() != 0 {
		t.Error("不该真的调兑换")
	}
	if !hasGrowthRecord(log, checkinlog.StatusSkip, "[redeem]") {
		t.Error("缺少 skip 历史")
	}
}

// TestGrowthRedeemSkipsAlreadyClaimedTier 已兑换过的档位要跳过，避免制造假失败。
func TestGrowthRedeemSkipsAlreadyClaimedTier(t *testing.T) {
	g := defaultGrowthStub()
	g.streakJSON = `{"streak":{"days":30,"makeup_dates":[]},"makeup_cards":{"balance":0,"max":4},
      "redemption_status":{"remaining_days":30,"tier_28d_count":1,"tier_28d_status":"claimed",
        "tiers":[{"tier":"28d","days":28},{"tier":"14d","days":14}]}}`
	s, _ := newGrowthHarness(t, g)
	res := s.GrowthRedeemFor("u1", "", triggerManual)

	if res.Status != checkinlog.StatusOK {
		t.Fatalf("status=%s detail=%s", res.Status, res.Detail)
	}
	if got := g.lastTier.Load(); got != "14d" {
		t.Errorf("档位=%v，期望跳过已领的 28d 选 14d", got)
	}
}

// TestGrowthOpenSkipWhenEnergyInsufficient 能量不足时 skip 并说明余额与单价。
func TestGrowthOpenSkipWhenEnergyInsufficient(t *testing.T) {
	g := defaultGrowthStub()
	g.quotaJSON = `{"affordable":0,"balance":8,"cost_per_open":10,"max_open_count":5}`
	s, _ := newGrowthHarness(t, g)
	res := s.GrowthOpenFor("u1", 0, triggerManual)

	if res.Status != checkinlog.StatusSkip {
		t.Fatalf("status=%s，期望 skip", res.Status)
	}
	if !strings.Contains(res.Detail, "能量不足") {
		t.Errorf("原因描述不清: %s", res.Detail)
	}
	if g.openCalls.Load() != 0 {
		t.Error("不该真的开盲盒")
	}
}

// TestGrowthSnapshotCachesWithoutUpstream 非强制扫描不重复打上游，force 则必回源。
func TestGrowthSnapshotCachesWithoutUpstream(t *testing.T) {
	g := defaultGrowthStub()
	s, _ := newGrowthHarness(t, g)

	s.RefreshGrowth(true, false)
	first := s.GrowthSnapshots()
	if len(first) != 1 {
		t.Fatalf("快照数=%d", len(first))
	}
	s.RefreshGrowth(false, false)
	second := s.GrowthSnapshots()
	if !second[0].ObservedAt.Equal(first[0].ObservedAt) {
		t.Error("未到期却重新回源（快照时间变了）")
	}
	time.Sleep(2 * time.Millisecond)
	s.RefreshGrowth(true, false)
	third := s.GrowthSnapshots()
	if third[0].ObservedAt.Equal(first[0].ObservedAt) {
		t.Error("force 刷新未回源")
	}
}

// TestGrowthTaskCarriesBothDescriptions 守住「两段说明不能搞混/丢失」：
// 上游 description 是「怎么做」，task_desc 是「达成条件」，
// 界面要同时展示，所以快照必须把两者分别带出来。
func TestGrowthTaskCarriesBothDescriptions(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"create_canvas","title":"体验「设计创意模式」",
       "description":"前往Workbuddy，进入「新建任务」，切换滑块到「设计创意」模式，并创建1个画布。",
       "task_desc":"在「设计创意」模式下成功创建1个画布。",
       "accept_status":"completed","tag":"PC","reward_credit":300,"reward_energy":5,
       "progress":{"current":1,"target":1}}
    ]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)

	sn := s.GrowthSnapshots()[0]
	if len(sn.Tasks) != 1 {
		t.Fatalf("Tasks 期望 1 条，得到 %d", len(sn.Tasks))
	}
	tv := sn.Tasks[0]
	if tv.Description != "在「设计创意」模式下成功创建1个画布。" {
		t.Errorf("Description 应取上游 task_desc（达成条件），得到 %q", tv.Description)
	}
	if tv.HowTo != "前往Workbuddy，进入「新建任务」，切换滑块到「设计创意」模式，并创建1个画布。" {
		t.Errorf("HowTo 应取上游 description（操作指引），得到 %q", tv.HowTo)
	}
	if tv.Tag != "PC" {
		t.Errorf("Tag 应为 PC，得到 %q", tv.Tag)
	}
}

// TestGrowthTaskMissingHowToIsEmpty 不是每个任务都有操作指引，缺了不能变成怪值。
func TestGrowthTaskMissingHowToIsEmpty(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"x","title":"只有达成条件","task_desc":"完成1次对话","accept_status":"accepted"}
    ]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)

	tv := s.GrowthSnapshots()[0].Tasks[0]
	if tv.Description != "完成1次对话" {
		t.Errorf("Description = %q", tv.Description)
	}
	if tv.HowTo != "" {
		t.Errorf("无操作指引时应为空串，得到 %q", tv.HowTo)
	}
}

// ---------------------------------------------------------------------------
// 领奖（claim）
// ---------------------------------------------------------------------------

// TestGrowthClaimForClaimsCompletedOnly 只有 completed 能领：
// accepted / in_progress 还没达标，claimed 已经领过，都不该发请求。
func TestGrowthClaimForClaimsCompletedOnly(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"done","accept_status":"completed","reward_credit":300,"progress":{"current":1,"target":1}},
      {"task_code":"running","accept_status":"accepted","reward_credit":100},
      {"task_code":"doing","accept_status":"in_progress","reward_credit":100,"progress":{"current":3,"target":5}},
      {"task_code":"already","accept_status":"claimed","reward_credit":100}
    ]}`
	s, log := newGrowthHarness(t, g)

	res := s.GrowthClaimFor("u1", "", triggerManual)
	if res.Status != checkinlog.StatusOK {
		t.Fatalf("status=%s detail=%s", res.Status, res.Detail)
	}
	if g.claimCalls.Load() != 1 {
		t.Errorf("只应对 completed 的 1 个任务发起领奖，实际 %d 次", g.claimCalls.Load())
	}
	if got, _ := g.lastClaimCode.Load().(string); got != "done" {
		t.Errorf("领奖的 task_code=%q 应为 done", got)
	}
	if res.Credits != 100 {
		t.Errorf("到账积分=%d，应为上游返回的 100", res.Credits)
	}
	if !hasGrowthRecord(log, checkinlog.StatusOK, "[claim]") {
		t.Error("缺少 claim 历史记录")
	}
}

// TestGrowthClaimForNoClaimable 没有可领任务时记为跳过，不是失败。
func TestGrowthClaimForNoClaimable(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"x","accept_status":"accepted","reward_credit":100}]}`
	s, _ := newGrowthHarness(t, g)

	res := s.GrowthClaimFor("u1", "", triggerManual)
	if res.Status != checkinlog.StatusSkip {
		t.Errorf("status=%s 应为 skip", res.Status)
	}
	if g.claimCalls.Load() != 0 {
		t.Errorf("不该发出领奖请求，实际 %d 次", g.claimCalls.Load())
	}
}

// TestGrowthClaimForAlreadyClaimedIsNotFailure 上游返回 already_claimed 是幂等成功，
// 必须记成跳过而不是失败，否则每次轮询都会刷一堆假错误。
func TestGrowthClaimForAlreadyClaimedIsNotFailure(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"done","accept_status":"completed","reward_credit":300}]}`
	g.claimJSON.Store(`{"already_claimed":true,"credit":0,"energy":0}`)
	s, log := newGrowthHarness(t, g)

	res := s.GrowthClaimFor("u1", "", triggerManual)
	if res.Status == checkinlog.StatusFail {
		t.Fatalf("已领取不应记为失败: %s", res.Detail)
	}
	if res.Credits != 0 {
		t.Errorf("已领取时到账积分应为 0，得到 %d", res.Credits)
	}
	if !res.AlreadyClaimed {
		t.Error("应标记 AlreadyClaimed")
	}
	if !hasGrowthRecord(log, checkinlog.StatusSkip, "[claim]") {
		t.Error("应记 skip 历史")
	}
}

// TestGrowthClaimForSpecificTask 指定 task_code 时只领那一个，即使它不在可领集合里
// （比如刚被别的路径领掉了）也要如实报错，而不是静默跳过。
func TestGrowthClaimForSpecificTask(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"a","accept_status":"completed","reward_credit":100},
      {"task_code":"b","accept_status":"completed","reward_credit":200}
    ]}`
	s, _ := newGrowthHarness(t, g)

	res := s.GrowthClaimFor("u1", "b", triggerManual)
	if res.Status != checkinlog.StatusOK {
		t.Fatalf("status=%s detail=%s", res.Status, res.Detail)
	}
	if g.claimCalls.Load() != 1 {
		t.Errorf("指定任务时只应调用 1 次，实际 %d", g.claimCalls.Load())
	}
	if got, _ := g.lastClaimCode.Load().(string); got != "b" {
		t.Errorf("领奖 task_code=%q 应为 b", got)
	}
}

// TestGrowthClaimUpstreamFailureIsFail 上游报错时要如实记为失败并带上原因。
func TestGrowthClaimUpstreamFailureIsFail(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"done","accept_status":"completed","reward_credit":300}]}`
	g.claimFail.Store(true)
	s, log := newGrowthHarness(t, g)

	res := s.GrowthClaimFor("u1", "", triggerManual)
	if res.Status != checkinlog.StatusFail {
		t.Fatalf("status=%s 应为 fail", res.Status)
	}
	if !strings.Contains(res.Detail, "领取失败") {
		t.Errorf("错误详情应说明失败: %s", res.Detail)
	}
	if !hasGrowthRecord(log, checkinlog.StatusFail, "[claim]") {
		t.Error("应记 fail 历史")
	}
}

// TestGrowthSnapshotCountsClaimable 快照要单独统计「现在能领多少分」——
// 这是 completed 的任务，和 Acceptable 是两码事。
func TestGrowthSnapshotCountsClaimable(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"c1","accept_status":"completed","reward_credit":300,"reward_energy":5},
      {"task_code":"c2","accept_status":"completed","reward_credit":100,"reward_energy":5},
      {"task_code":"n1","accept_status":"not_accepted","reward_credit":50},
      {"task_code":"k1","accept_status":"claimed","reward_credit":100}
    ]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)

	sn := s.GrowthSnapshots()[0]
	if sn.ClaimableCount != 2 || sn.ClaimableCredit != 400 || sn.ClaimableEnergy != 10 {
		t.Errorf("可领统计错误: count=%d credit=%d energy=%d",
			sn.ClaimableCount, sn.ClaimableCredit, sn.ClaimableEnergy)
	}
	// 待接单是另一回事，不能被 completed 影响。
	if sn.AcceptableCount != 1 {
		t.Errorf("可接单应为 1，得到 %d", sn.AcceptableCount)
	}
	if sn.TasksCompleted != 2 {
		t.Errorf("已完成计数=%d 应为 2（claimed 不算 completed）", sn.TasksCompleted)
	}
	// 视图上的 Claimable 标记供界面直接使用。
	for _, tv := range sn.Tasks {
		want := tv.TaskCode == "c1" || tv.TaskCode == "c2"
		if tv.Claimable != want {
			t.Errorf("%s Claimable=%v want %v", tv.TaskCode, tv.Claimable, want)
		}
	}
}

// TestGrowthInProgressCountsAsRunning in_progress 是官方第五个状态，
// 必须与 accepted 同归「进行中」，否则这类任务在界面四个视图里一个都落不进去。
func TestGrowthInProgressCountsAsRunning(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"p","accept_status":"in_progress","reward_credit":100,"progress":{"current":3,"target":5}}
    ]}`
	s, _ := newGrowthHarness(t, g)
	s.RefreshGrowth(true, false)

	sn := s.GrowthSnapshots()[0]
	if sn.TasksAccepted != 1 {
		t.Errorf("in_progress 应计入 TasksAccepted，得到 %d", sn.TasksAccepted)
	}
	if sn.ClaimableCount != 0 {
		t.Errorf("in_progress 不可领奖，ClaimableCount=%d", sn.ClaimableCount)
	}
	if sn.Tasks[0].Status != upstream.GrowthStatusInProgress {
		t.Errorf("状态应原样保留 in_progress，得到 %s", sn.Tasks[0].Status)
	}
}

// TestGrowthAutoClaimRunsByDefault 自动领奖默认开：这是唯一让积分到账的动作，
// 关着的话「做完任务积分不涨」会一直复现。
func TestGrowthAutoClaimRunsByDefault(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"done","accept_status":"completed","reward_credit":300}]}`
	s, _ := newGrowthHarness(t, g)

	_, _, _, _, _, claim := s.GrowthToggles()
	if !claim {
		t.Fatal("自动领奖应默认开启")
	}
	s.RefreshGrowth(true, true)
	if g.claimCalls.Load() == 0 {
		t.Error("自动刷新时应自动领奖")
	}
}

// TestGrowthAutoClaimRespectsToggle 关掉后一次都不能发生。
func TestGrowthAutoClaimRespectsToggle(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"done","accept_status":"completed","reward_credit":300}]}`
	s, _ := newGrowthHarness(t, g)
	s.SetGrowthToggles(false, false, false, false, false, false)

	s.RefreshGrowth(true, true)
	if g.claimCalls.Load() != 0 {
		t.Errorf("关闭自动领奖后仍调用了 %d 次", g.claimCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// 接单的「预期拒绝」不应被当成失败
//
// 现场（用户实测）：某账号有 17 个可接任务、合计 2150 分，点「全部接单」后
// 结果是「接单 0 个（失败 1）；最后错误 create_canvas: prerequisite not met: first_buddy」。
// 逐任务试下来，上游其实会回三种**预期内**的拒绝，而它们都不是"出错"：
//
//	① prerequisite not met: first_buddy  —— 前置任务 first_buddy 未完成
//	② task does not require acceptance    —— 该任务本来就不需要接单
//	③ message 为空（任务已经是 accepted） —— 重复接单
//
// 原实现把「status != accepted」一律计入 failN，于是 17 个任务全被判失败、
// HTTP 502、前端一片红；用户完全看不出「其实只是差一个前置任务」。
// 正确的做法是把这三种归为**跳过**，只在真的出现未知错误时报失败。

// growthStub 目前只支持给所有 task_code 返回同一个 status/message。
// 这三种拒绝是按任务区分，所以这里需要逐 code 控制。
func (g *growthStub) handlerPerCode() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/v2/activity/growth"
		switch {
		case r.URL.Path == base+"/tasks":
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":%s}`, g.tasksJSON)
		case r.URL.Path == base+"/streak":
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":%s}`, g.streakJSON)
		case r.URL.Path == base+"/energy":
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":%s}`, g.energyJSON)
		case r.URL.Path == base+"/buddy/quota":
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":%s}`, g.quotaJSON)
		case r.URL.Path == base+"/lottery/chances":
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":%s}`, g.lotteryJSON)
		case r.URL.Path == base+"/tasks/accept":
			var b struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			perCode, _ := g.acceptPerCode.Load().(map[string]string)
			rs := make([]string, 0, len(b.TaskCodes))
			for _, c := range b.TaskCodes {
				raw, ok := perCode[c]
				if !ok {
					rs = append(rs, fmt.Sprintf(`{"task_code":%q,"status":"accepted","message":""}`, c))
					continue
				}
				// raw 是完整的 result 对象（由测试直接给出）
				rs = append(rs, raw)
			}
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"results":[%s]}}`, strings.Join(rs, ","))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func newGrowthHarnessPerCode(t *testing.T, g *growthStub) *Scheduler {
	t.Helper()
	srv := httptest.NewServer(g.handlerPerCode())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Nickname: "测试号"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	log := checkinlog.New(filepath.Join(t.TempDir(), "checkin-log.json"), 30)
	return New(Config{Pool: p, Upstream: up, Log: log})
}

// 三种预期拒绝全部归为「跳过」，不报失败。
func TestGrowthAcceptExpectedRejectionsAreSkipped(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"create_canvas","accept_status":"not_accepted","reward_credit":300},
      {"task_code":"first_buddy","accept_status":"not_accepted","reward_credit":300},
      {"task_code":"black_cat","accept_status":"not_accepted","reward_credit":0}
    ]}`
	// create_canvas 被 first_buddy 前置挡住；first_buddy 自身不需要接单。
	g.acceptPerCode.Store(map[string]string{
		"create_canvas": `{"task_code":"create_canvas","status":"error","message":"prerequisite not met: first_buddy"}`,
		"first_buddy":   `{"task_code":"first_buddy","status":"error","message":"task does not require acceptance"}`,
		"black_cat":     `{"task_code":"black_cat","status":"accepted","message":""}`,
	})
	s := newGrowthHarnessPerCode(t, g)

	res := s.GrowthAcceptFor("u1", "", "manual")

	if res.Status == checkinlog.StatusFail {
		t.Errorf("预期拒绝被当成了失败：Status=%s Detail=%s", res.Status, res.Detail)
	}
	if res.Count != 1 {
		t.Errorf("成功接单数应为 1（只有 black_cat），得到 %d", res.Count)
	}
	if !strings.Contains(res.Detail, "跳过 2") {
		t.Errorf("Detail 应说明跳过了 2 个，得到 %q", res.Detail)
	}
	if strings.Contains(res.Detail, "失败") {
		t.Errorf("不该出现「失败」字样，得到 %q", res.Detail)
	}
}

// 全被前置挡住时：状态是「跳过」而不是「失败」，且说明里点出前置任务名。
// 这就是用户遇到的那个账号（17 个任务全被 first_buddy 挡住）。
func TestGrowthAcceptAllBlockedByPrerequisiteIsSkip(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
      {"task_code":"create_canvas","accept_status":"not_accepted","reward_credit":300},
      {"task_code":"chat_5","accept_status":"not_accepted","reward_credit":100}
    ]}`
	g.acceptPerCode.Store(map[string]string{
		"create_canvas": `{"task_code":"create_canvas","status":"error","message":"prerequisite not met: first_buddy"}`,
		"chat_5":        `{"task_code":"chat_5","status":"error","message":"prerequisite not met: first_buddy"}`,
	})
	s := newGrowthHarnessPerCode(t, g)

	res := s.GrowthAcceptFor("u1", "", "manual")

	if res.Status != checkinlog.StatusSkip {
		t.Errorf("全部被前置挡住应为 skip，得到 %s（Detail=%s）", res.Status, res.Detail)
	}
	// Detail 必须给出**可照做的**指引，而不是只丢一个内部任务码。
	if !strings.Contains(res.Detail, "first_buddy") {
		t.Errorf("Detail 应点明被哪个前置任务挡住，得到 %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "领取一只 Buddy") {
		t.Errorf("Detail 应把 first_buddy 翻译成可读指引，得到 %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "WorkBuddy 客户端") {
		t.Errorf("Detail 应说明要去哪里做，得到 %q", res.Detail)
	}
	if strings.Contains(res.Detail, "失败") {
		t.Errorf("不该出现「失败」字样，得到 %q", res.Detail)
	}
}

// 未登记的前置任务码原样带出，不丢信息。
func TestPrerequisiteHintFallsBackToCode(t *testing.T) {
	if got := prerequisiteHint("first_buddy"); !strings.Contains(got, "领取一只 Buddy") {
		t.Errorf("first_buddy 应有中文指引，得到 %q", got)
	}
	if got := prerequisiteHint("some_future_task"); got != "some_future_task" {
		t.Errorf("未登记的码应原样返回，得到 %q", got)
	}
}

// 真正的未知错误仍需报失败 —— 不能把分类做成"什么都跳过"。
func TestGrowthAcceptUnknownErrorStillFails(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"weird","accept_status":"not_accepted","reward_credit":100}]}`
	g.acceptPerCode.Store(map[string]string{
		"weird": `{"task_code":"weird","status":"error","message":"internal server error"}`,
	})
	s := newGrowthHarnessPerCode(t, g)

	res := s.GrowthAcceptFor("u1", "", "manual")

	if res.Status != checkinlog.StatusFail {
		t.Errorf("未知错误应为 fail，得到 %s（Detail=%s）", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "internal server error") {
		t.Errorf("Detail 应带出原始错误，得到 %q", res.Detail)
	}
}

// 空 message（任务已 accepted）也算跳过，不是失败。
func TestGrowthAcceptAlreadyAcceptedIsSkip(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[{"task_code":"x","accept_status":"not_accepted","reward_credit":100}]}`
	g.acceptPerCode.Store(map[string]string{
		"x": `{"task_code":"x","status":"error","message":""}`,
	})
	s := newGrowthHarnessPerCode(t, g)

	res := s.GrowthAcceptFor("u1", "", "manual")
	if res.Status == checkinlog.StatusFail {
		t.Errorf("空 message 的拒绝应为跳过，得到 %s（Detail=%s）", res.Status, res.Detail)
	}
}
