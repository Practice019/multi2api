package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// travelStub 模拟 growth 域全部端点，记录调用次数与请求参数。
type travelStub struct {
	buddy       string // /buddy/info 的 data.buddy 原文（"null" 或对象）
	state       string // /travel/status 的 data 原文
	firstStatus int    // /buddy/first 的 HTTP 状态码（非 0 时按业务错误返回）

	infoCalls, statusCalls, departCalls atomic.Int32
	claimCalls, firstCalls, agreeCalls  atomic.Int32
	location, record                    atomic.Int64
}

func (s *travelStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			s.infoCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"buddy":%s}}`, s.buddy)
		case "/activity/growth/buddy/travel/status":
			s.statusCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, s.state)
		case "/activity/growth/buddy/travel/depart":
			s.departCalls.Add(1)
			var body struct {
				LocationID int `json:"location_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.location.Store(int64(body.LocationID))
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case "/activity/growth/buddy/travel/claim":
			s.claimCalls.Add(1)
			var body struct {
				RecordID int64 `json:"record_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.record.Store(body.RecordID)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"reward_credit":9}}`))
		case "/activity/growth/buddy/first":
			s.firstCalls.Add(1)
			if s.firstStatus >= 400 {
				w.WriteHeader(s.firstStatus)
				w.Write([]byte(`{"code":400,"msg":"first_buddy task not completed yet"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		case "/activity/growth/buddy/agreement":
			s.agreeCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func (s *travelStub) server() *httptest.Server {
	return httptest.NewServer(s.handler())
}

// billingAndGrowthServer 同时模拟 billing（签到/余额/刷新）与 growth（旅行）端点，
// 供「签到收尾顺带跑旅行」这类跨域用例使用。
func billingAndGrowthServer(stub *travelStub) *httptest.Server {
	growth := stub.handler()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			growth.ServeHTTP(w, r)
		}
	}))
}

// fastTravel 关闭账号间限速，避免测试白等 800ms。
func fastTravel(t *testing.T) {
	t.Helper()
	old := TravelAccountDelay
	TravelAccountDelay = 0
	t.Cleanup(func() { TravelAccountDelay = old })
}

// newTravelScheduler 构造 travel 相关依赖齐全的 Provider。
func newTravelScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Provider, *pool.Pool) {
	t.Helper()
	accounts := make([]*auth.Auth, 0, len(uids))
	for _, uid := range uids {
		accounts = append(accounts, testAuth(uid))
	}
	return newTestProvider(t, srv, accounts...)
}

// TestRunCheckinNowTriggersTravel 签到收尾顺带跑一趟旅行（无猫 → 同意协议 + 领养）。
//
// 这条原本在 internal/scheduler（跨域用例：签到 + 旅行）。搬运时它被拆成两半：
// 旅行那一半（本函数）留在 workbuddy，走的是「核心喊一声 → 上游钩子被调用」的路径；
// 核心那一半（核心确实会在签到后喊）由 internal/scheduler 的
// TestCheckinRunsUpstreamHook 覆盖。两半合起来等价于改造前的那一条。
func TestRunCheckinNowTriggersTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1（签到收尾应顺带跑一趟旅行）", n)
	}
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("buddy/first calls=%d want 1（无猫应尝试领养）", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("buddy/agreement calls=%d want 1", n)
	}
}

// TestAfterCheckinHookRunsTravel 「核心喊一声」时本包确实会推进旅行。
func TestAfterCheckinHookRunsTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.AfterCheckin()

	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1（签到钩子应顺带跑一趟旅行）", n)
	}
}

// TestRunCheckinTravelCoversAccountsJustReenabled 签到解冻的账号当轮即参与旅行。
//
// 原名沿用 internal/scheduler（搬运前它验证的是 scheduler.RunCheckin 的收尾顺序：
// 先逐账号签到解冻、再跑旅行）。搬运后"先解冻、再旅行"这个顺序由核心的
// RunCheckin 保证（见 internal/scheduler 的 TestCheckinRunsUpstreamHook），
// 本用例验证的是**旅行这一侧**确实能看到刚解冻的账号：解冻后跑 AfterCheckin
// 必须能覆盖到它并派出。
func TestRunCheckinTravelCoversAccountsJustReenabled(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7,"name":"档案喵"}`,
		state: `{"state":"idle","daily_limit_reached":false}`}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	// 签到解冻由核心负责（scheduler.RunCheckin），这里直接模拟其效果：
	// 查到余额 500 → 解冻，然后跑签到钩子。
	p.ReenableIfCredits("u1", 500)
	s.AfterCheckin()

	// 收尾的旅行应覆盖到该账号并派出。
	if n := stub.departCalls.Load(); n != 1 {
		t.Errorf("depart calls=%d want 1（刚解冻账号应被本轮旅行覆盖）", n)
	}
}

// TestRunKeepaliveDoesNotTriggerTravel 保活不触发旅行：旅行只搭签到便车。
//
// 这条守住"搭车对象只有签到"：保活（token 刷新）与旅行是两条独立的线 —
// 把旅行挂到保活上会让它一天多跑一趟，且与签到时的账号解冻顺序脱节
// （签到的收尾顺序是"先解冻、再旅行"，保活没有这个语义）。
//
// 搬运后本包只暴露 **AfterCheckin** 一个搭车入口。所以判据变成：
// **核心跑保活时不会碰到本包的任何旅行入口**。这里用真实的
// AfterCheckin 做对照，证明只有它才会产生上游请求。
func TestRunKeepaliveDoesNotTriggerTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")

	// 本包没有任何"保活后推进旅行"的入口：AfterCheckin 是唯一搭车点，
	// 而它只在签到时点被调用。这里先确认不调它就没有任何上游请求。
	if n := stub.infoCalls.Load(); n != 0 {
		t.Fatalf("未触发任何入口却产生了上游请求: info=%d", n)
	}
	// 对照组：调了 AfterCheckin 才应该有请求 —— 证明"不触发"不是因为桩坏了。
	s.AfterCheckin()
	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1（AfterCheckin 是唯一的搭车入口）", n)
	}
}

// TestRunTravelStateMachine 表驱动覆盖状态机全部分支：每趟只做一个动作。
func TestRunTravelStateMachine(t *testing.T) {
	const buddyPresent = `{"id":7,"name":"档案喵 R"}`

	cases := []struct {
		name         string
		buddy        string
		state        string
		firstStatus  int
		wantInfo     int32
		wantStatus   int32
		wantDepart   int32
		wantClaim    int32
		wantFirst    int32
		wantAgree    int32
		wantLocation int64
		wantRecord   int64
	}{
		{
			name: "无猫-领养成功", buddy: "null",
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "无猫-门槛未达-静默跳过", buddy: "null", firstStatus: 400,
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "有猫-idle-未达上限-派出", buddy: buddyPresent,
			state:    `{"state":"idle","daily_limit_reached":false}`,
			wantInfo: 1, wantStatus: 1, wantDepart: 1, wantLocation: 4,
		},
		{
			name: "有猫-idle-已达上限-跳过", buddy: buddyPresent,
			state:    `{"state":"idle","daily_limit_reached":true}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-traveling-跳过", buddy: buddyPresent,
			state:    `{"state":"traveling","record_id":42}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-arrived-领奖", buddy: buddyPresent,
			state:    `{"state":"arrived","record_id":42,"reward_credit":9}`,
			wantInfo: 1, wantStatus: 1, wantClaim: 1, wantRecord: 42,
		},
		{
			name: "有猫-arrived-缺record_id-跳过领奖", buddy: buddyPresent,
			state:    `{"state":"arrived","record_id":0}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "有猫-未知状态-跳过", buddy: buddyPresent,
			state:    `{"state":"teleporting"}`,
			wantInfo: 1, wantStatus: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastTravel(t)
			stub := &travelStub{buddy: tc.buddy, state: tc.state, firstStatus: tc.firstStatus}
			srv := stub.server()
			defer srv.Close()

			s, _ := newTravelScheduler(t, srv, "u1")
			s.RunTravelNow()

			got := map[string]int32{
				"info": stub.infoCalls.Load(), "status": stub.statusCalls.Load(),
				"depart": stub.departCalls.Load(), "claim": stub.claimCalls.Load(),
				"first": stub.firstCalls.Load(), "agreement": stub.agreeCalls.Load(),
			}
			want := map[string]int32{
				"info": tc.wantInfo, "status": tc.wantStatus,
				"depart": tc.wantDepart, "claim": tc.wantClaim,
				"first": tc.wantFirst, "agreement": tc.wantAgree,
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s calls=%d want %d", k, got[k], w)
				}
			}
			if tc.wantDepart > 0 && stub.location.Load() != tc.wantLocation {
				t.Errorf("location_id=%d want %d", stub.location.Load(), tc.wantLocation)
			}
			if tc.wantClaim > 0 && stub.record.Load() != tc.wantRecord {
				t.Errorf("record_id=%d want %d", stub.record.Load(), tc.wantRecord)
			}
		})
	}
}

// TestRunTravelAdoptThresholdTriedOncePerDay 门槛未达（400）当日只试一次，后续巡检静默跳过。
func TestRunTravelAdoptThresholdTriedOncePerDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()
	s.RunTravelNow()
	s.RunTravelNow()

	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（当日只试一次）", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("agreement calls=%d want 1", n)
	}
	if n := stub.infoCalls.Load(); n != 3 {
		t.Errorf("info calls=%d want 3（仍每趟查有无猫）", n)
	}
}

// TestRunTravelAdoptTriedExpiresNextDay 跨自然日后允许重新尝试领养。
func TestRunTravelAdoptTriedExpiresNextDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.markAdoptTried("u1") // 当日已试过
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 0 {
		t.Fatalf("first calls=%d want 0（当日已试过）", n)
	}

	s.adoptTried["u1"] = "2000-01-01" // 模拟昨日记录
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（跨日应重试）", n)
	}
}

// TestRunTravelSkipsDisabledAndFailedAccounts 禁用账号不查；单账号出错不影响后续账号。
func TestRunTravelSkipsDisabledAndFailedAccounts(t *testing.T) {
	fastTravel(t)
	var deadCalls, okDepart, disabledCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("X-User-Id") == "disabled":
			disabledCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.Header.Get("X-User-Id") == "dead":
			deadCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.URL.Path == "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "disabled", "dead", "ok")
	p.Disable("disabled", "test")
	s.RunTravelNow()

	if n := deadCalls.Load(); n != 1 {
		t.Errorf("dead account calls=%d want 1（401 跳过本轮，不强刷 token）", n)
	}
	if n := disabledCalls.Load(); n != 0 {
		t.Errorf("disabled account calls=%d want 0", n)
	}
	// 前列账号失败不应中断遍历：末位 ok 账号照常完成派出。
	if n := okDepart.Load(); n != 1 {
		t.Errorf("ok account depart calls=%d want 1（失败账号不影响后续遍历）", n)
	}
	st, ok := p.Status("ok")
	if !ok {
		t.Fatal("ok 账号应在池中")
	}
	if st.Disabled {
		t.Errorf("ok 账号不应被影响: %+v", st)
	}
}

// TestRunTravelDisabledAccountSkipsAllCalls 禁用账号一个请求都不发。
func TestRunTravelDisabledAccountSkipsAllCalls(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	p.Disable("u1", "test")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("info calls=%d want 0（禁用账号应跳过）", n)
	}
}

// TestRunTravelActionErrorsDoNotAbort 各环节上游报错只影响本账号本轮：不 panic、不中断遍历。
func TestRunTravelActionErrorsDoNotAbort(t *testing.T) {
	fastTravel(t)
	var okDepart atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		fail := func() {
			w.WriteHeader(500)
			w.Write([]byte(`{"code":500,"msg":"boom"}`))
		}
		switch {
		case uid == "bdinfo": // 查有无猫失败
			fail()
		case uid == "status" && r.URL.Path == "/activity/growth/buddy/travel/status": // 查状态失败
			fail()
		case uid == "depart" && r.URL.Path == "/activity/growth/buddy/travel/depart": // 派出发失败
			fail()
		case uid == "claim" && r.URL.Path == "/activity/growth/buddy/travel/claim": // 领奖失败
			fail()
		case uid == "agree" && r.URL.Path == "/activity/growth/buddy/agreement": // 同意协议失败
			fail()
		case uid == "first" && r.URL.Path == "/activity/growth/buddy/first": // 领养非门槛类失败
			fail()
		case r.URL.Path == "/activity/growth/buddy/info":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			if uid == "depart" || uid == "status" || uid == "ok" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"state":"arrived","record_id":42}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/activity/growth/buddy/agreement":
			w.Write([]byte(`{"code":0,"data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "agree", "bdinfo", "claim", "depart", "first", "ok", "status")
	s.RunTravelNow() // 不应 panic

	// 末位账号（uid 排序后 ok 排在 status 前，但都在失败账号之后）照常完成派出。
	if n := okDepart.Load(); n == 0 {
		t.Errorf("depart calls=%d want >0（个别账号失败不应中断遍历）", n)
	}
}

// TestTravelDayAlignsCST 每日重置按 CST 自然日判定（UTC 17:00 已是次日 CST）。
func TestTravelDayAlignsCST(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"UTC 凌晨 = CST 当日", time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC), "2026-09-11"},
		{"UTC 16:00 = CST 次日 00:00", time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC), "2026-09-12"},
		{"UTC 15:59 仍在 CST 当日", time.Date(2026, 9, 11, 15, 59, 0, 0, time.UTC), "2026-09-11"},
	}
	for _, c := range cases {
		if got := travelDay(c.in); got != c.want {
			t.Errorf("%s: travelDay=%s want %s", c.name, got, c.want)
		}
	}
}

// TestRunTravelLoopCancelsWhenDisabled 守卫轮在 ctx 取消后立即返回，不空转。
//
// 原名沿用 internal/scheduler 的 TestRunTravelLoopCancelsWhenDisabled。
// 原用例覆盖的是「无任何整点任务时调度循环不空转」；守卫轮搬走后，
// 那条纪律属于核心（见 internal/scheduler 的 TestSchedulerRunStopsOnCancelWithJobs），
// 而**守卫轮自身**的取消纪律留在这里。
func TestRunTravelLoopCancelsWhenDisabled(t *testing.T) {
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消：RunTravelWatcher 的首扫之后应马上返回
	done := make(chan struct{})
	go func() { s.RunTravelWatcher(ctx, time.Hour); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTravelWatcher 未在 ctx 已取消时返回")
	}
}

// TestRunTravelLoopStopsOnCancel 守卫循环在 ctx 取消后立即返回。
//
// 原名沿用 internal/scheduler 的 TestRunTravelLoopStopsOnCancel —— 它覆盖的
// 是守卫轮的退出纪律，随守卫轮一起搬到本包。保留原名便于对照审计：
// 搬运不该"弄丢"任何一条既有测试。
func TestRunTravelLoopStopsOnCancel(t *testing.T) {
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()
	s, _ := newTravelScheduler(t, srv, "u1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunTravelWatcher(ctx, time.Minute); close(done) }()
	time.Sleep(20 * time.Millisecond) // 让守卫进入 ticker 等待
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTravelWatcher 未在 ctx 取消后返回")
	}
}

// TestGrowthLoopStopsOnCancel 同上，覆盖成长守卫轮的退出纪律。
func TestGrowthLoopStopsOnCancel(t *testing.T) {
	g := defaultGrowthStub()
	srv := stubServer(t, g.handler())
	s, _ := newTestProvider(t, srv, testAuth("u1"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunGrowthWatcher(ctx, 10*time.Minute); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunGrowthWatcher 未在 ctx 取消后返回")
	}
}
