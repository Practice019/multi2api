// sse.go OpenAI Responses 的 SSE → OpenAI chat/completions 的 SSE。
//
// # 为什么要自己写（而不是复用 anthroconv）
//
// anthroconv 转的是 **Anthropic** 的 SSE（`content_block_delta` 那套）。
// Responses 的事件集完全不同：
//
//	response.output_text.delta          文本增量
//	response.reasoning_summary_text.delta  思考增量
//	response.output_item.added/done     工具调用块的开始/结束
//	response.completed                  终态（含 usage）
//
// 两套都是 SSE，但字段语义不同，只能各写一份。
//
// # 上游的实际事件序列（实测抓包）
//
//	response.created
//	response.in_progress
//	response.output_item.added        ×2
//	response.reasoning_summary_part.added
//	response.reasoning_summary_text.delta / .done
//	response.reasoning_summary_part.done
//	response.output_item.done         ×2
//	response.content_part.added
//	response.output_text.delta        ×N
//	response.output_text.done
//	response.content_part.done
//	response.completed
//
// # 容错
//
// ① 没有 `event:` 行也能转（只看 `data:`）② 未知事件**跳过而不是报错**
// ③ 坏帧跳过 ④ 空输入也给一个合法的 `[DONE]`。
// 这四条都是 anthroconv 上踩过的教训，这里是同款。
package monkeycode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// responsesSSEToOpenAI 把 Responses 的 SSE 流转成 OpenAI chat/completions 的 SSE 流。
//
// 写出的每一帧形如 `data: {…}\n\n`，最后以 `data: [DONE]\n\n` 收尾。
func responsesSSEToOpenAI(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	// 单帧可能很长（工具调用参数），默认 64KB 不够。
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var dataLines []string
	started := false
	finished := false

	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if payload == "[DONE]" {
			return nil
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil // 坏帧跳过
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "response.output_text.delta":
			delta, _ := ev["delta"].(string)
			if delta == "" {
				return nil
			}
			if err := emitContent(w, delta, &started); err != nil {
				return err
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			delta, _ := ev["delta"].(string)
			if delta == "" {
				return nil
			}
			if err := emitReasoning(w, delta, &started); err != nil {
				return err
			}
		case "response.completed":
			// 终态：带 usage，写完就结束。
			if err := emitFinal(w, ev); err != nil {
				return err
			}
			finished = true
		case "response.failed", "response.error", "error":
			// 上游报错：透成一个 OpenAI 风格的 error 帧，然后收尾。
			msg := "上游返回错误"
			if e, ok := ev["error"].(map[string]any); ok {
				if s, ok2 := e["message"].(string); ok2 && s != "" {
					msg = s
				}
			}
			_ = emitError(w, msg)
			finished = true
		}
		return nil
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		// event: / id: / 注释行 —— 忽略（只看 data）。
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}

	// 上游没给终态就断了：补一个 finish + DONE，别让客户端挂住。
	if !finished {
		_ = emitRole(w, &started)
		_ = emitFinal(w, nil)
	}
	return emitDone(w)
}

// emitRole 只在第一帧写 role（OpenAI 客户端靠它初始化）。
func emitRole(w io.Writer, started *bool) error {
	if *started {
		return nil
	}
	*started = true
	frame := map[string]any{
		"id":      "chatcmpl-monkeycode",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   providerID,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": ""},
		}},
	}
	return writeFrame(w, frame)
}

// emitContent 输出一段正文增量。
func emitContent(w io.Writer, text string, started *bool) error {
	if err := emitRole(w, started); err != nil {
		return err
	}
	frame := map[string]any{
		"id":      "chatcmpl-monkeycode",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   providerID,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": text},
		}},
	}
	return writeFrame(w, frame)
}

// emitReasoning 输出一段思考增量（放在 reasoning_content，OpenAI 生态通用）。
func emitReasoning(w io.Writer, text string, started *bool) error {
	if err := emitRole(w, started); err != nil {
		return err
	}
	frame := map[string]any{
		"id":      "chatcmpl-monkeycode",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   providerID,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"reasoning_content": text},
		}},
	}
	return writeFrame(w, frame)
}

// emitFinal 输出带 finish_reason 与 usage 的收尾帧。
//
// ev 为 nil 表示"上游没给终态，我们自己补一个"。
func emitFinal(w io.Writer, ev map[string]any) error {
	frame := map[string]any{
		"id":      "chatcmpl-monkeycode",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   providerID,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	}
	if ev != nil {
		if usage := responsesUsageToOpenAI(ev); usage != nil {
			frame["usage"] = usage
		}
		if resp, ok := ev["response"].(map[string]any); ok {
			if u := responsesUsageToOpenAI(resp); u != nil {
				frame["usage"] = u
			}
		}
	}
	return writeFrame(w, frame)
}

// responsesUsageToOpenAI 把 Responses 的 usage 转成 OpenAI 的 usage。
//
// 字段映射：input_tokens → prompt_tokens，output_tokens → completion_tokens。
// 思考 token 已经包含在 output_tokens 里（实测 input=1635 output=126），不单列。
func responsesUsageToOpenAI(m map[string]any) map[string]any {
	usage, ok := m["usage"].(map[string]any)
	if !ok {
		return nil
	}
	in := intOf(usage["input_tokens"])
	out := intOf(usage["output_tokens"])
	total := intOf(usage["total_tokens"])
	if total == 0 {
		total = in + out
	}
	if in == 0 && out == 0 && total == 0 {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      total,
	}
}

// emitError 透出一个错误帧。
func emitError(w io.Writer, msg string) error {
	return writeFrame(w, map[string]any{
		"error": map[string]any{"message": msg, "type": "upstream_error"},
	})
}

// emitDone 输出终止帧。
func emitDone(w io.Writer) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// writeFrame 写一个 SSE data 帧。
func writeFrame(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

// intOf 从 any 里取 int（JSON 数字默认是 float64）。
func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, err := x.Int64()
		if err == nil {
			return int(n)
		}
	}
	return 0
}
