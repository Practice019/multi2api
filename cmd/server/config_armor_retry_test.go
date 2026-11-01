package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadArmorCfg 写一份最小配置并 Load（失败即 t.Fatal）。
func loadArmorCfg(t *testing.T, body string) *Config {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// TestArmorRetryAdminReadBack 设置页保存的那份（admin 段）必须在启动时被读回。
//
// 这条就是实测 bug 的回归：以前 PromptArmorRetry **只有声明、没有解析**，
// 于是 PUT 返回 200、config.json 里也有值，**重启后静默复位成 false**（mode 活得好好的）。
func TestArmorRetryAdminReadBack(t *testing.T) {
	c := loadArmorCfg(t, "{\"listen\":\":9999\",\"api_key\":\"k\",\"admin\":{\"prompt_armor_retry\":true}}")
	if !c.PromptArmorRetry {
		t.Error("admin.prompt_armor_retry=true 没有被读回（这就是重启复位那个 bug）")
	}
}

// TestArmorRetryHandwrittenFallback 手写 prompt.armor_retry 仍然生效（老配置与文档路径不能被打破）。
func TestArmorRetryHandwrittenFallback(t *testing.T) {
	c := loadArmorCfg(t, "{\"listen\":\":9999\",\"api_key\":\"k\",\"prompt\":{\"armor_retry\":true}}")
	if !c.PromptArmorRetry {
		t.Error("手写 prompt.armor_retry=true 没生效")
	}
}

// TestArmorRetryDefaultsOff 都没配 ⇒ 关（与原项目一致）。
func TestArmorRetryDefaultsOff(t *testing.T) {
	c := loadArmorCfg(t, "{\"listen\":\":9999\",\"api_key\":\"k\"}")
	if c.PromptArmorRetry {
		t.Error("没配任何值时应当是关的")
	}
}

// TestArmorRetryAdminExplicitFalseBeatsHandwrittenTrue 界面上关掉必须压过手写的 true。
//
// 这是把 admin 那份做成 *bool 的**唯一理由**：普通 bool 加 a || b 合成时，
// 用户关掉开关（显式 false）会被手写位置的 true 顶回来 —— 界面显示关、行为却是开。
func TestArmorRetryAdminExplicitFalseBeatsHandwrittenTrue(t *testing.T) {
	c := loadArmorCfg(t, "{\"listen\":\":9999\",\"api_key\":\"k\","+
		"\"admin\":{\"prompt_armor_retry\":false},\"prompt\":{\"armor_retry\":true}}")
	if c.PromptArmorRetry {
		t.Error("admin 段显式 false 必须压过手写的 true（否则界面关了、行为还在开）")
	}
}

// armorSettingKeysFromAdapter 从适配器源码里抠出所有 prompt_armor_* 设置键。
//
// 刻意不用正则：这里只想拿「keyArmorX = "prompt_armor_..."」这一行的字面量，
// 朴素的字符串扫描足够，而且不会因为我写错转义而假绿（它自己会先 t.Fatal）。
func armorSettingKeysFromAdapter(t *testing.T, src string) []string {
	t.Helper()
	var keys []string
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		// 注意：常量声明在源码里是 `const keyArmorMode = "prompt_armor_mode"`，
		// 所以判据是"这行含 keyArmor"，不是 HasPrefix（第一版写成 HasPrefix ⇒ 一个键都抠不到，
		// 而那条 t.Fatal 让它变成"守卫失效"的红，而不是假绿）。
		if !strings.Contains(line, "keyArmor") {
			continue
		}
		at := strings.Index(line, "\"prompt_armor_")
		if at < 0 {
			continue
		}
		rest := line[at+1:]
		if end := strings.Index(rest, "\""); end > 0 {
			keys = append(keys, rest[:end])
		}
	}
	if len(keys) == 0 {
		t.Fatal("没从适配器源码里抠出任何 prompt_armor_* 键 —— 守卫失效了（键常量写法变了？）")
	}
	return keys
}

// TestEveryArmorSettingKeyIsResolvedInNormalize 源码级守卫：适配器能写的破甲键，normalize 必须解析。
//
// 这一族在本仓已中三次（monkeycode 出口、prompt_armor_mode、prompt_armor_retry）⇒
// 每次都是「设置页能写、启动不读、重启静默回退」。守卫的作用是：将来再加一个键而忘了解析，
// **测试变红**，而不是让用户在一次重启之后才发现开关自己回去了。
func TestEveryArmorSettingKeyIsResolvedInNormalize(t *testing.T) {
	adapterSrc, err := os.ReadFile("upstream_settings_armor.go")
	if err != nil {
		t.Fatal(err)
	}
	keys := armorSettingKeysFromAdapter(t, string(adapterSrc))
	cfgSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	// 复用仓库既有的 funcBody（大括号配对取函数体，见 refresh_ratio_test.go）。
	body, ok := funcBody(string(cfgSrc), "func (c *Config) normalize() error {")
	if !ok {
		t.Fatal("取不到 normalize() 的函数体 —— 守卫无法工作")
	}
	// 先剥掉行注释再做包含判断 —— 变异验证暴露的漏洞：
	//   **注释掉的** `// c.X = ...` 同样包含这个子串 ⇒ 守卫假绿。
	//   守卫的全部价值就是"解析行被删或被停用时变红"，所以必须先剥注释。
	body = stripLineComments(body)
	for _, key := range keys {
		field := "Prompt" + camelKey(strings.TrimPrefix(key, "prompt_"))
		if !strings.Contains(body, "c."+field+" =") {
			t.Errorf("设置项 %q 在 normalize() 里没有解析赋值 c.%s = ... —— "+
				"这正是「写进去、重启读不回」那一族（本仓已中三次）", key, field)
		}
	}
}

// stripLineComments 去掉行注释（含行尾注释）。
//
// 局限：会把字符串字面量里的 `//` 也当注释起点 —— 对 normalize() 这段代码可接受，
// 因为守卫只关心赋值行是否还在。
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if at := strings.Index(line, "//"); at >= 0 {
			lines[i] = line[:at]
		}
	}
	return strings.Join(lines, "\n")
}

// camelKey 把 snake_case 转 CamelCase（armor_retry → ArmorRetry）。
func camelKey(s string) string {
	var b strings.Builder
	for _, p := range strings.Split(s, "_") {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}
