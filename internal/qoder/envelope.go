// envelope.go Qoder 加密推理端点的**响应解包**。
//
// # 为什么需要它（不是可选优化，是功能前提）
//
// 加密端点 `agent_chat_generation` 的响应是 SSE，但**每帧多包一层信封**：
//
//	data:{"headers":{...},"body":"{\"choices\":[{\"delta\":{\"content\":\"Q\"}}]}",
//	     "statusCodeValue":200,"statusCode":"OK"}
//	          ↑ 这里才是标准 OpenAI chunk（**JSON 字符串**，被转义了一层）
//
// 直接把这种流交给下游，下游看到的是"每帧都没有 choices 字段"——
// 表现为**对话没有任何输出且不报错**（最坏的一类故障：静默无输出）。
//
// # ⚠ 内层 body **没有**加密
//
// 只有**请求**体需要 WASM 加密（那是 qoder 需要 WASM 的唯一原因）。
// 响应侧只是多了一层信封，**纯文本处理**即可 ——
// 所以本文件是**完全独立于 WASM 签名器**的：即使签名器没接上，
// 解包逻辑也能单独测试（见 envelope_test.go）。
//
// # 错误形态（必须**抛出**而不是当成无内容）
//
// 失败时内层 body 是业务错误 JSON（如 `{"code":10605,"message":"..."}`），
// 必须转成标准 error 帧交给下游统一抛错 ——
// 否则会重演"静默停止"那个缺陷（流结束但用户什么都没看到）。
//
// ⚠ **转发时必须保留 `code` 字段**（参照项目的真实缺陷，用户报障 2026-09-27）：
// 参照实现曾把内层 `{code, message}` 降级重组为 `{error:{message:"… (code)"}}`，
// 把 code 拼成文案后缀并**丢掉字段**，于是下游按顶层 `code === '10605'`
// 识别排队错误的逻辑**永远不命中** → 排队被归为普通服务端错误 →
// 客户端以 500…8000ms 快退避重试 5 次（共约 15.5 秒），
// 而服务端要求等 30 秒 —— **永远等不到**。
//
// 故这里**保真转发**：`code` 独立成字段、`message` 原样不拼后缀。
package qoder

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// unwrapEnvelopeLine 把一行 SSE 文本解包成标准 OpenAI 帧。
//
// 返回 (新行, 是否改写)。false 表示"不是信封，原样透传"——
// 这是**容错**而非兜底：服务端某天若直接回标准帧，这里必须放行。
//
// 处理规则（逐条对应参照实现，不自行发挥）：
//
//	非 `data:` 行（如 `event:finish`） → 原样保留（对诊断有价值）
//	`data: [DONE]`                    → 原样保留
//	信封（有 body 字段）               → 取出内层，按内容分两种：
//	      内层含 "choices" 或 "[DONE]" → 标准帧，直接透传内层
//	      否则                        → 业务错误，转成 error 帧（保留 code）
//	非信封                            → 原样透传
func unwrapEnvelopeLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return line, false
	}
	payload := strings.TrimSpace(line[len("data:"):])
	if payload == "" || payload == "[DONE]" {
		return line, false
	}

	inner, ok := innerTextOf(payload)
	if !ok {
		return line, false
	}

	// 内层是错误 JSON（没有 choices）→ 转成标准 error 帧。
	//
	// 判据用**子串**而不是解析 JSON：内层是**转义过的 JSON 字符串**，
	// 先解析外层拿到字符串再判断更贵，且这里只需要"是不是对话帧"这一个粗判据。
	// 与参照实现同款判据（它也是 includes('"choices"')）。
	if !strings.Contains(inner, `"choices"`) && !strings.Contains(inner, "[DONE]") {
		return "data: " + errorFrameOf(inner), true
	}
	return "data: " + inner, true
}

// innerTextOf 从信封 JSON 里取出内层 body 文本。
//
// 返回 ok=false 表示"这不是信封"（缺 body 字段 / 不是合法 JSON）——
// 调用方据此原样透传，**不要**把解析失败当成错误：服务端某天直接回
// 标准帧是完全合法的，那时这里就该放行。
func innerTextOf(payload string) (string, bool) {
	var env struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return "", false
	}
	if len(env.Body) == 0 {
		return "", false
	}
	// body 是**字符串**时取出原文（未再转义）；
	// 是对象/数组时重新序列化（服务端两种形态都可能，参照实现同样两路处理）。
	var s string
	if err := json.Unmarshal(env.Body, &s); err == nil {
		return s, true
	}
	return string(env.Body), true
}

// errorFrameOf 把内层错误 JSON 转成标准 error 帧。
//
// ⚠ `code` 必须保持为**独立字段**、`message` 不得拼后缀 ——
// 理由见文件头（排队识别依赖顶层 code）。
//
// 内层不是合法 JSON 时退化成把原文当 message：宁可显示原文，
// 也不要因为"解析不了"而把错误吞掉（那正是"静默停止"的成因）。
func errorFrameOf(inner string) string {
	var parsed struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	frame := map[string]any{"type": "model_error"}
	if err := json.Unmarshal([]byte(inner), &parsed); err != nil {
		frame["message"] = inner
	} else {
		frame["message"] = parsed.Message
		if len(parsed.Code) > 0 {
			// 原样搬运：code 在上游有数字与字符串两种形态，
			// 这里不猜测类型（用 RawMessage 直接塞进 map）。
			frame["code"] = json.RawMessage(parsed.Code)
		}
	}
	out, err := json.Marshal(frame)
	if err != nil {
		// Marshal 一个 map[string]any 只在含不可序列化值时才失败，
		// 而这里全是 string / RawMessage —— 理论上不可达。
		// 真到了这一步，退化成只带 message 的帧比 panic 好。
		out, _ = json.Marshal(map[string]any{"type": "model_error", "message": inner})
	}
	return string(out)
}

// unwrapEnvelopeStream 把 Qoder 信封 SSE 流转成标准 OpenAI SSE 流。
//
// # 为什么要流式而不是读完再改
//
// 对话是 SSE 流式输出，读完再改等于**取消流式**（用户要等到全部生成完
// 才看到第一个字）。所以必须逐行转换、边收边发。
//
// # 缓冲策略
//
// 按 `\n` 切行，最后一段不完整的行留在缓冲里等下一个 chunk ——
// 这是流式解析的必需处理：TCP 分片不会尊重行边界，
// 一次 Read 很可能拿到半行（把半行当整行发出去会得到一段无法解析的 JSON）。
//
// 行尾的 `\r` 会剥掉：SSE 规范允许 CRLF，而 `data:` 前缀判断不受影响，
// 但把 `\r` 留在 JSON 里会让解析失败。
func unwrapEnvelopeStream(src io.Reader) io.ReadCloser {
	return &envelopeReader{src: src}
}

// envelopeReader 逐行解包 SSE 的 io.ReadCloser。
type envelopeReader struct {
	src io.Reader
	// buf 已读入但尚未成行的字节。
	buf []byte
	// out 已转换好、等待被读走的字节。
	out []byte
	// eof 源已读完（buf 里可能还有最后一行）。
	eof bool
	// closed 显式关闭标记。
	closed bool
}

func (r *envelopeReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		if err := r.fill(); err != nil && err != io.EOF {
			return 0, err
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// fill 读一块源数据并转换出完整的行。
func (r *envelopeReader) fill() error {
	chunk := make([]byte, 32<<10)
	n, err := r.src.Read(chunk)
	if n > 0 {
		r.buf = append(r.buf, chunk[:n]...)
		r.drain()
	}
	if err == io.EOF {
		r.eof = true
		// 处理最后一段没有换行结尾的内容（合法：SSE 最后一帧可能没有 \n）。
		if len(r.buf) > 0 {
			r.emit(r.buf)
			r.buf = nil
		}
	}
	return err
}

// drain 把 buf 里所有完整行转换并挪进 out。
func (r *envelopeReader) drain() {
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i < 0 {
			return
		}
		r.emit(r.buf[:i])
		r.buf = r.buf[i+1:]
	}
}

// emit 转换一行（含行尾处理）并追加到 out。
func (r *envelopeReader) emit(line []byte) {
	s := strings.TrimSuffix(string(line), "\r")
	if s == "" {
		// 空行是 SSE 的帧分隔符，必须保留 —— 丢了会让下游把
		// 相邻两帧拼成一帧（表现为"内容粘连"）。
		r.out = append(r.out, '\n')
		return
	}
	out, _ := unwrapEnvelopeLine(s)
	r.out = append(r.out, out...)
	r.out = append(r.out, '\n')
}

// Close 关闭源流。
//
// ⚠ 必须转发到源流：不转发会让 HTTP 连接泄漏 ——
// 上游一多就表现成"跑一会儿全卡住"（契约测试专门检查这一条）。
func (r *envelopeReader) Close() error {
	if r.closed {
		// 二次 Close 不报错、不 panic（有些实现会）。
		return nil
	}
	r.closed = true
	if c, ok := r.src.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
