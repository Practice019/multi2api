package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingRefresh 假刷新：数调用次数，可选"每次调用都 panic"或"阻塞到 ctx 结束"。
type countingRefresh struct {
	calls   int32
	panicOn bool
	block   chan struct{} // 非 nil 时每次调用都阻塞在这里（测"不阻塞调用方"）
}

func (c *countingRefresh) run() {
	atomic.AddInt32(&c.calls, 1)
	if c.block != nil {
		<-c.block
	}
	if c.panicOn {
		panic("假刷新：模拟刷新函数内部 panic")
	}
}

func (c *countingRefresh) count() int { return int(atomic.LoadInt32(&c.calls)) }

// TestPrewarmClosesFailOpenWindow 这条就是本任务的核心判据：
// 缓存冷 ⇒ 未知名**放行**（fail-open，老行为）；预热一次 ⇒ 同一个名字变 400。
//
// 复现的是用户实测的那个窗口：重启后立刻打错名字会掉进账号池、逐个试号白烧上游，
// 先 GET 一次 /v1/models 把缓存灌热，同样请求才被拦。
func TestPrewarmClosesFailOpenWindow(t *testing.T) {
	resetDynamicCache()
	sp := newSpyRouter("workbuddy")
	h := guardHandler(t, sp)

	// ① 冷缓存：守卫 fail-open ⇒ 不拦（这就是要被关掉的窗口）
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, guardChatReq(t, "definitely-not-a-model-xyz"))
	if rec.Code == http.StatusBadRequest && errorCodeOf(t, rec.Body.Bytes()) == "unknown_model" {
		t.Fatalf("冷缓存本应 fail-open（放行），却被拦了：%s —— 那这条测试就不在测窗口", rec.Body.String())
	}

	// ② 预热一次：缓存被灌热
	h.prewarmModelsOnce()
	if h.catalogCold() {
		t.Fatal("预热之后缓存仍为空 ⇒ 预热没起作用")
	}

	// ③ 同一个名字：现在必须 400，且**不调 Provider**（拦在试号之前）
	sp.chatCall = 0
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, guardChatReq(t, "definitely-not-a-model-xyz"))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("预热后未知名应回 400，实际 %d（body=%s）", rec2.Code, rec2.Body.String())
	}
	if code := errorCodeOf(t, rec2.Body.Bytes()); code != "unknown_model" {
		t.Errorf("error.code = %q，want unknown_model", code)
	}
	if sp.chatCall != 0 {
		t.Errorf("拦住必须发生在试号之前，实际调了 Provider %d 次", sp.chatCall)
	}

	// ④ 目录里真实存在的裸名不受影响（预热不许误伤合法调用）
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, guardChatReq(t, "auto"))
	if rec3.Code == http.StatusBadRequest && errorCodeOf(t, rec3.Body.Bytes()) == "unknown_model" {
		t.Errorf("目录里的裸名被误拦：%s", rec3.Body.String())
	}
}

// TestPrewarmDoesNotBlockCaller StartModelsPrewarm 必须立刻返回 —— 即使刷新函数卡死。
//
// 这条是"预热放在启动路径上会不会把启动拖住"的保险：刷新在 goroutine 里跑，
// 调用方（main 的启动流程）不许被它挡住。
func TestPrewarmDoesNotBlockCaller(t *testing.T) {
	resetDynamicCache()
	h := NewHandler(Config{})
	blocked := make(chan struct{})
	cr := &countingRefresh{block: blocked}
	h.cfg.ModelsRefresh = cr.run

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		h.StartModelsPrewarm(ctx, time.Hour)
		done <- time.Since(start)
	}()

	select {
	case d := <-done:
		if d > 500*time.Millisecond {
			t.Errorf("StartModelsPrewarm 阻塞了 %v —— 它必须立刻返回", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartModelsPrewarm 被卡死的刷新函数挡住了（没有立刻返回）")
	}

	// 刷新确实已经在后台跑起来了（否则"不阻塞"就只是"什么都没做"）
	deadline := time.Now().Add(2 * time.Second)
	for cr.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cr.count() == 0 {
		t.Error("后台没有跑刷新")
	}
	close(blocked) // 放它走，避免测试结束时留下卡死的 goroutine
	cancel()
}

// TestPrewarmIsIdempotent 对同一个 Handler 重复调用不会起第二份跑批。
func TestPrewarmIsIdempotent(t *testing.T) {
	resetDynamicCache()
	h := NewHandler(Config{})
	cr := &countingRefresh{}
	h.cfg.ModelsRefresh = cr.run
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.StartModelsPrewarm(ctx, time.Hour)
	h.StartModelsPrewarm(ctx, time.Hour)
	h.StartModelsPrewarm(ctx, time.Hour)

	deadline := time.Now().Add(2 * time.Second)
	for cr.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !h.prewarmRunning() {
		t.Error("预热没有启动")
	}
	// interval=1h ⇒ 只有"立刻那一次"；三次调用若各起一份 goroutine，调用次数会翻倍。
	time.Sleep(150 * time.Millisecond)
	if got := cr.count(); got != 1 {
		t.Errorf("刷新次数 = %d，want 1（重复调用不得起第二份跑批）", got)
	}
}

// TestPrewarmStopsOnContextCancel ctx 取消后不再刷新（不泄漏 goroutine）。
func TestPrewarmStopsOnContextCancel(t *testing.T) {
	resetDynamicCache()
	h := NewHandler(Config{})
	cr := &countingRefresh{}
	h.cfg.ModelsRefresh = cr.run
	ctx, cancel := context.WithCancel(context.Background())

	h.StartModelsPrewarm(ctx, 20*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for cr.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cr.count() < 2 {
		t.Fatalf("周期性刷新没跑起来（次数=%d）", cr.count())
	}
	cancel()
	time.Sleep(120 * time.Millisecond) // 比 interval 长得多：还在跑的话一定会多出调用
	before := cr.count()
	time.Sleep(200 * time.Millisecond)
	if after := cr.count(); after != before {
		t.Errorf("ctx 取消后仍在刷新：%d → %d", before, after)
	}
}

// TestPrewarmRecoversFromPanic 刷新函数 panic 不许把预热带走（也不许冒到调用方）。
//
// ⚠ 这条是**变异验证的靶子**：把 prewarmModelsOnce 里的 recover 去掉，本用例必须红
// （panic 会直接冒到 goroutine 外面，进程级崩溃）。
func TestPrewarmRecoversFromPanic(t *testing.T) {
	resetDynamicCache()
	h := NewHandler(Config{})
	cr := &countingRefresh{panicOn: true}
	h.cfg.ModelsRefresh = cr.run

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("刷新 panic 冒出了 prewarmModelsOnce（recover 失效）：%v", r)
		}
	}()
	h.prewarmModelsOnce()

	if cr.count() == 0 {
		t.Error("刷新根本没被调用")
	}
	// panic 之后仍要能再刷（不能把状态写坏）
	h.prewarmModelsOnce()
}

// TestPrewarmSkipsWhenUnassembled 未装配（没有 Pool/Upstream，也没注入刷新）时直接跳过，不 panic。
//
// 这条对应真实场景：单上游裸跑 / 测试里新建的 Handler。fetchDynamicModels 第一件事是
// Pool.PickForExcluding，nil 池会 panic —— 所以必须在调用前挡住。
func TestPrewarmSkipsWhenUnassembled(t *testing.T) {
	resetDynamicCache()
	h := NewHandler(Config{})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("未装配时应当跳过，实际 panic：%v", r)
		}
	}()
	h.prewarmModelsOnce()
	if !h.catalogCold() {
		t.Error("未装配时不该写入任何缓存")
	}
}
