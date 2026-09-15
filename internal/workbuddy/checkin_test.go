// checkin_test.go workbuddy 的账号级定时业务测试。
//
// # 搬运说明
//
// 这些用例原先在 internal/scheduler（改造前签到/保活是核心的内置任务）。
// 业务搬进本包后，它们跟着一起搬 —— **用例名与断言逐字保留**，
// 这是"搬运没有改行为"的唯一证据。
//
// 改动仅限构造方式（scheduler.New(Config{Pool, Upstream})
// → NewWithConfig(Config{Pool: 适配器, Client})），语义一一对应。
package workbuddy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// fakeUpstream 同时模拟 billing 与 refresh（原样搬自 internal/scheduler）。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// newCheckinProvider 造一个挂了 fakeUpstream 的 Provider。
func newCheckinProvider(t *testing.T, f *fakeUpstream, accounts ...*auth.Auth) (*Provider, *pool.Pool) {
	t.Helper()
	srv := f.server()
	t.Cleanup(srv.Close)

	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up}), p
}

// TestRunCheckinReenablesCoolingAccount 签到查到余额后解冻冷却中的账号。
func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	srv := f.server()
	defer srv.Close()
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up})

	s.RunCheckinAll("schedule")
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

// TestRunKeepaliveRefreshesTokens 保活刷新 token。
func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	s, _ := newCheckinProvider(t, f, a)

	s.RunKeepaliveAll("schedule")
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

// TestRunKeepaliveSessionDeadDisables session 死亡的账号被自动禁用。
func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up})

	s.RunKeepaliveAll("schedule")
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("should disable session-dead account: %+v", st)
	}
}

// TestCheckinErrorDoesNotCrash 上游整体 500 时两类任务都不 panic。
func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up})

	// 不应 panic
	s.RunCheckinAll("schedule")
	s.RunKeepaliveAll("schedule")
}

// ---------------------------------------------------------------------------
// 槽位名与单账号入口
// ---------------------------------------------------------------------------

// TestRunSlotDispatchesBySlotName 本包按槽位名分派到自己认得的业务。
//
// 核心把槽位名原样转过来（它不认识 checkedin/keepalive 是什么），
// "这个名字对应哪段业务"的判断发生在本包。
//
// ⚠ 保活的 fixture token 必须是**临近过期**的：保活已统一为被动后台扫描
// （只刷 NeedsRefresh(10m) 内的），新鲜 token 会被跳过 —— 用远未来时间戳
// 会让"槽位应触发保活"这条断言恒失败（那是旧"无条件刷新"语义的遗留写法）。
func TestRunSlotDispatchesBySlotName(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	s, _ := newCheckinProvider(t, f,
		&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(5 * time.Minute).Unix()})

	s.RunSlot(SlotCheckin, "schedule")
	if f.checkinCalls.Load() != 1 {
		t.Errorf("SlotCheckin 应触发签到，实际 %d 次", f.checkinCalls.Load())
	}

	s.RunSlot(SlotKeepalive, "schedule")
	if f.refreshCalls.Load() != 1 {
		t.Errorf("SlotKeepalive 应触发保活，实际 %d 次", f.refreshCalls.Load())
	}
}

// TestRunSlotUnknownNameDoesNotCallUpstream 不认识的槽位名不该猜着打上游。
func TestRunSlotUnknownNameDoesNotCallUpstream(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	s, _ := newCheckinProvider(t, f,
		&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	s.RunSlot("who-knows", "schedule")
	if f.checkinCalls.Load() != 0 || f.refreshCalls.Load() != 0 {
		t.Errorf("未知槽位不该产生上游请求：checkin=%d refresh=%d",
			f.checkinCalls.Load(), f.refreshCalls.Load())
	}
}

// TestRunSlotForSingleAccount 单账号入口按槽位名分派且能报"账号不存在"。
func TestRunSlotForSingleAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	s, _ := newCheckinProvider(t, f,
		&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	res, ok := s.RunSlotFor(SlotCheckin, "u1", "manual")
	if !ok || res.Status != "ok" {
		t.Errorf("res=%+v ok=%v want ok/true", res, ok)
	}
	if _, ok := s.RunSlotFor(SlotCheckin, "missing", "manual"); ok {
		t.Error("账号不存在应 ok=false")
	}
	if _, ok := s.RunSlotFor("unknown-slot", "u1", "manual"); ok {
		t.Error("未知槽位应 ok=false（不猜）")
	}
}

// TestRunCheckinForMissingAccount 单账号签到：账号不存在时 ok=false。
func TestRunCheckinForMissingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	s, _ := newCheckinProvider(t, f,
		&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	res, ok := s.RunCheckinFor("missing", "manual")
	if ok {
		t.Fatal("账号不存在应 ok=false")
	}
	if res.Status != "fail" || res.Detail != "账号不存在" {
		t.Errorf("res=%+v want fail/账号不存在", res)
	}
	if f.checkinCalls.Load() != 0 {
		t.Errorf("账号不存在不该打上游，实际 %d 次", f.checkinCalls.Load())
	}
}

// TestRunKeepaliveForMissingAccount 单账号保活：账号不存在时 ok=false。
func TestRunKeepaliveForMissingAccount(t *testing.T) {
	f := &fakeUpstream{}
	s, _ := newCheckinProvider(t, f,
		&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	if _, ok := s.RunKeepaliveFor("missing", "manual"); ok {
		t.Fatal("账号不存在应 ok=false")
	}
}

// TestCheckinSkipsDisabledAndCredentialessAccounts 禁用的账号被跳过；
// 无凭证的账号记 skip 而**不**打上游。
func TestCheckinSkipsDisabledAndCredentialessAccounts(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	s, p := newCheckinProvider(t, f,
		&auth.Auth{UID: "enabled", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999},
		&auth.Auth{UID: "norefresh", AccessToken: "at", RefreshToken: "", ExpiresAt: 9999999999},
	)
	p.Disable("enabled", "测试禁用")

	s.RunCheckinAll("schedule")

	if n := f.checkinCalls.Load(); n != 0 {
		t.Errorf("禁用 + 无凭证的账号都不该打上游，实际 %d 次", n)
	}
}

// ---------------------------------------------------------------------------
// isAlreadyCheckin —— 上游专属的错误文案匹配（改造前在核心调度器里）
// ---------------------------------------------------------------------------

// TestIsAlreadyCheckin 上游的业务错误识别。
//
// 这条规则**必须在 workbuddy**：它匹配的是腾讯上游的中文文案与私有错误码。
func TestIsAlreadyCheckin(t *testing.T) {
	yes := []string{
		`{"code":400,"msg":"今日已签到"}`,
		`{"code":0,"msg":"already checked in"}`,
		`upstream error: checkin failed`,
		"code=400",
		"ALREADY DONE", // 大小写不敏感
	}
	for _, s := range yes {
		if !isAlreadyCheckin(s) {
			t.Errorf("应识别为已签到: %q", s)
		}
	}
	no := []string{
		`{"code":500,"msg":"内部错误"}`,
		`{"code":12153,"msg":"Offline user session not found"}`,
		"",
		"connection reset",
		// 裸 JSON 形态的 code:400 不在判据里：改造前只认字符串 "code=400"
		// （Go 的 error.Error() 对 upstream.Error 拼出来的就是 `code=400` 那种形态）。
		// 这里把它钉住，避免有人"顺手放宽"而改变既有行为。
		`{"code":400}`,
	}
	for _, s := range no {
		if isAlreadyCheckin(s) {
			t.Errorf("不该识别为已签到: %q", s)
		}
	}
}

// TestCheckinAlreadyReportsAlready 上游回"已签到"时 status=already 且仍继续查余额。
func TestCheckinAlreadyReportsAlready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.WriteHeader(400)
			w.Write([]byte(`{"code":400,"msg":"今日已签到"}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":77,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up})

	res, _ := s.RunCheckinFor("u1", "manual")
	if res.Status != "already" {
		t.Errorf("status=%q want already（详情 %q）", res.Status, res.Detail)
	}
	if res.Detail != "今天已签到" {
		t.Errorf("detail=%q want 今天已签到", res.Detail)
	}
	// 已签到也要继续查余额（改造前就是这条路径）
	if !res.HasQuota || res.Credits != 77 {
		t.Errorf("credits=%d hasQuota=%v want 77/true", res.Credits, res.HasQuota)
	}
}

// TestCheckinFailureRecordsShortDetail 真正的失败：status=fail，detail 被压行。
func TestCheckinFailureRecordsShortDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(strings.Repeat("x", 400)))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up})

	res, _ := s.RunCheckinFor("u1", "manual")
	if res.Status != "fail" {
		t.Fatalf("status=%q want fail", res.Status)
	}
	if len(res.Detail) > 200 {
		t.Errorf("detail 过长（%d 字节），应被 shortErr 压行", len(res.Detail))
	}
}

// TestKeepaliveSaveFailureStillReportsOK 刷新成功但落盘失败：status 仍是 ok，
// detail 说明落盘问题（与改造前逐字一致）。
func TestKeepaliveSaveFailureStillReportsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	// Path 指向一个不存在的目录 → SaveAtomic 必然失败
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: 1, FilePath: filepath.Join(t.TempDir(), "no-such-dir-xyz", "auth.json")}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up})

	res, _ := s.RunKeepaliveFor("u1", "manual")
	if res.Status != "ok" {
		t.Errorf("刷新成功就该是 ok，得到 %q（%s）", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "落盘失败") {
		t.Errorf("detail 应说明落盘失败，得到 %q", res.Detail)
	}
}
