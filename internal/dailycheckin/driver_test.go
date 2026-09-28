package dailycheckin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
)

// stub 一个可编程的上游桩，记录被问了什么、领了什么。
type stub struct {
	accounts []Account
	// signed 已签的 uid 集合
	signed map[string]bool
	// inactive 未开放签到的 uid 集合
	inactive map[string]bool
	// statusErr / claimErr 按 uid 注入错误
	statusErr map[string]error
	claimErr  map[string]error
	// actionRequired 按 uid 注入"需要用户去官方侧做一步"
	actionRequired map[string]bool
	// credits 领取返回的积分
	credits int64

	// 记录
	statusCalls []string
	claimCalls  []string
}

func newStub() *stub {
	return &stub{
		signed:    map[string]bool{},
		inactive:  map[string]bool{},
		statusErr:      map[string]error{},
		claimErr:       map[string]error{},
		actionRequired: map[string]bool{},
		credits:   100,
	}
}

func (s *stub) Accounts(context.Context) ([]Account, error) { return s.accounts, nil }

func (s *stub) Status(_ context.Context, a Account) (bool, bool, error) {
	s.statusCalls = append(s.statusCalls, a.UID)
	if err := s.statusErr[a.UID]; err != nil {
		return false, false, err
	}
	if s.inactive[a.UID] {
		return false, false, nil
	}
	return s.signed[a.UID], true, nil
}

func (s *stub) Claim(_ context.Context, a Account) (int64, bool, error) {
	s.claimCalls = append(s.claimCalls, a.UID)
	if err := s.claimErr[a.UID]; err != nil {
		return 0, s.actionRequired[a.UID], err
	}
	return s.credits, false, nil
}

// newTestLog 造一个临时 checkinlog。
func newTestLog(t *testing.T) *checkinlog.Log {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "checkin.json")
	return checkinlog.New(fp, 30)
}

func testDesc() Descriptor {
	return Descriptor{
		ID:     "probe-checkin",
		Label:  "签到",
		Title:  "探测签到",
		OneURL: "/admin/probe/checkin",
		AllURL: "/admin/probe/checkin/all",
	}
}

// TestCheckinOneWritesHistory ★ 签一次**必须**写一条历史。
//
// # 这是本包存在的首要理由（用户报的「今日签到列是 —」）
//
// 界面「今日签到」列读的就是 checkinlog 的 KindCheckin 记录
//（internal/admin/admin.go）。不写它 → 那一列永远是 `—`，
// 而用户看到的是"签到按钮点了、列还是空"。
//
// 本仓已有先例：trae 的代码注释写着同样的话。lobsterai/qoder 移植时漏了。
//
// 变异可检：把 CheckinOne 里的 d.record(...StatusOK...) 删掉 → 本用例红。
func TestCheckinOneWritesHistory(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	d := New(s, testDesc(), Options{Log: lg})

	res := d.CheckinOne(context.Background(), Account{UID: "u1"}, "manual")
	if res.Status != checkinlog.StatusOK {
		t.Fatalf("status = %q，want ok（detail=%s）", res.Status, res.Detail)
	}
	if res.Credits != 100 {
		t.Errorf("credits = %d，want 100", res.Credits)
	}

	// ✅ 历史必须有一条
	recs := lg.Since(time.Now().Add(-time.Minute))
	if len(recs) != 1 {
		t.Fatalf("历史记录数 = %d，want 1 —— 不写历史，界面「今日签到」列就永远是 `—`", len(recs))
	}
	r := recs[0]
	if r.Kind != checkinlog.KindCheckin {
		t.Errorf("kind = %q，want %q（admin 按这个 kind 过滤）", r.Kind, checkinlog.KindCheckin)
	}
	if r.UID != "u1" || r.Status != checkinlog.StatusOK {
		t.Errorf("记录 = %+v，want uid=u1 status=ok", r)
	}
	if r.Credits != 100 {
		t.Errorf("credits = %d，want 100", r.Credits)
	}
	if r.Trigger != "manual" {
		t.Errorf("trigger = %q，want manual", r.Trigger)
	}
}

// TestAlreadySignedDoesNotClaim 今天已签 → **不再领**（只记 already）。
//
// 判据不仅看返回值，还要看 claim 有没有被调用 —— 重复提交领取请求
// 会被上游当异常流量（本仓 codearts 的一次性 token 就是同类教训）。
func TestAlreadySignedDoesNotClaim(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	s.signed["u1"] = true
	d := New(s, testDesc(), Options{Log: lg})

	res := d.CheckinOne(context.Background(), Account{UID: "u1"}, "sched")
	if res.Status != checkinlog.StatusAlready {
		t.Errorf("status = %q，want already", res.Status)
	}
	if len(s.claimCalls) != 0 {
		t.Errorf("已签却仍调了 Claim（%v）—— 重复提交领取请求", s.claimCalls)
	}
	if recs := lg.Since(time.Now().Add(-time.Minute)); len(recs) != 1 || recs[0].Status != checkinlog.StatusAlready {
		t.Errorf("历史应为一条 already，得到 %+v", recs)
	}
}

// TestInactiveWritesNoHistory 上游未开放签到 → **不记历史**。
//
// ⚠ 与"查询失败"严格区分：把"上游没开这个活动"记成 fail 会把它
// 染成用户的失败，而且每天一条噪音。
//
// 变异可检：把 !enabled 分支改成也 record(fail) → 本用例红。
func TestInactiveWritesNoHistory(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	s.inactive["u1"] = true
	d := New(s, testDesc(), Options{Log: lg})

	res := d.CheckinOne(context.Background(), Account{UID: "u1"}, "sched")
	if res.Status != checkinlog.StatusSkip {
		t.Errorf("status = %q，want skip", res.Status)
	}
	if recs := lg.Since(time.Now().Add(-time.Minute)); len(recs) != 0 {
		t.Errorf("上游未开放签到**不该**记历史，得到 %+v —— "+
			"那会把它染成用户的失败，且每天一条噪音", recs)
	}
	if len(s.claimCalls) != 0 {
		t.Error("未开放时不该调 Claim")
	}
}

// TestStatusErrorRecordsFail 查询失败 → 记 fail（让用户看得见原因）。
func TestStatusErrorRecordsFail(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	s.statusErr["u1"] = errors.New("network down")
	d := New(s, testDesc(), Options{Log: lg})

	res := d.CheckinOne(context.Background(), Account{UID: "u1"}, "sched")
	if res.Status != checkinlog.StatusFail {
		t.Errorf("status = %q，want fail", res.Status)
	}
	recs := lg.Since(time.Now().Add(-time.Minute))
	if len(recs) != 1 || recs[0].Status != checkinlog.StatusFail {
		t.Errorf("查询失败应记一条 fail，得到 %+v", recs)
	}
	if !strings.Contains(recs[0].Detail, "network down") {
		t.Errorf("detail 应带上原因，得到 %q", recs[0].Detail)
	}
}

// TestClaimErrorRecordsFail 领取失败 → 记 fail。
func TestClaimErrorRecordsFail(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	s.claimErr["u1"] = errors.New("上游 429")
	d := New(s, testDesc(), Options{Log: lg})

	res := d.CheckinOne(context.Background(), Account{UID: "u1"}, "manual")
	if res.Status != checkinlog.StatusFail {
		t.Errorf("status = %q，want fail", res.Status)
	}
	if recs := lg.Since(time.Now().Add(-time.Minute)); len(recs) != 1 || recs[0].Status != checkinlog.StatusFail {
		t.Errorf("领取失败应记一条 fail，得到 %+v", recs)
	}
}

// TestRunAllCoversEveryAccount 全量任务逐个都签，且每个都写历史。
//
// 这条同时守住"三条路径共用同一实现"：定时任务与全量端点都走 RunAll
// → CheckinOne，所以历史条数应与账号数相等。
func TestRunAllCoversEveryAccount(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	s.accounts = []Account{{UID: "u1"}, {UID: "u2"}, {UID: "u3"}}
	s.signed["u2"] = true // 一个已签
	d := New(s, testDesc(), Options{Log: lg})

	if err := d.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(s.statusCalls) != 3 {
		t.Errorf("查了 %d 个账号，want 3", len(s.statusCalls))
	}
	if len(s.claimCalls) != 2 {
		t.Errorf("领了 %d 次，want 2（u2 已签不领）", len(s.claimCalls))
	}
	// ✅ 三个账号各一条历史（含已签那个）
	recs := lg.Since(time.Now().Add(-time.Minute))
	if len(recs) != 3 {
		t.Errorf("历史记录数 = %d，want 3（每个账号一条）—— "+
			"漏记会让界面上那些账号的「今日签到」列恒为空", len(recs))
	}
	if recs[0].Trigger != "sched" {
		t.Errorf("全量任务的 trigger = %q，want sched", recs[0].Trigger)
	}
}

// TestRunAllStopsOnContextCancel 取消后不再继续打剩下的账号。
//
// 全量任务可能扫几十个号；换班/退出时应当尽快停，而不是把剩下的打完。
func TestRunAllStopsOnContextCancel(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	for i := 0; i < 10; i++ {
		s.accounts = append(s.accounts, Account{UID: string(rune('a' + i))})
	}
	d := New(s, testDesc(), Options{Log: lg})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 一开始就取消

	// 注意 RunAll 在**入口**就检查 ctx，所以一个都不该扫。
	_ = d.RunAll(ctx)
	if len(s.statusCalls) != 0 {
		t.Errorf("ctx 已取消却扫了 %d 个账号", len(s.statusCalls))
	}
}

// TestNoLogIsSafe 不传 Log 时不 panic（合法配置，只是不记历史）。
func TestNoLogIsSafe(t *testing.T) {
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	d := New(s, testDesc(), Options{}) // Log = nil

	res := d.CheckinOne(context.Background(), Account{UID: "u1"}, "manual")
	if res.Status != checkinlog.StatusOK {
		t.Errorf("status = %q，want ok（不记历史不该影响签到本身）", res.Status)
	}
}

// TestNotReadyIsSafe 未接线时所有入口都不 panic。
func TestNotReadyIsSafe(t *testing.T) {
	d := New(nil, testDesc(), Options{})
	if d.Ready() {
		t.Error("up=nil 时 Ready 应为 false")
	}
	if !d.AutoEnabled() == false && d.Interval() != 0 {
		t.Error("未接线时不该报出间隔")
	}
	if res := d.CheckinOne(context.Background(), Account{UID: "x"}, "manual"); res.Status != checkinlog.StatusFail {
		t.Errorf("未接线时应如实报 fail，得到 %q", res.Status)
	}
	// 两个 handler 也不能 panic。
	for _, h := range []http.HandlerFunc{d.HandlerOne, d.HandlerAll} {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	}
}

// ── 端点 ────────────────────────────────────────────────────────────────

func doReq(t *testing.T, h http.HandlerFunc, target, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, target, nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestHandlerOneAcceptsUIDFromQueryOrBody uid 两种来源都认。
//
// "两种都认"是**统一接口**的一部分：既有前端走 body，手工 curl 走 query。
// 各上游此前写法不一，正是"接口不统一"的表现。
func TestHandlerOneAcceptsUIDFromQueryOrBody(t *testing.T) {
	for _, tc := range []struct {
		name, target, body string
	}{
		{"body", "/admin/probe/checkin", `{"uid":"u1"}`},
		{"query", "/admin/probe/checkin?uid=u1", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub()
			s.accounts = []Account{{UID: "u1"}}
			d := New(s, testDesc(), Options{Log: newTestLog(t)})
			code, out := doReq(t, d.HandlerOne, tc.target, tc.body)
			if code != http.StatusOK {
				t.Fatalf("HTTP %d，want 200", code)
			}
			if out["ok"] != true {
				t.Errorf("ok = %v，want true（out=%v）", out["ok"], out)
			}
			if len(s.claimCalls) != 1 {
				t.Errorf("该签一次，实际 %v", s.claimCalls)
			}
		})
	}
}

// TestHandlerOneMissingUIDIs400 两种来源都缺 → 400（不是静默成功）。
func TestHandlerOneMissingUIDIs400(t *testing.T) {
	d := New(newStub(), testDesc(), Options{})
	code, _ := doReq(t, d.HandlerOne, "/admin/probe/checkin", ``)
	if code != http.StatusBadRequest {
		t.Errorf("HTTP %d，want 400", code)
	}
}

// TestHandlerOneUnknownUIDIs404 账号不在本上游 → 404（不拿空凭证去签）。
func TestHandlerOneUnknownUIDIs404(t *testing.T) {
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	d := New(s, testDesc(), Options{Log: newTestLog(t)})
	code, _ := doReq(t, d.HandlerOne, "/admin/probe/checkin", `{"uid":"nope"}`)
	if code != http.StatusNotFound {
		t.Errorf("HTTP %d，want 404", code)
	}
	if len(s.claimCalls) != 0 {
		t.Error("404 时不该签任何账号")
	}
}

// TestHandlerAllReportsClaimed 全量端点的回执形状（前端按 claimed 认它）。
func TestHandlerAllReportsClaimed(t *testing.T) {
	s := newStub()
	s.accounts = []Account{{UID: "u1"}, {UID: "u2"}}
	s.signed["u2"] = true
	d := New(s, testDesc(), Options{Log: newTestLog(t)})

	code, out := doReq(t, d.HandlerAll, "/admin/probe/checkin/all", ``)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200", code)
	}
	// ⚠ claimed 的类型必须是 number —— 前端判据是
	// `typeof r.claimed === 'number'`（见 webui.html），字符串会走错分支。
	if _, ok := out["claimed"].(float64); !ok {
		t.Errorf("claimed 类型 = %T，want number —— 前端按 typeof 判据选分支", out["claimed"])
	}
	if out["claimed"] != float64(1) {
		t.Errorf("claimed = %v，want 1（只有 u1 真领到）", out["claimed"])
	}
	if _, ok := out["results"].([]any); !ok {
		t.Errorf("results 缺失或类型不对：%T", out["results"])
	}
}

// TestDescriptorDrivesAllThreeWiring 描述符是唯一的真相源。
//
// 上游侧那三行转发全部读同一个 Descriptor —— 于是 ID/路径/文案
// 不可能三处不一致（那是"各写一份"必然的漂移）。
func TestDescriptorDrivesAllThreeWiring(t *testing.T) {
	d := New(newStub(), testDesc(), Options{})
	got := d.Descriptor()
	if got.ID != "probe-checkin" || got.OneURL != "/admin/probe/checkin" || got.AllURL != "/admin/probe/checkin/all" {
		t.Errorf("Descriptor 与预期不符：%+v", got)
	}
	if !d.Ready() {
		t.Error("有 up 时 Ready 应为 true")
	}
}

// TestAutoEnabledFollowsOptions 自动签到开关只看 Options。
func TestAutoEnabledFollowsOptions(t *testing.T) {
	s := newStub()
	cases := []struct {
		name      string
		opt       Options
		wantAuto  bool
		wantInter time.Duration
	}{
		{"启用且有间隔", Options{Enabled: true, Interval: 30 * time.Minute}, true, 30 * time.Minute},
		{"未启用", Options{Enabled: false, Interval: 30 * time.Minute}, false, 30 * time.Minute},
		{"间隔为 0（视为不注册）", Options{Enabled: true, Interval: 0}, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := New(s, testDesc(), c.opt)
			if d.AutoEnabled() != c.wantAuto {
				t.Errorf("AutoEnabled = %v，want %v", d.AutoEnabled(), c.wantAuto)
			}
			if d.Interval() != c.wantInter {
				t.Errorf("Interval = %v，want %v", d.Interval(), c.wantInter)
			}
		})
	}
}

// TestNoGatewayImport 本包**不得** import internal/gateway（架构硬约束）。
//
// # 为什么要一条测试守它
//
// 架构判据把"internal/ 下依赖 gateway 的包"一律当成上游实现
//（gateway/arch_test.go 的 discoverUpstreams），而"上游之间不得互相依赖"。
// 本包一旦 import gateway，各上游依赖它就会被判成"上游依赖上游"：
//
//	第一次构建就是这样被 arch_test 抓住的
//
// 两条豁免通道都走不通（corePackages 会被判成"绕过约束"；
// discoveryExemptPackages 的前提是**不依赖 gateway**，与需求矛盾）。
//
// 所以正确答案是"本包不认识 gateway 的类型" —— 这条测试把它钉住，
// 免得将来有人为了省三行样板而把 import 加回来（那会立刻破坏架构）。
func TestNoGatewayImport(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// ⚠ 跳过**测试文件**：本条测试自己必须写下那个包名字面串
		// 才能检查它（否则无法表达判据）。测试文件不在生产构建里，
		// 不参与架构判据的 go list -deps。
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"workbuddy2api/internal/gateway"`) {
			t.Errorf("%s 引用了 internal/gateway —— 本包必须与契约解耦：\n"+
				"  依赖 gateway 会让本包被 arch_test 当成「上游实现」，\n"+
				"  于是各上游 import 它就变成「上游依赖上游」= 架构违规。\n"+
				"  修法：返回本包自己的类型，由上游侧翻译成 gateway 类型。", e.Name())
		}
	}
}
