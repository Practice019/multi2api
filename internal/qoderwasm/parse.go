// parse.go 把 OpenAI 形状的请求体拆成 InferAsk。
//
// # 为什么单独一个文件
//
// 这段是**协议转换**，与"怎么调 WASM"是两件事。分开之后：
//
//	payload.go   Qoder 载荷形状（纯数据，可对着参照实现逐字段核）
//	parse.go     OpenAI → InferAsk（纯函数，可喂各种畸形输入测）
//	signer.go    调 WASM（要真 WASM 才能测）
//
// 前两个都能**无 WASM** 测试 —— 这对"载荷字段漏一个就功能级故障"
// 的那类缺陷尤其重要：不用等 WASM 跑起来就能把字段对齐钉住。
package qoderwasm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// openAIBody 我们收到的 OpenAI 请求体（只声明用得到的字段）。
type openAIBody struct {
	Model    string            `json:"model"`
	Messages []openAIMessage   `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
	// MaxTokens 两个名字都收：老客户端发 max_tokens，
	// 新的是 max_completion_tokens。只认一个会让另一类客户端"配了不生效"。
	MaxTokens           *int   `json:"max_tokens"`
	MaxCompletionTokens *int   `json:"max_completion_tokens"`
	ReasoningEffort     string `json:"reasoning_effort"`
}

// openAIMessage OpenAI 形状的一条消息。
type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// parseOpenAIBody 拆解请求体。
//
// model 参数优先于 body 里的 model：路由层已经把前缀（`qoder/`）剥掉，
// 它比 body 里的原文更权威（body 里可能是带前缀的裸名）。
func parseOpenAIBody(model string, body []byte) (InferAsk, error) {
	var ob openAIBody
	if err := json.Unmarshal(body, &ob); err != nil {
		return InferAsk{}, fmt.Errorf("qoderwasm: 请求体不是合法 JSON: %w", err)
	}
	if model == "" {
		model = ob.Model
	}
	if model == "" {
		return InferAsk{}, errors.New("qoderwasm: 请求体没有 model（加密端点靠目录 key 路由）")
	}

	ask := InferAsk{
		ModelKey:        model,
		ReasoningEffort: ob.ReasoningEffort,
	}
	if ob.MaxTokens != nil {
		ask.MaxTokens = ob.MaxTokens
	} else if ob.MaxCompletionTokens != nil {
		ask.MaxTokens = ob.MaxCompletionTokens
	}

	// system 单独取：Qoder 的 `system` 是**顶层**数组，不在 messages 里。
	// 多条 system 合并成一段（协议只给一个 text 槽位）。
	for _, m := range ob.Messages {
		switch m.Role {
		case "system", "developer":
			if t := textOf(m.Content); t != "" {
				if ask.SystemText != "" {
					ask.SystemText += "\n\n"
				}
				ask.SystemText += t
			}
		default:
			ask.History = append(ask.History, InferMessage{
				Role:       m.Role,
				Content:    contentOf(m.Content),
				ToolCalls:  rawOrNil(m.ToolCalls),
				ToolCallID: m.ToolCallID,
			})
		}
	}

	// UserText 取**最后一条** user 消息 —— 它是本轮输入。
	// `chat_context.text` 语义是"这次问的"，不是全部历史。
	for i := len(ob.Messages) - 1; i >= 0; i-- {
		m := ob.Messages[i]
		if m.Role == "user" {
			ask.UserText = textOf(m.Content)
			break
		}
	}
	if ask.UserText == "" && len(ask.History) == 0 {
		// 只有 system、没有任何对话 —— 用 system 兜底，避免发一个空 text。
		ask.UserText = ask.SystemText
	}

	// tools 逐条原样搬运。
	//
	// ⚠ **必须真的发出去**：这是模型唯一能学到函数 schema 的通道。
	// 早期硬编码空数组让模型只能用正文 XML 臆造调用（用户报障
	// 「任务调用 xml 泄露任务终止」）。
	for _, t := range ob.Tools {
		var v any
		if err := json.Unmarshal(t, &v); err == nil {
			ask.Tools = append(ask.Tools, v)
		}
	}
	return ask, nil
}

// textOf 从 content 里抽出纯文本。
//
// content 有两种形态：
//
//	"字符串"                                    → 直接是文本
//	[{"type":"text","text":"…"}, {image_url…}]  → 拼所有 text 块
//
// 多模态块里的图片**这里丢掉**：加密端点的图片走 `chat_context.imageUrls`
// 与 `content` 数组的另一种形状，而那条路径尚未验证。宁可先只发文本
//（模型至少能回答），也不要发一个形状没验证过的数组让它报错。
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	out := ""
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if out != "" {
				out += "\n"
			}
			out += p.Text
		}
	}
	return out
}

// contentOf 保留 content 的原始形态（字符串或数组）。
//
// 与 textOf 的区别：`messages[].content` 要保持调用方给的形状
//（数组里有图时别压成文本），而 `chat_context.text` 只要文本。
func contentOf(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}

// rawOrNil 把 RawMessage 转成 any；空则返回 nil（让 omitempty 生效）。
func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}
