// intl_identity_test.go 国际版对话身份头的判据。
//
// 背景：国际版上游的渠道校验按会话头族 + 身份头识别客户端，缺一项就 400/11128。
// 这三层是 2026-10-21 在本机逐项验证出来的（Agent2API 的抓包结论一致）：
//
//	只补身份头       ⇒ 仍 11128
//	再补会话头族     ⇒ 11128，但 msg 变成 "first message is not system prompt"
//	再补 system 首条 ⇒ 200 + SSE
//
// 本文件把"该发的都发了""cn 不该被改"钉住。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newChatReq() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	return r
}

// 国际版：三层身份头必须齐，且会话 id 形态正确。
func TestApplyIntlChatHeadersSetsFullFamily(t *testing.T) {
	req := newChatReq()
	ApplyIntlChatHeaders(req, "www.workbuddy.ai")

	must := []string{
		"X-IDE-Type", "X-IDE-Name", "X-IDE-Version", "X-Product",
		"X-Agent-Intent", "X-Agent-Purpose", "X-Agent-Type", "X-Private-Data",
		"X-Conversation-ID", "X-Conversation-Request-ID", "X-Root-Request-ID",
		"X-Conversation-Message-ID", "X-Request-ID",
		"X-Trace-ID", "traceparent", "b3", "X-B3-TraceId", "X-B3-SpanId", "X-B3-Sampled",
		"Accept-Language", "X-Requested-With", "X-CodeBuddy-Request", "Origin", "Referer",
	}
	for _, h := range must {
		if req.Header.Get(h) == "" {
			t.Errorf("缺少 %s —— 国际版渠道校验会回 400/11128", h)
		}
	}
	if got := req.Header.Get("User-Agent"); got != intlChatUA {
		t.Errorf("UA = %q，want %q", got, intlChatUA)
	}
	if got := req.Header.Get("X-IDE-Version"); got != "5.8.4" {
		t.Errorf("X-IDE-Version = %q，want 5.8.4", got)
	}
	// -Request-ID 与 -Root-Request-ID 必须同值（官方一次 send 内复用）
	if a, b := req.Header.Get("X-Conversation-Request-ID"), req.Header.Get("X-Root-Request-ID"); a != b {
		t.Errorf("X-Conversation-Request-ID 与 X-Root-Request-ID 必须同值（%q vs %q）", a, b)
	}
	// 会话 id 带连字符、其余是 32 位 hex
	conv := req.Header.Get("X-Conversation-ID")
	if len(conv) != 36 || strings.Count(conv, "-") != 4 {
		t.Errorf("X-Conversation-ID = %q，应为 36 位带连字符 UUID", conv)
	}
	msg := req.Header.Get("X-Request-ID")
	if len(msg) != 32 || strings.Contains(msg, "-") {
		t.Errorf("X-Request-ID = %q，应为 32 位无连字符 hex", msg)
	}
	if got := req.Header.Get("Origin"); got != "https://www.workbuddy.ai" {
		t.Errorf("Origin = %q", got)
	}
	if got := req.Header.Get("Accept-Language"); got != "en-US" {
		t.Errorf("Accept-Language = %q，国际版应为 en-US", got)
	}
}

// 每次请求的会话 id 必须**新生成**（上游不靠它存历史，但重复 id 是可识别特征）。
func TestApplyIntlChatHeadersGeneratesFreshIDs(t *testing.T) {
	r1 := newChatReq()
	r2 := newChatReq()
	ApplyIntlChatHeaders(r1, "www.workbuddy.ai")
	ApplyIntlChatHeaders(r2, "www.workbuddy.ai")
	if r1.Header.Get("X-Conversation-ID") == r2.Header.Get("X-Conversation-ID") {
		t.Error("两次请求的 X-Conversation-ID 不能相同")
	}
	if r1.Header.Get("X-Request-ID") == r2.Header.Get("X-Request-ID") {
		t.Error("两次请求的 X-Request-ID 不能相同")
	}
}

// 域名判据：只有 workbuddy.ai 算国际版。
func TestIsIntlDomain(t *testing.T) {
	cases := map[string]bool{
		"www.workbuddy.ai":    true,
		"workbuddy.ai":        true,
		"WWW.WORKBUDDY.AI":    true,
		"www.codebuddy.cn":    false,
		"copilot.tencent.com": false,
		"":                    false,
	}
	for d, want := range cases {
		if got := IsIntlDomain(d); got != want {
			t.Errorf("IsIntlDomain(%q) = %v，want %v", d, got, want)
		}
	}
}

// 首条不是 system ⇒ 插一条；已经是 system/developer ⇒ 原样返回（不动原 body）。
func TestEnsureSystemFirst(t *testing.T) {
	noSys := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out := EnsureSystemFirst(noSys)
	if !strings.Contains(string(out), `"role":"system"`) {
		t.Fatalf("未插入 system: %s", out)
	}
	// 其它字段必须原样保留
	for _, k := range []string{`"model":"m"`, `"stream":true`, `"role":"user"`} {
		if !strings.Contains(string(out), k) {
			t.Errorf("重写 body 时丢了字段 %s: %s", k, out)
		}
	}

	withSys := []byte(`{"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)
	if got := EnsureSystemFirst(withSys); string(got) != string(withSys) {
		t.Errorf("已有 system 时不该改写，实际 %s", got)
	}
	withDev := []byte(`{"messages":[{"role":"developer","content":"x"},{"role":"user","content":"hi"}]}`)
	if got := EnsureSystemFirst(withDev); string(got) != string(withDev) {
		t.Errorf("developer 首条同样满足要求，不该改写，实际 %s", got)
	}
	// 非法 JSON / 空 messages ⇒ 原样返回（不 panic、不破坏请求）
	bad := []byte(`{not json`)
	if got := EnsureSystemFirst(bad); string(got) != string(bad) {
		t.Error("非法 JSON 必须原样返回")
	}
	empty := []byte(`{"messages":[]}`)
	if got := EnsureSystemFirst(empty); string(got) != string(empty) {
		t.Error("空 messages 必须原样返回")
	}
}
