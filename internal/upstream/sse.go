// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// sseData 判定一行是否是 SSE 的 data 行，并把 payload 切出来。
//
// # 为什么必须同时接受 "data:" 与 "data: "（这是实测抓到的真 bug）
//
// SSE 规范（W3C EventSource）里冒号后的空格是**可选**的：
//
//	data:{...}      ← CodeArts（华为 InferHub）实测形态
//	data: {...}     ← 标准 OpenAI / workbuddy 形态
//
// 早先的实现只认带空格的形态（`strings.HasPrefix(line, "data: ")`），
// 于是 CodeArts 的每一帧都匹配不上：
//
//	Aggregate → 有效事件数恒为 0 → 502 upstream_parse
//	Stream    → 落进"注释/其它行"分支原样透传 → 不计数也不规范化
//
// 症状因此被**按流式与否劈成两半**：同一个上游、同一个模型，
// `stream:true` 看着是好的（帧被原样透传），`stream:false` 恒 502。
// 这正是"一个解析假设，两种截然不同的失败表现"。
//
// 判据写成一个函数而不是在两处各写一遍：两处各写一遍正是这个 bug 的成因
// （Aggregate 与 Stream 对同一份帧形态给过不同的答案）。
func sseData(line string) (payload string, ok bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "), true
}

// InBandError 是**上游用 2xx + 流内错误信封**返回的业务错误。
//
// # 为什么需要这个类型（这是"外部调用 api 只有 codearts 报错"的根因）
//
// 有的上游（实测：CodeArts / 华为 InferHub）在拒绝请求时**不改 HTTP 状态码**，
// 而是把错误塞进 200 的流里：
//
//	data:{"error":{"code":"InferHub.002002009","message":"The model is not registered"}}
//	data:{"error":"InferHub.4005.200 unsupported model"}
//
// 这与 quota.go 注释里记的额度耗尽形态是同一类（"它不是 HTTP 错误码，
// 而是塞在 SSE 流里的业务错误"）。
//
// 改造前这类帧会被**洗成一个空的成功响应**，两处叠加：
//
//	normalizeFrame       白名单只转发标准字段 → error 字段被剥掉
//	空流兜底（validFrames==0）  错误信封是合法 JSON，会被计为有效帧 → 守门永不成立
//
// 客户端因此收到 `HTTP 200` + `content:""`（流式则是一帧没有 choices 的空壳 +
// [DONE]），只能显示成"报错/空回复"；而错误原文连日志都没有，排查时看不到
// 任何上游线索。
//
// # 为什么 Body 是原始信封文本而不是解析后的字段
//
// 分类判据属于**上游自己**（core 不得认识任何具体上游，见 gateway 包注释
// 与 arch_test.go 判据 3）。本类型只负责"把这段文本原样带出去"，
// 由调用方交给该上游的 gateway.ErrorClassifier 分类 —— 与
// handler.classifyErr 在非 2xx 路径上的做法完全一致。
type InBandError struct {
	// Body 原始错误信封文本（喂给该上游自己的分类器）。
	Body string
	// Message 便于日志与回显的简短描述。
	Message string
	// Committed 表示**响应状态码是否已经发给客户端**。
	//
	// 只有流式路径会把它置 true：错误出现在已流出内容之后时，
	// 200 已经不可回收。调用方据此决定"还能不能换号/回错"：
	//
	//	Committed == false → 状态码未提交，按正常错误路径处理（分类 + 换号）
	//	Committed == true  → 客户端已看到内容与错误帧，只能记录，不能改响应
	//
	// 非流式路径（Aggregate）恒为 false：那时一个字节都还没写给客户端。
	Committed bool
}

func (e *InBandError) Error() string {
	if e.Message != "" {
		return "上游以 200 + 流内错误返回: " + e.Message
	}
	return "上游以 200 + 流内错误返回（无 message 字段）"
}

// inBandErrorOf 判定一个**已解析**的 data 帧是否为错误信封。
//
// 判据（刻意收紧，避免误伤正常帧）：
//
//	choices 缺失或为空  且  命中下列任一错误字段形态
//
// 只多了诊断字段而仍有 choices 的帧**不算**错误信封 —— 那是正常数据帧上
// 多带了字段，不能把它当失败处理。
//
// raw 是原始 payload：作为 Body 带回，保证分类器看到的是**上游原文**
// （而不是我们重建的字段），否则像 InferHub.4291 这类只出现在原文里的
// 判据就会丢。
//
// # ⚠ 字段名必须按**实测**写，不能猜（本函数第一版就栽在这里）
//
// 第一版只认 OpenAI 风格的顶层 `error`，结果线上仍然回 200 + 空响应 ——
// 因为 CodeArts 用的根本不是那个名字。用 `go test -tags probe` 抓到原文：
//
//	模型未注册：
//	{"text":"[DONE]","error_code":"InferHub.002002009.404",
//	 "error_msg":"The model is not registered, please request other model"}
//
//	benefit 通道不可用：
//	{"error_code":"InferHub.4004.200","error_msg":"benefit not found",
//	 "details":[{"error_code":"InferHub.4004.200","error_msg":"requestId: ..."}]}
//
// 即华为系的 `error_code` / `error_msg` 双字段。顶层 `error` 形态一并保留
// 只为通用性（其它上游可能是那种），不是 CodeArts 的实际形态。
func inBandErrorOf(obj map[string]any, raw string) (*InBandError, bool) {
	// choices 非空 → 正常数据帧（哪怕多带了诊断字段）。
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		return nil, false
	}

	code, msg, found := "", "", false

	// 形态 1（**实测** CodeArts / 华为 InferHub）：error_code / error_msg。
	// 两个字段任一非空即认定 —— 实测两帧都同时带，但只带其一的形态
	// 不值得赌（少认一个就是把错误重新变成空回复）。
	if s, ok := obj["error_code"].(string); ok && s != "" {
		code, found = s, true
	}
	if s, ok := obj["error_msg"].(string); ok && s != "" {
		msg, found = s, true
	}

	// 形态 2（OpenAI 风格顶层 error，通用性保留）。
	if !found {
		switch e := obj["error"].(type) {
		case string:
			if e != "" {
				msg, found = e, true
			}
		case map[string]any:
			errMsg, _ := e["message"].(string)
			errCode, _ := e["code"].(string)
			code, msg, found = errCode, errMsg, true
		}
	}

	if !found {
		return nil, false
	}

	// 组合成可读文案：`code: msg`（缺一就只留有的那个）。
	readable := msg
	switch {
	case code != "" && msg != "":
		readable = code + ": " + msg
	case code != "":
		readable = code
	}

	// Body 必须是**上游原始文本**：错误分类器（如 codearts 的
	// DetectQuotaExhausted）认的是 InferHub.4291 / insufficient quota
	// 这类只存在于原文里的判据。
	body := raw
	if body == "" {
		body = readable
	}
	return &InBandError{Body: body, Message: readable}, true
}

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		validEvents   int
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if payload, ok := sseData(line); ok {
			if payload == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
				break
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					// 流内错误信封：必须先于"有效事件"计数判定。
					//
					// 顺序是有意义的：错误信封是合法 JSON，若先 ++validEvents，
					// 它就会被算作"有数据"，于是既不走空流兜底、又没有任何
					// choices 可聚合 —— 最终合成一个 content:"" 的**成功响应**，
					// 错误原文彻底消失。这正是本次要修的静默失败。
					if ib, isErr := inBandErrorOf(chunk, payload); isErr {
						return nil, ib
					}
					// 有效事件计数：仅 JSON 解析成功的数据帧计入（解析失败沿用静默 continue）。
					validEvents++
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									content.WriteString(txt)
									gotAnyContent = true
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									for _, tc := range tcs {
										call, ok := tc.(map[string]any)
										if !ok {
											continue
										}
										idx := 0
										if v, ok := call["index"].(float64); ok {
											idx = int(v)
										}
										merged, seen := toolCalls[idx]
										if !seen {
											merged = map[string]any{"index": idx}
											toolCalls[idx] = merged
											toolOrder = append(toolOrder, idx)
										}
										mergeToolCallDelta(merged, call)
									}
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								if txt, ok := msg["content"].(string); ok {
									content.WriteString(txt)
								}
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, fmt.Errorf("upstream stream contained no valid data events")
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
//
// # 返回值与状态码的约定（⚠ 调用方必须读）
//
// 返回 *InBandError 表示上游用 2xx + 流内错误信封拒绝了这次请求。
// 此时**是否已经提交了状态码**取决于错误出现在第几帧：
//
//	首帧即错误（尚未写过任何字节）→ 状态码仍可改，调用方应当走正常的
//	                                 错误路径（分类 + 换号），而不是回 200
//	内容之后才错误（已提交）        → 无法回收状态码，错误帧已原样下发，
//	                                 调用方只需记录/应用策略
//
// 因此本函数**不再假设调用方已设置过 status 200**：调用方必须等本函数
// 返回后再决定状态码（见 handler.chatCompletions 的流式分支）。
//
// 流式策略：逐帧透传（规范化已剥空 content 噪声），恢复与上游一致的平滑流式。
func Stream(w http.ResponseWriter, r io.Reader) error {
	// headersSet 让 SSE 响应头**延迟到第一次真正写出时**才设置。
	//
	// 为什么不能在这里直接 h.Set(...)：本函数可能在"首帧即流内错误"时
	// 一个字节都不写并把错误交回调用方，让调用方改用 4xx/5xx 回一个 JSON 错误。
	// 若此时 Header() 里已经躺着一个 text/event-stream，那个 JSON 错误
	// 就会带着 SSE 的 Content-Type 发出去 —— 客户端按事件流解析 JSON，
	// 报出的错与真正的原因毫不相干。
	headersSet := false
	ensureHeaders := func() {
		if headersSet {
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		headersSet = true
	}
	fl, _ := w.(http.Flusher)

	// committed 记录"是否已向客户端写过任何字节"。
	//
	// 为什么单靠 validFrames==0 不够：注释/其它行（下面的 else 分支）也会
	// 直接写 w 并提交状态码，但它不计入 validFrames。用显式标志表达
	// "状态码还能不能改"这个真正关心的事实。
	committed := false

	// writeRaw 原样写出一帧（绕过 normalizeFrame）并 flush。
	// 错误帧需保留 error 字段，不能被白名单剥掉，故不经 writeFrame 规范化。
	writeRaw := func(payload string) error {
		ensureHeaders()
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		committed = true
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
	// 仅 JSON 解析成功时计数记为一次有效转发（JSON 解析失败照常降级原样写出，但不计数）。
	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		valid := 0
		if json.Unmarshal([]byte(payload), &obj) == nil {
			// 流内错误信封：上游用 2xx + 错误体返回业务错误。
			if ib, isErr := inBandErrorOf(obj, payload); isErr {
				if !committed {
					// 一个字节都还没写给客户端 → 状态码/Content-Type 尚未提交，
					// 把错误交回调用方，让它走真正的错误路径。**不能**在这里
					// 写出任何东西，否则 200 就被钉死了（那正是本 bug 的形态）。
					return 0, ib
				}
				// 已经流出了内容：状态码收不回来，但错误细节必须到达客户端。
				// normalizeFrame 会剥掉 error 字段，所以这里走 writeRaw。
				if werr := writeRaw(payload); werr != nil {
					return 0, werr
				}
				// 补 [DONE] 让客户端能正常收尾（否则会挂到超时）。
				if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr != nil {
					return 0, werr
				}
				if fl != nil {
					fl.Flush()
				}
				// 标记状态码已提交：调用方不得再去改它。
				ib.Committed = true
				return 0, ib
			}
			if raw, err := json.Marshal(normalizeFrame(obj)); err == nil {
				payload = string(raw)
			}
			valid = 1
		}
		ensureHeaders()
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return 0, werr
		}
		committed = true
		if fl != nil {
			fl.Flush()
		}
		return valid, nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	validFrames := 0
readLoop:
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		// 先按 data 行解析（兼容 "data:" 与 "data: " 两种形态，见 sseData）。
		//
		// ⚠ 顺序很重要：早先这里是 `case HasPrefix(trimmed, "data: [DONE]")`
		// 与 `case HasPrefix(trimmed, "data: ")` 两个字面量分支，
		// CodeArts 的无空格帧两个都不匹配 → 落进下面的"注释/其它行"分支
		// **原样透传且不计数**。后果有两层：
		//   1. validateFrames 恒为 0 → 循环结束后误报一帧
		//      "empty upstream stream"（明明有数据）；
		//   2. 帧绕过 normalizeFrame 白名单重建 → 上游的额外字段直接漏给客户端。
		if payload, isData := sseData(trimmed); isData {
			if strings.TrimSpace(payload) == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据（含垃圾帧）一律不再透传。
				// [DONE] 统一在循环结束后写出，保证恰好一个。
				break readLoop
			}
			n, werr := writeFrame(payload)
			validFrames += n
			if werr != nil {
				return werr
			}
		} else {
			switch {
			case trimmed != "":
				// 注释/其他行：原样透传
				ensureHeaders()
				if _, werr := io.WriteString(w, line); werr != nil {
					return werr
				}
				// 这一行也提交了状态码 —— 必须登记，否则后续的流内错误
				// 会以为"还没写过"而让调用方去改一个已经发出去的状态码。
				committed = true
				if fl != nil {
					fl.Flush()
				}
			}
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	// 空流（0 有效帧）：先写一帧 error（绕过 normalizeFrame 原样保留 error 字段），
	// 再补 [DONE] 保证客户端能正常收尾，并返回非 nil error 供调用方记录。
	if validFrames == 0 {
		_ = writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error"}}`)
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	// ensureHeaders 在此是幂等兜底：正常路径上第一个数据帧已设置过。
	ensureHeaders()
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if validFrames == 0 {
		return fmt.Errorf("upstream stream contained no valid data events")
	}
	return nil
}
