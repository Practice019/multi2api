// sse_usage_full_cache_test.go 全命中 / 全未命中的守卫（2026-10-20 review 补）。
//
// # 为什么要单独钉
//
// `/review` 自查 ff80045 时发现我自己引入的坑：`promptTotal()` 当时写成
//
//	if u.CacheHitTokens > 0 && u.CacheMissTokens > 0 {   // ← 要求两者都 >0
//
// 于是**全命中**（`miss=0`）与**全未命中**（`hit=0`）的合法帧被判"输入未知"，
// `valid()` 可能为 false ⇒ **整帧丢弃** ⇒ total 退化成估算。
//
// 讽刺的是：这正是 ff80045 要修的那个 bug 的翻版 —— 我为修"缓存量消失"
// 新建了一个"全命中被丢掉"。而全命中恰恰是高缓存命中会话的正常形态。
//
// 根因是 JSON 里**「键缺失」与「值为 0」不可区分**：都是 Go 零值 0。
// 修法是 frameUsage 里额外解析一次原始键，记录 hasCacheHit / hasCacheMiss。
package server

import "testing"

// TestSSEUsageAllHit 全命中：miss 键存在且为 0，应按 hit 单独还原输入。
func TestSSEUsageAllHit(t *testing.T) {
	u := frameUsage(`{"usage":{"completion_tokens":100,"prompt_cache_hit_tokens":63360,"prompt_cache_miss_tokens":0}}`)
	if u == nil {
		t.Fatal("frameUsage 返回 nil")
	}
	if !u.valid() {
		t.Fatalf("★ 全命中的合法帧被判无效 —— 那会整帧丢弃，total 退化成估算")
	}
	if got := u.promptTotal(); got != 63360 {
		t.Fatalf("★ 全命中时 promptTotal 应为 63360（miss=0 不是「未知」），实际 %d", got)
	}
	if got := u.total(); got != 63460 {
		t.Fatalf("total 应为 63360+100=63460，实际 %d", got)
	}
}

// TestSSEUsageAllMiss 全未命中：hit 键存在且为 0，应按 miss 单独还原输入。
func TestSSEUsageAllMiss(t *testing.T) {
	u := frameUsage(`{"usage":{"completion_tokens":100,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":5000}}`)
	if !u.valid() {
		t.Fatalf("★ 全未命中的合法帧被判无效")
	}
	if got := u.promptTotal(); got != 5000 {
		t.Fatalf("★ 全未命中时 promptTotal 应为 5000（hit=0 不是「未知」），实际 %d", got)
	}
	if got := u.total(); got != 5100 {
		t.Fatalf("total 应为 5100，实际 %d", got)
	}
}

// TestSSEUsageMissingMissKeyIsUnknown 键真的缺失时才按未知处理。
//
// ⚠ 这条与 TestSSEUsageHalfCacheIsNotSummed 是同一个判据的两面：
//
//	· miss 键**不存在** ⇒ 未知 ⇒ 不猜（返回 0）
//	· miss 键存在且为 0 ⇒ 全命中 ⇒ 可以算
func TestSSEUsageMissingMissKeyIsUnknown(t *testing.T) {
	u := frameUsage(`{"usage":{"completion_tokens":100,"prompt_cache_hit_tokens":800}}`)
	if u.hasCacheMiss {
		t.Fatal("★ miss 键不存在时 hasCacheMiss 必须为 false")
	}
	if !u.hasCacheHit {
		t.Fatal("hit 键存在时 hasCacheHit 必须为 true")
	}
	if got := u.promptTotal(); got != 0 {
		t.Fatalf("只给 hit 时 promptTotal 应为 0（不猜），实际 %d", got)
	}
}

// TestSSEUsageZeroHitKeyStillCounts 两个键都给且都是 0 时不算"未知"。
//
// ⚠ 这是最容易被忽略的边界：`{hit:0, miss:0}` 意味着"输入是 0"吗？
//
//	实际上多数上游不会在这个帧上给真 0（有输入时至少 miss>0），
//	但若真出现，两键齐备 ⇒ promptTotal=0 ⇒ valid 走 TotalTokens 分支。
//	这里只钉"不 panic、不误判键缺失"，不强行给语义。
func TestSSEUsageZeroHitKeyStillCounts(t *testing.T) {
	u := frameUsage(`{"usage":{"completion_tokens":100,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":0,"total_tokens":9999}}`)
	if !u.hasCacheHit || !u.hasCacheMiss {
		t.Fatal("★ 两个键都存在时两个 has* 都应为 true（值为 0 也算存在）")
	}
	if !u.valid() {
		t.Fatal("有 total_tokens 时 valid 应为 true")
	}
	if got := u.total(); got != 9999 {
		t.Fatalf("total 应优先用 total_tokens=9999，实际 %d", got)
	}
}
