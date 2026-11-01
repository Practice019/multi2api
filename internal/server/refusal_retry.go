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
	"regexp"
	"strings"
	"unicode/utf8"

	"workbuddy2api/internal/prompt"
)

// refusalHeadBufChars 结算前要缓冲的文本字符数（原实现 bufferChars=400）。
const refusalHeadBufChars = 400

// RefusalAuditFn 审计回调（导出版本，供 handler 的 Config 字段用）。
type RefusalAuditFn = refusalAuditFn

// MaybeRefusalRetry 按开关决定要不要包一层重试流。
//
// 开关关着（默认）时**原样返回** —— 未启用功能不得改变任何字节，
// 这是本层唯一能被下游观察到的承诺。
//
// refetch：拿"带 nudge 的请求体"重新取一次流（调用方提供 —— 只有它知道怎么再发一次）。
func (h *Handler) MaybeRefusalRetry(first io.ReadCloser, body []byte, refetch func([]byte) (io.ReadCloser, error)) io.ReadCloser {
	if h == nil || first == nil {
		return first
	}
	if h.cfg.Armor == nil || !h.cfg.Armor.RetryEnabled() {
		return first
	}
	return newRefusalRetryStream(first, body, refetch, h.cfg.RefusalAudit)
}

// refusalAuditFn 审计回调（原实现有两条事件，见文件头）。
//
// 用回调而不是直接写日志：本文件在核心包里，不该认识具体的审计落盘方式；
// 装配层把它接到请求日志/审计文件上（与 PromptGate 同一手法）。
//
// 返回 bool：与原实现的 logAudit 一致（"写进去了没有"）。调用方**不**据此改变行为 ——
// 审计失败绝不能影响主流程，但把它吐出来便于测试断言。
type refusalAuditFn func(event string, data map[string]any) bool

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
	// scanned headBuf 里**已经解析过**的字节数。
	//
	// ⚠ 这个字段是 2026-11-01 真机 bug 的直接产物：scanHead 原来用局部 `consumed := 0`，
	//   每次从 headBuf **开头**重扫，而 headTxt 是**追加**写 —— 于是每多一次 Read，
	//   已解析过的帧就被再追加一遍。真机审计行长这样：
	//     chars=72 head="I can't I can't help with I can't help with that.I can't help with that."
	//   而上游只发了三段（I can't / help with / that.）。
	//   一帧一读时必然复现；假流一次给全时只触发一次调用，所以单测要先红后绿。
	//
	// 半帧（找不到分帧空行）**不推进**它 —— 留到下一次 Read 收全了再算。
	scanned int
	closed  bool

	// ── 重试流的观测（照 lib/stream-observation.js）────────────────────
	//
	// **只在发生过重试**（refetch 成功之后）时统计：未命中的普通流量一个字节都不碰，
	// 也不多写一条审计（约束："未命中的普通流量不得多写审计"）。
	//
	// 原件的纪律也照搬：observing 只做**传输诊断**，
	// 绝不因为计数而改写、缓冲或重试内容（它自己文件头那句 never classify/rewrite/buffer/retry）。
	obs        bool   // 是否在观测（refetch 成功后置 true）
	obsChunks  int    // 看到的帧数（对应原件的 chunks）
	obsChars   int    // text 字符数（对应原件的 chars）
	obsTool    bool   // 见到 tool_calls
	obsReason  bool   // 见到 reasoning_content
	obsPartial []byte // 半帧（SSE 按空行分帧，跨读的残帧要拼起来）
	obsDone    bool   // 只上报一次（EOF / Close 都只算第一次）
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
	_ = s.audit(event, data)
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
	if s.first == nil {
		// 流已经没了：重试流走到这里就是正常收尾（EOF 上报一次观测）。
		s.reportOutcome("", nil)
		return 0, io.EOF
	}
	n, err := s.first.Read(p)
	if n > 0 {
		s.observe(p[:n])
	}
	if err != nil {
		if err == io.EOF {
			s.reportOutcome("", nil) // 正常结尾：outcome 由计数器决定
		} else {
			s.reportOutcome("error", err) // 出错：outcome=error，且**错误继续向上抛**（原件 throw）
		}
	}
	return n, err
}

// Close 关掉当前流（含已换成的重试流），并保证只关一次。
func (s *refusalRetryStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	// 被提前关掉（客户端断开 / 下游不再读）⇒ outcome=cancelled。
	// 与原件的初始值一致：它把 outcome 初始化成 "cancelled"，只有正常跑完才改写。
	s.reportOutcome("cancelled", nil)
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
	for {
		rest := raw[s.scanned:]
		if s.early {
			// 信号之后的帧不算头部：整段搬进 late，并把进度推到末尾，
			// 避免它们在下一次调用里被当成新帧再处理一遍。
			s.late.WriteString(rest)
			s.scanned = len(raw)
			return
		}
		idx := strings.Index(rest, "\n\n")
		if idx < 0 {
			return // 半帧：**不推进 scanned**，留到下一次（头部最多几百字节，代价可忽略）
		}
		frame := rest[:idx]
		s.scanned += idx + 2
		s.scanFrame(frame)
	}
}

// parseFrame 把一帧 SSE 解析成对象：同一帧里可能有多行 data:（按 SSE 规范拼接），
// `[DONE]` 与空行不算一帧。
//
// 抽出来是因为"判定用"和"观测用"必须走**同一套**解析口径 ——
// 两处各写一份的话，同一个流会出现两种 chunks/chars 计数（原件只有一条路径，不会分叉）。
func parseFrame(frame string) (map[string]any, bool) {
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
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload.String()), &obj); err != nil {
		return nil, false // 非 JSON 行跳过：不该因为上游偶发的一行非标准报文让重试层出错
	}
	return obj, true
}

// scanFrame 处理一帧：解析后喂给判定扫描。
func (s *refusalRetryStream) scanFrame(frame string) {
	if obj, ok := parseFrame(frame); ok {
		s.scanChunk(obj)
	}
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
	// 从这里开始观测**重试流**（只在重试发生后才统计，未命中流量不受影响）。
	s.obs = true
	s.first = next
	// 命中时不回放旧头（原实现：丢弃这次输出）。缓冲直接丢。
	s.headBuf.Reset()
	s.headTxt.Reset()
	return nil
}

// ── 重试流的观测（照 lib/stream-observation.js）──────────────────────────

// observe 累计从**重试流**读到的字节：按空行分帧，逐帧计数。
//
// 只在 obs（发生过重试）时工作；未命中的普通流量这里一个字节都不看。
func (s *refusalRetryStream) observe(b []byte) {
	if !s.obs || s.obsDone || len(b) == 0 {
		return
	}
	s.obsPartial = append(s.obsPartial, b...)
	for {
		idx := bytes.Index(s.obsPartial, []byte("\n\n"))
		if idx < 0 {
			return // 半帧：留到下一次（SSE 帧可能跨两次 Read）
		}
		frame := string(s.obsPartial[:idx])
		s.obsPartial = s.obsPartial[idx+2:]
		if obj, ok := parseFrame(frame); ok {
			s.observeChunk(obj)
		}
	}
}

// observeChunk 一帧的计数：chunks / chars / tool_calls / reasoning_content。
//
// 对应原件的：chunks += 1；text-delta → chars；tool-call(-delta) → toolCall；
// reasoning-delta → reasoning。
func (s *refusalRetryStream) observeChunk(obj map[string]any) {
	s.obsChunks++
	choices, _ := obj["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := cm["delta"].(map[string]any)
		if !ok {
			continue
		}
		if txt, ok := delta["content"].(string); ok {
			s.obsChars += utf8.RuneCountInString(txt)
		}
		if _, ok := delta["tool_calls"]; ok {
			s.obsTool = true
		}
		if _, ok := delta["reasoning_content"]; ok {
			s.obsReason = true
		}
	}
}

// reportOutcome 上报**一次**观测结果。
//
// outcome 为空 ⇒ 由计数器决定（tool_call / text / reasoning_only / empty，与原件的三目链一致：
// 工具调用优先于文本，文本优先于"只有推理"）。
// 非空 ⇒ 强制用它（error / cancelled），其中 error 要附**净化过**的错误码。
//
// 只上报一次：EOF 与 Close 谁先到就谁报（原件靠 generator 的 finally 天然只跑一次）。
func (s *refusalRetryStream) reportOutcome(outcome string, err error) {
	if !s.obs || s.obsDone {
		return
	}
	s.obsDone = true
	// 残帧结算：EOF 时最后一段可能没有结尾空行（原件按块迭代，不存在这个边界）。
	if len(s.obsPartial) > 0 {
		if obj, ok := parseFrame(string(s.obsPartial)); ok {
			s.observeChunk(obj)
		}
		s.obsPartial = nil
	}
	if outcome == "" {
		switch {
		case s.obsTool:
			outcome = "tool_call"
		case s.obsChars > 0:
			outcome = "text"
		case s.obsReason:
			outcome = "reasoning_only"
		default:
			outcome = "empty"
		}
	}
	data := map[string]any{
		"outcome":   outcome,
		"chunks":    s.obsChunks,
		"chars":     s.obsChars,
		"toolCall":  s.obsTool,
		"reasoning": s.obsReason,
	}
	if outcome == "error" {
		data["error"] = sanitizeErrCode(err)
	}
	s.auditEvent("refusal_retry_outcome", data)
}

// errCodeRe 错误码白名单（原件：/^[A-Z][A-Z0-9_]{0,39}$/）。
var errCodeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,39}$`)

// sanitizeErrCode 只放行"长得像错误码"的东西，其余一律 STREAM_ERROR。
//
// 原件的注释写得很明确：Do not store arbitrary error messages, URLs, response content or credentials.
// 所以**绝不**把上游原始错误文本写进审计 —— 那里面常见 URL、凭据片段、响应片段。
func sanitizeErrCode(err error) string {
	if err == nil {
		return "STREAM_ERROR"
	}
	// 带类型的错误可以自带码（Go 里对应 error?.code）。
	if ce, ok := err.(interface{ ErrorCode() string }); ok {
		if code := ce.ErrorCode(); errCodeRe.MatchString(code) {
			return code
		}
	}
	// 退一步：整个错误串**恰好**是一个码才用（不是"包含"）。
	if errCodeRe.MatchString(err.Error()) {
		return err.Error()
	}
	return "STREAM_ERROR"
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

// ═══════════════════════════════════════════════════════════════════
// 非流式路径的拒绝重试 —— **我们的扩展**，原件没有这一层
// ═══════════════════════════════════════════════════════════════════
//
// # 为什么原件没有
//
// dsh-purge 的 lib/refusal-retry.js 只挂 llm/stream（一个 async generator 包装器）：
// 它整套机制都建立在"边到边流"上（缓冲前 400 字符 → 结算 → 丢弃已吐出的、换新流）。
// 非流式根本没有这个形状，所以原件不覆盖它。
//
// # 为什么我们补它
//
// 我们这边的用户一路用 `stream:false` 测 —— 于是拒绝重试**对他从不触发**，
// 他得到的结论会是"这个功能没用"。而补它的代价是零：非流式响应**本来就要整段读完**
// 才能返回客户端，判定与重发都发生在"已读完"之后，不增加任何等待。
//
// # 与流式版共用什么
//
//	prompt.LooksLikeRefusal  同一份检测表与元拒绝豁免（不另写一份）
//	prompt.ApplyRetryNudge   同一份话术追加（只看 messages[0]、仅当它是 system）
//	h.cfg.RefusalAudit       同一批事件名，多带 mode=non_stream 便于区分
//	只重试一次               与原件一致，没有循环
//
// # 不变式
//
//   · 开关关着 ⇒ 不判定、不审计、返回**同一个 map / 同一段字节**（行为与改造前逐字节一致）
//   · 重发失败或上游 >=400 ⇒ **保留第一份回答**（绝不把错误体当回答）
//   · 第一份回答为空 ⇒ 不判定（空文本不是拒绝）

// refusalAuditEvent 发一条审计事件（nil 安全；审计失败绝不影响主流程）。
func (h *Handler) refusalAuditEvent(event string, data map[string]any) {
	if h == nil || h.cfg.RefusalAudit == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		_ = h.cfg.RefusalAudit(event, data)
	}()
}

// nonStreamText 取非流式响应里的回答正文（判定用）。
//
// 兼容两种形态：message.content 是字符串，或是由若干 part 组成的数组（只取 text part）。
func nonStreamText(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	cm, _ := choices[0].(map[string]any)
	if cm == nil {
		return ""
	}
	msg, _ := cm["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["type"].(string); t != "text" {
				continue
			}
			if s, ok := pm["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

// nonStreamOutcome 给非流式响应归类（与流式版的 outcome 取值同一套）。
//
// 口径出处：lib/stream-observation.js 的 outcome 只描述**传输形态**
// （text / tool_call / reasoning_only / empty），不代表回答正确或任务成功。
func nonStreamOutcome(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return "empty"
	}
	cm, _ := choices[0].(map[string]any)
	if cm == nil {
		return "empty"
	}
	msg, _ := cm["message"].(map[string]any)
	if msg == nil {
		return "empty"
	}
	if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
		return "tool_call"
	}
	if nonStreamText(resp) != "" {
		return "text"
	}
	if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
		return "reasoning_only"
	}
	return "empty"
}

// retryRefusalNonStreamMap 非流式拒绝重试的**核心**（两个调用点共用）。
//
// 返回最终响应对象与"是否换过"。未命中 / 开关关 / 重发失败都返回入参 first 本身（changed=false）。
func (h *Handler) retryRefusalNonStreamMap(body []byte, first map[string]any, refetch func(nudged []byte) (map[string]any, error)) (map[string]any, bool) {
	if h == nil || h.cfg.Armor == nil || !h.cfg.Armor.RetryEnabled() {
		return first, false
	}
	text := nonStreamText(first)
	if text == "" || !prompt.LooksLikeRefusal(text) {
		return first, false
	}
	h.refusalAuditEvent("refusal_retry", map[string]any{
		"chars": len([]rune(text)),
		"head":  head80(text),
		"match": prompt.RefusalMatchIndex(text),
		"mode":  "non_stream",
	})
	if refetch == nil {
		return first, false
	}
	next, err := refetch(prompt.ApplyRetryNudge(body))
	if err != nil || next == nil {
		// 重发失败：**保留第一份回答**。把错误体当回答是这里最坏的错法。
		h.refusalAuditEvent("refusal_retry_failed", map[string]any{"error": "refetch", "mode": "non_stream"})
		return first, false
	}
	h.refusalAuditEvent("refusal_retry_outcome", map[string]any{
		"outcome": nonStreamOutcome(next),
		"chars":   len([]rune(nonStreamText(next))),
		"mode":    "non_stream",
	})
	return next, true
}

// retryRefusalNonStreamBytes 字节版（渠道路径用：那边拿到的是一整段响应字节）。
//
// ⚠ 只在"能解析成 JSON 对象"时才判定；解析不了、或没换过对象 ⇒ **逐字节返回原字节**。
func (h *Handler) retryRefusalNonStreamBytes(body []byte, raw []byte, refetch func(nudged []byte) ([]byte, error)) []byte {
	if h == nil || h.cfg.Armor == nil || !h.cfg.Armor.RetryEnabled() {
		return raw
	}
	var first map[string]any
	if err := json.Unmarshal(raw, &first); err != nil {
		return raw
	}
	out, changed := h.retryRefusalNonStreamMap(body, first, func(nudged []byte) (map[string]any, error) {
		nraw, err := refetch(nudged)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(nraw, &m); err != nil {
			return nil, err
		}
		return m, nil
	})
	if !changed {
		return raw
	}
	reencoded, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return reencoded
}
