// contract_hermetic_test.go MonkeyCode 的契约测试与假上游。
//
// 全程 hermetic：自建 httptest 服务器扮演上游，不碰网络。
//
// # 假上游为什么必须校验签名
//
// 本上游唯一独特的约束就是签名。真实上游签名错了回 403
// "invalid ohmyagent request"，而那个报错**不会**告诉你错在哪。
// 假上游能精确说出「你应该签 system/instructions 字段」。
package monkeycode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/gateway"
)

// fakeUpstream 假 MonkeyCode 上游。
type fakeUpstream struct {
	srv *httptest.Server

	secret     string            // 期望的 signing_secret
	interfaces map[string]string // 模型名 → 协议

	mu          sync.Mutex
	chatCalls   int
	modelsCalls int
	lastSigOK   bool
	lastPath    string
}

// newFakeUpstream 起一个假上游。
func newFakeUpstream(t *testing.T, secret string, interfaces map[string]string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{secret: secret, interfaces: interfaces}
	mux := http.NewServeMux()

	// ---- 模型目录（只需 session cookie）----
	mux.HandleFunc(pathModels, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.modelsCalls++
		f.mu.Unlock()
		if !strings.Contains(r.Header.Get("Cookie"), "monkeycode_ai_session=") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"Unauthorized"}`)
			return
		}
		entries := make([]map[string]any, 0, len(f.interfaces))
		for name, iface := range f.interfaces {
			entries = append(entries, map[string]any{
				"model":          name,
				"interface_type": iface,
				"access_level":   "basic",
				"context_limit":  131072,
				"output_limit":   16384,
			})
		}
		body, _ := json.Marshal(map[string]any{
			"code": 0, "message": "success",
			"data": map[string]any{"models": entries},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	// ---- 两种对话端点 ----
	responsesHandler := f.chatHandler(ifaceResponses)
	anthropicHandler := f.chatHandler(ifaceAnthropic)
	mux.HandleFunc("/v1/responses", responsesHandler)
	mux.HandleFunc("/v1/messages", anthropicHandler)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// chatHandler 造一个按协议校验的对话处理器。
func (f *fakeUpstream) chatHandler(wantIface string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.chatCalls++
		f.lastPath = r.URL.Path
		f.mu.Unlock()

		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		model, _ := m["model"].(string)

		// ① 模型必须走它该走的那条协议（走错在真实上游是 404 路由不存在）。
		if got := f.interfaces[model]; got != "" && got != wantIface {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"路由不存在","type":"not_found_error"}`)
			return
		}

		// ② 签名必须对得上**实际发出去的那段提示词**。
		signedText := instructionsOf(raw)
		if wantIface == ifaceAnthropic {
			signedText = systemOf(raw)
		}
		if signedText == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"invalid ohmyagent request"}`)
			return
		}
		if !VerifySignature(f.secret, signedText, r.Header.Get(signatureHeader)) {
			f.mu.Lock()
			f.lastSigOK = false
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"invalid ohmyagent request"}`)
			return
		}
		f.mu.Lock()
		f.lastSigOK = true
		f.mu.Unlock()

		// ③ 回一段最小 SSE。
		w.Header().Set("Content-Type", "text/event-stream")
		if wantIface == ifaceAnthropic {
			_, _ = io.WriteString(w, anthropicSSEFixture)
			return
		}
		_, _ = io.WriteString(w, responsesSSEFixture)
	}
}

// authFor 造一份指向假上游的凭证。
func (f *fakeUpstream) authFor(region string) *Auth {
	return &Auth{
		Session:       "test-session-uuid",
		APIKey:        "oma_test_key",
		SigningSecret: f.secret,
		Region:        region,
		UserID:        "test-user",
		Nickname:      "测试号",
	}
}

// TestContract 跑核心的契约测试。
func TestContract(t *testing.T) {
	gateway.RunProviderContract(t, NewProvider)
}

// 两段最小 SSE 固定件。
const responsesSSEFixture = "event: response.created\n" +
	"data: {\"type\":\"response.created\"}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\n"

const anthropicSSEFixture = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"
