// chattool_stream.go 工具循环的**流式**实现。
//
// # 为什么要有这个文件（它是修一个我自己引入的回归）
//
// 第一版的工具循环只有一条路：**内部非流式**跑完整轮，最后用 writeAsSSE
// 把结果编成一整块发出去。当时的理由是"必须先知道模型要不要调工具，
// 才知道给客户端什么"。
//
// 那个理由在"整轮都可能有工具调用"的假设下成立，但**假设本身错了**。
// 实测（真实上游，25 帧的流）：
//
//	第一个含 content 的帧 : 不存在（-1）
//	第一个含 tool_calls 的帧: 第 8 帧
//
// 也就是：**模型要调工具时，正文一个字都不会先发**；反过来，
// 纯文字回复**从头到尾没有任何 tool_calls 帧**。
//
// 所以"边流边判断"完全可行，而且代价只在真正调工具的那一轮：
//
//	普通文字回复  → 逐帧透传，**100% 保持原来的流式体验**
//	调我们的工具  → 已流出的思考文字照常流；工具调用帧被拦下，
//	                执行完后下一轮的正文继续逐帧流出
//	调客户端的工具 → 工具调用帧原样透传，客户端自己驱动
//
// 第一版没有这个区分，于是**每一个** loomy 请求都被缓冲成一整块 ——
// 用户看到的就是"文字不是流式的了"。这是范围远大于"生图"的回归。
//
// # 为什么单独一个文件
//
// 非流式那条路（runToolLoop）是已验证的、行为正确的，不该被这次改动
// 牵连。两条路各占一个文件，语义边界在文件名上就看得见。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/wire"
)

// sseWriter 把上游帧写到客户端，并管理 SSE 响应头与 flush。
//
// # 为什么头是"延迟设置"的
//
// 与 wire.Stream 同一个理由：首帧即流内错误时我们一个字节都不该写，
// 好让调用方还能回一个 4xx/5xx 的 JSON 错误。若提前把 Content-Type
// 钉成 text/event-stream，那个 JSON 错误就会带着 SSE 的类型发出去。
type sseWriter struct {
	w          http.ResponseWriter
	fl         http.Flusher
	headersSet bool
	// committed 记录"是否已向客户端写过任何字节"（决定状态码还能不能改）。
	committed bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	fl, _ := w.(http.Flusher)
	return &sseWriter{w: w, fl: fl}
}

func (s *sseWriter) ensureHeaders() {
	if s.headersSet {
		return
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.headersSet = true
}

// raw 原样写出一段文本（调用方保证它已是合法的 SSE 片段）并 flush。
func (s *sseWriter) raw(text string) error {
	s.ensureHeaders()
	if _, err := io.WriteString(s.w, text); err != nil {
		return err
	}
	s.committed = true
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

// data 写一个 data 帧。
func (s *sseWriter) data(payload string) error {
	return s.raw("data: " + payload + "\n\n")
}

// frame 写一帧上游数据（用**规范化后**的对象，剥掉上游的额外字段）。
func (s *sseWriter) frame(f *wire.Frame) error {
	if f.Normalized != nil {
		if b, err := json.Marshal(f.Normalized); err == nil {
			return s.data(string(b))
		}
	}
	// 非 JSON 帧：原样透传（与 wire.Stream 的降级行为一致）。
	return s.data(f.Raw)
}

// comment 写一个 SSE 注释帧（客户端会忽略）。
//
// # 为什么需要它（不是可有可无的礼貌）
//
// 工具执行期间（生图实测 13-30 秒）网关**一个字节都不发**。
// 中间的代理/客户端常在 30-60 秒空闲后掐掉连接 —— 那样用户不但看不到图，
// 连已经生成好的结果都拿不到。注释帧是零副作用的保活手段
// （SSE 规范里客户端必须忽略它们）。
func (s *sseWriter) comment(text string) {
	_ = s.raw(": " + text + "\n\n")
}

// done 写终止标记。
func (s *sseWriter) done() {
	_ = s.raw("data: [DONE]\n\n")
}

// roundOutcome 一轮上游流的消费结果。
type roundOutcome struct {
	// takeOver 模型调用了**我们的**工具（已被拦下，等待执行）。
	takeOver bool
	// handedOff 模型调用了**客户端的**工具（已原样透传，网关就此收手）。
	handedOff bool
	// bufferedSSE 被拦下的那些帧（完整 SSE 文本）。
	//
	// 用 Aggregate 把它聚合成 assistant 消息（含 tool_calls），
	// 从而复用那份**已被测试覆盖**的 delta 合并逻辑 ——
	// 自己再写一遍合并就是第二个实现，迟早会与它漂移。
	bufferedSSE string
	// err 硬错误（交给调用方分类 + 换号）。
	err error
}

// runToolLoopStream 流式跑工具循环。
//
// 与非流式那条（runToolLoop）的区别：**不预先把整轮缓冲**，
// 而是逐帧转发，只在模型确实要调我们的工具时才拦下相关帧。
func (h *Handler) runToolLoopStream(w http.ResponseWriter, p toolLoopParams, tools []gateway.ChatTool) (bool, error) {
	// 网关注入的工具名集合 —— 决定"这个调用归谁"。
	mine := make(map[string]bool, len(tools))
	for _, t := range tools {
		mine[t.Name] = true
	}

	body, err := injectTools(p.body, tools, existingToolNames(p.body))
	if err != nil {
		return false, fmt.Errorf("注入工具失败: %w", err)
	}

	sw := newSSEWriter(w)
	collected := make([]gateway.Artifact, 0)

	for round := 1; round <= maxToolRounds; round++ {
		out := h.consumeRound(sw, p.providerID, p.uid, body, mine)
		if out.err != nil {
			return false, out.err
		}
		if out.handedOff {
			// 客户端的工具，已原样透传 → 网关收手，让它自己驱动。
			//
			// ⚠ 这里**也**要补产物：本轮若有我们已生成的图（模型先调我们的
			// 工具、再调客户端的工具），那张图花了积分，不能因为
			// "这轮交给客户端了"就丢掉。
			emitArtifactsAsText(sw, collected)
			sw.done()
			return true, nil
		}
		if !out.takeOver {
			// 纯文字回复，已经逐帧流出去了 —— 这正是本次修复的目标。
			//
			// ⚠ 但产物必须补上：模型收尾那句话是逐帧流出去的，而**图片
			// markdown 还没有地方发**。漏了这一步的现象是"图生成了、
			// 模型也说话了，但用户看不到图" —— 而积分已经扣了。
			// （我第一版就是漏了这里，实测才发现图片 markdown 缺失。）
			emitArtifactsAsText(sw, collected)
			sw.done()
			return true, nil
		}

		// 模型调用了我们的工具：执行它们，把结果回喂，进入下一轮。
		resp, aerr := wire.Aggregate(strings.NewReader(out.bufferedSSE))
		if aerr != nil {
			return false, fmt.Errorf("聚合工具调用失败: %w", aerr)
		}
		msg := firstMessage(resp)
		calls := parseToolCalls(msg)
		if len(calls) == 0 {
			// 不该发生（out.takeOver 就代表看到了 tool_calls）。真发生了
			// 说明聚合结果与判定不一致 —— 明确报错，不要静默当成正常回复。
			return false, fmt.Errorf("判定为工具调用但聚合后取不到 tool_calls")
		}

		// 保活：工具执行期间不写任何字节，先发一个注释帧。
		sw.comment(fmt.Sprintf("正在执行工具 %s…", toolNamesOf(calls)))

		results := make([]toolExecResult, 0, len(calls))
		for _, call := range calls {
			results = append(results, h.execOneTool(p, call.ID, call.Name, call.Args))
		}
		collected = append(collected, artifactsOf(results)...)

		var obj map[string]any
		if jerr := json.Unmarshal(body, &obj); jerr != nil {
			return false, fmt.Errorf("工具循环内解析请求体失败: %w", jerr)
		}
		if aerr := appendToolMessages(obj, msg, results); aerr != nil {
			return false, aerr
		}
		body, err = json.Marshal(obj)
		if err != nil {
			return false, fmt.Errorf("工具循环内重编码请求体失败: %w", err)
		}
	}

	// 到顶了仍在调我们的工具：去掉工具定义再问一次，强制它用文字收尾。
	//
	// 比"直接报错"或"只把图丢回去"都好：用户拿到的是**模型的一句总结**
	// + 图片，而不是一张没有解释的图，也不是一个错误页。
	log.Printf("chat tools(stream): 达到轮次上限 %d，去掉工具强制收尾 uid=%s provider=%s",
		maxToolRounds, p.uid, p.providerID)

	noTools, nerr := stripTools(body)
	if nerr == nil {
		// mine 传空：**所有**工具调用都视为"不是我们的" → consumeRound
		// 会把它们原样透传（不会误拦）。这一步的目标只是拿一句文字总结。
		out := h.consumeRound(sw, p.providerID, p.uid, noTools, nil)
		if out.err == nil {
			emitArtifactsAsText(sw, collected)
			sw.done()
			return true, nil
		}
	}

	// 兜底：最后一次也没成。至少把已经生成好的东西交给用户
	//（图片已经在 collected 里，丢了它用户就白花积分了）。
	emitArtifactsAsText(sw, collected)
	sw.done()
	return true, nil
}

// consumeRound 消费一轮上游流，边读边决定"转发 / 拦下"。
//
// 状态机（两态）：
//
//	forward  逐帧透传。遇到第一个 tool_calls 帧时判断归属：
//	           全是我们的 → 切到 buffer（还不能确定后面会不会冒出客户端的）
//	           有客户端的 → 原样透传，标记 handedOff
//	buffer   拦下帧。若之后发现客户端的工具名 → 把已拦下的**回放**，
//	         切回 forward 并标记 handedOff。
//
// # 为什么第一个 tool_calls 帧不能立刻定性
//
// 模型可能一轮里调多个工具，而流式帧是**逐个**到的（先 index 0，
// 再 index 1）。看到 index 0 是我们的工具时，index 1 有可能是客户端的。
// 所以先拦下（可回放），等看清了再决定。
//
// 回放是合法的：客户端本来就要把 tool_calls 的 delta 累积起来，
// 晚几个毫秒到不影响正确性。
func (h *Handler) consumeRound(sw *sseWriter, providerID, uid string, body []byte, mine map[string]bool) roundOutcome {
	var out roundOutcome

	rc, status, herr := h.chatStreamOnce(uid, body)
	if herr != nil {
		out.err = herr
		return out
	}
	defer rc.Close()

	if status >= 400 {
		// 上游拒绝：把错误体读出来交给调用方分类（与普通路径同一条）。
		raw, _ := io.ReadAll(io.LimitReader(rc, maxToolLoopRespBytes))
		out.err = &chatError{
			Kind:   h.classifyErr(providerID, status, raw),
			Status: status,
			Msg:    clipForLog(string(raw)),
		}
		return out
	}

	sc := wire.NewFrameScanner(rc)
	var buf strings.Builder
	inToolCalls := false
	handedOff := false

	for {
		f, ferr := sc.Next()
		if ferr == io.EOF {
			break
		}
		if ferr != nil {
			// 读流中断：如果客户端已经收到内容，只能记日志收尾
			//（状态码与已发内容都收不回来）。
			if sw.committed {
				log.Printf("chat tools(stream): 读上游流中断（客户端已收内容）uid=%s: %v", uid, ferr)
				out.handedOff = true
				return out
			}
			out.err = ferr
			return out
		}
		if f.IsDone {
			break
		}
		if f.NonData {
			// 注释/其它行：只在没拦的时候透传（拦下期间它们属于同一轮的噪声）。
			if !inToolCalls {
				_ = sw.raw(f.Raw)
			}
			continue
		}
		if f.Obj == nil {
			// 非 JSON 的 data 帧：原样透传（与 wire.Stream 的降级一致）。
			if !inToolCalls {
				_ = sw.data(f.Raw)
			}
			continue
		}

		// 流内错误信封：与普通路径**同一条**处理。
		if ib, isErr := wire.InBandErrorOf(f.Obj, f.Raw); isErr {
			if !sw.committed {
				// 还没写过任何字节 → 状态码可改，交给调用方走错误路径。
				out.err = &chatError{
					Kind:   h.classifyErr(providerID, http.StatusOK, []byte(ib.Body)),
					Status: http.StatusOK,
					Msg:    ib.Message,
				}
				return out
			}
			// 已流出内容：错误帧必须到达客户端，然后收尾。
			_ = sw.data(f.Raw)
			out.handedOff = true
			return out
		}

		names := toolCallNamesIn(f.Obj)

		if handedOff {
			// 已经决定交给客户端 → 之后所有帧都透传（含 tool_calls 的续帧）。
			if f.Normalized != nil {
				_ = sw.frame(f)
			} else {
				_ = sw.data(f.Raw)
			}
			continue
		}

		if inToolCalls || len(names) > 0 {
			if !inToolCalls {
				inToolCalls = true
				sw.comment("检测到工具调用") // 保活：接下来可能有一段静默
			}
			buf.WriteString("data: " + f.Raw + "\n\n")
			if anyNameNotMine(names, mine) {
				// 有客户端的工具 → 回放已拦下的帧，切回透传。
				_ = sw.raw(buf.String())
				buf.Reset()
				handedOff = true
			}
			continue
		}

		// 普通帧（role / reasoning_content / content / finish_reason）→ 逐帧透传。
		if f.Normalized != nil {
			_ = sw.frame(f)
		} else {
			_ = sw.data(f.Raw)
		}
	}

	out.bufferedSSE = buf.String()
	out.handedOff = handedOff
	out.takeOver = inToolCalls && !handedOff
	return out
}

// toolCallNamesIn 取一帧 delta 里的工具名（可能多个，也可能一个都没有）。
//
// 流式里名字只在**第一个** delta 出现（后续 delta 只带 arguments 片段），
// 所以返回空切片是常态 —— 调用方必须把"空"当作"本帧没提供名字"，
// 而不是"没有工具".
func toolCallNamesIn(obj map[string]any) []string {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	c0, _ := choices[0].(map[string]any)
	delta, _ := c0["delta"].(map[string]any)
	if delta == nil {
		return nil
	}
	tcs, _ := delta["tool_calls"].([]any)
	if len(tcs) == 0 {
		return nil
	}
	var names []string
	for _, it := range tcs {
		m, _ := it.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if n, _ := fn["name"].(string); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// anyNameNotMine 报告是否出现了不属于我们的工具名。
//
// mine 为 nil/空 → 任何工具名都是"不是我们的"（用于"工具已去掉"的收尾轮）。
func anyNameNotMine(names []string, mine map[string]bool) bool {
	for _, n := range names {
		if !mine[n] {
			return true
		}
	}
	return false
}

// artifactsOf 从工具结果里收集产物（含把 markdown 塞进 artifact）。
//
// 与非流式那条路（runToolLoop）同一套规则，只是提成函数避免两处各写一遍。
func artifactsOf(results []toolExecResult) []gateway.Artifact {
	out := make([]gateway.Artifact, 0, len(results))
	for _, r := range results {
		arts := r.artifacts
		if len(arts) == 0 && r.markdown == "" {
			continue
		}
		if len(arts) == 0 {
			arts = []gateway.Artifact{{"type": "note"}}
		}
		if r.markdown != "" {
			withMD := make([]gateway.Artifact, 0, len(arts))
			for i, a := range arts {
				if i != 0 {
					withMD = append(withMD, a)
					continue
				}
				merged := make(gateway.Artifact, len(a)+1)
				for k, v := range a {
					merged[k] = v
				}
				merged["markdown"] = r.markdown
				withMD = append(withMD, merged)
			}
			arts = withMD
		}
		out = append(out, arts...)
	}
	return out
}

// toolNamesOf 把若干调用名连成一句（日志/保活提示用）。
func toolNamesOf(calls []toolCall) string {
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// emitArtifactsAsText 兜底：把产物的 markdown 当正文发给客户端。
//
// 只在"模型最终没能用文字收尾"时用（轮次到顶、或收尾轮出错）——
// 图片已经生成好了、积分已经花了，绝不能因为拿不到一句总结就丢掉它。
func emitArtifactsAsText(sw *sseWriter, arts []gateway.Artifact) {
	md := markdownForArtifacts(arts)
	if md == "" {
		return
	}
	frame := map[string]any{
		"id":      "chatcmpl-wb2api",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   "tool-loop",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": md},
			"finish_reason": nil,
		}},
	}
	if b, err := json.Marshal(frame); err == nil {
		_ = sw.data(string(b))
	}
	fin := map[string]any{
		"id":      "chatcmpl-wb2api",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   "tool-loop",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	}
	if b, err := json.Marshal(fin); err == nil {
		_ = sw.data(string(b))
	}
}

// stripTools 从请求体里去掉 tools / tool_choice（强制模型用文字收尾）。
func stripTools(body []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	delete(obj, "tools")
	delete(obj, "tool_choice")
	return json.Marshal(obj)
}
