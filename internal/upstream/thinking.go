// thinking.go DeepSeek 思维链开关：出站请求体注入 thinking:{type:"enabled"} + 默认档位，
// 以及多轮一致性所需的 reasoning_content 回填。
//
// # 来源（借鉴 workbuddy2api-panel，其注释记录了 Hermes 对官方客户端 codebuddy.js 的逆向）
//
// 官方客户端对 deepseek 系模型标记 thinkingFormat:"deepseek" +
// requiresReasoningContentOnAssistantMessages。发请求时「开思考」必须显式带
// thinking:{type:"enabled"}，否则上游默认按**不思考**应答（思维链不返回，
// reasoning_content 长度 0）。网关的 payload 层此前完全不感知该字段，
// 透传请求没有这个开关 → 上游不给思维链。
//
// 为什么 glm / kimi / qwen 不受影响：它们走别的 thinkingFormat
// （qwen 系是 enable_thinking 或默认开），所以只有 deepseek 系会"静默不思考"。
//
// # 为什么单一 thinking 字段还不够（B 的验收实测结论）
//
// 真实上游 deepseek-v4-flash 对「不带 reasoning_effort」的裸请求仍然按不思考应答
// （reasoning_content 长度 0），**带 reasoning_effort:high 才有思维链**。
// 逆向 codebuddy.js 证实：isThinkingEnabled = !!(reasoning_summary || reasoning_effort
// || reasoning?.effort)，case "deepseek" 的 enabled 分支在实际出站里同时保留 reasoning_effort。
//
// 即：官方「开思考」= thinking.type:enabled **+ 某档 effort**，
// 默认档来自 reasoning.defaultEffort ?? 兜底 "high"。
//
// # 与 sanitize 开关解耦（与 normalizeRoles 同一理由）
//
// 本文件是**协议兼容**（补上游认识的字段），不是**内容脱敏**。
// 因此即使 `Client.SanitizeFingerprints=false` 也照常生效 ——
// 理由见 payload.go 的 normalizeRoles 注释：脱敏关掉的是"改写用户内容"，
// 不是"不补上游必需的协议字段"。这两件事混在一个开关下会让
// "我不想改内容" 意外变成 "我的思维链没了"。
package upstream

import (
	"strings"
)

// defaultDeepSeekEffort 官方客户端默认档兜底。
//
// 官方 `configure thinking` 在无来源时 warn fallback to 'high'，
// REASONING_SUPPLEMENTS.defaultEffort 亦为 "high"。
//
// 补入后**不是终点**：它会立刻走 payload.go 的 normalizeReasoningEffort 降级管线，
// 模型不支持 high 时自动落到 ≤high 的最高支持档（见 prepareBodyOptWithLimits 的调用顺序）。
// 因此这里填的是"意图"，不是"最终出站值"。
const defaultDeepSeekEffort = "high"

// isDeepSeekModel 模型名以 deepseek 为前缀（不区分大小写、忽略首尾空白）。
//
// 前缀匹配而非相等：覆盖 deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 等变体，
// 与官方 thinkingFormat:"deepseek" 的判定口径一致 —— 用相等匹配会漏注变体。
//
// 空模型名返回 false（没有模型名时不该由我们猜一个上游行为出来）。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectThinking 按 DeepSeek 思维链开关规则改写请求体。非 deepseek 模型**零改动**。
//
// # 四种输入形态与对应行为（对齐官方客户端）
//
//	① 无 thinking 字段            → 注入 {type:"enabled"} + 补默认档
//	② thinking.type == "enabled"  → 尊重；缺 effort 时补默认档（官方 configure 行为）
//	③ thinking.type == "disabled" → 尊重；**删** reasoning_effort（snake + camel 双字段）
//	④ thinking.type 非空其它值     → 尊重；缺 effort 时补默认档
//
// # 两条不可越过的红线
//
//   - **显式 `thinking.type` 一律不覆盖**。它是客户端明确的意图表达，
//     覆盖它等于替用户决定"要不要思考"。disabled 分支尤其重要：
//     照抄 case 行为把 effort 一起删掉，否则"关思考"会带着档位发出去。
//   - **显式 `reasoning_effort` 一律不覆盖、不降级**。降级是
//     normalizeReasoningEffort 的职责（它知道模型支持哪些档位），
//     本函数只负责"缺档时补一个默认意图"。
func injectThinking(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}

	// 显式控制分支：type 非空（enabled / disabled 都是明确意图）→ 绝不改 type。
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			// 关思考且不带任何 effort —— 照抄客户端 case 行为。
			// snake 与 camel 双删：客户端两种写法都出现过，
			// 只删一种会留下一份"关思考但带档位"的自相矛盾请求。
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return
		}
		ensureDeepSeekEffort(obj) // 显式 enabled（或其它非空值）缺 effort → 补默认档
		return
	}

	// 无 thinking（或 thinking 是非对象值、对象里 type 缺失/为空）：
	// 注入 enabled —— 这是客户端 case "deepseek" 的行为。
	//
	// 注意这里**不检查是否已有 reasoning_effort**：有 effort 也照开开关
	// （官方语义是"有档位 ⇒ 意图就是思考"，与"开关"一致）。
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureDeepSeekEffort(obj)
}

// ensureDeepSeekEffort 缺 effort 档位时补默认档（snake 优先，camel 兜底都不存在才补）。
//
// 已有任一 effort → 不覆盖：显式档位不做任何改写，降级交给 normalizeReasoningEffort。
// camelCase 也要认，否则客户端用 `reasoningEffort` 时我们会多塞一个 snake 字段，
// 出一条同时带两个 effort 的请求（上游取哪个不确定）。
func ensureDeepSeekEffort(obj map[string]any) {
	if _, hasSnake := obj["reasoning_effort"]; hasSnake {
		return
	}
	if _, hasCamel := obj["reasoningEffort"]; hasCamel {
		return
	}
	obj["reasoning_effort"] = defaultDeepSeekEffort
}

// backfillReasoningContent DeepSeek 多轮一致性回填。
//
// # 上游要求
//
// requiresReasoningContentOnAssistantMessages（官方客户端 matches 规则）：
// 一旦会话里出现 reasoning 痕迹，后续请求的**所有** assistant 消息都必须带
// reasoning_content 字段（string，可为空串）。缺字段的行为未定义 ——
// 官方客户端的做法就是全量补齐，本函数照抄。
//
// # 规则（两遍扫描）
//
// 第一遍（判定 hasTrace）：会话内任一 assistant 消息带非空 `reasoning`（string），
// 或已存在 `reasoning_content` 键 → hasTrace = true。
// 第二遍（仅 hasTrace 时执行）：所有 assistant 消息确保有 reasoning_content：
//
//	reasoning 非空且无 reasoning_content → 复制 reasoning 的值
//	已有 reasoning_content             → 原样保留（绝不覆盖）
//	两者皆无                            → 补空串 ""
//
// # 为什么"无痕迹时零改动"
//
// 没有任何 reasoning 痕迹的会话（普通对话）不该被白白加上字段：
// 那是我们凭空构造的字段，可能触发上游对非推理模型的校验。
// 只有确认这条会话用过推理，才补齐一致性所需的部分。
//
// # 为什么只对 assistant 消息补
//
// 该规则的名字本身就是 "OnAssistantMessages"。给 user/tool 消息加 reasoning_content
// 没有依据，而且会污染 tool 调用轮的形状。
func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}

	// 第一遍：检测是否存在任何 reasoning 痕迹。
	// 只看 assistant 之外的角色没意义 —— 规则只针对 assistant 消息。
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		// 键存在即算痕迹（哪怕值是空串）：说明上一轮已经补过，
		// 这一轮必须继续补，否则"补过一轮之后又断掉"会让上游看到形状变化。
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !hasTrace {
		return
	}

	// 第二遍：所有 assistant 消息补齐。
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if _, ok := msg["reasoning_content"]; ok {
			continue // 已有 → 不覆盖（客户端自己填的更权威）
		}
		if r, ok := msg["reasoning"].(string); ok {
			msg["reasoning_content"] = r
		} else {
			msg["reasoning_content"] = ""
		}
	}
}
