// prompt_test.go 提示词改写的判据锁定。
//
// # 这一层的失败模式
//
// 全是静默的：改写没生效 → 指纹照旧发上游（用户看到偶发 400）；
// 改写过头 → 客户端的 user/assistant/tool 消息被动了（用户看到上下文丢失）。
// 两者都不会报错，所以每条边界都要有断言。
package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roles 提取 messages 的角色序列。
func roles(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages 不是数组: %v", obj["messages"])
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("msg 不是对象: %v", m)
		}
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

// TestRewriteReplacesAllSystemAndDeveloper 删掉**全部** system/developer，头部插一条。
//
// 钉住"全删"而不是"只删第一条"：客户端会塞多条 system
// （Claude Code 的 system + 一段 developer 补充），
// 只删第一条会让后面那些继续带着模板句漏网 —— 那正是本层要消灭的东西。
func TestRewriteReplacesAllSystemAndDeveloper(t *testing.T) {
	in := []byte(`{
		"model":"deepseek-v4-flash",
		"messages":[
			{"role":"system","content":"旧提示词 A"},
			{"role":"developer","content":"开发者指令 B"},
			{"role":"system","content":"旧提示词 C"},
			{"role":"user","content":"你好"}
		],
		"metadata":{"conversation_id":"c1"}
	}`)
	out := Rewrite(in, "我是自有提示词")

	got := roles(t, out)
	want := []string{"system", "user"}
	if len(got) != len(want) {
		t.Fatalf("roles=%v want %v（所有 system/developer 都应被删掉，只留头部一条）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roles=%v want %v", got, want)
		}
	}

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	first := obj["messages"].([]any)[0].(map[string]any)
	if first["content"] != "我是自有提示词" {
		t.Errorf("头部 system content=%v，期望自有提示词", first["content"])
	}
	// 三条旧消息的内容一个字都不能残留。
	for _, leak := range []string{"旧提示词 A", "开发者指令 B", "旧提示词 C"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("旧 system/developer 内容泄漏 %q: %s", leak, out)
		}
	}
}

// TestRewriteKeepsUserAssistantToolByteIdentical user/assistant/tool 逐字不动。
func TestRewriteKeepsUserAssistantToolByteIdentical(t *testing.T) {
	in := []byte(`{
		"messages":[
			{"role":"user","content":"u-content"},
			{"role":"assistant","content":"a-content","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"t1","content":"tool-result"}
		]
	}`)
	out := Rewrite(in, "SYS")

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("len=%d，期望 4（新 system + 原有三条）", len(msgs))
	}
	if msgs[1].(map[string]any)["content"] != "u-content" {
		t.Errorf("user content 被改动: %v", msgs[1])
	}
	if msgs[2].(map[string]any)["content"] != "a-content" {
		t.Errorf("assistant content 被改动: %v", msgs[2])
	}
	// tool_calls 必须整体保留：丢了它，后续 tool 结果消息就成了孤儿，
	// 上游会因为"找不到对应的 tool_call"报参数错误。
	tc, ok := msgs[2].(map[string]any)["tool_calls"].([]any)
	if !ok || len(tc) != 1 || tc[0].(map[string]any)["id"] != "c1" {
		t.Errorf("assistant.tool_calls 被改动: %v", msgs[2])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "t1" || toolMsg["content"] != "tool-result" {
		t.Errorf("tool 消息被改动: %v", toolMsg)
	}
}

// TestRewriteKeepsOtherTopLevelFields 顶层其他字段（含 metadata）不动。
//
// metadata 尤其重要：会话粘性靠它里面的 conversation_id 派生会话键。
// 丢了它，粘性路由会静默退化成"每次随机选号"。
func TestRewriteKeepsOtherTopLevelFields(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"stream":true,
		"metadata":{"conversation_id":"c1","user_id":"u9"},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out := Rewrite(in, "SYS")

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "glm-5.2" {
		t.Errorf("model 被改动: %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Errorf("stream 被改动: %v", obj["stream"])
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok || meta["conversation_id"] != "c1" || meta["user_id"] != "u9" {
		t.Errorf("metadata 被改动: %v", obj["metadata"])
	}
}

// TestRewriteInjectsSystemWhenAbsent 原本没有 system 时也要插一条。
//
// 这是 custom 模式的核心价值：无论客户端带不带 system，
// 出站 body 的形状都是**稳定的**（首条恒为我们的 system）。
func TestRewriteInjectsSystemWhenAbsent(t *testing.T) {
	out := Rewrite([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "SYS")
	got := roles(t, out)
	if len(got) != 2 || got[0] != "system" || got[1] != "user" {
		t.Fatalf("roles=%v want [system user]", got)
	}
}

// TestRewriteNoMessagesField 没有 messages 字段时插一条并保留其余字段。
func TestRewriteNoMessagesField(t *testing.T) {
	out := Rewrite([]byte(`{"model":"glm-5.2"}`), "SYS")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "glm-5.2" {
		t.Errorf("model 丢失: %v", obj)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("未插入 system: %v", obj["messages"])
	}
}

// TestRewriteMultimodalUserContentUntouched 多模态 user content 的内部结构不动。
func TestRewriteMultimodalUserContentUntouched(t *testing.T) {
	in := []byte(`{
		"messages":[
			{"role":"system","content":"old"},
			{"role":"user","content":[
				{"type":"text","text":"看图"},
				{"type":"image_url","image_url":{"url":"data:..."}}
			]}
		]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("len=%d", len(msgs))
	}
	arr, ok := msgs[1].(map[string]any)["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("多模态 content 被改动: %v", msgs[1].(map[string]any)["content"])
	}
	if arr[0].(map[string]any)["text"] != "看图" {
		t.Errorf("text part 被改动: %v", arr[0])
	}
}

// TestRewriteFailuresReturnInputVerbatim 所有失败路径都返回**入参本身**（同一份字节）。
//
// 钉住"解析失败不报错、不造错误"这条设计：本函数位于出站关键路径，
// 它的职责是优化而不是校验。若哪天改成"解析失败返回错误响应"，
// 会把一个上游能诊断的问题变成一个我们自己编的问题。
func TestRewriteFailuresReturnInputVerbatim(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		sys  string
	}{
		{"非法 JSON", []byte(`{not valid json`), "SYS"},
		{"空 body", []byte{}, "SYS"},
		{"空提示词", []byte(`{"messages":[{"role":"system","content":"old"}]}`), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Rewrite(c.body, c.sys)
			if string(out) != string(c.body) {
				t.Errorf("应原样返回入参：got %s want %s", out, c.body)
			}
			// 更强的一条：必须是**同一份底层数组**（没有偷偷重编码）。
			if len(c.body) > 0 && len(out) > 0 && &out[0] != &c.body[0] {
				t.Errorf("返回了新的切片而不是入参本身（重编码会重排键序、丢空白）")
			}
		})
	}
}

// ── Load ─────────────────────────────────────────────────────────────────

// TestLoadDefaultWhenFileEmpty 未配文件 → 内置默认，且非空。
func TestLoadDefaultWhenFileEmpty(t *testing.T) {
	got, err := Load(ModeCustom, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultPrompt {
		t.Errorf("应返回内置默认（len=%d vs %d）", len(got), len(defaultPrompt))
	}
	if strings.TrimSpace(got) == "" {
		t.Error("内置默认提示词为空 —— embed 漏了或文件是空的")
	}
	if got != Default() {
		t.Error("Load(\"\") 与 Default() 必须一致（同一份 embed）")
	}
}

// TestLoadFileOverride 文件非空且可读 → 整体替换内置默认。
func TestLoadFileOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "my.md")
	want := "这是我的自定义人格入口。"
	if err := os.WriteFile(fp, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ModeCustom, fp)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Load()=%q want %q", got, want)
	}
}

// TestLoadMissingFileFailsFast 路径非空但不可读 → **报错**，绝不静默回落。
//
// # 为什么这是硬判据
//
// 静默回落的后果是"网关一切正常，只是我的人格没了"：
// 用户改配置时路径打错一个字符，得到的是一个完全无信号的失败。
// 他会去查提示词内容、查上游、查缓存，而真因只是一个路径。
//
// fail fast 把这次失败暴露在唯一能立刻发现的位置 —— 启动。
func TestLoadMissingFileFailsFast(t *testing.T) {
	if _, err := Load(ModeCustom, filepath.Join(t.TempDir(), "不存在.md")); err == nil {
		t.Fatal("文件缺失必须返回 error（fail fast），不得静默回落内置默认")
	}
}

// TestLoadEmptyFileFailsFast 空文件同样报错。
//
// 空文件是一个"几乎必然是配置错误"的状态：用户显然想要一份提示词。
// 静默用它会让上游收到一条 content 为空的 system 消息 —— 一个新形态，
// 可能触发参数校验；而报错让用户在启动时就知道。
func TestLoadEmptyFileFailsFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(fp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(ModeCustom, fp); err == nil {
		t.Fatal("空提示词文件必须返回 error")
	}
}

// TestDegradedPromptHasNoFingerprint 降级提示词本身不能带任何已知指纹。
//
// 它是"最后一次机会"：如果它自己就含被拦的模板句，降级机制等于没做。
func TestDegradedPromptHasNoFingerprint(t *testing.T) {
	for _, fp := range []string{
		"You are Claude Code", "Anthropic's official CLI", "Main branch (",
		"Codex CLI", "github.com/anthropics/", "11128",
		"x-anthropic-billing-header", "cc_entrypoint=",
	} {
		if strings.Contains(Degraded, fp) {
			t.Errorf("降级提示词含指纹 %q —— 它自己就会触发拦截", fp)
		}
	}
	if strings.TrimSpace(Degraded) == "" {
		t.Error("降级提示词为空")
	}
}
