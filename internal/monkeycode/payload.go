// payload.go 请求体转换：OpenAI chat/completions → 上游两种协议。
//
// 出口层给进来的一律是 OpenAI chat/completions 形状（gateway.Provider 契约）。
// 本文件负责把它变成上游要的形状。
//
// # 为什么 anthropic 那条走共享层
//
// internal/anthroconv 已经是"OpenAI ↔ Anthropic"的唯一实现（zcode / minimax 在用）。
// 再写一份就是第三份实现，必然漂移。所以这里只做**本上游特有的那一步**：
// 确保 system 字段存在且等于签名对象。
package monkeycode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// modelOf 从 OpenAI 请求体里取 model。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// ensureSystemSignature 确保 Anthropic 请求体里的 system 就是签名对象。
//
// # 为什么必须改写
//
// 签名算的是 defaultInstructions，而 anthroconv 会把 OpenAI 的 system/developer
// 消息拼成 Anthropic 的 system 字段 —— 那份内容和 defaultInstructions 通常不同。
// 不改写就是"签名与 body 不一致"，上游回 403 `invalid ohmyagent request`。
//
// # 改写策略：把 defaultInstructions 放在最前，保留调用方原有的 system 内容在其后
//
// ⚠ 上游签的**只是整段 system 字符串**，所以拼接后的整段才是签名对象。
// 这里返回改写后的 body，调用方必须用**同一个** defaultInstructions 算签名，
// 而拼接结果与 defaultInstructions 不等 ⇒ 所以拼完再用拼完的串签名。
//
// 见 signatureFor：它从这个函数返回的 body 里把 system 取出来签名。
func ensureSystemSignature(anthro []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(anthro, &m); err != nil {
		return nil, fmt.Errorf("monkeycode: 解析转换后的 anthropic 请求失败: %w", err)
	}

	// 把 defaultInstructions 放到最前。
	existing := ""
	switch v := m["system"].(type) {
	case string:
		existing = v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if mm, ok := part.(map[string]any); ok {
				if t, ok2 := mm["text"].(string); ok2 {
					b.WriteString(t)
				}
			}
		}
		existing = b.String()
	}

	combined := defaultInstructions
	if strings.TrimSpace(existing) != "" {
		combined = defaultInstructions + "\n\n" + existing
	}
	m["system"] = combined

	// Anthropic 要求 max_tokens 必须存在（anthroconv 已保证，这里兜底）。
	if _, ok := m["max_tokens"]; !ok {
		m["max_tokens"] = 8192
	}

	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// systemOf 从 Anthropic 请求体里取 system（签名对象）。
func systemOf(anthro []byte) string {
	var m map[string]any
	if err := json.Unmarshal(anthro, &m); err != nil {
		return ""
	}
	switch v := m["system"].(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if mm, ok := part.(map[string]any); ok {
				if t, ok2 := mm["text"].(string); ok2 {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// toResponsesBody 把 OpenAI chat/completions 转成 OpenAI Responses 形状。
//
// # 两者的区别（只列本上游用到的）
//
//	messages[]  →  input[]      role 保留，system/developer 提到顶层 instructions
//	max_tokens  →  max_output_tokens
//	temperature / top_p / tools 名字相同，直接透传
//
// # instructions 的处理
//
// Responses 协议里 system 走**顶层** instructions，不在 input 里。
// 这里把调用方的 system 拼到 defaultInstructions 后面，整段作为 instructions ——
// 它同时就是签名对象。
func toResponsesBody(openAI []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(openAI, &src); err != nil {
		return nil, fmt.Errorf("monkeycode: 解析 OpenAI 请求失败: %w", err)
	}

	out := map[string]any{
		"model":  src["model"],
		"store":  false,
		"stream": true,
	}

	// system / developer 消息收进 instructions。
	var sysParts []string
	var input []any
	if msgs, ok := src["messages"].([]any); ok {
		for _, raw := range msgs {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			if role == "system" || role == "developer" {
				if t := textOf(m["content"]); t != "" {
					sysParts = append(sysParts, t)
				}
				continue
			}
			input = append(input, map[string]any{
				"type":    "message",
				"role":    role,
				"content": responsesContent(m["content"]),
			})
		}
	}

	instructions := defaultInstructions
	if len(sysParts) > 0 {
		instructions += "\n\n" + strings.Join(sysParts, "\n\n")
	}
	out["instructions"] = instructions
	out["input"] = input

	// 参数透传。
	if v, ok := src["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if v, ok := src["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p", "tools", "tool_choice", "parallel_tool_calls"} {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}

	return json.Marshal(out)
}

// responsesContent 把 OpenAI 的 content 转成 Responses 的形状。
//
// 纯文本直接用字符串（上游接受）；多模态转成 input_text / input_image 数组。
func responsesContent(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		out := make([]any, 0, len(c))
		for _, raw := range c {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text":
				out = append(out, map[string]any{"type": "input_text", "text": m["text"]})
			case "image_url":
				out = append(out, map[string]any{"type": "input_image", "image_url": urlOf(m["image_url"])})
			default:
				out = append(out, m)
			}
		}
		if len(out) == 0 {
			return ""
		}
		return out
	default:
		return ""
	}
}

// urlOf 从 image_url 的两种形态里取 URL。
func urlOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if s, ok := x["url"].(string); ok {
			return s
		}
	}
	return ""
}

// textOf 从 OpenAI 的 content 里取纯文本。
func textOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			if m, ok := raw.(map[string]any); ok {
				if t, ok2 := m["text"].(string); ok2 {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// instructionsOf 从 Responses 请求体里取 instructions（签名对象）。
func instructionsOf(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	if s, ok := m["instructions"].(string); ok {
		return s
	}
	return ""
}

// forceStream 把请求体的 stream 强制改成 true。
//
// # 为什么必须（实测的真 bug）
//
// 客户端发 `stream:false` 时，anthroconv 转换会保留 `stream:false` 发给上游，
// 于是上游 **anthropic 端点回的是 JSON**，而我们的转换链按 **SSE** 解析 ——
// 结果是一个 HTTP 200 + **空 content** 的响应：
//
//	{"choices":[{"finish_reason":"stop","message":{"content":"","role":"assistant"}}]}
//
// 不报错、不中断，只是回答为空 —— 这是最难察觉的一类故障。
//
// 正解由核心架构决定：非流式请求由出口层用 wire.Aggregate 把 SSE 合成一条
// JSON（见 minimax 的同一段注释）。所以上游侧**恒发 stream:true**，
// 给它一份好的 SSE 就够了，不需要为非流式做第二套转换。
func forceStream(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("monkeycode: 转换后的请求体不是 JSON: %w", err)
	}
	m["stream"] = true
	return json.Marshal(m)
}
