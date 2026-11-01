// refusal_retry.go 拒绝重试：照 dsh-purge 的 lib/refusal-retry.js 逐条移植。
//
// # 原实现（我抄出来的算法，出处：lib/refusal-retry.js 的 streamWithRefusalRetry）
//
//	① 先在开头**缓冲**上游的输出，同时累计 text-delta 的字符数
//	② 结算条件 decideNow：text 长度 ≥ bufferChars(400)
//	   或见到 finish / tool-call-delta / reasoning-delta（这三类一出现就不必再等）
//	③ 结算：looksLikeRefusal(缓冲的文本)
//	     · 命中 → **关掉当前流**（不留一个还在跑的上游请求）→ 审计 refusal_retry
//	              → 用 withNudge(options) **重跑一次** → 转发新流
//	     · 未命中 → 把缓冲的块**原样吐出去**，然后继续透传
//	④ 只重试**一次**（没有循环）
//	⑤ 默认**关闭**（原文件头：llm/stream 钩子，默认关闭）
//
// # 为什么做成 io.ReadCloser 包装而不是改转发循环
//
// 我们的转发循环（writeChannelStream / 账号池那条）已经承担三件事：
// 旁路采集、按行脱敏、flush 时机。把重试塞进去要改三处，而**包一层流**只改取流那一行：
// 下游读到的字节要么是"原流的字节"，要么是"重试流的字节"，其余逻辑一行不动。
// 这也让它可以单测：喂假流、假 refetch，不需要网络。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"workbuddy2api/internal/prompt"
)

// refusalHeadBufChars 结算前要缓冲的文本字符数（原实现 bufferChars=400）。
const refusalHeadBufChars = 400

// refusalAuditFn 审计回调（原实现有两条事件，见文件头）。
//
// 用回调而不是直接写日志：本文件在核心包里，不该认识具体的审计落盘方式；
// 装配层把它接到请求日志/审计文件上（与 PromptGate 同一手法）。
type refusalAuditFn func(event string, data map[string]any)

// refusalRetryStream 见文件头。零值不可用，用 newRefusalRetryStream 构造。
type refusalRetryStream struct {
	first   io.ReadCloser // 当前正在读的上游流
	body    []byte        // 本次请求**已发出去**的请求体（重试时用它算 nudge 版）
	refetch func(body []byte) (io.ReadCloser, error)
	audit   refusalAuditFn
	// nudgeFn 默认 prompt.ApplyRetryNudge；测试可替换。
	nudgeFn func([]byte) []byte

	out     bytes.Buffer // 已决定要交给下游的字节（未命中时=缓冲头，命中时=空）
	decided bool
	retried bool
	// early 见到 finish / tool_calls / reasoning_content 之一 ⇒ 不必再等满 400 字符。
	//
	// 与 headTxt **分开**存：混在一起会污染判定文本（见 judgeText 的注释）。
	early bool
	// late 结算信号之后"晚到"的帧：它们不算头部（不参与判定），
	// 但未命中时要原样发给客户端 —— 见 scanHead 的注释。
	late    bytes.Buffer
	headBuf bytes.Buffer // 决定之前的原始字节
	headTxt strings.Builder
	closed  bool
}

// newRefusalRetryStream 包一层流。
//
// refetch 拿"带 nudge 的请求体"重新取流 —— 由调用方提供（它才知道怎么再发一次）。
func newRefusalRetryStream(first io.ReadCloser, body []byte, refetch func([]byte) (io.ReadCloser, error), audit refusalAuditFn) *refusalRetryStream {
	return &refusalRetryStream{first: first, body: body, refetch: refetch, audit: audit, nudgeFn: prompt.ApplyRetryNudge}
}

func (s *refusalRetryStream) auditEvent(event string, data map[string]any) {
	if s.audit == nil {
		return
	}
	// 审计失败不影响主流程（原实现同样如此：try/catch 包住 logAudit）。
	defer func() { _ = recover() }()
	s.audit(event, data)
}

// Read 第一次调用时先走"缓冲—结算"，之后就是纯透传。
func (s *refusalRetryStream) Read(p []byte) (int, error) {
	if !s.decided {
		if err := s.decide(); err != nil && s.out.Len() == 0 {
			return 0, err
		}
	}
	if s.out.Len() > 0 {
		return s.out.Read(p)
	}
	if s.retried && s.first == nil {
		return 0, io.EOF
	}
	return s.first.Read(p)
}

// Close 关掉当前流（含已换成的重试流），并保证只关一次。
func (s *refusalRetryStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.first != nil {
		return s.first.Close()
	}
	return nil
}

// decide 读上游直到"可以结算"，然后按 ③ 处理。
func (s *refusalRetryStream) decide() error {
	buf := make([]byte, 8<<10)
	for {
		if s.headTxt.Len() >= refusalHeadBufChars || s.early {
			break
		}
		n, err := s.first.Read(buf)
		if n > 0 {
			s.headBuf.Write(buf[:n])
			s.scanHead()
		}
		if err != nil {
			// EOF ⇒ 上游给的就是这么短：就地结算（原实现 step.done 时同样结算）。
			s.decided = true
			return s.settle()
		}
	}
	s.decided = true
	return s.settle()
}

// scanHead 逐**帧**扫描头部：累计文本，并在结算信号出现时**立刻停止**。
//
// # 为什么要逐帧，而不是整段一次扫完（第一版整段扫，测试当场报红）
//
// 原实现逐块迭代：`decideNow(chunk)` 在**每一块之后**判断，见到 reasoning-delta 就
// decided=true 并**停止迭代** —— 后面的块**根本没被读过**。
// 所以"先 reasoning 后拒绝文本"的实际判定文本是**空**（不重试）。
//
// 我第一版一次 Read 把两帧都读进来、整段扫完，后到的拒绝文本也被算进判定 ⇒ 多一次重试。
// 差异只在流的切分上，单测（假流一次给全）正好把它逼出来。
//
// 现在：逐帧处理；信号命中后剩下的帧移到 `late`（它们**不属于头部**）：
// 未命中时随头部一起回放，命中时随首次输出一起丢弃。
func (s *refusalRetryStream) scanHead() {
	raw := s.headBuf.String()
	consumed := 0
	for {
		rest := raw[consumed:]
		if s.early {
			s.late.WriteString(rest)
			return
		}
		idx := strings.Index(rest, "\n\n")
		if idx < 0 {
			return // 半帧：留到下一次（头部最多几百字节，代价可忽略）
		}
		frame := rest[:idx]
		consumed += idx + 2
		s.scanFrame(frame)
	}
}

// scanFrame 处理一帧（同一帧里可能有多行 data:，按 SSE 规范要拼接）。
func (s *refusalRetryStream) scanFrame(frame string) {
	var payload strings.Builder
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		part := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if part == "" || part == "[DONE]" {
			continue
		}
		payload.WriteString(part)
	}
	if payload.Len() == 0 {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload.String()), &obj); err != nil {
		return // 非 JSON 行跳过：不该因为上游偶发的一行非标准报文让重试层出错
	}
	s.scanChunk(obj)
}

// scanChunk 处理一条增量：累计 content 文本；见到 finish / tool_calls / reasoning 就打标。
func (s *refusalRetryStream) scanChunk(obj map[string]any) {
	if fr, ok := obj["finish_reason"].(string); ok && fr != "" {
		s.early = true
	}
	choices, _ := obj["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
			s.early = true
		}
		delta, ok := cm["delta"].(map[string]any)
		if !ok {
			continue
		}
		if txt, ok := delta["content"].(string); ok {
			s.headTxt.WriteString(txt)
		}
		if _, ok := delta["tool_calls"]; ok {
			s.early = true
		}
		if _, ok := delta["reasoning_content"]; ok {
			s.early = true
		}
	}
}

// judgeText 判定用的纯文本。
//
// ⚠ 第一版我把 finish/tool/reasoning 这几类信号也写进了同一个 builder，用 \x00 分隔、
//
//	判定时取第一段 —— 那是错的：若信号先到（例如先来一个 reasoning 增量），
//	真实文本会被整段截掉，判定看着"没有文本"。现在信号走独立的 early 标志位，
//	文本始终只有 content，与"原实现只累计 text-delta"一致。
func (s *refusalRetryStream) judgeText() string { return s.headTxt.String() }

// settle 结算：命中 ⇒ 关旧流 + 审计 + 带 nudge 重取；未命中 ⇒ 回放缓冲。
func (s *refusalRetryStream) settle() error {
	text := s.judgeText()
	if !prompt.LooksLikeRefusal(text) {
		// 未命中：缓冲的字节原样交给下游（原实现 for (const chunk of buffered) yield chunk）。
		//
		// ⚠ headBuf **已经包含** late（late 是 headBuf 的后缀，只是没参与判定）——
		//   第一版在这里又写了一遍 late，于是那几帧被发了两次。
		//   "字节完全一致"的断言把它逼出来了（它比"内容在不在"严得多）。
		s.out.Write(s.headBuf.Bytes())
		return nil
	}
	// 命中：先关掉当前流（别留一个还在跑的上游请求）。
	_ = s.first.Close()
	s.auditEvent("refusal_retry", map[string]any{
		"chars": len([]rune(text)),
		"head":  head80(text),
		"match": prompt.RefusalMatchIndex(text),
	})
	if s.refetch == nil {
		// 没给重取能力 ⇒ 只能把已经拿到的转发出去（不能把内容吞掉）。
		s.out.Write(s.headBuf.Bytes())
		return nil
	}
	next, err := s.refetch(s.nudgeFn(s.body))
	if err != nil {
		// 重取失败：把原始头部回放（有内容总比空响应好），并如实记下。
		s.auditEvent("refusal_retry_failed", map[string]any{"error": "refetch"})
		s.out.Write(s.headBuf.Bytes())
		return nil
	}
	s.retried = true
	s.first = next
	// 命中时不回放旧头（原实现：丢弃这次输出）。缓冲直接丢。
	s.headBuf.Reset()
	s.headTxt.Reset()
	return nil
}

// head80 取前 80 个字符（原实现审计里 head: text.slice(0, 80)）。
func head80(s string) string {
	r := []rune(s)
	if len(r) <= 80 {
		return s
	}
	return string(r[:80])
}

// 编译期断言：它就是个 io.ReadCloser（下游按流消费）。
var _ io.ReadCloser = (*refusalRetryStream)(nil)
