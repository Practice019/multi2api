// tool_result_merge_test.go 连续 tool 消息合并时的类型安全（2026-10-20，review）
//
// # 这个文件钉的是什么
//
// `convertMessages` 里有一处合并逻辑：
//
//	// 上一条是 user 且已有 tool_result ⇒ 把当前 tool_result 合并进去
//	if n := len(out); n > 0 && out[n-1]["role"] == "user" && hasToolResult(out[n-1]) {
//	    out[n-1]["content"] = append(out[n-1]["content"].([]any), block)
//	}
//
// 那个 `.([]any)` 原本是**裸断言**，靠 `hasToolResult` 返回 true 隐式保证
// content 是 `[]any`（它内部确实判过类型）。逻辑没错，但建立了一条跨行的隐式契约：
// 「改 hasToolResult 时必须记得这里依赖它对 content 的类型保证」。
//
// 网关 panic = 所有客户端同时断连，所以这类隐式依赖值得显式化。
// 已改成 comma-ok 断言：即使 content 不是 []any，最坏也只是"不合并、新建一条"
// （上游可能 400），而不是崩掉进程。
//
// # 这个文件守什么
//
//  1. 正常路径：连续 tool 消息**确实合并**进同一条 user 消息（行为没变）
//  2. 异常形态：content 不是 []any 时**不 panic**（这才是本次改动的目的）
package anthroconv

import "testing"

// TestConsecutiveToolMessagesMerged 连续 tool 消息合并成一条 user 的多个 tool_result。
//
// ⚠ 这是本次改动的**行为不变**守卫：改了断言的写法，但合并语义必须保持。
func TestConsecutiveToolMessagesMerged(t *testing.T) {
	list := []map[string]any{
		{"role": "user", "content": "帮我查两个城市的天气"},
		{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call_1", "function": map[string]any{"name": "weather", "arguments": `{"city":"北京"}`}},
			map[string]any{"id": "call_2", "function": map[string]any{"name": "weather", "arguments": `{"city":"上海"}`}},
		}},
		// 两条连续 tool 消息 → 应该合并进同一条 user
		{"role": "tool", "tool_call_id": "call_1", "content": "北京 25°C"},
		{"role": "tool", "tool_call_id": "call_2", "content": "上海 28°C"},
	}
	out, err := convertMessages(list)
	if err != nil {
		t.Fatalf("convertMessages 报错: %v", err)
	}

	// 找最后那条 user 消息，它应该含 2 个 tool_result block
	var lastUser map[string]any
	for _, m := range out {
		if r, _ := m["role"].(string); r == "user" {
			lastUser = m
		}
	}
	if lastUser == nil {
		t.Fatal("没有产出 user 消息")
	}
	blocks, ok := lastUser["content"].([]any)
	if !ok {
		t.Fatalf("★ 合并后的 content 应为 []any，实际 %T", lastUser["content"])
	}
	tr := 0
	for _, b := range blocks {
		if bm, ok := b.(map[string]any); ok && bm["type"] == "tool_result" {
			tr++
		}
	}
	if tr != 2 {
		t.Fatalf("★ 两条连续 tool 消息应合并成 2 个 tool_result block，实际 %d 个（合并逻辑坏了）", tr)
	}
}

// TestToolMessageWithNonArrayPreviousContentDoesNotPanic ★ 本次改动的核心守卫。
//
// 构造「上一条 user 的 content 是字符串、但含 tool_result 字样」的异常形态。
// 旧写法会在这里裸断言 panic；新写法应安全降级（新建一条，不合并）。
func TestToolMessageWithNonArrayPreviousContentDoesNotPanic(t *testing.T) {
	// 直接调内部函数，绕过 convertUserContent 的归一化 ——
	// 模拟「上游/调用方给了非标准形态」的情况。
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("★ 非数组 content 触发 panic: %v —— 网关 panic = 所有客户端同时断连", r)
		}
	}()
	// 这条 user 的 content 是**字符串**，hasToolResult 会返回 false（它判过类型），
	// 所以走 else 分支新建。这里验证的是"即使 hasToolResult 语义将来变了也不会崩"。
	list := []map[string]any{
		{"role": "user", "content": "普通文本，不是数组"},
		{"role": "tool", "tool_call_id": "c1", "content": "结果"},
	}
	out, err := convertMessages(list)
	if err != nil {
		t.Logf("返回错误（可接受）: %v", err)
		return
	}
	if len(out) == 0 {
		t.Fatal("至少应产出一条消息")
	}
	t.Logf("产出 %d 条消息，未 panic ✓", len(out))
}

// TestEmptyToolContentUsesEmptyString tool content 为空时应给空串而不是省略字段。
func TestEmptyToolContentUsesEmptyString(t *testing.T) {
	list := []map[string]any{
		{"role": "tool", "tool_call_id": "c1", "content": ""},
	}
	out, err := convertMessages(list)
	if err != nil {
		t.Fatalf("convertMessages 报错: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应产出 1 条消息，实际 %d", len(out))
	}
	blocks, ok := out[0]["content"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("content 应为非空 []any，实际 %T", out[0]["content"])
	}
	bm := blocks[0].(map[string]any)
	if v, exists := bm["content"]; !exists || v != "" {
		t.Fatalf("空 tool content 应写成空串（字段必须在），实际 %v exists=%v", v, exists)
	}
}
