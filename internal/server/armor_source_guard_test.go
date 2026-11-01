package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/prompt"
)

// handsBodyToUpstream 这行是不是"把用户 body 交给上游"的调用。
//
// 只认**调用**形态（带左括号）；注释里的 `.Chat(` 会先被 stripComments 去掉。
var handsBodyToUpstream = regexp.MustCompile(`\.(Chat|ChatStream|ChatStreamWithIP)\(`)

// armorWhitelist 允许**不**经过 armorBody 的调用点（显式 + 理由，不留"默认放过"）。
var armorWhitelist = []struct{ pattern, why string }{
	{
		"Upstream.ChatStreamWithIP(",
		"单上游回落：body 进的是 upstream.Client，它内部 applyPrompt 已注入（client.go 调 ApplyArmor）；" +
			"二次注入由标记幂等兜住（client.go 的 applyPrompt 也是共享实现）",
	},
}

// TestAllProviderChatsAreArmored 源码级守卫：**每一条**把用户 body 交给上游的路径都必须过破甲。
//
// # 为什么用源码级而不是行为级
//
// 破甲失败的表现是"某条路径静默漏注入"，而三条路径的差异只在装配细节上 ——
// 行为测试要造三个假 Provider 才能覆盖，且将来**新加第四条**路径时它不会红。
// 源码级守卫正好补这一刀：枚举全部调用点（剥注释后），谁漏了就点名。
//
// # 白名单是刻意显式的
//
// 只允许"body 最终一定会被注入"的路径豁免（见 armorWhitelist）。
// 加新豁免必须写理由 —— 否则豁免表会慢慢变成"全都放过"。
func TestAllProviderChatsAreArmored(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked, armored := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue // 守卫不检查测试自身
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(stripComments(string(raw)), "\n") {
			line = trimInlineComment(line)
			if !handsBodyToUpstream.MatchString(line) {
				continue
			}
			checked++
			if strings.Contains(line, "armorBody(") {
				armored++
				continue
			}
			excused := false
			for _, w := range armorWhitelist {
				if strings.Contains(line, w.pattern) {
					excused = true
					break
				}
			}
			if !excused {
				t.Errorf("%s:%d 把 body 交给上游但没过破甲（也没在白名单里）：%s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if checked < 3 {
		t.Errorf("只扫到 %d 个调用点（预期 ≥3）—— 守卫的打点方式可能失效了", checked)
	}
	if armored < 3 {
		t.Errorf("只有 %d 处调用了 armorBody（预期 ≥3）", armored)
	}
}

// trimInlineComment 去掉**行尾**注释（整行注释由同包的 stripComments 处理，见
// autodisable_hardcredit_test.go —— 那里已经有一份，本文件复用它、不写第二份）。
//
// 不剥注释的后果实测过：本文件上方与 chattool_send.go 的注释里都出现过 `.Chat(`，
// 直接扫会把说明文字当成调用点，守卫要么误报、要么被"顺手加个豁免"腌掉。
func trimInlineComment(line string) string {
	if idx := strings.Index(line, "//"); idx >= 0 {
		return line[:idx]
	}
	return line
}

// TestArmorBodyGateOffIsZeroCopy Armor 为 nil ⇒ 原样返回**同一个切片**（零开销、零行为变化）。
func TestArmorBodyGateOffIsZeroCopy(t *testing.T) {
	h := NewHandler(Config{})
	body := []byte("{\"messages\":[{\"role\":\"system\",\"content\":\"S\"}]}")
	got := h.armorBody(body)
	if len(got) != len(body) || (len(body) > 0 && &got[0] != &body[0]) {
		t.Error("未接破甲时应当原样返回同一个切片（未启用功能不得改变任何字节）")
	}
	var _ = gateway.ChatStream{} // 保持与本包其它测试同风格的空引用（gateway 已在用）
}

// TestArmorBodyGateOnInjects 接了状态且 mode=append ⇒ 确实注入了。
func TestArmorBodyGateOnInjects(t *testing.T) {
	st := prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorAppend})
	h := NewHandler(Config{Armor: st})
	body := []byte("{\"messages\":[{\"role\":\"system\",\"content\":\"S\"}]}")
	out := h.armorBody(body)
	sys, ok := prompt.SystemText(out)
	if !ok || !strings.Contains(sys, "Little Code Sauce") {
		t.Errorf("mode=append 时应当注入破甲正文: %q", sys)
	}
}
