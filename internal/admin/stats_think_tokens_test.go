// stats_think_tokens_test.go 推理 token 的「跨期不可比」守卫（2026-10-20）。
//
// # 这个文件钉的是什么
//
// 用户报「仪表盘数据不太准」时，实际读数是：
//
//	输出 token 1.2M  |  推理 token 32.1M     ← 推理比输出大 27 倍
//
// 而推理是输出的**子集**，不可能更大。全库取证（35,620 行）：
//
//	Σ out_tokens   = 1,188,551     （out_tokens 10-20 才引入，只覆盖 0.6% 的行）
//	Σ think_tokens = 32,147,395    （think_tokens 早就存在，覆盖 62% 的行）
//
// **单行全部正常（0 行 think > out）** ⇒ 问题不在解析层，只在**聚合范围**：
// 两个数覆盖的时间窗不同，并列显示就成了自相矛盾的读数。
//
// # 解法：两个累加器并存
//
//	think_tokens           全部历史（原语义不动，历史推理量不凭空消失）
//	think_tokens_with_out  只算有输出记录的行（与 out_tokens 同期，可比）
//
// 前端并列显示时用后者 —— 那样 think ≤ out 恒成立。
// ⚠ 不要试图"修好" think_tokens 让它等于同期值：那会让历史推理量消失，是另一种失真。
package admin

import (
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// TestThinkTokensTwoAccumulators 两个累加器各算各的。
func TestThinkTokensTwoAccumulators(t *testing.T) {
	now := time.Now()
	items := []logbuf.Entry{
		// 新行：两者都有
		{At: now, Model: "m", Status: 200, Tokens: 1000, OutTokens: 500, ThinkTokens: 200},
		{At: now, Model: "m", Status: 200, Tokens: 800, OutTokens: 400, ThinkTokens: 100},
		// 历史行：有推理但**没有** out_tokens（out_tokens 引入前的数据）
		{At: now, Model: "m", Status: 200, Tokens: 900, ThinkTokens: 300},
		{At: now, Model: "m", Status: 200, Tokens: 700, ThinkTokens: 400},
	}
	got := aggregateChatLog(items)

	thinkAll, _ := got["think_tokens"].(int64)
	thinkOut, _ := got["think_tokens_with_out"].(int64)
	out, _ := got["out_tokens"].(int64)

	// 全部历史：200+100+300+400 = 1000
	if thinkAll != 1000 {
		t.Fatalf("think_tokens 应为 1000（含历史行），实际 %d", thinkAll)
	}
	// 同期：200+100 = 300
	if thinkOut != 300 {
		t.Fatalf("★ think_tokens_with_out 应为 300（只算有 out 的行），实际 %d", thinkOut)
	}
	// 同期子集关系成立
	if thinkOut > out {
		t.Fatalf("★ 同期推理(%d) 不得大于输出(%d) —— 它是子集", thinkOut, out)
	}
	// 而全历史版确实会大于输出（这正是不能并列显示的原因）
	if thinkAll <= out {
		t.Fatalf("本夹具里 think_tokens(1000) 应大于 out_tokens(900)，否则测不出跨期问题")
	}
}

// TestThinkTokensWithOutAlwaysSubset 极端历史数据下，同期版本仍守住子集关系。
func TestThinkTokensWithOutAlwaysSubset(t *testing.T) {
	now := time.Now()
	var items []logbuf.Entry
	// 1000 条历史行，每条 1000 推理但无输出
	for i := 0; i < 1000; i++ {
		items = append(items, logbuf.Entry{
			At: now, Model: "legacy", Status: 200, Tokens: 2000, ThinkTokens: 1000,
		})
	}
	// 只有 1 条新行
	items = append(items, logbuf.Entry{
		At: now, Model: "m", Status: 200, Tokens: 500, OutTokens: 400, ThinkTokens: 150,
	})

	got := aggregateChatLog(items)
	thinkAll, _ := got["think_tokens"].(int64)
	thinkOut, _ := got["think_tokens_with_out"].(int64)
	out, _ := got["out_tokens"].(int64)

	if thinkAll != 1000150 {
		t.Fatalf("全历史应为 1,000,150，实际 %d", thinkAll)
	}
	if thinkOut != 150 {
		t.Fatalf("★ 同期应只有 150（那 1,000,000 历史推理不该进来），实际 %d", thinkOut)
	}
	if thinkOut > out {
		t.Fatalf("★ 同期子集关系被破坏：%d > %d", thinkOut, out)
	}
	// 全历史版必然远大于输出 —— 这就是仪表盘上那个荒谬读数的来源
	if thinkAll <= out*10 {
		t.Fatalf("全历史版应远大于 out（这些历史行没有对应的输出记录）")
	}
}
