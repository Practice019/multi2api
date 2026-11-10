package server

import (
	"os"
	"strings"
	"testing"
)

// 本文件守的是**两条路径必须同源**这一类不变式（与 armor_source_guard_test.go 同一动机）。
//
// # 为什么必须用源码级守卫（这两条都真出过事）
//
// 两条路径的差异只在装配细节上，行为测试要同时驱动"普通 chat"与"工具循环"才覆盖得到，
// 而且**将来新加第三条路径时它不会红** —— 而那正是缺陷再次出现的方式。
// 源码级守卫正好补这一刀：盯住那两行必须存在的赋值/初始化，谁漏了就点名。

func handlerSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("handler.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestToolLoopFailureSetsLastKind 工具循环的硬失败分支必须**同时**设 lastErr 与 lastKind。
//
// # 漏掉它的后果（2026-11-10 实测，55300 条请求日志）
//
// lastKind 停在零值 ErrKindNone ⇒ 收尾那段按 lastKind 选状态码/错误码/文案的判决三条全落错分支：
// 上游 502 被报成 **503 + code=no_healthy_account + 「all accounts unavailable (cooling/disabled)」**。
//
// loomy 走工具循环 ⇒ 它的**每个**请求都经过这个分支（shouldRunToolLoop 只看上游是否声明了
// ChatToolExt，不看客户端带没带工具），于是 loomy 的 326 次失败里 296 次（91%）是这种误报，
// 而 loomy 一次 502 都没回过。日志里 `kind=none` 与 `last=loomy server 502` 并存就是证据。
//
// # 为什么按"标记 + 窗口"扫，而不是行号
// 行号会随任何一次无关编辑漂移；标记是那段的语义锚点。窗口给足（40 行）以容纳注释增长，
// 但**不**跨越更多分支 —— 否则守卫会被相邻分支的赋值蒙混过关。
func TestToolLoopFailureSetsLastKind(t *testing.T) {
	src := handlerSource(t)
	const marker = "循环内的硬失败"
	at := strings.Index(src, marker)
	if at < 0 {
		t.Fatalf("找不到工具循环硬失败分支的语义锚点 %q —— 守卫的打点方式失效了，请同步更新", marker)
	}
	window := src[at:]
	if len(window) > 40*80 {
		window = window[:40*80]
	}
	if !strings.Contains(window, "lastKind = gateway.ErrKindServer") {
		t.Errorf("工具循环的硬失败分支没有设 lastKind（只设 lastErr 会让 502 被报成 503 no_healthy_account）。" +
			"该分支的 failBrief 用的是 ErrKindServer，lastKind 必须与它一致")
	}

	// 反向断言：lastErr 也必须还在（只补 lastKind 而丢了 lastErr 是另一种走偏）。
	if !strings.Contains(window, "lastErr = werr") {
		t.Errorf("工具循环的硬失败分支丢了 lastErr = werr（错误原文会消失）")
	}
}

// TestSwitchUpstreamNeverSwitchesToItself 跨上游切换必须把**当前上游**一开始就登记为已试过。
//
// # 不登记会怎样（2026-11-10 实测）
//
// nextAliasUpstream 只在 `tried` 里排除候选，而 aliasOrder 的第一个候选往往就是当前这家
// （用户配的正是「这几个内部模型对外同名」）⇒ 它返回**自己** ⇒ 调用方 `goto retryUpstream`
// 重跑整轮 ⇒ 同一家上游、同一批号被打了两遍（3 次 → 6 次）；且 attempts 在每轮开头重算，
// 日志里看不出真实次数。实测 124 次切换里 **111 次切到了自己（89.5%）**。
func TestSwitchUpstreamNeverSwitchesToItself(t *testing.T) {
	src := handlerSource(t)
	const want = "switchedFrom := map[string]bool{reqProvider: true}"
	if !strings.Contains(src, want) {
		t.Errorf("switchedFrom 没有把当前上游登记为已试过（期望源码里出现 %q）。"+
			"少了它，nextAliasUpstream 会返回自己，切换退化成空转 —— 上游调用翻倍", want)
	}
	// 反向断言：不得退回到空初始化（那正是被修掉的写法）。
	if strings.Contains(src, "switchedFrom := map[string]bool{}") {
		t.Errorf("switchedFrom 又是空初始化的（当前上游没登记）—— 空转切换会立刻复发")
	}
}
