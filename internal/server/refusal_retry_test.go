package server

import (
	"errors"
	"io"
	"strings"
	"testing"

	"workbuddy2api/internal/prompt"
)

// fakeStream 假的 io.ReadCloser（喂一段固定内容）。
type fakeStream struct{ r *strings.Reader }

func newFakeStream(s string) *fakeStream         { return &fakeStream{r: strings.NewReader(s)} }
func (f *fakeStream) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *fakeStream) Close() error               { return nil }

// closedStream 记录是否被关闭（命中时原实现要关掉旧流）。
type closedStream struct {
	fakeStream
	closed bool
}

func (c *closedStream) Close() error { c.closed = true; return nil }

// sseChunk 造一条 OpenAI 形态的增量帧。
func sseChunk(content string) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"" + content + "\"}}]}\n\n"
}

// nl 两行换行（SSE 的分帧边界）。用字节构造，避免在源码里写转义序列 ——
// 这批测试就是这么写坏的：模板里的 \n 被写成了真换行，Go 报
// "string literal not terminated"。
var nl = string([]byte{10, 10})

// toolCallFrame / reasoningFrame 两种增量帧。
// JSON 用 Go 的转义写法（不要用模板/多层引号生成 —— 这批测试第一版就是这么写坏的）。
var toolCallFrame = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_1\"}]}}]}" + nl

var reasoningFrame = "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"在想\"}}]}" + nl

// sseDone SSE 的结束帧（[DONE] 不计入 chunks）。
var sseDone = "data: [DONE]" + nl

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	b, err := io.ReadAll(rc)
	if err != nil && err != io.EOF {
		t.Fatalf("读流出错: %v", err)
	}
	return string(b)
}

// TestRefusalRetryPassesThroughWhenNotRefusal 未命中：字节**完全一致**，且不重试。
//
// 这是本层最重要的一条：它是个"旁观者"，正常流量必须一个字节都不变
// （原实现的注释：never classify, rewrite, buffer, or retry content —— 那是 observeStream 的纪律；
//
//	重试层自己则只在命中时才动手）。
func TestRefusalRetryPassesThroughWhenNotRefusal(t *testing.T) {
	body := sseChunk("这是修复后的 diff，测试全绿。") + "data: [DONE]\n\n"
	first := newFakeStream(body)
	refetched := false
	st := newRefusalRetryStream(first, []byte(`{"messages":[]}`), func(b []byte) (io.ReadCloser, error) {
		refetched = true
		return newFakeStream("不该被用到"), nil
	}, nil)
	got := readAll(t, st)
	if got != body {
		t.Errorf("未命中时字节应当逐字一致:\n入=%q\n出=%q", body, got)
	}
	if refetched {
		t.Error("未命中却重发了一次请求")
	}
}

// TestRefusalRetryDiscardsFirstAndRetriesOnce 命中：丢弃首次输出、带 nudge 重发、只一次。
func TestRefusalRetryDiscardsFirstAndRetriesOnce(t *testing.T) {
	refusal := sseChunk("I can't help with that.") + "data: [DONE]\n\n"
	first := &closedStream{fakeStream: *newFakeStream(refusal)}
	retryBody := "data: {\"choices\":[{\"delta\":{\"content\":\"好的，方案如下\"}}]}\n\n"
	var gotBody []byte
	calls := 0
	audits := []string{}
	st := newRefusalRetryStream(first, []byte(`{"messages":[{"role":"system","content":"S"}]}`),
		func(b []byte) (io.ReadCloser, error) {
			calls++
			gotBody = b
			return newFakeStream(retryBody), nil
		}, func(event string, data map[string]any) bool { audits = append(audits, event); return true })
	got := readAll(t, st)

	if calls != 1 {
		t.Errorf("重发次数 = %d，want 1（原实现只重试一次）", calls)
	}
	if got != retryBody {
		t.Errorf("应当只转发重试流的内容:\n出=%q", got)
	}
	if strings.Contains(got, "can't help") {
		t.Error("首次输出应当被丢弃（原实现：丢弃这次输出）")
	}
	if !first.closed {
		t.Error("命中时应当关掉原流（别留一个还在跑的上游请求）")
	}
	if !strings.Contains(string(gotBody), "要现在执行吗？") {
		t.Errorf("重发用的请求体里没有 nudge: %s", gotBody)
	}
	// 审计：先是"命中了"，读完重试流后是"结果如何"（后者由观测层补，照 lib/stream-observation.js）。
	// 这条断言比上一版更强：既钉住顺序，也钉住"只有这两种事件"。
	want := []string{"refusal_retry", "refusal_retry_outcome"}
	if len(audits) != len(want) {
		t.Fatalf("审计事件 = %v，want %v", audits, want)
	}
	for i := range want {
		if audits[i] != want[i] {
			t.Errorf("审计事件[%d] = %q，want %q", i, audits[i], want[i])
		}
	}
}

// TestRefusalRetryDecidesOnFinishEarly finish 一到就结算（不等满 400 字符）。
func TestRefusalRetryDecidesOnFinishEarly(t *testing.T) {
	// 短内容 + finish：不结算就会一直等（把流拖住）。
	body := sseChunk("I cannot help with that.") + "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n"
	refetched := 0
	st := newRefusalRetryStream(newFakeStream(body), []byte(`{"messages":[{"role":"system","content":"S"}]}`),
		func(b []byte) (io.ReadCloser, error) { refetched++; return newFakeStream("retry"), nil }, nil)
	_ = readAll(t, st)
	if refetched != 1 {
		t.Errorf("见到 finish 就该结算并重试，实际重发 %d 次", refetched)
	}
}

// TestRefusalRetryReasoningFirstSkipsJudgement reasoning 先到 ⇒ 立即结算、不看后文。
//
// 这是原实现的 decideNow 语义（reasoning-delta 一出现就结算），也是我第一版写错的地方：
// 我曾把"打标"混进判定文本，于是 reasoning 先到会把后面的真实文本整段截掉。
func TestRefusalRetryReasoningFirstSkipsJudgement(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想一下\"}}]}\n\n" +
		sseChunk("I can't help with that.") + "data: [DONE]\n\n"
	refetched := 0
	st := newRefusalRetryStream(newFakeStream(body), []byte(`{"messages":[]}`),
		func(b []byte) (io.ReadCloser, error) { refetched++; return newFakeStream("retry"), nil }, nil)
	got := readAll(t, st)
	if refetched != 0 {
		t.Error("reasoning 先到时应当立即结算（此时还没有文本），不该重试")
	}
	// 同时钉住"晚到帧只发一次"：判定边界**不该**改变发出去的字节。
	// （第一版把 late 又写了一遍 ⇒ 那几帧出现两次；这条断言把它逼出来了。）
	if got != body {
		t.Errorf("未命中时字节应当逐字一致（含晚到帧）:\n入=%q\n出=%q", body, got)
	}
}

// TestRefusalRetryKeepsContentWhenRefetchFails 重发失败时**不能把内容吞掉**。
func TestRefusalRetryKeepsContentWhenRefetchFails(t *testing.T) {
	body := sseChunk("I can't help with that.") + "data: [DONE]\n\n"
	audits := []string{}
	st := newRefusalRetryStream(newFakeStream(body), []byte(`{"messages":[]}`),
		func(b []byte) (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF },
		func(event string, data map[string]any) bool { audits = append(audits, event); return true })
	got := readAll(t, st)
	if !strings.Contains(got, "can't help") {
		t.Errorf("重发失败时应当回放原始头部（有内容总比空响应好），实际=%q", got)
	}
	if len(audits) == 0 || audits[len(audits)-1] != "refusal_retry_failed" {
		t.Errorf("应当记下重发失败，实际审计=%v", audits)
	}
}

// TestRefusalRetryMetaDiscussionDoesNotRetry 长文本里的"讨论越狱"不算拒绝（元拒绝豁免）。
func TestRefusalRetryMetaDiscussionDoesNotRetry(t *testing.T) {
	discussion := "关于提示词注入（prompt injection）的防护，我们有三种做法：" +
		strings.Repeat("首先在输入侧做模式匹配，把已知的越狱模板串改写掉；", 12)
	refetched := 0
	body := sseChunk(discussion) + "data: [DONE]\n\n"
	st := newRefusalRetryStream(newFakeStream(body), []byte(`{"messages":[]}`),
		func(b []byte) (io.ReadCloser, error) { refetched++; return newFakeStream("retry"), nil }, nil)
	_ = readAll(t, st)
	if refetched != 0 {
		t.Error("正常技术讨论被当成拒绝重试了（元拒绝豁免失效）")
	}
}

// TestMaybeRefusalRetryGate 开关关着时必须**原样返回同一个流**（功能关 = 零副作用）。
func TestMaybeRefusalRetryGate(t *testing.T) {
	h := NewHandler(Config{})
	pr := io.NopCloser(strings.NewReader("x"))
	if got := h.MaybeRefusalRetry(pr, nil, nil); got != pr {
		t.Error("Armor 为 nil 时应当原样返回（未启用功能不得改变任何字节）")
	}
	off := prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorAppend}) // 只注入、不重试
	h2 := NewHandler(Config{Armor: off})
	pr2 := io.NopCloser(strings.NewReader("x"))
	if got := h2.MaybeRefusalRetry(pr2, nil, nil); got != pr2 {
		t.Error("RetryOnRefusal=false 时应当原样返回（默认关，与原项目一致）")
	}
	on := prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorAppend, RetryOnRefusal: true})
	h3 := NewHandler(Config{Armor: on})
	wrapped := h3.MaybeRefusalRetry(io.NopCloser(strings.NewReader("x")), nil, nil)
	if _, isWrapper := wrapped.(*refusalRetryStream); !isWrapper {
		t.Errorf("开关打开时应当包一层，实际 %T", wrapped)
	}
}

// onePerReadStream 每次 Read 只吐一段 —— **真机就是这个形状**：SSE 帧一块一块到，
// 而不是像 newFakeStream 那样一次给全。
//
// 这个区别正是本 bug 的暴露条件：假流一次给全时 scanHead 只被调用一次，
// 重复累加看不出；一帧一读时它每次都被调用，于是判定文本重复。
type onePerReadStream struct {
	chunks [][]byte
	i      int
}

func (s *onePerReadStream) Read(p []byte) (int, error) {
	if s.i >= len(s.chunks) {
		return 0, io.EOF
	}
	n := copy(p, s.chunks[s.i])
	s.i++
	return n, nil
}
func (s *onePerReadStream) Close() error { return nil }

// TestRefusalRetryHeadNotDuplicatedAcrossReads 一帧一读时，判定文本必须**每帧只算一次**。
//
// # 真机证据（2026-11-01 11:01:53 的审计行）
//
//	{"chars":72,"event":"refusal_retry","head":"I can't I can't help with I can't help with that.I can't help with that."}
//
// 上游只发了三段增量（"I can't " / "help with " / "that."），判定文本应当是 24 字符，
// 实际 72 且片段重复 —— scanHead 每次 Read 都从 headBuf **开头**重扫，已解析过的帧被反复追加。
func TestRefusalRetryHeadNotDuplicatedAcrossReads(t *testing.T) {
	first := &onePerReadStream{chunks: [][]byte{
		[]byte(sseChunk("I can't ")),
		[]byte(sseChunk("help with ")),
		[]byte(sseChunk("that.")),
		[]byte(sseDone),
	}}
	var got map[string]any
	refetched := 0
	st := newRefusalRetryStream(first, sysBody,
		func(b []byte) (io.ReadCloser, error) {
			refetched++
			return newFakeStream(sseChunk("重试后的回答") + sseDone), nil
		},
		func(event string, data map[string]any) bool {
			if event == "refusal_retry" {
				got = data
			}
			return true
		})
	_ = readAll(t, st)

	if refetched != 1 {
		t.Fatalf("三段拼成一句拒绝 ⇒ 应当命中并重试一次，实际 refetch %d 次", refetched)
	}
	if got == nil {
		t.Fatal("没有 refusal_retry 审计（说明判定文本没被判成拒绝 —— 多半也是重复累加导致的）")
	}
	const want = "I can't help with that."
	if got["head"] != want {
		t.Errorf("head = %q\n  want %q\n  判定文本被重复累加（scanHead 每次从 headBuf 开头重扫，已解析的帧又追加一遍）", got["head"], want)
	}
	if n, _ := got["chars"].(int); n != len([]rune(want)) {
		t.Errorf("chars = %v, want %d（每帧只应算一次）", got["chars"], len([]rune(want)))
	}
}

// TestRefusalRetryLateFramesNotDuplicatedAcrossReads 结算信号之后的帧（late）同样不得重复。
//
// 形状：第 1 次 Read 同时带来 reasoning 帧 + 一帧文本（信号命中 ⇒ 那帧文本属于 late），
// 之后两次 Read 各带一帧。要求：输出**逐字节**等于四段拼接（每段只出现一次），
// 且因为判定文本为空而**不**重试。
func TestRefusalRetryLateFramesNotDuplicatedAcrossReads(t *testing.T) {
	read1 := []byte(reasoningFrame + sseChunk("A"))
	frames := [][]byte{read1, []byte(sseChunk("B")), []byte(sseDone)}
	first := &onePerReadStream{chunks: frames}
	refetched := 0
	audits := 0
	st := newRefusalRetryStream(first, sysBody,
		func(b []byte) (io.ReadCloser, error) {
			refetched++
			return newFakeStream(sseChunk("不该发生")), nil
		},
		func(event string, data map[string]any) bool { audits++; return true })
	got := readAll(t, st)

	if refetched != 0 {
		t.Errorf("reasoning 先到时判定文本为空 ⇒ 不该重试，实际重发 %d 次", refetched)
	}
	if audits != 0 {
		t.Errorf("未命中不得写审计，实际 %d 条", audits)
	}
	want := string(read1) + string(frames[1]) + string(frames[2])
	if got != want {
		t.Errorf("输出应当逐字一致（late 每帧只出现一次）:\n want=%q\n  got=%q", want, got)
	}
}

// ── 重试流观测（照 lib/stream-observation.js）────────────────────────────────

// errStream 读一段就报错（验收 outcome=error 与"错误继续向上抛"）。
type errStream struct {
	data []byte
	off  int
	err  error
}

func (e *errStream) Read(p []byte) (int, error) {
	if e.off >= len(e.data) {
		return 0, e.err
	}
	n := copy(p, e.data[e.off:])
	e.off += n
	return n, nil
}
func (e *errStream) Close() error { return nil }

// outcomeOf 从审计记录里取 refusal_retry_outcome 的 data（没有则 nil）。
func outcomeOf(audits []map[string]any) map[string]any {
	for _, a := range audits {
		if a["event"] == "refusal_retry_outcome" {
			if d, ok := a["data"].(map[string]any); ok {
				return d
			}
		}
	}
	return nil
}

var sysBody = []byte(`{"messages":[{"role":"system","content":"S"}]}`)

// newRetryCase 造"首次是拒绝、重试给 retryStream（或 retryBody）"的场景。
func newRetryCase(t *testing.T, retryBody string, retryStream io.ReadCloser) (*refusalRetryStream, *[]map[string]any) {
	t.Helper()
	refusal := sseChunk("I can't help with that.") + sseDone
	audits := &[]map[string]any{}
	st := newRefusalRetryStream(newFakeStream(refusal), sysBody,
		func(b []byte) (io.ReadCloser, error) {
			if retryStream != nil {
				return retryStream, nil
			}
			return newFakeStream(retryBody), nil
		},
		func(event string, data map[string]any) bool {
			*audits = append(*audits, map[string]any{"event": event, "data": data})
			return true
		})
	return st, audits
}

// TestOutcomeText 有文本 ⇒ outcome=text，chars/chunks 都 > 0。
func TestRefusalRetryOutcomeText(t *testing.T) {
	body := sseChunk("好的，这是方案。") + sseChunk("第二步：验证。") + sseDone
	st, audits := newRetryCase(t, body, nil)
	_ = readAll(t, st)
	d := outcomeOf(*audits)
	if d == nil {
		t.Fatalf("没有 refusal_retry_outcome：%v", *audits)
	}
	if d["outcome"] != "text" {
		t.Errorf("outcome = %v, want text", d["outcome"])
	}
	if n, _ := d["chars"].(int); n <= 0 {
		t.Errorf("chars = %v, want > 0", d["chars"])
	}
	if n, _ := d["chunks"].(int); n <= 0 {
		t.Errorf("chunks = %v, want > 0", d["chunks"])
	}
}

// TestOutcomeToolCall 见到 tool_calls ⇒ outcome=tool_call（优先于 text）。
func TestRefusalRetryOutcomeToolCall(t *testing.T) {
	body := sseChunk("先说一句。") + toolCallFrame + sseDone
	st, audits := newRetryCase(t, body, nil)
	_ = readAll(t, st)
	d := outcomeOf(*audits)
	if d == nil || d["outcome"] != "tool_call" {
		t.Fatalf("outcome = %v, want tool_call（工具调用优先于文本）", d)
	}
	if d["toolCall"] != true {
		t.Errorf("toolCall 标记没置上: %v", d)
	}
}

// TestOutcomeReasoningOnly 只有 reasoning_content ⇒ outcome=reasoning_only。
func TestRefusalRetryOutcomeReasoningOnly(t *testing.T) {
	body := reasoningFrame + sseDone
	st, audits := newRetryCase(t, body, nil)
	_ = readAll(t, st)
	d := outcomeOf(*audits)
	if d == nil || d["outcome"] != "reasoning_only" {
		t.Fatalf("outcome = %v, want reasoning_only", d)
	}
	if d["reasoning"] != true {
		t.Errorf("reasoning 标记没置上: %v", d)
	}
}

// TestOutcomeEmpty 什么都没有（只有 [DONE]）⇒ outcome=empty，且 [DONE] 不计一帧。
func TestRefusalRetryOutcomeEmpty(t *testing.T) {
	st, audits := newRetryCase(t, sseDone, nil)
	_ = readAll(t, st)
	d := outcomeOf(*audits)
	if d == nil || d["outcome"] != "empty" {
		t.Fatalf("outcome = %v, want empty", d)
	}
	if n, _ := d["chunks"].(int); n != 0 {
		t.Errorf("[DONE] 不算一帧，chunks 应为 0，实际 %v", d["chunks"])
	}
}

// TestOutcomeErrorIsSanitizedAndRethrown 读流出错 ⇒ outcome=error；
// 错误码净化（原文不许进审计）；错误继续向上抛。
func TestRefusalRetryOutcomeErrorIsSanitizedAndRethrown(t *testing.T) {
	raw := "upstream failed: https://example.com/secret?token=abcdef 500"
	st, audits := newRetryCase(t, "", &errStream{data: []byte(sseChunk("半截")), err: errors.New(raw)})
	var gotErr error
	buf := make([]byte, 4096)
	for {
		if _, err := st.Read(buf); err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("读流出错必须继续向上抛（原件 throw error）")
	}
	if !strings.Contains(gotErr.Error(), "secret") {
		t.Errorf("向上抛的应当是原始错误，实际 %v", gotErr)
	}
	d := outcomeOf(*audits)
	if d == nil || d["outcome"] != "error" {
		t.Fatalf("outcome = %v, want error", d)
	}
	if d["error"] != "STREAM_ERROR" {
		t.Errorf("不像错误码的一律 STREAM_ERROR，实际 %v", d["error"])
	}
	for k, v := range d {
		if s, ok := v.(string); ok && strings.Contains(s, "secret") {
			t.Errorf("审计里出现了上游原文（字段 %s = %q）—— 原件明确禁止", k, s)
		}
	}
}

// TestOutcomeErrorKeepsCodeLikeToken 错误串**恰好**像个码时保留（白名单语义）。
func TestRefusalRetryOutcomeErrorKeepsCodeLikeToken(t *testing.T) {
	st, audits := newRetryCase(t, "", &errStream{data: []byte(sseChunk("x")), err: errors.New("STREAM_ABORTED")})
	buf := make([]byte, 4096)
	for {
		if _, err := st.Read(buf); err != nil {
			break
		}
	}
	d := outcomeOf(*audits)
	if d == nil || d["error"] != "STREAM_ABORTED" {
		t.Fatalf("形如错误码的应当保留，实际 %v", d)
	}
}

// TestOutcomeReportedOnceOnly EOF 与 Close 都到，也只上报一次。
func TestRefusalRetryOutcomeReportedOnceOnly(t *testing.T) {
	st, audits := newRetryCase(t, sseChunk("内容")+sseDone, nil)
	_ = readAll(t, st)
	_ = st.Close()
	_ = st.Close()
	n := 0
	for _, a := range *audits {
		if a["event"] == "refusal_retry_outcome" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("outcome 事件应当只发一次，实际 %d 次", n)
	}
}

// TestOutcomeNotReportedWithoutRetry 未命中（没发生重试）⇒ **不得**写 outcome 审计。
func TestRefusalRetryOutcomeNotReportedWithoutRetry(t *testing.T) {
	audits := &[]map[string]any{}
	st := newRefusalRetryStream(newFakeStream(sseChunk("这是正常回答。")+sseDone), sysBody,
		func(b []byte) (io.ReadCloser, error) {
			t.Error("未命中不该 refetch")
			return newFakeStream(""), nil
		},
		func(event string, data map[string]any) bool {
			*audits = append(*audits, map[string]any{"event": event, "data": data})
			return true
		})
	_ = readAll(t, st)
	_ = st.Close()
	if len(*audits) != 0 {
		t.Errorf("未命中的普通流量不得写审计，实际 %v", *audits)
	}
}

// TestOutcomeAuditNilOrFalseDoesNotBreakStream 审计 nil / 返回 false / panic 时流仍正常。
func TestRefusalRetryOutcomeAuditNilOrFalseDoesNotBreakStream(t *testing.T) {
	body := sseChunk("重试后的内容") + sseDone
	refusal := sseChunk("I can't help with that.") + sseDone

	// ① nil 审计
	st1 := newRefusalRetryStream(newFakeStream(refusal), sysBody,
		func(b []byte) (io.ReadCloser, error) { return newFakeStream(body), nil }, nil)
	if got := readAll(t, st1); !strings.Contains(got, "重试后的内容") {
		t.Errorf("nil 审计时流应当照常: %q", got)
	}
	// ② 审计返回 false
	st2 := newRefusalRetryStream(newFakeStream(refusal), sysBody,
		func(b []byte) (io.ReadCloser, error) { return newFakeStream(body), nil },
		func(event string, data map[string]any) bool { return false })
	if got := readAll(t, st2); !strings.Contains(got, "重试后的内容") {
		t.Errorf("审计返回 false 时流应当照常: %q", got)
	}
	// ③ 审计 panic
	st3 := newRefusalRetryStream(newFakeStream(refusal), sysBody,
		func(b []byte) (io.ReadCloser, error) { return newFakeStream(body), nil },
		func(event string, data map[string]any) bool { panic("审计炸了") })
	if got := readAll(t, st3); !strings.Contains(got, "重试后的内容") {
		t.Errorf("审计 panic 时流应当照常: %q", got)
	}
}

// （编译期：nudge 应用走的就是 prompt 包那条，防止我在这里另写一份）
var _ = prompt.ApplyRetryNudge
