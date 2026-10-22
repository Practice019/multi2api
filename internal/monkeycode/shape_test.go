package monkeycode

import (
	"encoding/json"
	"testing"

	"workbuddy2api/internal/anthroconv"
)

// TestToResponsesBodyShape 检查转换出来的 Responses 体。
func TestToResponsesBodyShape(t *testing.T) {
	in := []byte(`{"model":"monkeycode-basic/qwen3.8-flash",
		"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],
		"max_tokens":100,"temperature":0.7,"stream":false}`)
	out, err := toResponsesBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	t.Logf("输出: %s", out)
	for _, k := range []string{"model", "instructions", "input", "stream", "store", "max_output_tokens"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺字段 %s", k)
		}
	}
}

// TestToAnthropicBodyShape 检查 anthropic 那条的形状。
func TestToAnthropicBodyShape(t *testing.T) {
	in := []byte(`{"model":"monkeycode-basic/deepseek-flash",
		"messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	anthro, err := anthroconv.OpenAIToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	withSys, err := ensureSystemSignature(anthro)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("anthropic 体: %s", withSys)
	if systemOf(withSys) == "" {
		t.Error("system 为空，签名会失败")
	}
	iface := interfaceOf("monkeycode-basic/deepseek-flash")
	t.Logf("协议: %s", iface)
	if iface != ifaceAnthropic {
		t.Fatalf("deepseek-flash 应走 anthropic，实际 %s", iface)
	}
}
