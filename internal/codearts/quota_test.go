package codearts

import (
	"strings"
	"testing"
	"time"
)

// TestDetectQuotaExhaustedRecognizesBenefitError 锁定 benefit 免费额度耗尽的识别。
//
// 这是实测的完整错误体（HTTP 200 + 流内业务错误）：
//
//	{"error_code":"InferHub.4291.200","error_msg":"insufficient quota",
//	 "details":[{"error_msg":"modelId: glm-5.3-flash"}, ...]}
//
// 关键：它不是 HTTP 错误码，而是塞在 SSE 流里的业务错误 ——
// 所以必须按**响应体内容**识别，不能只看 status。
func TestDetectQuotaExhaustedRecognizesBenefitError(t *testing.T) {
	real := `data:{"error_code":"InferHub.4291.200","error_msg":"insufficient quota",` +
		`"details":[{"error_code":"InferHub.4291.200","error_msg":"requestId: 58b9e0"},` +
		`{"error_code":"InferHub.4291.200","error_msg":"modelId: glm-5.3-flash"},` +
		`{"error_code":"InferHub.4291.200","error_msg":"traceId: 021f44"}]}`

	reason, ok := DetectQuotaExhausted(real)
	if !ok {
		t.Fatal("应识别为额度耗尽")
	}
	if !strings.Contains(reason, "免费额度") {
		t.Errorf("原因应说明是免费额度，实际 %q", reason)
	}
}

// TestDetectQuotaExhaustedIgnoresUnrelatedErrors 确认不误判其他错误。
//
// 误标的代价很大：用户会以为模型不可用而错过它，
// 而真正的原因（并发上限、参数问题、网络抖动）是暂时或可修的。
func TestDetectQuotaExhaustedIgnoresUnrelatedErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"并发上限", `{"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)"}`},
		{"参数非法", `{"error_code":"InferHub.001001005.400","error_msg":"The request param is invalid"}`},
		{"模型未注册", `{"error_code":"InferHub.002002009.404","error_msg":"The model is not registered"}`},
		{"正常内容", `data:{"choices":[{"delta":{"content":"你好"}}]}`},
		{"空", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if reason, ok := DetectQuotaExhausted(c.body); ok {
				t.Errorf("不应识别为额度耗尽，实际 reason=%q", reason)
			}
		})
	}
}

// TestMarkAndClearQuota 锁定标记的写入与清除语义。
//
// 清除能生效很重要：额度可能是周期性恢复的（如每月重置），
// 一旦某次调用成功就说明它现在可用，旧标记必须失效，
// 否则用户会一直看到"额度耗尽"而实际上早就能用了。
func TestMarkAndClearQuota(t *testing.T) {
	const model = "test-model-quota"

	MarkQuotaExhausted(model, "insufficient quota（免费额度已耗尽）")
	states, _ := QuotaStates()
	s, ok := states[model]
	if !ok || !s.Exhausted {
		t.Fatalf("标记后应显示额度耗尽，实际 %+v", s)
	}

	ClearQuota(model)
	states, _ = QuotaStates()
	if s, ok := states[model]; ok && s.Exhausted {
		t.Error("ClearQuota 后不应仍是耗尽状态")
	}
}

// TestQuotaStatesStaleFlag 确认 TTL 过期会被标记为 stale。
func TestQuotaStatesStaleFlag(t *testing.T) {
	old := QuotaTTL()
	defer SetQuotaTTL(old)

	SetQuotaTTL(time.Hour)
	const m = "test-model-ttl"
	MarkQuotaExhausted(m, "x")

	if _, stale := QuotaStates(); stale {
		t.Error("刚标记过的结果不应是 stale")
	}

	// 把 TTL 缩到极小，模拟过期
	SetQuotaTTL(time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	if _, stale := QuotaStates(); !stale {
		t.Error("TTL 过期后应标记为 stale（调用方需重新探测）")
	}
}

// TestMarkQuotaEmptyModelIgnored 确认空模型名不会污染缓存。
func TestMarkQuotaEmptyModelIgnored(t *testing.T) {
	before, _ := QuotaStates()
	MarkQuotaExhausted("", "x")
	after, _ := QuotaStates()
	if len(after) != len(before) {
		t.Error("空模型名不应写入缓存")
	}
}

// TestBenefitModelsAreExactlyTheThree 锁定"哪三个模型走 benefit 通道"。
//
// 这三个正是免费额度耗尽的受害者；通道判定若错了，
// 额度探测就会打到错误通道上，得到假结果。
func TestBenefitModelsAreExactlyTheThree(t *testing.T) {
	wantBenefit := []string{
		"deepseek-v4-flash-0731",
		"deepseek-v4-pro-0813",
		"glm-5.3-flash",
	}
	for _, id := range wantBenefit {
		if got := ChannelFor(id); got != ChannelBenefit {
			t.Errorf("%s 应在 benefit 通道，实际 %v", id, got)
		}
	}
}
