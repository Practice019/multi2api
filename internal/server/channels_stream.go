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
	"io"
	"net/http"
)

// writeChannelStream 把一次渠道转发的流写回客户端。
func (h *Handler) writeChannelStream(w http.ResponseWriter, r *http.Request, fw channelForward) {
	defer fw.Stream.Body.Close()

	// 上游给的 Content-Type 优先；没给就按 SSE 兜底（这条链路基本只有流式）。
	if ct := upstreamContentType(fw); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	// 渠道信息回给调用方：排查"这条请求走了哪条渠道、上游模型叫什么"时不必翻日志。
	w.Header().Set("X-Channel-Id", fw.ChannelID)
	w.Header().Set("X-Upstream-Model", fw.UpstreamModel)

	status := fw.Stream.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := fw.Stream.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断了：正常结束，不要报错刷屏
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				// 上游中途出错：我们**无法**再改状态码（头已经发出去了），
				// 所以只能静默结束 —— 客户端会因为流提前结束而发现异常。
				return
			}
			return
		}
	}
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
