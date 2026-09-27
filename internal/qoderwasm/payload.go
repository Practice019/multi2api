// payload.go 把 OpenAI 形状的请求体编成 Qoder 加密端点认的载荷。
//
// # 为什么不能直接把 OpenAI body 发过去
//
// 加密端点的载荷是**客户端私有形状**（`chat_context` / `chat_task` /
// `model_config` / `business` …），与 OpenAI 的
// `{model, messages, tools, max_tokens}` 完全不同。服务端按这个形状
// 路由与取模型配置，缺字段的后果实测有两种：
//
//	缺 business            → 落到故障节点，恒 `[FAIL]node:... Execution failed`
//	缺顶层 tools           → 模型看不到函数 schema，只能用正文 XML 臆造调用
//	                         （用户报障「任务调用 xml 泄露」）
//
// 两条都不是"少个字段无所谓"，而是**功能级故障**。所以这里逐字段对齐
// 参照实现，不自行简化。
package qoderwasm

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// InferAsk 一次推理的输入（已从 OpenAI body 拆出）。
type InferAsk struct {
	// ModelKey 目录 key（`qfmodel` / `dmodel` …）—— 加密端点只认这个。
	ModelKey string
	// UserText 本轮用户文本（`chat_context.text`）。
	UserText string
	// SystemText 系统提示词；空则 `system` 为**空数组**（不是缺字段）。
	SystemText string
	// IsReasoning 该模型是否支持思考（进 `model_config.is_reasoning`）。
	IsReasoning bool
	// IsVL 是否多模态（进 `model_config.is_vl`）。
	IsVL bool
	// DisplayName 模型展示名（官方 `model_config` 必带）。
	DisplayName string
	// ContextWindow 上下文窗口（进 `max_input_tokens`）。
	ContextWindow int
	// History 历史消息（已规范化的 Qoder 形状）。
	History []InferMessage
	// Tools 工具定义（**顶层** `tools`）。
	Tools []any
	// MaxTokens 最大输出 token（可空）。
	MaxTokens *int
	// ReasoningEffort 思考档位（可空）。
	ReasoningEffort string
	// SessionType 会话类型（默认 `qodercli`）。
	SessionType string
	// Business 决定服务端路由（`sec_scan` → 安全池）。空则不出现该键。
	Business string
}

// InferMessage Qoder 形状的一条消息。
//
// 只保留协议认识的三个键 —— 与参照实现的"逐字段搬运"同一原则：
// 不要把调用方的内部字段原样发给上游。
type InferMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolCalls  any    `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// buildPayload 构造载荷。
func buildPayload(ask InferAsk) ([]byte, error) {
	if ask.ModelKey == "" {
		return nil, fmt.Errorf("qoderwasm: 载荷缺少模型 key（目录 key 是加密端点的唯一凭据）")
	}

	reqID := newUUID()
	sessionType := ask.SessionType
	if sessionType == "" {
		sessionType = "qodercli"
	}
	ctxWindow := ask.ContextWindow
	if ctxWindow <= 0 {
		ctxWindow = 200_000
	}

	// parameters 是**按需出现**的：没配就不出现该键
	//（出现一个 null 或 0 会让服务端按"显式要求 0 token"处理）。
	params := map[string]any{}
	if ask.MaxTokens != nil {
		params["max_tokens"] = *ask.MaxTokens
	}
	if ask.ReasoningEffort != "" {
		params["reasoning_effort"] = ask.ReasoningEffort
		params["enable_thinking"] = ask.ReasoningEffort != "none"
	}
	if ask.ContextWindow > 0 {
		params["context_length"] = ask.ContextWindow
	}

	messages := ask.History
	if len(messages) == 0 {
		messages = []InferMessage{{Role: "user", Content: ask.UserText}}
	}

	// system 是**空数组**而非缺字段（与客户端一致）。
	system := []any{}
	if ask.SystemText != "" {
		system = append(system, map[string]any{"type": "text", "text": ask.SystemText})
	}

	// tools 是**空数组**而非缺字段 —— 客户端源码 `tools: o?.tools ?? []`。
	tools := ask.Tools
	if tools == nil {
		tools = []any{}
	}

	payload := map[string]any{
		"request_id":     reqID,
		"request_set_id": reqID,
		"chat_record_id": reqID,
		"session_id":     newUUID(),
		"stream":         true,
		"chat_task":      "FREE_INPUT",
		"chat_context": map[string]any{
			"text":     ask.UserText,
			"features": []any{},
			"extra": map[string]any{
				"context": []any{},
				"modelConfig": map[string]any{
					"key":          ask.ModelKey,
					"is_reasoning": ask.IsReasoning,
				},
				"originalContent": ask.UserText,
			},
			"chatPrompt": "",
			"imageUrls":  nil,
		},
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"session_type":     sessionType,
		"aliyun_user_type": "",
		// 官方 `model_config` 有 **10 个字段**，逐项对齐（早期只传 6 个）。
		"model_config": map[string]any{
			"key":              ask.ModelKey,
			"display_name":     ask.DisplayName,
			"model":            "",
			"format":           "openai",
			"is_vl":            ask.IsVL,
			"is_reasoning":     ask.IsReasoning,
			"api_key":          "",
			"url":              "",
			"source":           "system",
			"max_input_tokens": ctxWindow,
		},
		"custom_model": nil,
		"system":       system,
		"messages":     messages,
		"tools":        tools,
		"parameters":   params,
	}
	if ask.Business != "" {
		payload["business"] = ask.Business
	}
	return json.Marshal(payload)
}

// newUUID 生成一个 v4 UUID。
//
// 自己拼而不用第三方库：格式固定（8-4-4-4-12 十六进制），
// 引入一个依赖只为这一件事不划算。版本位与变体位按 RFC 4122 置好 ——
// 不置的话服务端可能校验格式而拒绝（这类校验在服务端很常见）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败意味着系统熵源不可用 —— 此时任何"随机"都是假的，
		// 而这里生成的 ID 会进服务端会话，不该用一个可预测值兜底。
		// 但也不能 panic（会带崩进程）。退化成一个固定前缀的零值 ID：
		// 服务端会因重复会话而报错，那是**可见的**失败，比静默用弱随机好。
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122

	h := hex.EncodeToString(b[:])
	var sb strings.Builder
	sb.Grow(36)
	sb.WriteString(h[0:8])
	sb.WriteByte('-')
	sb.WriteString(h[8:12])
	sb.WriteByte('-')
	sb.WriteString(h[12:16])
	sb.WriteByte('-')
	sb.WriteString(h[16:20])
	sb.WriteByte('-')
	sb.WriteString(h[20:32])
	return sb.String()
}
