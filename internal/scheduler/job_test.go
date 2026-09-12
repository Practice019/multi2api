// job_test.go 调度框架本身的测试：注册、发现、到期判断、失败隔离、签到钩子。
//
// # 为什么不测具体的成长/旅行任务
//
// 那些在 internal/workbuddy 自己的测试里。本文件只测**框架**：
// 它必须对任何上游的任务一视同仁，且不认识任何具体任务名。
// 判据是"加新上游时本包零改动"能被这些测试守住。
package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// stubProvider 实现 gateway.Provider + JobExt —— 模拟一个注册了任务的上游。
type stubProvider struct {
	id   string
	jobs []gateway.Job
}

func (s *stubProvider) ID() string               { return s.id }
func (s *stubProvider) Caps() gateway.Capability { return gateway.CapChat }
func (s *stubProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, errors.New("stub")
}
func (s *stubProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, errors.New("stub")
}
func (s *stubProvider) Jobs() []gateway.Job { return s.jobs }

// plainProvider 只实现 Provider、**不**实现 JobExt。
type plainProvider struct{ stubProvider }

// TestJobsDiscoverFromRegistry 调度器通过 ExtOf 发现上游任务，不认识具体名字。
func TestJobsDiscoverFromRegistry(t *testing.T) {
	var ran atomic.Int32
	reg := gateway.NewRegistry()
	if err := reg.Register(&stubProvider{id: "stub", jobs: []gateway.Job{{
		Name: "alpha", Interval: time.Millisecond,
		Run: func(context.Context) error { ran.Add(1); return nil },
	}}}); err != nil {
		t.Fatal(err)
	}
	// 不实现 JobExt 的上游被静默跳过（不是错误）
	if err := reg.Register(&plainProvider{stubProvider{id: "plain"}}); err != nil {
		t.Fatal(err)
	}

	s := New(Config{Registry: reg})
	if got := s.jobs.Names(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("发现的任务 = %v，期望 [alpha]", got)
	}
	if got := s.jobs.Statuses()[0].Provider; got != "stub" {
		t.Errorf("任务归属上游 = %q，期望 stub", got)
	}
}

// TestJobsNoRegistryNoJobs 没给注册表时没有任何上游任务（既有测试走这条路径）。
func TestJobsNoRegistryNoJobs(t *testing.T) {
	s := New(Config{})
	if s.jobs.Len() != 0 {
		t.Errorf("无注册表时不该有任务，得到 %d", s.jobs.Len())
	}
}

// TestJobDueFixedInterval 无 Due 时按 Interval 固定间隔；从未跑过则立即跑。
func TestJobDueFixedInterval(t *testing.T) {
	var ran atomic.Int32
	j := NewJobs()
	if !j.add("p", gateway.Job{
		Name: "tick", Interval: time.Hour,
		Run: func(context.Context) error { ran.Add(1); return nil },
	}) {
		t.Fatal("注册失败")
	}

	now := time.Now()
	j.RunOnce(now) // 首轮：从未跑过 → 立即跑
	if ran.Load() != 1 {
		t.Fatalf("首轮应执行，实际 %d 次", ran.Load())
	}
	j.RunOnce(now.Add(time.Minute)) // 未到间隔 → 不跑
	if ran.Load() != 1 {
		t.Errorf("未到间隔不该执行，实际 %d 次", ran.Load())
	}
	j.RunOnce(now.Add(2 * time.Hour)) // 超过间隔 → 跑
	if ran.Load() != 2 {
		t.Errorf("超过间隔应执行，实际 %d 次", ran.Load())
	}
}

// TestJobDueOverridesInterval 有 Due 时**完全**由上游决定，核心不叠加间隔条件。
func TestJobDueOverridesInterval(t *testing.T) {
	var ran atomic.Int32
	var due atomic.Bool
	j := NewJobs()
	j.add("p", gateway.Job{
		Name: "gated", Interval: time.Hour,
		Run: func(context.Context) error { ran.Add(1); return nil },
		Due: func(time.Time) bool { return due.Load() },
	})

	due.Store(true)
	j.RunOnce(time.Now())
	if ran.Load() != 1 {
		t.Fatalf("Due 为真时应执行，实际 %d 次", ran.Load())
	}
	// 立刻再问一次：Due 仍为真 → 应再跑。若核心叠加了 Interval 就会被挡住。
	j.RunOnce(time.Now())
	if ran.Load() != 2 {
		t.Errorf("Due 为真时不该被 Interval 二次拦截（核心只信上游），实际 %d 次", ran.Load())
	}
	due.Store(false)
	j.RunOnce(time.Now())
	if ran.Load() != 2 {
		t.Errorf("Due 为假时不该执行，实际 %d 次", ran.Load())
	}
}

// TestJobPanicIsIsolated 单个任务 panic 不影响其它任务，且下一轮照常。
func TestJobPanicIsIsolated(t *testing.T) {
	var okRan atomic.Int32
	j := NewJobs()
	j.add("p1", gateway.Job{
		Name: "boom", Interval: time.Millisecond,
		Run: func(context.Context) error { panic("上游代码炸了") },
	})
	j.add("p2", gateway.Job{
		Name: "fine", Interval: time.Millisecond,
		Run: func(context.Context) error { okRan.Add(1); return nil },
	})

	j.RunOnce(time.Now()) // 不应把 RunOnce 带走
	if okRan.Load() != 1 {
		t.Fatalf("一个任务 panic 不该拖垮其它任务，实际 fine 跑了 %d 次", okRan.Load())
	}
	st := j.Statuses()
	for _, s := range st {
		if s.Name == "boom" && s.LastError == "" {
			t.Error("panic 应被记进 LastError")
		}
	}
}

// TestJobErrorDoesNotStopOthers 任务返回 error 也只记录，不影响其它任务。
func TestJobErrorDoesNotStopOthers(t *testing.T) {
	var okRan atomic.Int32
	j := NewJobs()
	j.add("p", gateway.Job{
		Name: "err", Interval: time.Millisecond,
		Run: func(context.Context) error { return errors.New("上游 500") },
	})
	j.add("p", gateway.Job{
		Name: "ok", Interval: time.Millisecond,
		Run: func(context.Context) error { okRan.Add(1); return nil },
	})
	j.RunOnce(time.Now())
	if okRan.Load() != 1 {
		t.Errorf("error 不该中断其它任务，实际 ok 跑了 %d 次", okRan.Load())
	}
}

// TestJobRejectsDuplicateName 重名任务被拒绝（不能静默接受）。
func TestJobRejectsDuplicateName(t *testing.T) {
	j := NewJobs()
	job := gateway.Job{Name: "dup", Run: func(context.Context) error { return nil }}
	if !j.add("p1", job) {
		t.Fatal("首个应注册成功")
	}
	if j.add("p2", job) {
		t.Error("重名任务应被拒绝 —— 静默接受会让日志分不清是谁在跑")
	}
	if j.Len() != 1 {
		t.Errorf("任务数 = %d，期望 1", j.Len())
	}
}

// TestJobRejectsEmptyNameAndNilRun 无名或无 Run 的任务被拒绝。
func TestJobRejectsEmptyNameAndNilRun(t *testing.T) {
	j := NewJobs()
	if j.add("p", gateway.Job{Run: func(context.Context) error { return nil }}) {
		t.Error("无名任务应被拒绝")
	}
	if j.add("p", gateway.Job{Name: "x"}) {
		t.Error("无 Run 的任务应被拒绝")
	}
	if j.Len() != 0 {
		t.Errorf("任务数 = %d，期望 0", j.Len())
	}
}

// TestJobsRunStopsOnCancel 无任务时 Run 直接等 ctx 退出，不空转。
func TestJobsRunStopsOnCancel(t *testing.T) {
	j := NewJobs()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("无任务时 Run 应阻塞等 ctx 退出")
	}
}

// TestSchedulerRunStopsOnCancelWithJobs 有上游任务时 Run 也能被 ctx 取消。
func TestSchedulerRunStopsOnCancelWithJobs(t *testing.T) {
	reg := gateway.NewRegistry()
	reg.Register(&stubProvider{id: "stub", jobs: []gateway.Job{{
		Name: "always", Interval: time.Millisecond,
		Run: func(context.Context) error { return nil },
	}}})
	s := New(Config{Registry: reg, CheckinDisabled: true, KeepaliveDisabled: true})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

// ---- 签到搭车钩子 ----

// stubHook 记录被调用的次数。
type stubHook struct {
	name string
	n    atomic.Int32
	// panicOn 为真时 AfterCheckin panic。
	panicOn bool
}

func (h *stubHook) AfterCheckin() {
	h.n.Add(1)
	if h.panicOn {
		panic("钩子炸了")
	}
}
func (h *stubHook) HookName() string { return h.name }

// TestCheckinRunsUpstreamHook 核心跑完签到后必须喊一次钩子。
//
// 这条覆盖「签到搭车」这半条链路：核心确实会在签到收尾时通知上游。
// 另外半条（上游收到通知确实推进了旅行）在 internal/workbuddy 的
// TestAfterCheckinHookRunsTravel 里。两半合起来等价于改造前的
// TestRunCheckinNowTriggersTravel。
func TestCheckinRunsUpstreamHook(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	s := New(Config{Pool: p, Upstream: up})
	h := &stubHook{name: "travel"}
	s.AddCheckinHook(h)

	s.RunCheckinNow()

	if h.n.Load() != 1 {
		t.Errorf("签到收尾应恰好喊一次钩子，实际 %d 次", h.n.Load())
	}
	if f.checkinCalls.Load() != 1 {
		t.Errorf("签到本身仍要执行，实际 %d 次", f.checkinCalls.Load())
	}
}

// TestCheckinHookPanicDoesNotAbort 钩子 panic 不影响签到主流程，也不影响其它钩子。
func TestCheckinHookPanicDoesNotAbort(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	s := New(Config{Pool: p, Upstream: up})
	bad := &stubHook{name: "bad", panicOn: true}
	good := &stubHook{name: "good"}
	s.AddCheckinHook(bad)
	s.AddCheckinHook(good)

	s.RunCheckinNow() // 不应 panic

	if good.n.Load() != 1 {
		t.Errorf("前一个钩子 panic 不该阻止后续钩子，实际 %d 次", good.n.Load())
	}
	// 签到本身照常完成：余额已同步进池。
	if st, _ := p.Status("u1"); st.Credits != 100 {
		t.Errorf("钩子 panic 不该影响签到结果，credits=%d want 100", st.Credits)
	}
}

// TestAddCheckinHookIgnoresNil nil 钩子被静默忽略（不会在收尾时 panic）。
func TestAddCheckinHookIgnoresNil(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	s := New(Config{Pool: p, Upstream: up})
	s.AddCheckinHook(nil)
	s.RunCheckinNow() // 不应 panic

	if len(s.hooks) != 0 {
		t.Errorf("nil 钩子不该被登记，实际 %d 个", len(s.hooks))
	}
}
