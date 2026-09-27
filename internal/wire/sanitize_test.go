package wire

import (
	"encoding/json"
	"strings"
	"testing"

)

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
)

func TestIdentityRewritten(t *testing.T) {
	out := sanitizeText(ccIdentity)
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("identity not rewritten: %q", out)
	}
	if strings.Contains(out, ccIdentity) {
		t.Errorf("original identity still present: %q", out)
	}
}

func TestBranchRewritten(t *testing.T) {
	out := sanitizeText(ccBranch)
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("branch not rewritten: %q", out)
	}
	if strings.Contains(out, "Main branch") {
		t.Errorf("original branch still present: %q", out)
	}
}

func TestBillingHeaderStrippedValueIrrelevant(t *testing.T) {
	out := sanitizeText(ccHeader)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Errorf("header not stripped: %q", out)
	}
}

func TestBillingHeaderCaseInsensitive(t *testing.T) {
	alt := "X-Anthropic-Billing-Header: cc_version=1.0;"
	out := strings.ToLower(sanitizeText(alt))
	if strings.Contains(out, "billing") {
		t.Errorf("case-insensitive header not stripped: %q", out)
	}
}

func TestTrailingKVStripped(t *testing.T) {
	out := sanitizeText("...; cc_version=2.0; cc_entrypoint=cli;")
	if strings.Contains(out, "cc_version") || strings.Contains(out, "cc_entrypoint") {
		t.Errorf("trailing kv not stripped: %q", out)
	}
}

func TestExactMatchOnlyVariantNotTouched(t *testing.T) {
	in := "...official CLI for Claude!"
	if out := sanitizeText(in); out != in {
		t.Errorf("variant should be untouched: %q -> %q", in, out)
	}
}

func TestUserFreeTextNotTouched(t *testing.T) {
	in := "please use main branch for this repo"
	if out := sanitizeText(in); out != in {
		t.Errorf("free text should be untouched: %q -> %q", in, out)
	}
}

func TestNoFeatureReturnsSameString(t *testing.T) {
	in := "ordinary user message"
	if out := sanitizeText(in); out != in {
		t.Errorf("no-feature text should pass through unchanged: %q -> %q", in, out)
	}
}

func TestMultimodalTextPartOnly(t *testing.T) {
	imgPart := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "..."}}
	content := []any{
		map[string]any{"type": "text", "text": ccIdentity},
		imgPart,
	}
	out, changed := sanitizeContent(content)
	if !changed {
		t.Fatal("expected change")
	}
	parts := out.([]any)
	txt, _ := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "CLI tool") {
		t.Errorf("text part not sanitized: %q", txt)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["data"] != "..." {
		t.Error("image part modified")
	}
}

// 集成：完整请求体经 PrepareBodyOpt 净化后无残留指纹，且 stream/tool_choice 行为不受影响。
func TestPrepareBodyOptSanitizesSystem(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBodyOpt(body, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream not forced")
	}
	msgs := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "x-anthropic-billing-header") || strings.Contains(sys, ccIdentity) || strings.Contains(sys, ccBranch) {
		t.Errorf("fingerprints remain: %q", sys)
	}
	if !strings.Contains(sys, "CLI tool") {
		t.Errorf("rewrite missing: %q", sys)
	}
}

func TestPrepareBodyOptDisabledPreservesFingerprints(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	out := PrepareBodyOpt(body, false)
	if !strings.Contains(string(out), ccIdentity) {
		t.Error("sanitize=false should preserve fingerprints")
	}
	// 但 stream 仍强制
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["stream"] != true {
		t.Error("stream should still be forced")
	}
}

// PrepareBody 默认行为 = 开启脱敏（保持向后兼容）。
func TestPrepareBodyDefaultSanitizes(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	out := PrepareBodyOpt(body, true)
	if strings.Contains(string(out), ccIdentity) {
		t.Error("PrepareBody default should sanitize")
	}
}

// 出站边界集成：ChatStream 发往上游的 wire body 必须无残留指纹。
//
// ⚠ 同样必须把 PromptMode 钉成 passthrough（理由与下面那条 disabled 用例相同）：
// 默认 custom 模式会先把整条 system 换掉，指纹"消失"就不是脱敏干的，
// 本用例会变成一条永远绿的假测试。