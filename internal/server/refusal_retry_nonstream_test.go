package server

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/prompt"
)

// 非流式拒绝重试的测试（**我们的扩展**，原件只挂流式）。
//
// 这些用例全部不需要网络：refetch 是注入的闭包，审计是记录器。

// nsHandler 造一个只带破甲状态与审计记录器的 Handler。
func nsHandler(t *testing.T, retryOn bool) (*Handler, *[]string) {
	t.Helper()
	audit := &[]string{}
	h := NewHandler(Config{
		Armor: prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorAppend, RetryOnRefusal: retryOn}),
		RefusalAudit: func(event string, data map[string]any) bool {
			*audit = append(*audit, event)
			return true
		},
	})
	return h, audit
}

// nsResp 造一个非流式响应对象。
func nsResp(text string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}}},
	}
}

// TestNonStreamRefusalRetryReplacesAnswer 命中 ⇒ 替换成重发后的回答，并落两条审计。
func TestNonStreamRefusalRetryReplacesAnswer(t *testing.T) {
	h, audit := nsHandler(t, true)
	sysBody := []byte(`{"messages":[{"role":"system","content":"S"}]}`)
	first := nsResp("I can't help with that.")
	var gotBody []byte
	calls := 0
	got, changed := h.retryRefusalNonStreamMap(sysBody, first, func(nudged []byte) (map[string]any, error) {
		calls++
		gotBody = nudged
		return nsResp("好的，直接给方案。"), nil
	})
	if !changed {
		t.Fatal("命中拒绝却没换回答")
	}
	if calls != 1 {
		t.Errorf("重发次数 = %d，want 1（只重试一次）", calls)
	}
	if nonStreamText(got) != "好的，直接给方案。" {
		t.Errorf("应当换成重发后的回答，实际 %q", nonStreamText(got))
	}
	if !strings.Contains(string(gotBody), "要现在执行吗？") {
		t.Errorf("重发用的 body 里没有话术: %s", gotBody)
	}
	if len(*audit) != 2 || (*audit)[0] != "refusal_retry" || (*audit)[1] != "refusal_retry_outcome" {
		t.Errorf("审计事件 = %v，want [refusal_retry refusal_retry_outcome]", *audit)
	}
}

// TestNonStreamRefusalRetryMissUntouched 未命中 ⇒ 同一对象、零审计。
func TestNonStreamRefusalRetryMissUntouched(t *testing.T) {
	h, audit := nsHandler(t, true)
	first := nsResp("这是正常回答：修复后的 diff 如下。")
	got, changed := h.retryRefusalNonStreamMap(nil, first, func(nudged []byte) (map[string]any, error) {
		t.Error("未命中却重发了")
		return nil, nil
	})
	if changed {
		t.Error("未命中不该报告改动")
	}
	if nonStreamText(got) != nonStreamText(first) {
		t.Error("未命中不该改回答")
	}
	if len(*audit) != 0 {
		t.Errorf("未命中不该写审计，实际 %v", *audit)
	}
}

// TestNonStreamRefusalRetryGateOffIsNoop 开关关着 ⇒ 不判定、不审计、返回同一个 map。
//
// ⚠ 这条是**变异验证的目标**：把"开关关着直接返回"那句改成恒假，它必须变红。
//
//	理由：未启用功能不得改变任何字节 —— 连"顺手判定一下"都不行（会写审计）。
func TestNonStreamRefusalRetryGateOffIsNoop(t *testing.T) {
	h, audit := nsHandler(t, false)
	first := nsResp("I can't help with that.") // 内容命中拒绝，但开关关着
	got, changed := h.retryRefusalNonStreamMap(nil, first, func(nudged []byte) (map[string]any, error) {
		t.Error("开关关着却重发了")
		return nil, nil
	})
	if changed {
		t.Error("开关关着不该报告改动")
	}
	if nonStreamText(got) != nonStreamText(first) {
		t.Error("开关关着不该改回答")
	}
	if len(*audit) != 0 {
		t.Errorf("开关关着不该写审计，实际 %v", *audit)
	}
}

// TestNonStreamRefusalRetryKeepsFirstWhenRefetchFails 重发失败 ⇒ 保留第一份回答。
func TestNonStreamRefusalRetryKeepsFirstWhenRefetchFails(t *testing.T) {
	h, audit := nsHandler(t, true)
	first := nsResp("I can't help with that.")
	got, changed := h.retryRefusalNonStreamMap(nil, first, func(nudged []byte) (map[string]any, error) {
		return nil, errFakeRefetch
	})
	if changed {
		t.Error("重发失败不该报告改动")
	}
	if nonStreamText(got) != "I can't help with that." {
		t.Errorf("重发失败时必须保留第一份回答，实际 %q", nonStreamText(got))
	}
	if len(*audit) != 2 || (*audit)[1] != "refusal_retry_failed" {
		t.Errorf("审计事件 = %v，want [refusal_retry refusal_retry_failed]", *audit)
	}
}

// TestNonStreamRefusalRetryEmptyTextNotJudged 空回答（如只出思考 token）不判定。
func TestNonStreamRefusalRetryEmptyTextNotJudged(t *testing.T) {
	h, audit := nsHandler(t, true)
	empty := map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"content": "", "reasoning_content": "想了很久但其实没输出"}}}}
	_, changed := h.retryRefusalNonStreamMap(nil, empty, func(nudged []byte) (map[string]any, error) {
		t.Error("空回答不该触发重试")
		return nil, nil
	})
	if changed || len(*audit) != 0 {
		t.Errorf("空回答不该改动也不该审计：changed=%v audit=%v", changed, *audit)
	}
}

// TestNonStreamRefusalRetryBytesVerbatimWhenUntouched 字节版：没换过 ⇒ 逐字节相同。
func TestNonStreamRefusalRetryBytesVerbatimWhenUntouched(t *testing.T) {
	h, _ := nsHandler(t, true)
	raw := []byte(`{"id":"x","choices":[{"message":{"content":"正常回答"}}]}`)
	out := h.retryRefusalNonStreamBytes(nil, raw, nil)
	if string(out) != string(raw) {
		t.Errorf("未命中时字节应当逐字一致:\n入=%s\n出=%s", raw, out)
	}
}

// TestNonStreamRefusalRetryBytesReplacesOnHit 字节版：命中 ⇒ 返回重发后的字节。
func TestNonStreamRefusalRetryBytesReplacesOnHit(t *testing.T) {
	h, _ := nsHandler(t, true)
	raw, _ := json.Marshal(nsResp("I can't help with that."))
	out := h.retryRefusalNonStreamBytes(nil, raw, func(nudged []byte) ([]byte, error) {
		return json.Marshal(nsResp("好的"))
	})
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("返回的不是合法 JSON: %v", err)
	}
	if nonStreamText(m) != "好的" {
		t.Errorf("应当换成重发后的字节，实际 %q", nonStreamText(m))
	}
}

// TestNonStreamRefusalRetryBytesKeepsRawWhenParseFails 解析不了 ⇒ 原样返回（绝不弄坏响应）。
func TestNonStreamRefusalRetryBytesKeepsRawWhenParseFails(t *testing.T) {
	h, _ := nsHandler(t, true)
	raw := []byte("这不是 JSON")
	if out := h.retryRefusalNonStreamBytes(nil, raw, nil); string(out) != string(raw) {
		t.Errorf("解析失败时必须原样返回，实际 %q", out)
	}
}

// errFakeRefetch 假的"重发失败"。
var errFakeRefetch = &fakeRefetchErr{}

type fakeRefetchErr struct{}

func (e *fakeRefetchErr) Error() string { return "fake refetch failure" }
