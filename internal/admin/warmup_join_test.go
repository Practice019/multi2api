// warmup_join_test.go 启动预热 goroutine 的"可等待"契约（2026-11-13 修 CI 抖动）。
//
// # 抖动的根因（证据在回报里：CI 原始日志 ×2 + 代码）
//
//	`admin.New` 末尾会起一个**不在任何人管辖范围内**的预热 goroutine，它调
//	`h.statsItems()` **读库**；而 `store.DB` 是**包级单例**，测试里它的文件位于
//	`t.TempDir()`。像 `TestChannelsKeysUnknownChannel` 这种 0.01s 的用例，
//	`New()` 之后立刻结束 ⇒ 预热还在读，`testing` 已经在拆那个目录 ⇒
//
//		TempDir RemoveAll cleanup: unlinkat /tmp/TestChannelsKeysUnknownChannel…/001: directory not empty
//
//	⇒ 红在**清理**，与用例断言毫无关系；因为时序相关，所以偶发（CI 上 3 次）。
//
// # 修法
//
//	给预热一个完成信号 `Handler.WaitWarmup`，测试的夹具在**清理阶段最前面**等它 ——
//	`t.Cleanup` 是 LIFO ⇒ 夹具后期注册的"等预热"先跑，然后才是 `store.Close()`
//	与 `t.TempDir()` 的 RemoveAll ⇒ 不再有"读库的活人在拆目录"。
package admin

import (
	"testing"
	"time"
)

// waitWarmupCleanup 在清理阶段**最前面**等预热跑完。
//
// ⚠ 必须在 `New(...)` **之后**调用：t.Cleanup 是 LIFO ⇒ 后注册的先执行，
//
//	于是顺序是「等预热」→「store.Close」→「t.TempDir 的 RemoveAll」。
//	反过来（先注册等待）等于没修：等待会排在关库与拆目录的后面。
func waitWarmupCleanup(t *testing.T, h *Handler) {
	t.Helper()
	if h == nil {
		return
	}
	t.Cleanup(func() {
		if !h.WaitWarmup(5 * time.Second) {
			t.Errorf("预热 goroutine 5s 内没结束 —— 它还在读库（store.DB 是包级单例），" +
				"会与 t.TempDir() 的 RemoveAll 抢目录（表现为 `directory not empty` 的假红）")
		}
	})
}

// TestWarmupIsJoinable 预热必须有可等信号，且等待是幂等的（可重复等）。
//
// ⚠ 这条**不**断言"预热做了什么"（那是 statsItems 的事），只断言"能等到它结束"——
//
//	因为"等不到"正是那次抖动里唯一让测试变红的东西。
func TestWarmupIsJoinable(t *testing.T) {
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true})
	if !h.WaitWarmup(10 * time.Second) {
		t.Fatal("预热 goroutine 没在 10s 内结束 —— 它没有完成信号，测试清理阶段必然与它抢资源")
	}
	// 幂等：再来一次仍然立刻为真（不是"一次性消费"的 channel 语义陷阱）。
	if !h.WaitWarmup(time.Second) {
		t.Error("WaitWarmup 不可重复等待 —— 第二次等到超时")
	}
	// 没有预热的 Handler（零值）视为已完成，调用方不必区分两种情况。
	var empty Handler
	if !empty.WaitWarmup(time.Second) {
		t.Error("零值 Handler.WaitWarmup 应视为已完成")
	}
}

// TestWarmupRunsSynchronouslyInTests ★ 测试进程里 `New()` 返回时预热必须**已经结束**。
//
// 这是那次 CI 抖动的**确定性守卫**（不依赖时序、不等超时）：
//
//	预热如果在后台跑，它就会在用例结束之后还去读 `store.DB`（包级单例，
//	文件在 `t.TempDir()`）⇒ 与 RemoveAll 抢目录 ⇒ `directory not empty` 的假红。
//	同步化之后这条不可能再发生，而**任何回到 `go h.runWarmup()` 的改动都会让本用例红**。
func TestWarmupRunsSynchronouslyInTests(t *testing.T) {
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true})
	select {
	case <-h.warmupDone:
		// 期望路径：New 返回时已完成（testing.Testing() 走了同步分支）
	default:
		t.Error("New() 返回时预热仍在后台跑 —— 它会与 t.TempDir() 的 RemoveAll 抢目录" +
			"（CI 上表现为 `TempDir RemoveAll cleanup: … directory not empty` 的偶发红）")
	}
}
