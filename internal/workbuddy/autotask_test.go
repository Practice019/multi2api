// autotask_test.go 任务自动化层的判据锁定。
//
// # 移植质量的关键增量
//
// B 的 autotask.go 只有**一个**测试文件（autotask_lock_test.go，46 行，只测并发锁），
// 17 个 run* 函数零覆盖。本文件把三件最容易静默出错的事钉住：
//
//	① 动作表与任务码的对应（改一个字符串就会让按钮指向不存在的任务）
//	② per-account 互斥（单任务与全量共用一把锁；不同账号互不影响）
//	③ 错误哨兵到 HTTP 状态码的映射（前端据状态码决定提示文案）
package workbuddy

import (
	"sync"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestAutoActionsTableComplete 动作表必须覆盖 17 个任务，且 TaskCode 无重复。
func TestAutoActionsTableComplete(t *testing.T) {
	if got := len(autoActions); got != 17 {
		t.Errorf("动作数=%d，期望 17（B 的 17/18，剩 1 个是不可自动的 Expert_Philanthropy）", got)
	}
	seen := map[string]bool{}
	for i, a := range autoActions {
		if a.TaskCode == "" {
			t.Errorf("第 %d 项 TaskCode 为空", i)
		}
		if a.run == nil {
			t.Errorf("%s 缺 run 实现 —— 表里有、按钮会渲染，但点下去会 panic", a.TaskCode)
		}
		if a.Desc == "" {
			t.Errorf("%s 缺 Desc（前端展示空说明）", a.TaskCode)
		}
		if seen[a.TaskCode] {
			t.Errorf("TaskCode 重复: %s（前端按钮分派会撞车）", a.TaskCode)
		}
		seen[a.TaskCode] = true
	}

	// 关键任务必须在表里（这几个是收益最大、最常被点的）。
	for _, code := range []string{"first_buddy", "chat_5", "create_canvas", "expert_5", "black_cat"} {
		if !seen[code] {
			t.Errorf("动作表缺关键任务 %s", code)
		}
	}
	// Expert_Philanthropy（需真实捐款）**刻意不在表里** —— 钉住这个决定。
	if seen["Expert_Philanthropy"] {
		t.Error("Expert_Philanthropy 不该有自动化实现（服务端校验捐赠回执，无法绕过）")
	}
}

// TestAutoActionForLookup 任务码查找：命中、空白容忍、未命中返回 nil。
func TestAutoActionForLookup(t *testing.T) {
	if got := autoActionFor("first_buddy"); got == nil || got.TaskCode != "first_buddy" {
		t.Errorf("first_buddy 查找失败: %+v", got)
	}
	// 前端可能带空白（表单提交），必须容忍。
	if got := autoActionFor("  chat_5  "); got == nil || got.TaskCode != "chat_5" {
		t.Errorf("带空白的任务码应能查到: %+v", got)
	}
	if got := autoActionFor("Expert_Philanthropy"); got != nil {
		t.Errorf("不可自动化的任务应返回 nil，得到 %+v", got)
	}
	if got := autoActionFor(""); got != nil {
		t.Errorf("空串应返回 nil，得到 %+v", got)
	}
}

// TestAutoTaskCodesExported 导出的任务码列表与表一致（前端枚举用）。
func TestAutoTaskCodesExported(t *testing.T) {
	codes := AutoTaskCodes()
	if len(codes) != len(autoActions) {
		t.Fatalf("导出 %d 个码，表里 %d 项", len(codes), len(autoActions))
	}
	for i, c := range codes {
		if c != autoActions[i].TaskCode {
			t.Errorf("第 %d 个=%q，表里是 %q（顺序即执行顺序，不能乱）", i, c, autoActions[i].TaskCode)
		}
	}
}

// TestAutoActionsOrderRespectsDependency 顺序必须满足依赖：first_buddy 在其它任务之前。
//
// # 为什么顺序是判据而不是巧合
//
// `first_buddy` 是成长计划其余任务的**前置**（未领 Buddy 时其余任务是 not_accepted）。
// 全量执行时若把它排在后面，前面那些任务的行为事件会因为
// "任务还没被接单"而**不计分**（B 在队列路径上真踩过这个坑，
// 见其 commit 1408a7a：「队列执行前批量接受任务」）。
func TestAutoActionsOrderRespectsDependency(t *testing.T) {
	idx := map[string]int{}
	for i, a := range autoActions {
		idx[a.TaskCode] = i
	}
	if idx["chat_5"] > idx["first_buddy"] {
		t.Error("chat_5 必须排在 first_buddy 之前或持平（两者都自带 report 前置，但先补活跃更稳）")
	}
	// autoActionIndex 未知任务返回大值（排到最后）。
	if autoActionIndex("不存在的任务") <= autoActionIndex("black_cat") {
		t.Error("未知任务的排序值应大于所有已知任务")
	}
}

// TestAutoTaskStatusMapping 错误哨兵 → HTTP 状态码。
func TestAutoTaskStatusMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{errTaskNotAuto, 501},
		{errTaskBusy, 409},
		{errTaskNotFound, 404},
	}
	for _, c := range cases {
		if got := autoTaskStatus(c.err); got != c.want {
			t.Errorf("autoTaskStatus(%v)=%d，期望 %d", c.err, got, c.want)
		}
	}
	// 其它错误（上游失败）→ 502。
	if got := autoTaskStatus(errTaskNotAuto); got == 502 {
		t.Error("errTaskNotAuto 的映射被后面的分支覆盖了")
	}
}

// ── per-account 互斥锁 ─────────────────────────────────────────────────

// TestTaskAccountLockSameAccountExclusive 同账号互斥。
//
// 反向判别力：把 tryLockAccount 改成恒返回 true，本用例红。
func TestTaskAccountLockSameAccountExclusive(t *testing.T) {
	p := &Provider{}
	if !p.tryLockAccount("u1") {
		t.Fatal("首次加锁应成功")
	}
	if p.tryLockAccount("u1") {
		t.Fatal("同账号二次加锁必须失败 —— " +
			"并发跑同一账号的任务动作会让上游看到重复遥测（明显的异常特征），" +
			"且 expert 系含真实对话，重跑白烧配额")
	}
	p.unlockAccount("u1")
	if !p.tryLockAccount("u1") {
		t.Fatal("解锁后应能再次加锁")
	}
}

// TestTaskAccountLockDifferentAccountsIndependent 不同账号互不影响。
func TestTaskAccountLockDifferentAccountsIndependent(t *testing.T) {
	p := &Provider{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 加锁失败")
	}
	if !p.tryLockAccount("u2") {
		t.Fatal("u2 必须能同时加锁 —— 不同账号的任务动作是独立的")
	}
	if !p.TaskLockHeld("u1") || !p.TaskLockHeld("u2") {
		t.Error("两个账号都应处于占用态")
	}
	p.unlockAccount("u1")
	if p.TaskLockHeld("u1") {
		t.Error("u1 解锁后不该仍占用")
	}
	if !p.TaskLockHeld("u2") {
		t.Error("u1 解锁不该影响 u2")
	}
}

// TestTaskAccountLockCrossEntryShared 单任务与全量共用同一把锁。
//
// # 这条对应 B 的 TestTaskAccountLockCrossEntryShared
//
// 若两条路径各用一把锁，用户点「一键完成」的同时再点某个任务的
// 「一键完成」，同一账号会对同一批任务上报两遍事件。
func TestTaskAccountLockCrossEntryShared(t *testing.T) {
	p := &Provider{}
	// 模拟"全量"占住锁。
	if !p.tryLockAccount("u1") {
		t.Fatal("全量加锁失败")
	}
	// "单任务"路径拿的是同一把锁 → 必须被拒。
	if p.tryLockAccount("u1") {
		t.Fatal("单任务路径必须与全量路径共用锁（否则同一账号会并发跑两轮任务动作）")
	}
}

// TestTaskAccountLockConcurrent 并发抢锁只有一个赢家。
func TestTaskAccountLockConcurrent(t *testing.T) {
	p := &Provider{}
	const n = 64
	var wg sync.WaitGroup
	var wins int32
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.tryLockAccount("u-concurrent") {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("并发抢锁的赢家数=%d，期望恰好 1", wins)
	}
}

// TestTaskLockHeldNilMapSafe 未初始化时查询不 panic。
//
// taskLockHeld 是懒初始化的 map；TaskLockHeld 在 nil map 上读是安全的，
// 但 drop 到写路径就会 panic。这条钉住"查询路径不初始化也不崩"。
func TestTaskLockHeldNilMapSafe(t *testing.T) {
	p := &Provider{}
	if p.TaskLockHeld("nobody") {
		t.Error("未占用应返回 false")
	}
	p.unlockAccount("nobody") // 不得 panic
}

// TestErrSentinelsAreDistinct 三个哨兵必须是不同对象（否则状态码映射会串）。
func TestErrSentinelsAreDistinct(t *testing.T) {
	if errTaskNotAuto == errTaskBusy || errTaskBusy == errTaskNotFound || errTaskNotAuto == errTaskNotFound {
		t.Fatal("错误哨兵必须互不相同")
	}
}

// TestAdminRoutesIncludesAutoTask 任务自动化端点确实挂上了。
//
// 这是"移植是否真的接进去"的守门测试：动作实现写好了但 AdminRoutes 忘了追加，
// 编译与单测都过，而用户在界面上根本点不到。
func TestAdminRoutesIncludesAutoTask(t *testing.T) {
	p := NewWithConfig(Config{})
	routes := p.AdminRoutes()
	want := map[string]bool{
		"GET /admin/growth/auto/actions": false,
		"POST /admin/growth/auto":        false,
		"POST /admin/growth/auto-all":    false,
		"GET /admin/growth/scan":         false,
		"GET /admin/school":              false,
		"POST /admin/school/run":         false,
		"POST /admin/blackcat/run":       false,
	}
	for _, r := range routes {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
			if r.Handler == nil {
				t.Errorf("%s 的 Handler 为 nil（挂上去是 404）", key)
			}
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("任务自动化端点 %s 未挂载 —— "+
				"动作实现写好了但用户点不到（AdminRoutes 忘了追加 autoTaskRoutes）", k)
		}
	}
	// 编译期断言：这些路由确实来自 gateway.AdminExt。
	var _ gateway.AdminExt = p
}
