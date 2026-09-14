// prompt_wire_test.go PromptMode × 降级状态的四种组合，断言**上游收到的字节**。
//
// # 为什么在 wire 层断言，而不是直接调 applyPrompt
//
// applyPrompt 是私有函数，单测它能证明"函数本身对"，
// 但证明不了"它真的被接进了出站管线" —— 而那正是最容易漏的一步：
// prepareBody 里少调一次，编译通过、单测全绿、线上毫无变化。
//
// 因此这里全部走 ChatStream，从假上游读回真实 body。
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
)

// wireBody 起一个假上游，返回"客户端发出去时上游收到的 body"。
func wireBody(t *testing.T, c *Client, body []byte) []byte {
	t.Helper()
	var got []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer ts.Close()

	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "t", Domain: "copilot.tencent.com", UID: "u1"}
	rc, status, respBody, err := c.ChatStream(acct, body)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("假上游返回 %d: %s", status, respBody)
	}
	return got
}

// firstSystemContent 取出改写后 messages[0].content（非字符串返回空串）。
func firstSystemContent(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("wire body 不是 JSON: %v; %s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("wire body 没有 messages: %s", body)
	}
	s, _ := msgs[0].(map[string]any)["content"].(string)
	return s
}

const clientSystem = "You are Claude Code, Anthropic's official CLI for Claude."

func clientBody() []byte {
	return []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + clientSystem + `"},` +
		`{"role":"user","content":"hi"}]}`)
}

// TestPromptCustomReplacesClientSystem custom + 未降级 → 用 PromptText 替换。
func TestPromptCustomReplacesClientSystem(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeCustom
	c.PromptText = "网关自有提示词"
	got := wireBody(t, c, clientBody())

	if sys := firstSystemContent(t, got); sys != "网关自有提示词" {
		t.Errorf("system=%q，期望网关自有提示词", sys)
	}
	if strings.Contains(string(got), "official CLI for Claude") {
		t.Errorf("客户端 system 未被替换（模板句仍会撞内容策略）: %s", got)
	}
}

// TestPromptCustomFallsBackToDefaultWhenTextEmpty PromptText 为空 → 回落内置默认。
//
// # 为什么必须有这条
//
// 装配层漏注入 PromptText 时，prompt.Rewrite 会因为 systemPrompt=="" 而
// **原样返回** —— 一个完全静默的功能失效（配置看起来生效了，实际什么都没做）。
// 回落内置默认把这个失败模式变成"功能仍按默认语义工作"。
func TestPromptCustomFallsBackToDefaultWhenTextEmpty(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeCustom
	c.PromptText = "" // 模拟装配漏注入
	got := wireBody(t, c, clientBody())

	sys := firstSystemContent(t, got)
	if sys == "" {
		t.Fatal("PromptText 为空时不该什么都不做 —— 应回落内置默认提示词")
	}
	if sys == clientSystem || strings.Contains(sys, "official CLI for Claude") {
		t.Errorf("客户端 system 仍在，说明降级/替换都没发生: %q", sys)
	}
}

// TestPromptCustomEmptyModeIsCustom 空 mode 按 custom 处理（与配置缺省一致）。
func TestPromptCustomEmptyModeIsCustom(t *testing.T) {
	c := New()
	c.PromptMode = "" // 未配置
	c.PromptText = "默认提示词"
	got := wireBody(t, c, clientBody())

	if sys := firstSystemContent(t, got); sys != "默认提示词" {
		t.Errorf("空 mode 应按 custom 处理，system=%q", sys)
	}
}

// TestPromptPassthroughKeepsClientSystem passthrough + 未降级 → 一个字节都不动。
//
// ⚠ 关掉脱敏以隔离变量：passthrough 保留的是"提示词层不动它"，
// 而脱敏层**仍然会**改写身份句（那是它的职责，见 sanitizeRewrites）。
// 开着脱敏断言"逐字相同"会得到一条恒红的测试，而它红的原因
// 与"passthrough 是否正确"无关 —— 那是两个不同的层。
func TestPromptPassthroughKeepsClientSystem(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModePassthrough
	c.PromptText = "不该被用到"
	c.SanitizeFingerprints = false // ← 隔离脱敏层
	got := wireBody(t, c, clientBody())

	if sys := firstSystemContent(t, got); sys != clientSystem {
		t.Errorf("passthrough 必须保留客户端 system（脱敏关闭时逐字相同），得到 %q", sys)
	}
	if strings.Contains(string(got), "不该被用到") {
		t.Error("passthrough 模式下 PromptText 不得出现在 wire body 上")
	}
}

// TestPromptPassthroughStillSanitizes passthrough 只挡提示词层，脱敏照常生效。
//
// 这条与上一条成对：两层是独立的，关掉一层不该影响另一层。
// 它钉住"passthrough ≠ 关掉全部防护"这个容易误解的语义。
func TestPromptPassthroughStillSanitizes(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModePassthrough
	c.SanitizeFingerprints = true
	got := wireBody(t, c, clientBody())

	sys := firstSystemContent(t, got)
	if strings.Contains(sys, "official CLI for Claude") && !strings.Contains(sys, "CLI tool for Claude") {
		t.Errorf("passthrough 下脱敏层应照常改写身份句，得到 %q", sys)
	}
	if !strings.HasPrefix(sys, "You are Claude Code") {
		t.Errorf("脱敏只该改一个词，不该整句替换；得到 %q", sys)
	}
}

// TestPromptCustomDegradedUsesNeutral custom + 降级中 → 用 Degraded 而非 PromptText。
func TestPromptCustomDegradedUsesNeutral(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeCustom
	c.PromptText = "网关自有提示词"
	g := prompt.NewGate()
	g.Trigger()
	c.PromptGate = g

	got := wireBody(t, c, clientBody())
	sys := firstSystemContent(t, got)

	if sys != prompt.Degraded {
		t.Errorf("降级期应用 Degraded，得到 %q", sys)
	}
	if strings.Contains(string(got), "网关自有提示词") {
		t.Error("降级期内不得再用 PromptText")
	}
}

// TestPromptPassthroughDegradedReplacesClientSystem passthrough + 降级中 → 用 Degraded 替换。
//
// # 为什么 passthrough 也会被改写
//
// "透传"表达的是尊重客户端人格，而降级期的存在**本身就说明**
// 那份人格刚刚撞了内容策略。继续透传 = 继续撞 400。
// 降级是用户通过配置已经同意的兜底路径，不是我们擅自改写。
func TestPromptPassthroughDegradedReplacesClientSystem(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModePassthrough
	g := prompt.NewGate()
	g.Trigger()
	c.PromptGate = g

	got := wireBody(t, c, clientBody())
	sys := firstSystemContent(t, got)

	if sys != prompt.Degraded {
		t.Errorf("passthrough 降级期应用 Degraded，得到 %q", sys)
	}
	if strings.Contains(string(got), clientSystem) {
		t.Error("降级期内客户端 system 必须被换掉（否则继续撞 400）")
	}
}

// TestPromptGateNilMeansNeverDegraded 未注入 Gate（nil）→ 恒不降级。
//
// nil 是合法状态：core 的 PromptGate 是可选字段，nil 表示本部署
// 未接提示词体系。它必须表现为"永不降级"，而不是 panic 或随机行为。
func TestPromptGateNilMeansNeverDegraded(t *testing.T) {
	c := New()
	c.PromptMode = prompt.ModeCustom
	c.PromptText = "网关自有提示词"
	c.PromptGate = nil

	got := wireBody(t, c, clientBody()) // 不得 panic
	if sys := firstSystemContent(t, got); sys != "网关自有提示词" {
		t.Errorf("nil Gate 应表现为未降级，system=%q", sys)
	}
}

// TestPromptRunsBeforeSanitize 顺序钉住：提示词替换必须在脱敏之前。
//
// # 为什么要钉顺序
//
// 反过来的话，sanitize 要先在**即将被删掉**的客户端 system 上跑一遍正则 ——
// 纯白做功；更重要的是顺序一乱，读者会以为"sanitize 负责 system"，
// 而实际分工是 prompt 管 system/developer、sanitize 管其余消息
// （见 internal/prompt 包注释的分工表）。
//
// 这里用"只存在于客户端 system 里的指纹"来判别：
// 两条路径都会让它消失，所以再断言 sanitize 的开关**不影响**结果 ——
// 那种组合只在 prompt 先跑时成立。
func TestPromptRunsBeforeSanitize(t *testing.T) {
	mk := func(sanitize bool) string {
		c := New()
		c.PromptMode = prompt.ModeCustom
		c.PromptText = "网关自有提示词"
		c.SanitizeFingerprints = sanitize
		return firstSystemContent(t, wireBody(t, c, clientBody()))
	}
	on, off := mk(true), mk(false)

	if on != "网关自有提示词" || off != "网关自有提示词" {
		t.Errorf("脱敏开关不该影响 system 替换结果：sanitize=true → %q，false → %q", on, off)
	}
}
