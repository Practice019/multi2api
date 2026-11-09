package wire

import (
	"encoding/json"
	"testing"
)

// TestNormalizeBodyForStreaming 钉住三件必须成立的事（T-335 抽共享时定的契约）。
func TestNormalizeBodyForStreaming(t *testing.T) {
	// ① 强制 stream
	out := NormalizeBodyForStreaming([]byte(`{"stream":false,"max_tokens":10}`), 200000, "t")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil || m["stream"] != true {
		t.Fatalf("stream 未被强制: %s", out)
	}
	// ② 超上界 ⇒ 收敛到上界（且用的是**传入**的上界）
	out = NormalizeBodyForStreaming([]byte(`{"max_tokens":999999999}`), 200000, "t")
	m = map[string]any{} // ⚠ 必须重置：Unmarshal 进非 nil map 会**合并**旧键
	_ = json.Unmarshal(out, &m)
	if m["max_tokens"] != float64(200000) {
		t.Fatalf("上界未生效: %s", out)
	}
	// ③ 非法值 ⇒ 删除字段（0 / 负数 / 非数字）
	for _, bad := range []string{`{"max_tokens":0}`, `{"max_tokens":-5}`, `{"max_tokens":"x"}`} {
		out = NormalizeBodyForStreaming([]byte(bad), 200000, "t")
		m = map[string]any{}
		_ = json.Unmarshal(out, &m)
		if _, ok := m["max_tokens"]; ok {
			t.Fatalf("非法值应删除: %s → %s", bad, out)
		}
	}
	// ④ 非 JSON / 空 ⇒ 原样返回（不猜）
	if got := string(NormalizeBodyForStreaming([]byte("not json"), 200000, "t")); got != "not json" {
		t.Fatalf("非 JSON 应原样: %s", got)
	}
	if got := NormalizeBodyForStreaming(nil, 200000, "t"); got != nil {
		t.Fatalf("空输入应原样: %v", got)
	}
}
