// chat_stream_test.go 非流式请求的守卫（端到端实测暴露的缺陷）。
//
// # 这个 bug 的形态最坏：不报错，只是回答是空的
//
// 端到端跑的时候，非流式请求返回 `content: ""` —— 没有 5xx、没有 error 字段、
// HTTP 200。这类缺陷不会引发排查，只会让用户以为"模型今天状态不好"。
//
// 根因：非流式分支按 Anthropic **JSON** 解析响应，而上游这个端点**恒回
// SSE**（我们恒发 stream:true）。JSON 解析失败后代码把原始 SSE 当成
// JSON 交了出去。
//
// 正解来自核心架构：非流式由出口层用 `wire.Aggregate` 把 SSE 合成一条
// JSON（handler.go:1603）。所以上游侧**只需要**产出一份好的 SSE。
package minimax

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游必须**无论客户端要不要流式**都回一份 OpenAI SSE。
func TestChatReturnsSSEForNonStreamRequest(t *testing.T) {
	var gotStream any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathInferMessages {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		gotStream = body["stream"]
		w.Header().Set("Content-Type", "text/event-stream")
		// Anthropic SSE：一段正文 + 结束。
		_, _ = io.WriteString(w, "event: message_start\n"+
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"+
			"event: content_block_start\n"+
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n"+
			"event: content_block_stop\n"+
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
			"event: message_stop\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	p := New(Config{AccountBase: srv.URL, APIBase: srv.URL})
	// ⚠ stream:false —— 正是出问题的那条路径。
	body := []byte(`{"model":"MiniMax-M2.7","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	cs, err := p.Chat(context.Background(), gatewayCred(&Auth{AccessToken: "mmoat_x", TokenType: "Bearer"}), body)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer cs.Body.Close()

	out, _ := io.ReadAll(cs.Body)
	text := string(out)

	// 1. 出去请求必须带 stream:true（这个端点只按 SSE 回）。
	if gotStream != true {
		t.Errorf("发往上游的 stream 应为 true，实际 %v", gotStream)
	}
	// 2. 回来的必须是 OpenAI SSE（可被核心的 Aggregate 合成）。
	if !strings.Contains(text, "data:") {
		t.Errorf("响应里没有任何 SSE 帧 —— 非流式若走 JSON 分支就会在这里失败，\n"+
			"然后把原始 SSE 当 JSON 交出去，客户端拿到空 content。实际:\n%s", text)
	}
	if !strings.Contains(text, "chat.completion.chunk") {
		t.Errorf("应是 OpenAI 的 chat.completion.chunk 流，实际:\n%s", text)
	}
	// 3. 正文必须在里面（丢正文就是"空回答"的直接形态）。
	if !strings.Contains(text, "你好") {
		t.Errorf("正文丢了（用户会看到空回答）:\n%s", text)
	}
	if !strings.Contains(text, "[DONE]") {
		t.Error("流必须以 [DONE] 收尾，否则核心会挂着等")
	}
}

// 用核心的聚合器验一次：非流式客户端最终能拿到完整正文。
//
// ⚠ 这条把"上游侧交 SSE、出口层合成 JSON"这个**分工**本身钉住 ——
// 少了它，未来有人"顺手"给非流式加回 JSON 分支也不会有测试变红。
func TestNonStreamAggregatesToContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"+
			"event: content_block_start\n"+
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"聚合测试\"}}\n\n"+
			"event: content_block_stop\n"+
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
			"event: message_stop\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	p := New(Config{AccountBase: srv.URL, APIBase: srv.URL})
	cs, err := p.Chat(context.Background(), gatewayCred(&Auth{AccessToken: "t"}),
		[]byte(`{"model":"MiniMax-M2.7","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Body.Close()

	// 手工按 SSE 拆帧，取出 delta.content（等价于核心 wire.Aggregate 的判据）。
	raw, _ := io.ReadAll(cs.Body)
	var content strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &ch) == nil && len(ch.Choices) > 0 {
			content.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if got := content.String(); got != "聚合测试" {
		t.Errorf("非流式客户端最终拿到的正文 = %q，期望 %q\n原始流:\n%s", got, "聚合测试", string(raw))
	}
}
