// anthropic.go OpenAI ⇄ Anthropic Messages 协议转换。
//
// # 为什么需要这个文件（一个实测否证过的假设）
//
// 本网关的出口层统一用 OpenAI 协议（见 gateway.Provider.Chat 的约定），
// 而 ZCode 的 **JWT 通道只有 Anthropic 协议**。我一开始推断"Coding Plan
// 应该也有 OpenAI 形态"，实测**否证**：
//
//	/api/v1/zcode-plan/paas/v4/chat/completions   → 404   ← 我想当然的路径
//	/api/v1/zcode-plan/v1/chat/completions        → 404
//	/api/v1/zcode-plan/anthropic/v1/messages      → 401   ← 只有这个
//	/api/v1/off-peak/anthropic/v1/messages        → 401
//	/api/v1/ultra-zai/anthropic/v1/messages       → 401
//
// 所以 JWT 通道必须转换。这个代价是实打实的（参照项目为此写了 27KB），
// 但它换来的是**订阅额度可用**而不是按量付费 —— 值得。
//
// # 与参照实现的关系
//
// 结构参考 `genevatrkassulkusc82-collab/zcode-proxy` 的 convert.go（Go），
// 但**逐字段的判定以本仓的 wire 层与官方 SDK 行为为准**，
// 因为参照实现里有几处为了它自己的场景做的取舍（比如把 thinking 丢给
// 客户端自己拼），而我们要产出**标准 OpenAI SSE**。
//
// # 三条不能违背的规则
//
//  1. **错误一律返回 error，绝不 panic**。SSE 转换跑在独立 goroutine 里
//     （见 Provider.translateAnthropic），panic 会打挂整个进程 ——
//     为了一个上游的畸形帧而让网关进程死掉是不可接受的。
//  2. **解析单帧失败时跳过该帧继续**，不要整体中断。上游偶尔会插入
//     我们没预期的帧（新版本加了事件类型），为此丢掉整条回复太贵。
//     只有**流本身读失败**才返回 error。
//  3. **不伪造数据**。转换不出来就如实报错或原样透传，
//     绝不编一个"看起来对"的空响应 —— 那会让"转换器有 bug"
//     伪装成"模型答了空话"，极难归因。
package zcode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// defaultMaxTokens Anthropic 的 max_tokens 是**必填**，而 OpenAI 请求
// 里可以不带。缺省时用这个值。
//
// ⚠ 为什么是 4096 而不是"用满模型的 128000"：max_tokens 是**上限声明**，
// 不是"要生成这么多"。声明一个很大的值有两个代价：
//   - 上游可能按它预留配额（订阅额度的计量会变难看）
//   - 有些上游版本会直接拒绝超过某个阈值的请求
//
// 4096 足够绝大多数对话，且远低于 GLM-5.3 的 128000 输出上限。
const defaultMaxTokens = 4096

// ---- 请求转换：OpenAI → Anthropic ----

// openAIToAnthropic 把 OpenAI chat/completions 请求体转成 Anthropic messages 请求体。
//
// 主要差异（这些是协议本身的差异，不是实现细节）：
//
//	system 消息    要抽出来放顶层 `system` 字段（Anthropic 的 messages 里不能有 system 角色）
//	role           只有 user / assistant；OpenAI 的 `tool` 要变成 user 里的 tool_result block
//	tool 定义      `function.parameters` → `input_schema`
//	tool 调用      OpenAI 的 arguments 是 **JSON 字符串**，Anthropic 的 input 是**对象**
//	tool 结果      要包成 `tool_result` block（且必须排在 user 消息里）
//	max_tokens     Anthropic **必填**
//	stop           Anthropic 叫 `stop_sequences`，且必须是**数组**
func openAIToAnthropic(openAIBody []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(openAIBody, &src); err != nil {
		return nil, fmt.Errorf("zcode: 请求体不是合法 JSON: %w", err)
	}

	out := map[string]any{}

	// model 原样透传（不篡改 —— 出口层已经按前缀改写过一次了）。
	if v, ok := src["model"]; ok {
		out["model"] = v
	}

	// max_tokens：**必填**，且要认 OpenAI 的两个字段名。
	//
	// 优先 max_completion_tokens（OpenAI 的新字段，语义更准），
	// 回落 max_tokens（老字段，仍然广泛使用）。
	if v, ok := firstNumber(src, "max_completion_tokens", "max_tokens"); ok {
		out["max_tokens"] = v
	} else {
		out["max_tokens"] = defaultMaxTokens
	}

	// 采样参数原样透传（两家同名同义）。
	for _, k := range []string{"temperature", "top_p", "stream"} {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}

	// stop → stop_sequences（字符串要变成数组）。
	if v, ok := src["stop"]; ok {
		switch t := v.(type) {
		case string:
			// 空字符串等于没给（OpenAI 允许 stop: ""，含义是"不设"）。
			if t != "" {
				out["stop_sequences"] = []string{t}
			}
		case []any:
			if len(t) > 0 {
				out["stop_sequences"] = t
			}
		}
	}

	// system 消息抽出来。
	// 多条要**合并**（Anthropic 只有一个 system 字段），用 \n\n 连接
	// —— 与官方 SDK 的做法一致，且保留段落边界。
	systemParts, messages := splitSystem(src["messages"])

	// messages 逐条转换（含 tool / tool_calls 的重写）。
	converted, err := convertMessages(messages)
	if err != nil {
		return nil, err
	}
	// ⚠ 空也要是**数组**而不是 nil：Anthropic 的 messages 是必填字段，
	// nil 在 json.Marshal 里会变成 `null`（或者整个字段消失），
	// 上游收到后报的是"messages 缺失"—— 而我们的输入其实只是空数组。
	if converted == nil {
		converted = []map[string]any{}
	}
	out["messages"] = converted

	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}

	// tools 转换。
	if v, ok := src["tools"]; ok {
		if tools := convertTools(v); len(tools) > 0 {
			out["tools"] = tools
			// tool_choice 只在有 tools 时才有意义。
			if tc, ok := src["tool_choice"]; ok {
				if converted := convertToolChoice(tc); converted != nil {
					out["tool_choice"] = converted
				}
			}
		}
	}

	return json.Marshal(out)
}

// firstNumber 取第一个存在且是数字的字段。
func firstNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n, ok := toFloat(v); ok && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

// toFloat 把 JSON 数字转成 float64（json 解出来本来就是 float64，
// 但别的调用路径可能给 int / json.Number）。
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// splitSystem 把 OpenAI 的 messages 拆成 (system 文本, 其余消息)。
//
// Anthropic 的 messages 里**不能有 system 角色** —— 它有独立的顶层字段。
// 不抽出来会被上游以 400 拒绝（错误信息通常是
// "messages: Unexpected role 'system'"，那还算好查的；有的版本直接报参数错）。
func splitSystem(v any) ([]string, []map[string]any) {
	list, ok := v.([]any)
	if !ok {
		return nil, nil
	}
	var systems []string
	var rest []map[string]any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			// OpenAI 的 developer 角色在新规范里等价于 system。
			if text := contentToText(m["content"]); text != "" {
				systems = append(systems, text)
			}
			continue
		}
		rest = append(rest, m)
	}
	return systems, rest
}

// contentToText 把 content 拍成纯文本（可能是字符串，也可能是 block 数组）。
func contentToText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, item := range t {
			switch b := item.(type) {
			case string:
				parts = append(parts, b)
			case map[string]any:
				// OpenAI 的 text block: {"type":"text","text":"..."}
				if s, ok := b["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// convertMessages 逐条把 OpenAI 消息转成 Anthropic 消息。
//
// # 最麻烦的部分：tool 角色与 tool_calls
//
// Anthropic 要求 user/assistant **交替**，且工具结果必须是 user 消息里的
// `tool_result` block。而 OpenAI 里 tool 结果是**独立的消息**（role=tool）。
// 所以这里要把连续的 tool 消息**合并进同一条 user 消息**的 content 数组 ——
// 否则会产出"两条连续的 user 消息"，上游直接 400。
func convertMessages(list []map[string]any) ([]map[string]any, error) {
	var out []map[string]any
	for _, m := range list {
		role, _ := m["role"].(string)
		switch role {
		case "tool":
			// 工具结果 → 合并进（或新建）一条 user 消息。
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": m["tool_call_id"],
			}
			// is_error 让上游知道这是失败结果（影响模型的后续判断）。
			if text := contentToText(m["content"]); text != "" {
				block["content"] = text
			} else {
				block["content"] = ""
			}
			// 上一条就是 user 且**已经有 tool_result** 才合并；
			// 否则新建一条（避免把普通用户消息和工具结果混在一起）。
			if n := len(out); n > 0 && out[n-1]["role"] == "user" && hasToolResult(out[n-1]) {
				out[n-1]["content"] = append(out[n-1]["content"].([]any), block)
			} else {
				out = append(out, map[string]any{"role": "user", "content": []any{block}})
			}
		case "assistant":
			blocks := []any{}
			if text := contentToText(m["content"]); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			// tool_calls → tool_use blocks。
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					call, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := call["function"].(map[string]any)
					name, _ := fn["name"].(string)
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    call["id"],
						"name":  name,
						"input": parseToolArguments(fn["arguments"]),
					})
				}
			}
			// 空的 assistant 消息（既无文本也无工具）跳过 ——
			// Anthropic 不接受空 content 的消息。
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		default:
			// user（含未知角色，按 user 处理更安全：Anthropic 只认两种角色，
			// 未知角色传过去必然 400，按 user 至少能工作）。
			content := convertUserContent(m["content"])
			if content == nil {
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": content})
		}
	}
	return out, nil
}

// hasToolResult 该消息（必须已是 Anthropic 形态）是否含 tool_result block。
func hasToolResult(m map[string]any) bool {
	blocks, ok := m["content"].([]any)
	if !ok {
		return false
	}
	for _, b := range blocks {
		if bm, ok := b.(map[string]any); ok && bm["type"] == "tool_result" {
			return true
		}
	}
	return false
}

// convertUserContent 转换 user 消息的 content。
//
// 字符串直接变成 block 数组（Anthropic 两种都接受，但统一成数组
// 便于与 tool_result 合并）。已经是数组的逐 block 转换
// （OpenAI 的 image_url → Anthropic 的 image）。
func convertUserContent(v any) any {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": t}}
	case []any:
		var blocks []any
		for _, item := range t {
			b, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch b["type"] {
			case "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": b["text"]})
			case "image_url":
				// OpenAI: {"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}
				// Anthropic: {"type":"image","source":{"type":"base64","media_type":"..","data":".."}}
				if img, ok := b["image_url"].(map[string]any); ok {
					if url, ok := img["url"].(string); ok {
						if src := dataURLToSource(url); src != nil {
							blocks = append(blocks, map[string]any{"type": "image", "source": src})
						}
					}
				}
			default:
				// 未知 block 原样透传 —— 上游可能认识我们不知道的新类型。
				blocks = append(blocks, b)
			}
		}
		if len(blocks) == 0 {
			return nil
		}
		return blocks
	}
	return nil
}

// dataURLToSource 把 data URL 转成 Anthropic 的 image source。
//
// 只处理 base64 data URL。**外部 http(s) URL 无法转换** ——
// Anthropic 的 source 只支持 base64 与 url 两种，但 url 类型
// 上游未必实现；这里保守地只转 base64，http URL 返回 nil
// （调用方会丢掉这个 block，而不是发一个上游必然拒绝的形态）。
func dataURLToSource(url string) map[string]any {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil
	}
	rest := url[len(prefix):]
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return nil
	}
	mediaType := rest[:semi]
	data := rest[semi+len(";base64,"):]
	if mediaType == "" || data == "" {
		return nil
	}
	return map[string]any{
		"type":       "base64",
		"media_type": mediaType,
		"data":       data,
	}
}

// parseToolArguments 把 OpenAI 的 arguments（JSON 字符串）解析成对象。
//
// ⚠ Anthropic 的 `input` 是**对象**，不是字符串。直接透传字符串
// 会被上游以参数校验失败拒绝（而那错误信息通常只指向 input 字段，
// 看不出是"类型不对"）。
//
// 解析失败时返回一个**带原始串的对象**而不是丢掉：
// 丢掉会让模型看到"调用了工具但没参数"，而保留原始串至少能
// 让上游报一个具体的错，比静默变成一个空调用好排查。
func parseToolArguments(v any) map[string]any {
	switch t := v.(type) {
	case string:
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			return map[string]any{}
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(trimmed), &out); err == nil {
			return out
		}
		// 不是对象（可能是数组或标量）→ 包一层。
		var anyVal any
		if err := json.Unmarshal([]byte(trimmed), &anyVal); err == nil {
			return map[string]any{"value": anyVal}
		}
		return map[string]any{"_raw": t}
	case map[string]any:
		return t
	}
	return map[string]any{}
}

// convertTools 把 OpenAI 的 tools 转成 Anthropic 的形态。
func convertTools(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI: {"type":"function","function":{"name","description","parameters"}}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			// 已经是 Anthropic 形态（有 name + input_schema）→ 原样保留。
			if _, has := t["input_schema"]; has {
				out = append(out, t)
			}
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		converted := map[string]any{"name": name}
		if d, ok := fn["description"].(string); ok {
			converted["description"] = d
		}
		// parameters → input_schema。
		// 缺省时给一个合法的空对象 schema —— Anthropic 要求这个字段存在。
		if p, ok := fn["parameters"].(map[string]any); ok {
			converted["input_schema"] = p
		} else {
			converted["input_schema"] = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		out = append(out, converted)
	}
	return out
}

// convertToolChoice 转换 tool_choice。
//
// 映射（Anthropic 的 tool_choice 形态）：
//
//	"auto"      → {"type":"auto"}
//	"none"      → nil（不发；Anthropic 没有 none，最接近的是不发 tools）
//	"required"  → {"type":"any"}
//	{"type":"function","function":{"name":"f"}} → {"type":"tool","name":"f"}
//
// "none" 返回 nil 是刻意的：Anthropic 没有"禁用工具"的等价物。
// 调用方拿到 nil 就不设这个字段（tools 仍然发），这与
// "强制不使用工具"有语义差异 —— 但那是协议本身的限制，
// 我们不该编一个上游不认识的值。
func convertToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required", "any":
			return map[string]any{"type": "any"}
		case "none":
			return nil
		}
	case map[string]any:
		if t["type"] == "function" {
			if fn, ok := t["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					return map[string]any{"type": "tool", "name": name}
				}
			}
		}
		// 已经是 Anthropic 形态（有 type: auto/any/tool）→ 原样。
		if _, ok := t["type"].(string); ok {
			return t
		}
	}
	return nil
}

// ---- 非流式响应转换：Anthropic → OpenAI ----

// anthropicJSONToOpenAI 把非流式 Anthropic 响应转成 OpenAI 响应。
func anthropicJSONToOpenAI(anthropicBody []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(anthropicBody, &src); err != nil {
		return nil, fmt.Errorf("zcode: 上游响应不是合法 JSON: %w", err)
	}

	// 上游若报错（{"type":"error","error":{...}}），原样交出去 ——
	// 出口层要按业务码判，重包装会丢掉错误结构。
	if t, _ := src["type"].(string); t == "error" {
		return anthropicBody, nil
	}

	// 上游直接回了 OpenAI 形态的体（有些兼容层会）→ 原样透传。
	if _, ok := src["choices"]; ok {
		return anthropicBody, nil
	}

	content := contentToOpenAIMessage(src["content"])
	msg := map[string]any{"role": "assistant"}
	if content.text != "" {
		msg["content"] = content.text
	} else {
		// OpenAI 的 content 在纯工具调用时是 null（不是空串）。
		msg["content"] = nil
	}
	if len(content.toolCalls) > 0 {
		msg["tool_calls"] = content.toolCalls
	}
	if content.reasoning != "" {
		// OpenAI 生态里思考链的通用字段名（DeepSeek 系用它）。
		msg["reasoning_content"] = content.reasoning
	}

	out := map[string]any{
		"id":      orDefault(src["id"], "chatcmpl-"+randomUUID()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   orDefault(src["model"], ""),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": mapStopReason(src["stop_reason"], len(content.toolCalls) > 0),
		}},
	}
	if usage := mapUsage(src["usage"]); usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

// openAIMessage 从 Anthropic content 数组提出来的 OpenAI 消息素材。
type openAIMessage struct {
	text      string
	reasoning string
	toolCalls []any
}

// contentToOpenAIMessage 把 Anthropic 的 content blocks 拍成 OpenAI 消息。
func contentToOpenAIMessage(v any) openAIMessage {
	var out openAIMessage
	blocks, ok := v.([]any)
	if !ok {
		// 有些兼容层直接给字符串。
		if s, ok := v.(string); ok {
			out.text = s
		}
		return out
	}
	var texts []string
	for _, item := range blocks {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch b["type"] {
		case "text":
			if s, ok := b["text"].(string); ok {
				texts = append(texts, s)
			}
		case "thinking":
			// Anthropic 的思考块 → OpenAI 生态的 reasoning_content。
			if s, ok := b["thinking"].(string); ok {
				out.reasoning += s
			}
		case "tool_use":
			// input 是对象 → 序列化成 JSON 字符串（OpenAI 的约定）。
			args, err := json.Marshal(b["input"])
			if err != nil || len(args) == 0 {
				args = []byte("{}")
			}
			out.toolCalls = append(out.toolCalls, map[string]any{
				"id":   orDefault(b["id"], "call_"+randomUUID()),
				"type": "function",
				"function": map[string]any{
					"name":      orDefault(b["name"], ""),
					"arguments": string(args),
				},
			})
		}
	}
	out.text = strings.Join(texts, "")
	return out
}

// mapUsage 把 Anthropic 的 usage 映射成 OpenAI 的字段名。
func mapUsage(v any) map[string]any {
	u, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	in, _ := toFloat(u["input_tokens"])
	outTok, _ := toFloat(u["output_tokens"])
	if in == 0 && outTok == 0 {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     int64(in),
		"completion_tokens": int64(outTok),
		"total_tokens":      int64(in + outTok),
	}
}

// mapStopReason 把 Anthropic 的 stop_reason 映射成 OpenAI 的 finish_reason。
//
//	hasToolCalls 优先：Anthropic 有时把 stop_reason 报成 end_turn
//	但 content 里其实有 tool_use —— 那种情况下报 tool_calls 才对，
//	否则客户端不会去执行工具，表现为"模型说要调工具但没人调"。
func mapStopReason(v any, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	s, _ := v.(string)
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return "stop"
	}
	// 未知原因按 stop 处理（比报一个 OpenAI 不认识的值安全）。
	return "stop"
}

// orDefault 取字符串值，空则用默认。
func orDefault(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// ---- 流式转换：Anthropic SSE → OpenAI SSE ----

// anthropicSSEToOpenAI 把 Anthropic 的 SSE 流转成 OpenAI 的 SSE 流。
//
// # 事件对照
//
//	message_start          → 首帧（带 role: assistant + 可能的 usage）
//	content_block_start    → 文本块忽略（等 delta）；tool_use 块发一帧带 id/name
//	content_block_delta    → text_delta → delta.content
//	                         input_json_delta → delta.tool_calls[].function.arguments（累加）
//	                         thinking_delta → delta.reasoning_content
//	content_block_stop     → 忽略（OpenAI 没有块概念）
//	message_delta          → 记录 finish_reason 与 usage（在收尾帧发）
//	message_stop           → 收尾帧 + [DONE]
//	ping                   → 忽略（OpenAI 没有心跳）
//	error                  → 转成 OpenAI 的 error 帧
//
// # 为什么必须容错
//
// 上游会加新事件类型（我们这个转换器写于某个版本）。
// 遇到不认识的 `event:` 名时**跳过并继续**，不要中断整条流 ——
// 否则上游一次小版本升级就会让我们这条通道全挂。
func anthropicSSEToOpenAI(r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 64*1024)
	bw := bufio.NewWriterSize(w, 32*1024)

	model := ""
	id := "chatcmpl-" + randomUUID()
	created := time.Now().Unix()
	sentRole := false
	finishReason := ""
	var usage map[string]any
	// index → tool call 在 OpenAI 数组里的位置。
	// Anthropic 的 index 是 content block 序号（文本块也占位），
	// 而 OpenAI 的 tool_calls 数组只数工具 —— 所以要做映射。
	toolIndexByBlock := map[int]int{}
	nextToolIndex := 0

	writeChunk := func(delta map[string]any, finish any) error {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := bw.WriteString("data: "); err != nil {
			return err
		}
		if _, err := bw.Write(raw); err != nil {
			return err
		}
		if _, err := bw.WriteString("\n\n"); err != nil {
			return err
		}
		return bw.Flush()
	}

	// ensureRole 首次输出时补一帧带 role 的 delta。
	// OpenAI 客户端依赖它初始化 assistant 消息，缺了有些客户端会丢掉全部内容。
	ensureRole := func() error {
		if sentRole {
			return nil
		}
		sentRole = true
		return writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
	}

	var event string
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			// 流本身读失败 —— 这是**唯一的**该返回 error 的情形。
			return fmt.Errorf("zcode: 读上游 SSE 失败: %w", err)
		}
		atEOF := err == io.EOF
		trimmed := strings.TrimRight(line, "\r\n")

		switch {
		case strings.HasPrefix(trimmed, "event:"):
			event = strings.TrimSpace(trimmed[len("event:"):])
		case strings.HasPrefix(trimmed, "data:"):
			// ⚠ 必须同时接受 "data:" 与 "data: "（SSE 规范里空格可选）。
			// 只认带空格的形态会让某些上游的每一帧都匹配不上 ——
			// 本仓在 CodeArts 上实测踩过这个坑（症状是按流式与否劈成两半）。
			payload := strings.TrimSpace(trimmed[len("data:"):])
			if payload != "" && payload != "[DONE]" {
				if werr := handleAnthropicFrame(payload, &frameState{
					event:            event,
					model:            &model,
					sentRole:         ensureRole,
					writeChunk:       writeChunk,
					finishReason:     &finishReason,
					usage:            &usage,
					toolIndexByBlock: toolIndexByBlock,
					nextToolIndex:    &nextToolIndex,
				}); werr != nil {
					return werr
				}
			}
			event = ""
		}

		if atEOF {
			break
		}
	}

	// 收尾：如果整个流没有任何事件（上游直接断了），也要给一个合法收尾，
	// 否则客户端会一直等 —— 但**不带任何内容**，不伪造数据。
	if err := ensureRole(); err != nil {
		return err
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	final := map[string]any{"id": id, "object": "chat.completion.chunk",
		"created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": finishReason,
		}}}
	if usage != nil {
		final["usage"] = usage
	}
	raw, err := json.Marshal(final)
	if err != nil {
		return err
	}
	if _, err := bw.WriteString("data: " + string(raw) + "\n\n"); err != nil {
		return err
	}
	if _, err := bw.WriteString("data: [DONE]\n\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// frameState 一帧处理所需的全部状态（用结构体传避免长参数列表）。
type frameState struct {
	event            string
	model            *string
	sentRole         func() error
	writeChunk       func(delta map[string]any, finish any) error
	finishReason     *string
	usage            *map[string]any
	toolIndexByBlock map[int]int
	nextToolIndex    *int
}

// handleAnthropicFrame 处理一帧 Anthropic SSE data。
func handleAnthropicFrame(payload string, st *frameState) error {
	var ev map[string]any
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		// 单帧解析失败 → 跳过，不中断整条流。
		return nil
	}
	// 事件类型优先取 data 里的 `type`，回落 event: 行。
	typ, _ := ev["type"].(string)
	if typ == "" {
		typ = st.event
	}

	switch typ {
	case "message_start":
		if msg, ok := ev["message"].(map[string]any); ok {
			if m, ok := msg["model"].(string); ok && m != "" {
				*st.model = m
			}
			if u := mapUsage(msg["usage"]); u != nil {
				*st.usage = u
			}
		}
		return st.sentRole()

	case "content_block_start":
		idx := intOf(ev["index"])
		cb, _ := ev["content_block"].(map[string]any)
		cbType, _ := cb["type"].(string)
		if cbType == "tool_use" {
			pos := *st.nextToolIndex
			*st.nextToolIndex = pos + 1
			st.toolIndexByBlock[idx] = pos
			if err := st.sentRole(); err != nil {
				return err
			}
			// 首帧带 id + name，arguments 空串（后续 delta 累加）。
			return st.writeChunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": pos,
					"id":    orDefault(cb["id"], "call_"+randomUUID()),
					"type":  "function",
					"function": map[string]any{
						"name":      orDefault(cb["name"], ""),
						"arguments": "",
					},
				}},
			}, nil)
		}
		// 文本块：不需要单独发帧（等 delta），但要确保 role 已发。
		return st.sentRole()

	case "content_block_delta":
		idx := intOf(ev["index"])
		d, _ := ev["delta"].(map[string]any)
		dt, _ := d["type"].(string)
		if err := st.sentRole(); err != nil {
			return err
		}
		switch dt {
		case "text_delta":
			if s, ok := d["text"].(string); ok && s != "" {
				return st.writeChunk(map[string]any{"content": s}, nil)
			}
		case "thinking_delta":
			if s, ok := d["thinking"].(string); ok && s != "" {
				return st.writeChunk(map[string]any{"reasoning_content": s}, nil)
			}
		case "input_json_delta":
			// 工具参数的**分片**：OpenAI 侧要累加拼接，
			// 所以这里只发增量片段，不解析成对象。
			pos, ok := st.toolIndexByBlock[idx]
			if !ok {
				// 没收到 content_block_start 就来了 delta —— 容错：当成新工具。
				pos = *st.nextToolIndex
				*st.nextToolIndex = pos + 1
				st.toolIndexByBlock[idx] = pos
			}
			frag, _ := d["partial_json"].(string)
			return st.writeChunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    pos,
					"function": map[string]any{"arguments": frag},
				}},
			}, nil)
		case "signature_delta":
			// 思考块的签名（Anthropic 特有）—— OpenAI 侧没有对应字段，丢弃。
			return nil
		}
		return nil

	case "message_delta":
		if d, ok := ev["delta"].(map[string]any); ok {
			var toolCalls bool
			if *st.finishReason == "tool_calls" {
				toolCalls = true
			}
			*st.finishReason = mapStopReason(d["stop_reason"], toolCalls)
		}
		// usage 在 message_delta 里补全（message_start 只有 input_tokens，
		// message_delta 只有 output_tokens）。
		//
		// ⚠ 合并时**不能无条件覆盖**：message_delta 的 usage 里
		// prompt_tokens 是 0（它没报输入 token），直接覆盖会把
		// message_start 报的 input_tokens 抹成 0 ——
		// 症状是"用量统计里输入永远是 0"，而且不报错。
		if raw, ok := ev["usage"].(map[string]any); ok {
			in := int64(0)
			if v, has := raw["input_tokens"]; has {
				f, _ := toFloat(v)
				in = int64(f)
			}
			outTok := int64(0)
			if v, has := raw["output_tokens"]; has {
				f, _ := toFloat(v)
				outTok = int64(f)
			}
			if in == 0 && outTok == 0 {
				return nil
			}
			if prev := *st.usage; prev != nil {
				// 只补非零的那个方向。
				if in > 0 {
					prev["prompt_tokens"] = in
				}
				if outTok > 0 {
					prev["completion_tokens"] = outTok
				}
				pin, _ := toFloat(prev["prompt_tokens"])
				pout, _ := toFloat(prev["completion_tokens"])
				prev["total_tokens"] = int64(pin + pout)
			} else {
				*st.usage = map[string]any{
					"prompt_tokens":     in,
					"completion_tokens": outTok,
					"total_tokens":      in + outTok,
				}
			}
		}
		return nil

	case "message_stop", "content_block_stop":
		// 收尾由外层统一发（保证只有一次 [DONE]）。
		return nil

	case "ping":
		// 心跳：OpenAI 没有，丢弃（但不能因此中断解析）。
		return nil

	case "error":
		// 上游中途报错 → 转成 OpenAI 形态的 error 帧。
		return st.writeChunk(map[string]any{
			"content": "",
			"error":   ev["error"],
		}, "stop")
	}

	// 未知事件类型：跳过（上游加新事件时我们不该整条流挂掉）。
	return nil
}

// intOf 把 JSON 数字转 int（缺省 0）。
func intOf(v any) int {
	f, ok := toFloat(v)
	if !ok {
		return 0
	}
	return int(f)
}
