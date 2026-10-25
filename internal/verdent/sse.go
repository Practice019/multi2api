// sse.go 上游 SSE（Verdent 自有事件集）→ OpenAI chat/completions SSE。
//
// # 上游的事件形状（实测抓包）
//
//	data: {"delta":{"text":"你"}}
//	data: {"type":"message_delta","usage":{...}}
//	data: [DONE]
//
// 与 monkeycode 处理的是**完全不同的事件集**（那是 Responses 的
// response.output_text.delta 那套），所以只能各写一份 —— 但产出的 OpenAI 帧形状
// 与 monkeycode 的 sse.go **逐字段一致**（id/object/created/model/choices），
// 因为下游（wire.Stream / 出口层）认的是那个形状。
//
// # 容错（照抄 anthroconv 上踩过的四条）
//
//	① 没有 `event:` 行也能转（只看 `data:`）  ② 未知字段**跳过而不是报错**
//	③ 坏帧跳过                              ④ 上游没给终态也要补 finish + [DONE]
//
// 第 ④ 条是必须的：上游断流（网络抖动/被掐）时若不补，客户端会一直挂着等 ——
// 它收到的是"看起来还没结束"的流。
package verdent

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// chunkID 产出的 OpenAI 帧的 id（与 monkeycode 同样用固定串）。
const chunkID = "chatcmpl-verdent"

// ConvertStreamToOpenAI 把上游 SSE 流包成"OpenAI SSE 流"（**调用方负责 Close**）。
//
// model 只用于回填帧里的 model 字段（展示用，上游不回显它）。
func ConvertStreamToOpenAI(r io.ReadCloser, model string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer r.Close()
		defer pw.Close()
		if err := convert(r, pw, model); err != nil {
			_ = pw.CloseWithError(err)
		}
	}()
	return pr
}

// convert 逐帧读上游 SSE 并写出 OpenAI SSE。
func convert(r io.Reader, w io.Writer, model string) error {
	sc := bufio.NewScanner(r)
	// 上游单帧可能很大（长正文一次给很多），默认 64KB 的扫描上限会截断。
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	started := false
	finished := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // 注释行 / 空行 / event: 行 —— 都不需要，只认 data:
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			finished = true
			break
		}
		// ⚠ 坏帧**跳过而不是 return**：上游偶尔会插入一段非 JSON 的心跳，
		// 为它整条流就断了，用户看到的是"回答到一半没了"。
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		// 正文增量。
		if d, ok := ev["delta"].(map[string]any); ok {
			if text, ok := d["text"].(string); ok && text != "" {
				if err := emitContent(w, text, &started, model); err != nil {
					return err
				}
			}
		}
		// 终态（含 usage）。
		if t, _ := ev["type"].(string); t == "message_delta" || t == "message_stop" || t == "done" {
			finished = true
			_ = emitRole(w, &started, model)
			if err := emitFinal(w, ev, model); err != nil {
				return err
			}
			break
		}
	}
	if err := sc.Err(); err != nil {
		// 读上游失败：能补就补一个终态再报错，别让客户端等一个永远不会来的帧。
		if !finished {
			_ = emitRole(w, &started, model)
			_ = emitFinal(w, nil, model)
			_ = emitDone(w)
		}
		return err
	}
	if !finished {
		// 上游没给终态就断了（见文件头第 ④ 条）。
		_ = emitRole(w, &started, model)
		_ = emitFinal(w, nil, model)
	}
	return emitDone(w)
}

// emitRole 只在第一帧写 role（OpenAI 客户端靠它初始化）。
func emitRole(w io.Writer, started *bool, model string) error {
	if *started {
		return nil
	}
	*started = true
	return writeFrame(w, map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": ""},
		}},
	})
}

// emitContent 输出一段正文增量。
func emitContent(w io.Writer, text string, started *bool, model string) error {
	if err := emitRole(w, started, model); err != nil {
		return err
	}
	return writeFrame(w, map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": text},
		}},
	})
}

// emitFinal 输出带 finish_reason（与 usage，若有）的收尾帧。
func emitFinal(w io.Writer, ev map[string]any, model string) error {
	frame := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	}
	if ev != nil {
		if u, ok := ev["usage"].(map[string]any); ok {
			frame["usage"] = normalizeUsage(u)
		}
	}
	return writeFrame(w, frame)
}

// normalizeUsage 把上游的 usage 归到 OpenAI 的三个字段名。
//
// 上游用的是 prompt_tokens / completion_tokens / total_tokens 还是别的拼法
// **没有实测样本**，所以这里做"能认出多少认多少"，认不出的原样保留 ——
// 丢掉 usage 只影响统计，编一个假的会影响对账，两者代价不对称。
func normalizeUsage(u map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range u {
		out[k] = v
	}
	// 常见别名归位（有就不覆盖）。
	alias := map[string][]string{
		"prompt_tokens":     {"input_tokens", "promptTokens"},
		"completion_tokens": {"output_tokens", "completionTokens"},
	}
	for dst, srcs := range alias {
		if _, ok := out[dst]; ok {
			continue
		}
		for _, s := range srcs {
			if v, ok := out[s]; ok {
				out[dst] = v
				break
			}
		}
	}
	return out
}

// emitDone 写结束标记。
func emitDone(w io.Writer) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// writeFrame 写一帧 OpenAI SSE。
func writeFrame(w io.Writer, frame map[string]any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
}
