package workbuddy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestKeepaliveIdleDue 心跳门：一小时放一个 INFO，窗口内其余全部走 DEBUG。
//
// 背景（2026-11-12，用户报"日志在刷屏"）：workbuddy 的"续期 0，跳过 6，失败 0"
// 在两天日志里出现 172 次。但它**不能直接删掉** —— keepalive_summary_test.go 里
// 那条测试的理由成立："扫了 0 个"与"没扫"是两件事，后者说明调度挂了。
// ⇒ 折中成心跳（见 keepaliveIdleEvery 的注释）。
func TestKeepaliveIdleDue(t *testing.T) {
	base := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	keepaliveIdleLast.Store(0)
	if !keepaliveIdleDue(base) {
		t.Fatal("首次必须放行：否则「调度还活着」这件事永远没有输出")
	}
	if keepaliveIdleDue(base.Add(59 * time.Minute)) {
		t.Fatal("一小时窗口内不该再放行")
	}
	if !keepaliveIdleDue(base.Add(time.Hour)) {
		t.Fatal("超过一小时后应再放行一次")
	}

	// 并发：一小时窗口内 100 个并发只放一个（CAS 保证）。
	keepaliveIdleLast.Store(0)
	now := base
	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if keepaliveIdleDue(now) {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("一小时窗口内应恰好放行 1 个，实际 %d", ok)
	}
}
