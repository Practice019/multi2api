// sse.go SOLO 自定义 SSE → OpenAI SSE chunk 的**流式转换**。
//
// # 为什么必须转换（这是 trae 上游能否接入的关键）
//
// SOLO 的 llm_utils_chat 返回的是**自定义 SSE**（event: metadata /
// timing_cost / output / extra_info / token_usage / done / error，
// data 形如 {"response":"内容增量","reasoning_content":"思考增量"}）。
// 而本网关的出口层（internal/server 的 upstream.Stream / upstream.Aggregate）
// **只认 OpenAI SSE**：`data: {"choices":[{"delta":{"content":...}}]}` + `[DONE]`。
//
// 所以 Provider.Chat 不能把上游 body 原样上交 —— 必须在这里转成 OpenAI 形状。
// 转换是**流式的**（一边读一边写），非流式请求由出口层的 Aggregate 聚合结果。
//
// # SOLO 事件序列（实测，见 traework2api SPEC §4.6）
//
//	event:output       ×N   data:{"response":"<增量>","reasoning_content":"<增量>","tool_calls":<null|对象|数组>}
//	event:token_usage       data:{"prompt_tokens":..,"completion_tokens":..,"total_tokens":..}
//	event:done              data:{"finish_reason":"stop"}
//	event:error             data:{"code":1005,"message":"..."}
package trae

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// soloEvent 单条 SOLO SSE 事件（归一化）。
type soloEvent struct {
	Event        string
	Response     string
	Reasoning    string
	ToolCalls    json.RawMessage
	Usage        map[string]any
	FinishReason string
	ErrorCode    int64
	ErrorMessage string
}

// parseSOLOLine 解析一条事件（eventName 为 event 行值，dataLine 为 data 行值）。
func parseSOLOLine(eventName, dataLine string) (*soloEvent, error) {
	ev := &soloEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
	}
	return ev, nil
}

// sseState 维护一行 SSE 的 event/data 跨行累积。
type sseState struct {
	event string
	data  strings.Builder
}

func (s *sseState) reset() { s.event = ""; s.data.Reset() }

// scanLine 处理一行；事件边界时解析并返回该事件。
func scanLine(st *sseState, line string) *soloEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.reset()
			return nil
		}
		ev, err := parseSOLOLine(st.event, st.data.String())
		st.reset()
		if err != nil {
			return nil
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, ":"):
		// 注释行忽略
	}
	return nil
}

// writeOpenAIChunk 写一个 OpenAI SSE 数据帧。
func writeOpenAIChunk(w io.Writer, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "data: "+string(raw)+"\n\n")
	return err
}

// ConvertSOLOToOpenAI 把 SOLO SSE 流逐事件转成 OpenAI SSE chunk 写进 w。
//
// 输出保证：至少一个 [DONE]；遇到 event:error 时写错误信封 + [DONE]
// （出口层据此识别流内错误并走分类器，见 handler.go 的 InBandError 路径）。
func ConvertSOLOToOpenAI(r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 64*1024)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	base := func() map[string]any {
		return map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   "",
		}
	}
	writeDelta := func(delta map[string]any, finish string) error {
		chunk := base()
		chunk["choices"] = []any{
			map[string]any{"index": 0, "delta": delta},
		}
		if finish != "" {
			chunk["choices"].([]any)[0].(map[string]any)["finish_reason"] = finish
		}
		return writeOpenAIChunk(w, chunk)
	}

	st := &sseState{}
	var pendingUsage map[string]any
	upstreamErr := error(nil)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				delta := map[string]any{}
				if ev.Response != "" {
					delta["content"] = ev.Response
				}
				if ev.Reasoning != "" {
					delta["reasoning_content"] = ev.Reasoning
				}
				if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
					delta["tool_calls"] = ev.ToolCalls
				}
				if len(delta) > 0 {
					if werr := writeDelta(delta, ""); werr != nil {
						return werr
					}
				}
			case "token_usage":
				pendingUsage = ev.Usage
			case "done":
				finish := ev.FinishReason
				if finish == "" {
					finish = "stop"
				}
				chunk := base()
				chunk["choices"] = []any{
					map[string]any{"index": 0, "delta": map[string]any{}},
				}
				if pendingUsage != nil {
					chunk["usage"] = pendingUsage
				}
				chunk["choices"].([]any)[0].(map[string]any)["finish_reason"] = finish
				if werr := writeOpenAIChunk(w, chunk); werr != nil {
					return werr
				}
			case "error":
				upstreamErr = &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if upstreamErr != nil {
		// 流内错误：写错误信封（choices 空 + error 字段 → 出口层识别为 InBandError）
		if werr := writeOpenAIChunk(w, map[string]any{
			"error": map[string]any{
				"code":    fmt.Sprintf("%d", upstreamErr.(*SOLOStreamError).Code),
				"message": upstreamErr.(*SOLOStreamError).Msg,
			},
		}); werr != nil {
			return werr
		}
	}
	// ⚠ 无条件以 [DONE] 收尾 —— OpenAI SSE 契约：finish_reason 之后、以及
	// 任何异常路径（空流/错误信封）都必须有一个 [DONE]（出口层据此结束读取）。
	_, werr := io.WriteString(w, "data: [DONE]\n\n")
	return werr
}

// SOLOStreamError 上游 SSE 流内的业务错误（event:error）。
type SOLOStreamError struct {
	Code int64
	Msg  string
}

func (e *SOLOStreamError) Error() string {
	return fmt.Sprintf("trae solo error code=%d msg=%s", e.Code, e.Msg)
}
