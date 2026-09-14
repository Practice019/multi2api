// thinking_test.go DeepSeek 思维链注入与 reasoning_content 回填。
//
// # 为什么这些断言值得写（判别力说明）
//
// 注入层的失败模式**全部是静默的**，没有一条会报错：
//
//	① 不注入          → 上游按"不思考"应答，reasoning_content 长度 0（用户以为模型变笨了）
//	② 注入被白名单剔除 → 代码写了、日志没有、线上毫无变化（最危险的一种）
//	③ 覆盖了显式意图   → 用户关掉思考却仍在思考，或者档位被我们改掉
//
// 因此每个用例断言的都是**上游最终收到的字节**（PrepareBodyOpt* 的出参），
// 而不是内部函数返回值 —— 只有前者能证明"真的发出去了"。
package upstream

import (
	"encoding/json"
	"testing"
)

// decodeBody 解出改写出参为 map；失败即 fatal（出参必须是合法 JSON）。
func decodeBody(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("出参不是合法 JSON: %v; body=%s", err, out)
	}
	return m
}

// thinkingType 取 obj["thinking"].type（不存在返回空串）。
func thinkingType(m map[string]any) string {
	th, ok := m["thinking"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := th["type"].(string)
	return s
}

// TestThinkingInjectedForDeepSeekWithoutField deepseek + 无 thinking → enabled + 默认档 high。
func TestThinkingInjectedForDeepSeekWithoutField(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"model":"deepseek-v4-flash","messages":[]}`), false)
	m := decodeBody(t, out)

	if got := thinkingType(m); got != "enabled" {
		t.Errorf("thinking.type=%q，期望 enabled（缺它上游按不思考应答）", got)
	}
	// 单一 thinking 字段不足：裸 enabled 仍按不思考应答（B 的验收实测）。
	// 必须同时带档位，否则这个注入等于没做。
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v，期望 high —— "+
			"只注入 thinking 不带档位时上游仍不返回思维链", m["reasoning_effort"])
	}
}

// TestThinkingInjectedForDeepSeekVariants 前缀匹配覆盖各 deepseek 变体与大小写。
func TestThinkingInjectedForDeepSeekVariants(t *testing.T) {
	for _, model := range []string{
		"deepseek-v4-flash", "DeepSeek-V4-Pro", "deepseek-r1", "  deepseek-v4.1-flash  ",
	} {
		t.Run(model, func(t *testing.T) {
			out := PrepareBodyOpt([]byte(`{"model":`+jsonString(model)+`,"messages":[]}`), false)
			if got := thinkingType(decodeBody(t, out)); got != "enabled" {
				t.Errorf("model=%q 未注入 thinking（得到 %q）", model, got)
			}
		})
	}
}

// TestThinkingNotInjectedForNonDeepSeek 非 deepseek 模型必须**零改动**。
//
// glm / kimi / qwen 走别的 thinkingFormat（qwen 系是 enable_thinking 或默认开），
// 给它们塞 deepseek 的开关是不认识字段，可能触发参数校验失败 ——
// 而参数校验失败会被误判成账号故障并冷却账号（见 supportedFields 的注释）。
func TestThinkingNotInjectedForNonDeepSeek(t *testing.T) {
	for _, model := range []string{"glm-5.2", "kimi-k2", "qwen3-coder", "gpt-5.5", ""} {
		t.Run(model, func(t *testing.T) {
			out := PrepareBodyOpt([]byte(`{"model":`+jsonString(model)+`,"messages":[]}`), false)
			m := decodeBody(t, out)
			if _, ok := m["thinking"]; ok {
				t.Errorf("model=%q 不该被注入 thinking（got %v）", model, m["thinking"])
			}
			if _, ok := m["reasoning_effort"]; ok {
				t.Errorf("model=%q 不该被补默认档（got %v）", model, m["reasoning_effort"])
			}
		})
	}
}

// TestThinkingDisabledRespectedAndStripsEffort 显式 disabled → 尊重，且必须删掉两个 effort 字段。
//
// 只删 snake 会留下"关思考但带档位"的自相矛盾请求；
// 更关键的是官方客户端在 disabled 分支里就是连 effort 一起删的，
// 留下档位等于绕过了用户的明确意图。
func TestThinkingDisabledRespectedAndStripsEffort(t *testing.T) {
	cases := []struct{ name, body string }{
		{"snake effort", `{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`},
		{"camel effort", `{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoningEffort":"high","messages":[]}`},
		{"both efforts", `{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","reasoningEffort":"low","messages":[]}`},
		{"no effort", `{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := decodeBody(t, PrepareBodyOpt([]byte(c.body), false))
			if got := thinkingType(m); got != "disabled" {
				t.Errorf("thinking.type=%q，期望 disabled（显式意图不得被覆盖）", got)
			}
			if v, ok := m["reasoning_effort"]; ok {
				t.Errorf("disabled 时 reasoning_effort 必须删除，仍存在 %v", v)
			}
			if v, ok := m["reasoningEffort"]; ok {
				t.Errorf("disabled 时 reasoningEffort 必须删除，仍存在 %v", v)
			}
		})
	}
}

// TestThinkingEnabledKeepsExplicitEffort 显式 enabled + 显式档位 → 档位原样保留（不被默认档覆盖）。
func TestThinkingEnabledKeepsExplicitEffort(t *testing.T) {
	m := decodeBody(t, PrepareBodyOpt(
		[]byte(`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"low","messages":[]}`), false))
	if got, _ := m["reasoning_effort"].(string); got != "low" {
		t.Errorf("显式档位被改写成 %v，期望保持 low（显式档位不做任何改写）", m["reasoning_effort"])
	}
	if got := thinkingType(m); got != "enabled" {
		t.Errorf("thinking.type=%q，期望 enabled", got)
	}
}

// TestInjectedDefaultEffortGoesThroughDowngrade 注入的默认档必须走既有降级管线。
//
// 这是 prepareBodyOptWithLimits 里"injectThinking 必须在 normalizeReasoningEffort 之前"
// 那条顺序约束的直接验证：反序的话注入的 high 会绕过降级，
// 向只支持到 medium 的模型发出 high —— 一次参数校验失败，
// 而它会被误判成账号故障去冷却账号。
func TestInjectedDefaultEffortGoesThroughDowngrade(t *testing.T) {
	efforts := map[string][]string{"deepseek-v4-flash": {"off", "low", "medium"}}
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`), false, efforts)
	m := decodeBody(t, out)

	if got := thinkingType(m); got != "enabled" {
		t.Fatalf("thinking.type=%q，期望 enabled", got)
	}
	if got, _ := m["reasoning_effort"].(string); got != "medium" {
		t.Errorf("注入的 high 应被降级到 medium（≤high 的最高支持档），得到 %v", m["reasoning_effort"])
	}
}

// TestThinkingSurvivesFieldWhitelist 注入的 thinking 必须活过 supportedFields 白名单。
//
// # 这是本文件最重要的一条
//
// supportedFields 是"只发上游认识的字段"的白名单，applyFieldWhitelist 会
// 剔除表外的一切顶层字段。thinking 是**唯一由网关自己注入**的字段 ——
// 只要漏把它加进白名单，injectThinking 写进去的键会立刻被删除。
//
// 那种失败**完全静默**：编译过、单测过（若只测 injectThinking 本身）、
// 线上请求里根本没有 thinking，用户只看到"思维链还是没有"。
// 本用例走完整条 prepareBodyOptWithLimits 管线，因此能抓住它。
func TestThinkingSurvivesFieldWhitelist(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`), false)
	m := decodeBody(t, out)

	if _, ok := m["thinking"]; !ok {
		t.Fatalf("thinking 被字段白名单剔除了 —— "+
			"supportedFields 里缺少 \"thinking\" 项；出参=%s", out)
	}
	if got := thinkingType(m); got != "enabled" {
		t.Errorf("thinking.type=%q，期望 enabled（出参=%s）", got, out)
	}
	// 反向对照：确认白名单**确实在工作**（否则本用例可能因为白名单整体失效而假通过）。
	if _, ok := m["some_unknown_field_xyz"]; ok {
		t.Error("白名单似乎没生效，本用例失去判别力")
	}
}

// TestThinkingInjectionIndependentOfSanitize 注入与脱敏开关解耦。
//
// 理由是协议兼容 vs 内容脱敏的分离（同 normalizeRoles）：
// "我不想改写用户内容"不该意外变成"我的思维链没了"。
func TestThinkingInjectionIndependentOfSanitize(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	on := decodeBody(t, PrepareBodyOpt(body, true))
	off := decodeBody(t, PrepareBodyOpt(body, false))

	if thinkingType(on) != "enabled" || thinkingType(off) != "enabled" {
		t.Errorf("sanitize=true/false 都必须注入：on=%q off=%q", thinkingType(on), thinkingType(off))
	}
}

// ── backfillReasoningContent ──────────────────────────────────────────────

// TestBackfillReasoningContentWhenTracePresent 有 reasoning 痕迹 → 所有 assistant 补齐。
func TestBackfillReasoningContentWhenTracePresent(t *testing.T) {
	src := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","reasoning":"先想 A","content":"A"},
		{"role":"user","content":"继续"},
		{"role":"assistant","content":"B"}
	]}`
	m := decodeBody(t, PrepareBodyOpt([]byte(src), false))
	msgs := m["messages"].([]any)

	got0, ok := msgs[0].(map[string]any)["reasoning_content"].(string)
	if !ok || got0 != "先想 A" {
		t.Errorf("msgs[0].reasoning_content=%v，期望从 reasoning 复制来的「先想 A」", msgs[0])
	}
	// 第二条 assistant 没有 reasoning 痕迹 → 补空串（**必须存在该键**，
	// 因为上游要求所有 assistant 消息形状一致）。
	got2, ok := msgs[2].(map[string]any)["reasoning_content"].(string)
	if !ok || got2 != "" {
		t.Errorf("msgs[2] 应补空串 reasoning_content，得到 %v（存在=%v）", got2, ok)
	}
	// user 消息不得被加该字段。
	if _, ok := msgs[1].(map[string]any)["reasoning_content"]; ok {
		t.Error("user 消息不该被补 reasoning_content")
	}
}

// TestBackfillReasoningContentNoTraceUntouched 无痕迹 → 零改动。
//
// 普通对话不该被凭空加上推理字段：那是我们构造的字段，
// 可能触发上游对非推理会话的校验。
func TestBackfillReasoningContentNoTraceUntouched(t *testing.T) {
	src := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"}
	]}`
	m := decodeBody(t, PrepareBodyOpt([]byte(src), false))
	for i, mm := range m["messages"].([]any) {
		if _, ok := mm.(map[string]any)["reasoning_content"]; ok {
			t.Errorf("msgs[%d] 无 reasoning 痕迹时不该被补 reasoning_content", i)
		}
	}
}

// TestBackfillReasoningContentDoesNotOverwrite 已有的 reasoning_content 绝不覆盖。
func TestBackfillReasoningContentDoesNotOverwrite(t *testing.T) {
	src := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","reasoning":"旧","reasoning_content":"客户端给的","content":"A"}
	]}`
	m := decodeBody(t, PrepareBodyOpt([]byte(src), false))
	got, _ := m["messages"].([]any)[0].(map[string]any)["reasoning_content"].(string)
	if got != "客户端给的" {
		t.Errorf("reasoning_content=%q，客户端显式给的值不得被 reasoning 覆盖", got)
	}
}

// TestBackfillReasoningContentNonDeepSeekUntouched 非 deepseek 模型零改动。
func TestBackfillReasoningContentNonDeepSeekUntouched(t *testing.T) {
	src := `{"model":"glm-5.2","messages":[{"role":"assistant","reasoning":"x","content":"A"}]}`
	m := decodeBody(t, PrepareBodyOpt([]byte(src), false))
	if _, ok := m["messages"].([]any)[0].(map[string]any)["reasoning_content"]; ok {
		t.Error("非 deepseek 模型不该回填 reasoning_content")
	}
}

// jsonString 把一个字符串编码成 JSON 字面量（供拼测试用 body）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
