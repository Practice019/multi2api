// provider_alias_test.go 钉住「不泄露上游身份」。
//
// # 用户的要求
//
// 「owned_by 我不希望让别人知道上游」
//
// 实测能从公开接口看到的指纹有 5 类（见 provider_alias.go 的注释）。
// 这里逐条钉住，防止将来改动把某一条又漏回去 ——
// 泄露是"看起来一切正常"的失效：没有报错、功能也对，
// 只是把后端是谁告诉了客户。
package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAliasForMapsKnownProvider 配了映射的上游必须返回代号。
func TestAliasForMapsKnownProvider(t *testing.T) {
	h := &Handler{cfg: Config{ProviderAlias: map[string]string{
		"workbuddy":      "ch-a",
		"workbuddy-intl": "ch-b",
	}}}
	if got := h.aliasFor("workbuddy-intl"); got != "ch-b" {
		t.Errorf("aliasFor(workbuddy-intl) = %q want ch-b", got)
	}
}

// TestAliasForUnlistedNeverLeaksRealName 漏配的上游**不能**回落成真名。
//
// # 为什么这条最重要
//
// 回落成真名会让「我已经隐藏了」的假设在**新增上游时静默失效** ——
// 而失效的表现是"别人的名字开始出现在我的响应里"，很难发现。
// 所以漏配必须表现为一个中性的、稳定的代号。
func TestAliasForUnlistedNeverLeaksRealName(t *testing.T) {
	h := &Handler{cfg: Config{ProviderAlias: map[string]string{"workbuddy": "ch-a"}}}
	got := h.aliasFor("brand-new-upstream")
	if got == "brand-new-upstream" {
		t.Fatal("★ 漏配的上游回落成了真名 —— 新增上游时会静默泄露")
	}
	if got == "" {
		t.Fatal("漏配的上游应当得到一个中性代号，而不是空")
	}
	// 稳定性：同一个输入必须得到同一个代号（否则客户端模型索引每次重启都失效）。
	if again := h.aliasFor("brand-new-upstream"); again != got {
		t.Errorf("代号不稳定: %q vs %q", got, again)
	}
}

// TestAliasNilMeansNoHiding 不配映射时行为不变（向后兼容）。
func TestAliasNilMeansNoHiding(t *testing.T) {
	h := &Handler{cfg: Config{}}
	if got := h.aliasFor("workbuddy"); got != "workbuddy" {
		t.Errorf("未配映射时应原样返回，得到 %q", got)
	}
}

// TestScrubChatResponseRemovesFingerprints 响应脱敏要抹掉 model/id/私有 usage。
func TestScrubChatResponseRemovesFingerprints(t *testing.T) {
	raw := []byte(`{
  "id": "chatcmpl-abc123",
  "model": "monkeycode-basic/deepseek-flash",
  "choices": [{"index":0,"message":{"role":"assistant","content":"OK"}}],
  "usage": {"prompt_tokens": 81, "completion_tokens": 2, "total_tokens": 83, "points_consumed": 1, "credit": 0}
}`)

	out := scrubChatResponse(raw, "005/deepseek-flash")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("脱敏后不是合法 JSON: %v", err)
	}

	// 3) model 回填客户端请求用的名字。
	if obj["model"] != "005/deepseek-flash" {
		t.Errorf("model 应当回填公开名，得到 %v", obj["model"])
	}
	// 4) id 前缀抹掉。
	id, _ := obj["id"].(string)
	if id == "chatcmpl-abc123" {
		t.Error("★ 上游 id 原样透传 —— chatcmpl- 前缀是 OpenAI 系上游的指纹")
	}
	// 5) 私有 usage 字段删掉，标准字段保留。
	u := obj["usage"].(map[string]any)
	for k := range privateUsageKeys {
		if _, has := u[k]; has {
			t.Errorf("★ 私有 usage 字段 %q 没删 —— 它是上游指纹（loomy 的 points_consumed）", k)
		}
	}
	if u["prompt_tokens"] == nil || u["total_tokens"] == nil {
		t.Error("标准 usage 字段必须保留 —— 删错会让计费失准，比泄露更严重")
	}
	// choices 不能动。
	if obj["choices"] == nil {
		t.Error("choices 必须保留")
	}
}

// TestScrubKeepsMalformedAsIs 畸形响应原样返回（脱敏是增强，不能吞真相）。
func TestScrubKeepsMalformedAsIs(t *testing.T) {
	raw := []byte("not json at all")
	if got := scrubChatResponse(raw, "x"); string(got) != string(raw) {
		t.Error("畸形响应必须原样返回 —— 否则排障时看不到上游真实返回")
	}
}

// TestScrubSSELinesHandlesSplitFrames SSE 帧被拆开时仍要脱敏。
//
// # 为什么这条必须测
//
// 一帧可能被拆成两次 Read。若按块脱敏，两块都解析不成 JSON，
// 于是静默不脱敏 —— 表现是「有时泄露有时不漏」，取决于网络切片。
func TestScrubSSELinesHandlesSplitFrames(t *testing.T) {
	h := &Handler{cfg: Config{}}
	full := []byte("data: {\"model\":\"upstream-real\",\"id\":\"chatcmpl-x\"}\n\n")
	// 从中间切开。
	cut := len(full) / 2
	out1, rest := h.scrubSSELines(full[:cut], "pub", true)
	if len(out1) != 0 {
		t.Error("未构成完整行时不该输出任何字节")
	}
	out2, _ := h.scrubSSELines(append(rest, full[cut:]...), "pub", true)
	joined := string(out1) + string(out2)
	if !strings.Contains(joined, "pub") {
		t.Errorf("拆帧后没脱敏，输出: %q", joined)
	}
	if strings.Contains(joined, "upstream-real") {
		t.Error("★ 拆帧后上游真名泄露了")
	}
}
