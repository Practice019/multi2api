// channels_stream.go 把渠道转发的上游流**原样**写回客户端。
//
// # 只做三件事，不碰内容
//
//  1. 复制上游的 Content-Type（SSE 要靠它，写错了客户端不会当流处理）
//  2. 复制状态码
//  3. io.Copy + Flush，让流**边到边发**
//
// ⚠ 为什么不做协议转换：网关的出口层将来要支持 /v1/messages 等（同一份流编成不同格式），
// 那是另一件事。这里只是"Key 型渠道的上游本来就是 OpenAI 兼容" ⇒ 字节直通。
// 一旦在这里顺手改点什么，"上游说了什么"就不再可信，排查会变成猜谜。
//
// ⚠ Flush 是流式的命门：net/http 默认会缓冲，不 Flush 就变成"上游全部发完、客户端才收到" ——
// 表现是"回答卡很久然后一次性蹦出来"，而日志里一切正常。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// writeChannelStream 把一次渠道转发的流写回客户端。
//
// cap 是**只读旁路**采集器（可为 nil）：每写出一块就顺手喂给它解析 usage。
// ⚠ 顺序是 Write → Flush → Feed，不能调换：
//   - Feed 里有 JSON 解析，放在 Write 之前会推迟客户端收到第一个字节；
//   - 放在 Flush 之后则对客户端延迟**零影响**，而采集结果一样（这一块已经发出去了）。
//
// 这就是"旁路"的全部含义：采集器的任何开销都不进入客户端的等待路径。
// writeChannelStream 把渠道响应写回客户端。
//
// scrub=true 时按 fw.PublicModel 改写 model、并把 id 换成 chatcmpl-gw-*；
// scrub=false（管理密钥）时字节原样透传 —— 用户要求「对话密钥按原来的上游即可」。
func (h *Handler) writeChannelStream(w http.ResponseWriter, r *http.Request, fw channelForward, cap *streamUsage, scrub bool) {
	defer fw.Stream.Body.Close()

	if ct := upstreamContentType(fw); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	// 渠道 ID 回给调用方：排查「这条请求走了哪条渠道」时不必翻日志。
	//
	// 不再回 X-Upstream-Model：那是上游的原生模型名，属于身份指纹
	//（monkeycode-basic/deepseek-flash 直接点名了上游）。
	// 客户需要知道走了哪条渠道，不需要知道那一头是谁。
	w.Header().Set("X-Channel-Id", fw.ChannelID)

	status := fw.Stream.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	publicModel := strings.TrimSpace(fw.PublicModel)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	// SSE 是按行的：脱敏必须以行为单位，不能跨块。
	//
	// 一帧 SSE 可能被拆成两次 Read（`data: {"mod` + `el":"x"}` + 空行）。
	// 若按块脱敏，两块都解析不成 JSON，于是静默不脱敏 ——
	// 表现是「有时泄露有时不漏」，取决于网络切片。
	var pending []byte
	for {
		n, err := fw.Stream.Body.Read(buf)
		if n > 0 {
			if cap != nil {
				cap.Feed(buf[:n])
			}
			pending = append(pending, buf[:n]...)
			out, rest := h.scrubSSELines(pending, publicModel, scrub)
			pending = rest
			if len(out) > 0 {
				if _, werr := w.Write(out); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if err != nil {
			// 收尾：remaining 里可能是**非流式的整块 JSON**（没有换行分隔），
			// 也可能是 SSE 最后一帧没有结尾换行。
			//
			// ⚠ 这里必须区分：非流式响应整块就是一个 JSON 对象，
			// 上一版只处理 `data:` 行，于是**非流式整体不脱敏** ——
			// 实测 chat 响应里 model=monkeycode-basic/deepseek-flash 原样回给了客户端。
			if len(pending) > 0 {
				trimmed := bytes.TrimSpace(pending)
				if len(trimmed) > 0 && trimmed[0] == 0x7b && scrub {
					// 整块 JSON：脱敏后写。
					_, _ = w.Write(scrubChatResponse(trimmed, publicModel))
				} else {
					// SSE 尾巴：按行脱敏后写。
					if out2, _ := h.scrubSSELines(append(pending, '\n'), publicModel, scrub); len(out2) > 0 {
						_, _ = w.Write(out2)
					} else {
						_, _ = w.Write(pending)
					}
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
	}
}

// scrubSSELines 对完整的 SSE 行做脱敏，返回（要写出去的字节, 未完成的尾巴）。
//
// 为什么按行而不是按块：SSE 的帧边界是空行。按块处理时一帧可能跨两次 Read，
// 两块都解析不成 JSON 就静默不脱敏 —— 那种失效随网络切片变化，最难查。
//
// 非 SSE（非流式的整块 JSON）也走这里：它没有换行分隔，整块会留在 rest 里，
// 由收尾那段原样发出去。那条路径由调用方在写之前用 scrubChatResponse 处理。
func (h *Handler) scrubSSELines(pending []byte, publicModel string, scrub bool) (out, rest []byte) {
	if len(pending) == 0 {
		return nil, pending
	}
	// ⚠ 先判**是不是 SSE**：非流式响应是一整块 JSON，里面可能**含换行**
	//（上游的 json.MarshalIndent 或人工拼的响应都会）。若直接按行切，
	// 整块 JSON 会被当成"N 行没有 data: 前缀的普通文本"原样输出 ——
	// 于是非流式渠道**一个字段都不脱敏**（实测 senseaudio 的 model 与 id
	// 原样回给了客户端，而同一把 key 走别的上游是脱敏的）。
	//
	// 判据：整块里**没有任何** data: 行 ⇒ 它不是 SSE，按整体 JSON 处理。
	// 这个判据比"首字节是 {"更宽：SSE 的首行是 "data: {"，不会误判。
	if !bytes.Contains(pending, []byte("data:")) {
		// 非 SSE：整块留给调用方（收尾处用 scrubChatResponse 处理）。
		//
		// 但如果它已经是**完整 JSON**（能被解码），就**当场**处理并输出 ——
		// 否则一个在流中途读完的整块 JSON 会一直压在 pending 里，
		// 直到 EOF 才发出去，等于把非流式变成"延迟到最后"。
		trimmed := bytes.TrimSpace(pending)
		if len(trimmed) > 0 && trimmed[0] == 0x7b && json.Valid(trimmed) {
			if scrub {
				return scrubChatResponse(trimmed, publicModel), nil
			}
			return pending, nil
		}
		return nil, pending
	}
	lastNL := bytes.LastIndexByte(pending, '\n')
	if lastNL < 0 {
		return nil, pending
	}
	complete := pending[:lastNL+1]
	rest = pending[lastNL+1:]

	out = make([]byte, 0, len(complete))
	for _, line := range bytes.SplitAfter(complete, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			out = append(out, line...)
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) == 0 || payload[0] != 0x7b {
			out = append(out, line...)
			continue
		}
		if !scrub {
			// 管理密钥：字节原样透传（不解析、不重编码）。
			//
			// ⚠ 连"解析后重新序列化"都不做：那会改变键顺序与数字格式，
			// 而用户要的是"按原来的上游即可"—— 包括它的字节形态。
			out = append(out, line...)
			continue
		}
		scrubbed := scrubChatResponse(payload, publicModel)
		out = append(out, []byte("data: ")...)
		out = append(out, scrubbed...)
		out = append(out, '\n')
	}
	return out, rest
}

// upstreamContentType 取上游响应的 Content-Type。
//
// ⚠ 当前 gateway.ChatStream 只带 Status/Body（没有 Header 字段），所以这里拿不到上游的
// Content-Type —— 返回空字符串让调用方按 SSE 兜底。等 ChatStream 将来带出 Header 时，
// 这个函数就是唯一要改的地方（把"暂时拿不到"换成真实值）。
func upstreamContentType(fw channelForward) string {
	_ = fw
	return ""
}
