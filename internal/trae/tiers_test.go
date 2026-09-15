// tiers_test.go 模型分档 / 排队事件 / 自动降级的单元测试。
package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFallbackChain 降级链的构成与封顶。
func TestFallbackChain(t *testing.T) {
	chain := fallbackChain("glm-5.2", 3)
	if chain[0] != "glm-5.2" {
		t.Fatalf("链首应是原模型: %v", chain)
	}
	if len(chain) > 3 {
		t.Errorf("链长超过 maxAttempts: %v", chain)
	}
	// T1 只有 glm-5.2 → 下一候选应来自 T2。
	if chain[1] != "glm-5.1" {
		t.Errorf("T1 的下一候选应是 T2 的 glm-5.1: %v", chain)
	}
	// 同链不重复。
	seen := map[string]bool{}
	for _, m := range chain {
		if seen[m] {
			t.Errorf("降级链出现重复模型: %v", chain)
		}
		seen[m] = true
	}
	// 未知模型 → [model, 兜底 glm-5]。
	unk := fallbackChain("some-unknown-model", 5)
	if len(unk) != 2 || unk[1] != fallbackModel {
		t.Errorf("未知模型的链应为 [model, glm-5]: %v", unk)
	}
	// maxAttempts=1 → 只有原模型。
	one := fallbackChain("glm-5", 1)
	if len(one) != 1 || one[0] != "glm-5" {
		t.Errorf("maxAttempts=1 应只有原模型: %v", one)
	}
	// T2 的候选先同档（排在后面的），再**下**档（T3）—— 降级方向是"变便宜"，不是"变贵"。
	t2 := fallbackChain("DeepSeek-V4-Pro", 6)
	if t2[1] != "glm-5" {
		t.Errorf("T2 最后一名的下一候选应是 T3 的 glm-5: %v", t2)
	}
	// 同档中间位置的模型：先补同档后面的，再下档。
	mid := fallbackChain("qwen-3.7-plus", 6)
	if mid[1] != "kimi-k2.6" {
		t.Errorf("同档后面的 kimi-k2.6 应先于下档: %v", mid)
	}
}

// TestParseQueueEvent 排队事件两种形态都能解出 position。
func TestParseQueueEvent(t *testing.T) {
	ev, err := parseSOLOLine("request_wait_in_queue", `{"position":350}`)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Position != 350 {
		t.Errorf("平铺形态 position = %d", ev.Position)
	}
	ev2, err := parseSOLOLine("request_wait_in_queue", `{"data":{"position":412}}`)
	if err != nil {
		t.Fatal(err)
	}
	if ev2.Position != 412 {
		t.Errorf("嵌套形态 position = %d", ev2.Position)
	}
}

// TestConvertSOLOWithQueueRetry 排队超阈值且可重试 → 放弃流并返回位置。
func TestConvertSOLOWithQueueRetry(t *testing.T) {
	input := "event:request_wait_in_queue\ndata:{\"position\":400}\n\n"
	var sb strings.Builder
	pos, err := convertSOLOWithQueue(strings.NewReader(input), &sb, 300, true)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 400 {
		t.Fatalf("应返回排队位置 400，得到 %d", pos)
	}
	if sb.Len() != 0 {
		t.Errorf("放弃流的路径不该写任何内容: %q", sb.String())
	}
}

// TestConvertSOLOWithQueueNoRetry 不可重试 → 继续流并 [DONE]。
func TestConvertSOLOWithQueueNoRetry(t *testing.T) {
	input := "event:request_wait_in_queue\ndata:{\"position\":400}\n\n" +
		"event:output\ndata:{\"response\":\"hi\"}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	var sb strings.Builder
	pos, err := convertSOLOWithQueue(strings.NewReader(input), &sb, 300, false)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Fatalf("不可重试时应继续流，返回 0，得到 %d", pos)
	}
	if !strings.Contains(sb.String(), `"content":"hi"`) || !strings.Contains(sb.String(), "data: [DONE]") {
		t.Errorf("输出不完整: %s", sb.String())
	}
}

// TestConvertSOLOWithQueueAfterContent 已出内容后排队 → 不放弃。
func TestConvertSOLOWithQueueAfterContent(t *testing.T) {
	input := "event:output\ndata:{\"response\":\"part\"}\n\n" +
		"event:request_wait_in_queue\ndata:{\"position\":900}\n\n"
	var sb strings.Builder
	pos, err := convertSOLOWithQueue(strings.NewReader(input), &sb, 300, true)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Errorf("已出内容后遇到排队不应放弃流: %d", pos)
	}
	if !strings.Contains(sb.String(), `"content":"part"`) {
		t.Errorf("已写内容缺失: %s", sb.String())
	}
}

// TestChatFallbackSwitchesModel 端到端：模型 A 排队 → 自动换模型 B 重发。
func TestChatFallbackSwitchesModel(t *testing.T) {
	var calls int
	var bodies []string
	mux := http.NewServeMux()
	mux.HandleFunc(EpChat, func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		model, _ := obj["config_name"].(string)
		bodies = append(bodies, model)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			// 第一次（原模型）：只报排队，不产内容。
			_, _ = io.WriteString(w, "event:request_wait_in_queue\ndata:{\"position\":50}\n\n")
			return
		}
		// 第二次（降级候选）：正常内容。
		_, _ = io.WriteString(w, "event:output\ndata:{\"response\":\"降级成功\"}\n\n")
		_, _ = io.WriteString(w, "event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := NewWithConfig(Config{
		Client:          NewWithBase(srv.URL),
		FallbackEnabled: true,
		QueueThreshold:  10,
		MaxAttempts:     3,
	})
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	cs, err := p.Chat(context.Background(), newContractCredential("u-fallback"), body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(cs.Body)
	_ = cs.Body.Close()
	out := string(raw)

	if calls != 2 {
		t.Errorf("应发 2 次请求（原模型 + 降级），实际 %d", calls)
	}
	if bodies[0] != "glm-5.2" {
		t.Errorf("第一次应发 glm-5.2，实际 %q", bodies[0])
	}
	if bodies[1] == "" || bodies[1] == "glm-5.2" {
		t.Errorf("第二次应换候选模型，实际 %q", bodies[1])
	}
	if !strings.Contains(out, `"content":"降级成功"`) {
		t.Errorf("降级后应输出内容: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("缺 [DONE]: %s", out)
	}
}

// TestChatFallbackDisabled 关闭降级 → 排队也只透传等待（单次请求）。
func TestChatFallbackDisabled(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc(EpChat, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event:request_wait_in_queue\ndata:{\"position\":50}\n\n")
		_, _ = io.WriteString(w, "event:output\ndata:{\"response\":\"ok\"}\n\n")
		_, _ = io.WriteString(w, "event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := NewWithConfig(Config{
		Client:          NewWithBase(srv.URL),
		FallbackEnabled: false, // 关降级
		QueueThreshold:  10,
		MaxAttempts:     3,
	})
	cs, err := p.Chat(context.Background(), newContractCredential("u-nofb"),
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(cs.Body)
	_ = cs.Body.Close()
	if calls != 1 {
		t.Errorf("关闭降级只应发 1 次，实际 %d", calls)
	}
	if !strings.Contains(string(raw), `"content":"ok"`) || !strings.Contains(string(raw), "data: [DONE]") {
		t.Errorf("透传等待后应正常输出: %s", string(raw))
	}
}
