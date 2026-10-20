// prefix_key_test.go 会话粘性的「对话前缀指纹」兜底（2026-10-20，issue #2）。
//
// # 这个文件钉的是什么
//
// issue #2 报的缺陷：`ExtractKey` 只认 body 的 conversation_id / metadata
// 三个字段，而 OpenAI 兼容客户端（DSH / Cline / Continue 等绝大多数）
// **从不发送这些字段** ⇒ sessKey 恒为空 ⇒ 粘性层退化为纯随机轮换。
//
// 实测后果（issue 原文）：
//
//	· /status 的 sticky_sessions 在 6927 个请求下恒为 0
//	· 同会话相邻两轮账号转移矩阵 ≈ 20/20/20/20/20
//	· 上游 prompt cache 按账号隔离 ⇒ 每次换号整段对话重新预填充
//	· 受控对照：不带会话标识 19.8% 命中 vs 带 conversation_id 98.7%
//
// 修法：三条显式键都落空时，用 `model + messages 前两条` 的 FNV 指纹兜底。
// 多轮对话的 messages 是前缀增长的 ⇒ 前两条不变 ⇒ 指纹恒定。
package session

import "testing"

// TestPrefixKeyStableAcrossTurns ★ 核心：多轮对话的指纹必须保持不变。
//
// 这是整个修复成立的唯一条件。如果指纹随轮次漂移，粘性等于没修。
func TestPrefixKeyStableAcrossTurns(t *testing.T) {
	turn1 := `{"model":"m","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"第一问"}]}`
	turn2 := `{"model":"m","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"第一问"},{"role":"assistant","content":"回答"},{"role":"user","content":"第二问"}]}`
	turn3 := `{"model":"m","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"第一问"},{"role":"assistant","content":"回答"},{"role":"user","content":"第二问"},{"role":"assistant","content":"再答"},{"role":"user","content":"第三问"}]}`

	k1 := ExtractKey([]byte(turn1))
	k2 := ExtractKey([]byte(turn2))
	k3 := ExtractKey([]byte(turn3))

	if k1 == "" {
		t.Fatal("★ 无显式会话键时应算出前缀指纹，实际为空 —— 说明兜底没生效，粘性仍是纯轮换")
	}
	if k1 != k2 || k2 != k3 {
		t.Fatalf("★ 同一对话的不同轮次必须同键：turn1=%s turn2=%s turn3=%s", k1, k2, k3)
	}
}

// TestPrefixKeyDiffersByFirstUser 不同会话必须不同键（否则全部粘到同一个号）。
func TestPrefixKeyDiffersByFirstUser(t *testing.T) {
	a := `{"model":"m","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"问题 A"}]}`
	b := `{"model":"m","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"问题 B"}]}`
	if ExtractKey([]byte(a)) == ExtractKey([]byte(b)) {
		t.Fatal("首条 user 不同的两个会话算出了同一个键 —— 它们会被粘到同一账号")
	}
}

// TestPrefixKeyDiffersByModel 不同模型的同名会话不该互相粘。
func TestPrefixKeyDiffersByModel(t *testing.T) {
	a := `{"model":"m-a","messages":[{"role":"user","content":"同一个问题"}]}`
	b := `{"model":"m-b","messages":[{"role":"user","content":"同一个问题"}]}`
	if ExtractKey([]byte(a)) == ExtractKey([]byte(b)) {
		t.Fatal("不同模型应算出不同键（模型进了指纹前缀）")
	}
}

// TestExplicitKeyWins 显式键优先于指纹 —— 不能让指纹覆盖客户端主动给的。
func TestExplicitKeyWins(t *testing.T) {
	body := `{"model":"m","conversation_id":"conv-123","messages":[{"role":"user","content":"x"}]}`
	if got := ExtractKey([]byte(body)); got != "conv-123" {
		t.Fatalf("显式 conversation_id 应原样返回，实际 %q", got)
	}
	body2 := `{"model":"m","metadata":{"conversation_id":"conv-456"},"messages":[{"role":"user","content":"x"}]}`
	if got := ExtractKey([]byte(body2)); got != "conv-456" {
		t.Fatalf("metadata.conversation_id 应原样返回，实际 %q", got)
	}
}

// TestContentBlocksFlattened content 是内容块数组时也要能算指纹。
func TestContentBlocksFlattened(t *testing.T) {
	// 同一段内容，一次用字符串、一次用内容块数组 ⇒ 应算出同一个键
	plain := `{"model":"m","messages":[{"role":"user","content":"你好世界"}]}`
	blocks := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"你好世界"}]}]}`
	if ExtractKey([]byte(plain)) != ExtractKey([]byte(blocks)) {
		t.Fatal("内容块数组应折平成同一段文本，两种形态必须同键")
	}
}

// TestUnknownShapeReturnsEmpty 形态不认识时返回空（宁可轮换，也不要漂移的键）。
func TestUnknownShapeReturnsEmpty(t *testing.T) {
	cases := []string{
		`{}`,                    // 无 messages
		`{"messages":[]}`,       // 空数组
		`{"messages":"不是数组"}`,   // 类型不对
		`{"messages":["不是对象"]}`, // 元素类型不对
		`{"model":"m"}`,         // 只有 model
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c)); got != "" {
			t.Fatalf("形态 %s 应返回空键（宁可轮换），实际 %q", c, got)
		}
	}
}

// TestSystemTimestampTailDoesNotDrift system 尾巴上有时间戳时不应影响指纹。
func TestSystemTimestampTailDoesNotDrift(t *testing.T) {
	// 前 512 字符相同、尾巴不同 ⇒ 同键（截断保护）
	long := ""
	for i := 0; i < 600; i++ {
		long += "a"
	}
	a := `{"model":"m","messages":[{"role":"system","content":"` + long + `2026-10-20 11:00:00"},{"role":"user","content":"问"}]}`
	b := `{"model":"m","messages":[{"role":"system","content":"` + long + `2026-10-20 23:59:59"},{"role":"user","content":"问"}]}`
	if ExtractKey([]byte(a)) != ExtractKey([]byte(b)) {
		t.Fatal("system 前 512 字符相同、只有尾巴时间戳不同 ⇒ 应同键（截断没生效）")
	}
}
