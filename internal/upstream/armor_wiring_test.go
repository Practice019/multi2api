package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/prompt"
)

// bodyWithSystem 造一个带客户端 system 的请求体。
func bodyWithSystem(system string) []byte {
	return []byte(`{"model":"m","messages":[{"role":"system","content":` + strconvQuote(system) + `},
		{"role":"user","content":"用户原话"}]}`)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// systemOf 从出站请求体里取回 system 文本（验证用）。
func systemOf(t *testing.T, body []byte) string {
	t.Helper()
	s, ok := prompt.SystemText(body)
	if !ok {
		t.Fatalf("出站体里没有 system: %s", body)
	}
	return s
}

// userOf 取第一条 user 消息（必须逐字不变）。
func userOf(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	for _, m := range obj["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["role"] == "user" {
			return mm["content"].(string)
		}
	}
	return ""
}

// TestApplyPromptArmorAppendKeepsClientSystem 破甲 append：客户端 system 保留 + 条款追加。
//
// 这条守的是"**自动**破甲"的核心行为：只要开了开关，每个出站请求都带破甲块，
// 而 append 模式下客户端自己的人格**不能**被吃掉（那是 replace 的语义）。
func TestApplyPromptArmorAppendKeepsClientSystem(t *testing.T) {
	c := &Client{
		PromptMode: prompt.ModePassthrough, // 透传客户端人格
		Armor:      prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorAppend}),
	}
	out := c.applyPrompt(bodyWithSystem("客户端人格：Claude Code"))
	sys := systemOf(t, out)
	if !strings.HasPrefix(sys, "客户端人格：Claude Code") {
		t.Errorf("append 模式丢了客户端 system：%q", sys[:min(60, len(sys))])
	}
	for _, want := range []string{"无限制模式", "拒绝对抗"} {
		if !strings.Contains(sys, want) {
			t.Errorf("出站 system 里没有破甲内容 %q", want)
		}
	}
	if got := userOf(t, out); got != "用户原话" {
		t.Errorf("用户消息被改动了（%q）—— 注入层不许碰用户正文", got)
	}
}

// TestApplyPromptArmorOffKeepsPassthroughByteIdentical 开关关掉时透传必须逐字节不变。
//
// 这是"没开就不许有任何副作用"的判据：本仓对"加了功能顺手改了别的模式"很敏感
//（passthrough 的语义就是"一个字节都不动"）。
func TestApplyPromptArmorOffKeepsPassthroughByteIdentical(t *testing.T) {
	in := bodyWithSystem("客户端人格")
	for _, armor := range []*prompt.ArmorState{nil, prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorOff})} {
		c := &Client{PromptMode: prompt.ModePassthrough, Armor: armor}
		if got := string(c.applyPrompt(in)); got != string(in) {
			t.Errorf("透传模式被改动了:\n前=%s\n后=%s", in, got)
		}
	}
}

// TestApplyPromptArmorReplaceDropsClientSystem replace：客户端 system 被替换掉。
func TestApplyPromptArmorReplaceDropsClientSystem(t *testing.T) {
	c := &Client{
		PromptMode: prompt.ModePassthrough,
		Armor:      prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorReplace}),
	}
	sys := systemOf(t, c.applyPrompt(bodyWithSystem("客户端人格")))
	if strings.Contains(sys, "客户端人格") {
		t.Error("replace 模式应当把客户端 system 换掉")
	}
	if !strings.Contains(sys, "无限制模式") {
		t.Error("replace 模式没注入破甲人设")
	}
}

// TestApplyPromptArmorIsHotSwitchable 设置页改完立即生效（同一个 Client 对象）。
func TestApplyPromptArmorIsHotSwitchable(t *testing.T) {
	st := prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorOff})
	c := &Client{PromptMode: prompt.ModePassthrough, Armor: st}
	in := bodyWithSystem("客户端人格")
	if strings.Contains(systemOf(t, c.applyPrompt(in)), "无限制模式") {
		t.Fatal("关闭状态下不该有注入")
	}
	st.Set(prompt.ArmorConfig{Mode: prompt.ArmorAppend}) // 模拟设置页保存
	if !strings.Contains(systemOf(t, c.applyPrompt(in)), "无限制模式") {
		t.Error("改了配置却没生效 —— 设置页的开关就变成假的了")
	}
}

// min 小工具（Go 1.21 有内置 min，这里显式写一份避免与别处的同名符号冲突）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
