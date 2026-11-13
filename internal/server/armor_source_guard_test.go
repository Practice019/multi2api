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
		src := stripComments(string(raw))
		sealedByFunc := sealedVarsByFunc(src)
		funcIdx := -1
		for i, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(line, "func ") {
				funcIdx++
			}
			line = trimInlineComment(line)
			if !handsBodyToUpstream.MatchString(line) {
				continue
			}
			checked++
			// ① 内联形态（旧写法）：这一行自己就调了破甲。
			if strings.Contains(line, "armorBody(") || strings.Contains(line, "ApplyArmorToBody") {
				armored++
				continue
			}
			// ② 变量形态（2026-11-13 三态改造后的写法）：递出去的 body 变量由**同一个函数**
			//    里的破甲决策产生 ⇒ 仍然是"过了破甲"。
			if id := lastCallArgIdent(line); id != "" && sealedByFunc[funcIdx][id] {
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
				t.Errorf("%s:%d 把 body 交给上游但没过破甲决策（也没在白名单里）：%s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if checked < 3 {
		t.Errorf("只扫到 %d 个调用点（预期 ≥3）—— 守卫的打点方式可能失效了", checked)
	}
	if armored < 3 {
		t.Errorf("只有 %d 处交给上游的 body 来自破甲决策（预期 ≥3）", armored)
	}
}

// sealedVarsByFunc 按**函数分组**收集“由破甲决策产生的变量名”。
//
// 为什么按函数而不是按文件：同文件里 A 函数判过、B 函数直接把裸 body 递出去，
// 按文件判会放过 B —— 而 B 正是这条守卫要抓的那条“漏掉的路径”。
func sealedVarsByFunc(src string) map[int]map[string]bool {
	out := map[int]map[string]bool{}
	idx := -1
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "func ") {
			idx++
			out[idx] = map[string]bool{}
		}
		if idx < 0 {
			continue
		}
		// 三种决策入口（见 armor_body.go）：armorBody 返两个值、armorSealed/armorCheck 一个。
		for _, m := range []string{"= h.armorBody(", "= h.armorCheck(", "= h.armorSealed("} {
			at := strings.Index(line, m)
			if at < 0 {
				continue
			}
			if name := firstIdentOf(line[:at]); name != "" {
				out[idx][name] = true
			}
		}
	}
	return out
}

// firstIdentOf 取赋值号左边**第一个**目标名（= 递出去的 body）。
//
// 为什么是第一个而不是最后一个：
//
//	sealed, rej := h.armorBody(...)   多返回值 ⇒ 第一个才是 body（第二个是拒绝）
//	rc := h.armorSealed(...)          单返回值 ⇒ 只有它
//	out, rej = h.armorBody(...)        同上
//
// 取错一个会怎样：把 rej 记成「已盖章的 body」，于是 `Chat(ctx, cred, rej)` 这种写错的
// 调用也会被判为「过了破甲」—— 守卫变成装饰品（fail-open）。
func firstIdentOf(s string) string {
	s = strings.TrimRight(s, " \t:")
	for _, part := range strings.Split(s, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		for i := 0; i < len(id); i++ {
			if !isIdentByte(id[i]) {
				return ""
			}
		}
		return id
	}
	return ""
}

// identBeforeUnused 保留旧名以防其它文件引用（本包内已无调用）。
func identBeforeUnused(s string) string {
	s = strings.TrimRight(s, " \t")
	s = strings.TrimRight(s, ", ")
	end := len(s)
	for end > 0 {
		if isIdentByte(s[end-1]) {
			end--
			continue
		}
		break
	}
	return s[end:]
}

// isIdentByte 该字节能不能出现在 Go 标识符里。
//
// 不用字符字面量（rune literal）而是查表：本文件里到处是字符串边界，
// 少一种引号就少一类“改一处崩另一处”的写法。
func isIdentByte(c byte) bool {
	return (c >= 0x61 && c <= 0x7a) || (c >= 0x41 && c <= 0x5a) || (c >= 0x30 && c <= 0x39) || c == 0x5f
}

// lastCallArgIdent 取这次调用**最后一个参数**的标识符（= 递给上游的那个 body）。
//
// 取最后一个参数而不是“找名字里带 body 的那个”：三处调用点的参数表不同
// （有的带 ctx/cred，有的只有 acct+body），而 body 恒在最后。
// 拿不到裸标识符（例如表达式、函数调用）时返回空串 —— 交给白名单判定，
// 不假装通过（fail-open 的守卫比没有守卫更坏）。
func lastCallArgIdent(line string) string {
	i := strings.Index(line, "(")
	j := strings.LastIndex(line, ")")
	if i < 0 || j <= i {
		return ""
	}
	args := line[i+1 : j]
	if k := strings.LastIndex(args, ","); k >= 0 {
		args = args[k+1:]
	}
	id := strings.TrimSpace(args)
	if id == "" || strings.ContainsAny(id, " ()") {
		return ""
	}
	return id
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
	got, rej := h.armorBody(prompt.Target{}, body)
	if rej != nil {
		t.Fatalf("未接破甲时不该产生拦截: %v", rej)
	}
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
	out, rej := h.armorBody(prompt.Target{}, body)
	if rej != nil {
		t.Fatalf("append 态不该产生拦截: %v", rej)
	}
	sys, ok := prompt.SystemText(out)
	if !ok || !strings.Contains(sys, "Little Code Sauce") {
		t.Errorf("mode=append 时应当注入破甲正文: %q", sys)
	}
}
