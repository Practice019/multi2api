package main

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/prompt"
)

// TestArmorModeReadBackFromAdminSection 设置页写的值必须**在启动时被读回**。
//
// # 这条是怎么来的（同一族 bug 在本仓踩过两次）
//
// 设置页保存走的是 admin 段（managed 位置），而手写配置在 prompt.armor_mode。
// 只在其中一处读写 ⇒ 用户看到的形态是：设置页显示 append、PUT 成功、config.json 里也有
// → **重启后回到 off**。MonkeyCode 的出口（wave32）中过一次，破甲这一轮又中了一次 ——
// 两次都是靠"重启后再读一遍"才发现的。所以把它钉成测试。
func TestArmorModeReadBackFromAdminSection(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	// 只写 admin 段（设置页的位置），**不写** prompt.armor_mode。
	body := "{\"listen\":\":9999\",\"api_key\":\"k\",\"admin\":{\"prompt_armor_mode\":\"append\"}}"
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptArmorMode != prompt.ArmorAppend {
		t.Errorf("PromptArmorMode = %q，want %q —— 设置页保存的值在重启后没被读回（实测到的那个 bug）",
			c.PromptArmorMode, prompt.ArmorAppend)
	}
}

// TestArmorModeNestedIsFallback 手写的嵌套键仍然生效，且两边都有时 **admin 段优先**。
//
// 反了的后果：手写一个旧值会永久压住设置页，用户在界面上怎么改都不生效。
//
// ⚠ 本用例 2026-11-13（T-902）改过一次取值：原来用 "replace" 当"嵌套键的值"，
//
//	而 replace 已从界面下线、会被 normalize 归一成 append（见
//	TestArmorReplaceLegacyNormalizedToAppend）—— 它不再是"一个普通档位"，
//	所以这里换成 "append" 来验同一条语义（fallback 与 admin 优先都没变）。
func TestArmorModeNestedIsFallback(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	write := func(s string) {
		if err := os.WriteFile(fp, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("{\"listen\":\":9999\",\"api_key\":\"k\",\"prompt\":{\"armor_mode\":\"append\"}}")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptArmorMode != prompt.ArmorAppend {
		t.Errorf("只有嵌套键时 PromptArmorMode = %q，want %q", c.PromptArmorMode, prompt.ArmorAppend)
	}

	write("{\"listen\":\":9999\",\"api_key\":\"k\",\"admin\":{\"prompt_armor_mode\":\"off\"},\"prompt\":{\"armor_mode\":\"append\"}}")
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.PromptArmorMode != prompt.ArmorOff {
		t.Errorf("两边都有时应当取 admin 段（off），得到 %q", c2.PromptArmorMode)
	}
}

// TestArmorModeInvalidRejected 非法值**启动报错**（不静默当 off）。
func TestArmorModeInvalidRejected(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := "{\"listen\":\":9999\",\"api_key\":\"k\",\"prompt\":{\"armor_mode\":\"on\"}}"
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fp); err == nil {
		t.Error("非法 armor_mode 应当让启动失败（静默当 off 会让用户以为配好了）")
	}
}
