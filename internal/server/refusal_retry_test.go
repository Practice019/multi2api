package server

import (
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
	if len(audits) != 1 || audits[0] != "refusal_retry" {
		t.Errorf("审计事件 = %v，want [refusal_retry]", audits)
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

// （编译期：nudge 应用走的就是 prompt 包那条，防止我在这里另写一份）
var _ = prompt.ApplyRetryNudge
