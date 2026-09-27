package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
)

// 出站边界的 fixture。
//
// ⚠ 与 internal/wire/sanitize_test.go 里的同名常量**必须一致** ——
// 它们是同一批指纹样本，分处两个包（一个测纯函数、一个测出站边界）。
// 值改了要两边同步，否则"纯函数会改写它"与"出站后它消失了"这两个断言
// 会基于不同的输入，"改写了但没生效"这类缺陷就会漏。
const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Default branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=2.0.0; cc_entrypoint=cli;"
)

func TestChatStreamWireBodySanitized(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints = true
	c.PromptMode = prompt.ModePassthrough // ← 隔离提示词层
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	// 上游收到的 body：stream 强制 + 指纹已净化
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, fp := range []string{"x-anthropic-billing-header", ccIdentity, ccBranch} {
		if strings.Contains(sys, fp) {
			t.Errorf("wire body contains fingerprint %q: %q", fp, sys)
		}
	}
}

// 出站边界：关闭脱敏后 wire body 原样保留指纹（验证开关真实有效）。
//
// ⚠ 必须同时把 PromptMode 设成 passthrough，否则本用例**测不到脱敏开关**。
//
// 为什么：出站管线是 `applyPrompt` → `PrepareBodyOpt...` 两层。
// 默认的 custom 模式会在脱敏之前就把整条 system 消息**删掉换成网关自有提示词**，
// 于是 wire body 里当然没有指纹 —— 但那是提示词层的功劳，与脱敏开关无关。
// 不隔离变量的话，本用例即使脱敏开关彻底失效也会通过（假绿）。
//
// 设成 passthrough 之后，system 原样保留，脱敏开关成为唯一的变量：
// 关掉它 ⇒ 指纹必须原样出现在 wire body 上。
func TestChatStreamWireBodySanitizeDisabled(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints = false
	c.PromptMode = prompt.ModePassthrough // ← 隔离提示词层，见上方注释
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	rc, status, _, err := c.ChatStream(acct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d", status)
	}
	if !strings.Contains(string(gotBody), ccIdentity) {
		t.Error("sanitize disabled should preserve fingerprint on wire")
	}
}

// newTestUpstream 起一个假上游并捕获请求。
func newTestUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(h)
}