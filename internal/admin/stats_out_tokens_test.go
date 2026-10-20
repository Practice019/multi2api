// stats_out_tokens_test.go 「消耗 = AI 生成 token」口径的守卫（2026-10-20）。
//
// # 这个文件钉的是什么
//
// 用户明确要求：面板里凡显示"token 数"的地方，一律是**纯输出**（completion_tokens），
// 不是 total（输入+输出）。依据是实测差距 —— 一条请求 total=37938，输出只有 176，
// 其余是缓存读取的上下文。用 total 画图会得出"一小时消耗 2.9M"，而真正生成约 1.3 万。
//
// 所以这里守两件事：
//
//	① `out_tokens` 汇总正确，且**不受** Tokens 影响（两者必须互不污染）
//	② `by_model_out` 按模型正确分桶，且历史行（OutTokens=0）不产生条目
//
// ⚠ 第 ② 条的"不产生条目"是刻意的：0 表示"没记录"，不是"消耗为 0"。
//
//	把它放进 map 会让前端画出高度为 0 的柱子，看起来像"这个模型没干活"。
package admin

import (
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// TestAggregateOutTokensSeparatesFromTotal total 与输出各算各的，互不污染。
func TestAggregateOutTokensSeparatesFromTotal(t *testing.T) {
	now := time.Now()
	items := []logbuf.Entry{
		// 典型真实数据：total 巨大（缓存读取），输出很小
		{At: now, Model: "m-a", Provider: "p", Status: 200, Tokens: 37938, OutTokens: 176},
		{At: now, Model: "m-a", Provider: "p", Status: 200, Tokens: 20000, OutTokens: 824},
		{At: now, Model: "m-b", Provider: "p", Status: 200, Tokens: 5000, OutTokens: 500},
	}
	got := aggregateChatLog(items)

	// total 照旧：37938 + 20000 + 5000 = 62938
	if v, ok := got["tokens"].(int64); !ok || v != 62938 {
		t.Fatalf("tokens 应为 62938，实际 %v", got["tokens"])
	}
	// 输出：176 + 824 + 500 = 1500
	if v, ok := got["out_tokens"].(int64); !ok || v != 1500 {
		t.Fatalf("★ out_tokens 应为 1500（176+824+500），实际 %v —— 这就是用户要的'消耗'", got["out_tokens"])
	}

	byModelOut, ok := got["by_model_out"].(map[string]int64)
	if !ok {
		t.Fatalf("by_model_out 缺失或类型不对：%T", got["by_model_out"])
	}
	if byModelOut["m-a"] != 1000 {
		t.Fatalf("m-a 输出应为 1000（176+824），实际 %d", byModelOut["m-a"])
	}
	if byModelOut["m-b"] != 500 {
		t.Fatalf("m-b 输出应为 500，实际 %d", byModelOut["m-b"])
	}
}

// TestAggregateOutTokensIgnoresLegacyRows 历史行（无 OutTokens）不产生条目。
//
// ⚠ 这是"0 不等于消耗 0"的守卫：历史行 OutTokens=0 表示**没记录**。
// 若把它们放进 by_model_out，前端会画出高度 0 的柱子，看起来像该模型没干活，
// 而真相是那时还没开始记录这个字段。
func TestAggregateOutTokensIgnoresLegacyRows(t *testing.T) {
	now := time.Now()
	items := []logbuf.Entry{
		// 旧行：只有 Tokens，没有 OutTokens
		{At: now, Model: "legacy", Provider: "p", Status: 200, Tokens: 999999},
		{At: now, Model: "legacy", Provider: "p", Status: 200, Tokens: 888888},
	}
	got := aggregateChatLog(items)

	if v, _ := got["out_tokens"].(int64); v != 0 {
		t.Fatalf("全旧行时 out_tokens 应为 0，实际 %d", v)
	}
	byModelOut, _ := got["by_model_out"].(map[string]int64)
	if len(byModelOut) != 0 {
		t.Fatalf("★ 全旧行时 by_model_out 必须是空 map（不能有 0 值条目），实际 %v", byModelOut)
	}
	// 而 total 仍然照常统计 —— 两个字段互不影响
	if v, _ := got["tokens"].(int64); v != 1888887 {
		t.Fatalf("tokens 应仍为 1888887，实际 %d", v)
	}
}

// TestAggregateOutTokensMatchesModelSum by_model_out 之和必须等于顶层 out_tokens。
//
// 为什么单独钉：这两个数由两次独立累加得到（顶层 += / map +=），
// 一旦某次改动只改了一边，前端就会看到"卡片 1500、柱状图合计 1200"这种自相矛盾。
func TestAggregateOutTokensMatchesModelSum(t *testing.T) {
	now := time.Now()
	items := []logbuf.Entry{
		{At: now, Model: "m-a", Status: 200, Tokens: 1000, OutTokens: 100},
		{At: now, Model: "m-b", Status: 200, Tokens: 2000, OutTokens: 200},
		{At: now, Model: "m-c", Status: 200, Tokens: 3000, OutTokens: 300},
		{At: now, Model: "", Status: 200, Tokens: 4000, OutTokens: 400}, // 无模型名：进顶层不进 map
	}
	got := aggregateChatLog(items)
	total, _ := got["out_tokens"].(int64)
	byModelOut, _ := got["by_model_out"].(map[string]int64)
	var sum int64
	for _, v := range byModelOut {
		sum += v
	}
	// 差额必须正好是"无模型名"那一条，与 series 的 by_model_out 同一约定
	if total-sum != 400 {
		t.Fatalf("★ 差额应正好等于无模型名那条的 400（total=%d, map 合计=%d）", total, sum)
	}
	if total != 1000 {
		t.Fatalf("out_tokens 应为 1000（100+200+300+400），实际 %d", total)
	}
}
