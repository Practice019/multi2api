package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func armorState(mode string) *ArmorState {
	return NewArmorState(ArmorConfig{Mode: mode})
}

// sysOf 取 body 里第一条 system/developer 消息的文本（测试夹具）。
func sysOf(t *testing.T, body []byte) string {
	t.Helper()
	s, ok := SystemText(body)
	if !ok {
		t.Fatal("body 里没有 system 文本")
	}
	return s
}

// TestArmorBodyOffIsByteIdentical off / nil 状态 ⇒ 字节逐字不变（开关语义本身）。
func TestArmorBodyOffIsByteIdentical(t *testing.T) {
	body := []byte("{\"model\":\"m\",\"messages\":[{\"role\":\"system\",\"content\":\"S\"},{\"role\":\"user\",\"content\":\"U\"}]}")
	if got := ApplyArmorToBody(body, nil); string(got) != string(body) {
		t.Error("状态为 nil 时应当原样返回")
	}
	if got := ApplyArmorToBody(body, armorState(ArmorOff)); string(got) != string(body) {
		t.Error("mode=off 时应当原样返回")
	}
	if got := ApplyArmorToBody(body, armorState("")); string(got) != string(body) {
		t.Error("mode 为空时应当原样返回")
	}
}

// TestArmorBodyAppendsToFirstSystem 常规形态：messages[0] 是 system 字符串。
func TestArmorBodyAppendsToFirstSystem(t *testing.T) {
	body := []byte("{\"model\":\"m\",\"messages\":[{\"role\":\"system\",\"content\":\"原始系统提示\"},{\"role\":\"user\",\"content\":\"用户原话\"}]}")
	got := ApplyArmorToBody(body, armorState(ArmorAppend))
	// ⚠ 断言看**解码后**的文本：Go 的 json.Marshal 会把标记里的 < > 转义成 \u003c，
	//   直接拿原始字节比标记会永远失败（语义其实是对的）。
	sys := sysOf(t, got)
	if !strings.Contains(sys, "原始系统提示") {
		t.Error("客户端原文被丢了（append 必须保留）")
	}
	if !strings.Contains(sys, "Little Code Sauce") || !strings.Contains(sys, armorStart) {
		t.Error("破甲正文没进 system")
	}
	if !strings.Contains(string(got), "用户原话") {
		t.Error("用户消息被动过")
	}
}

// TestArmorBodyFindsSystemNotAtZero system 不在第 0 条时也要盖到（口径与 SystemText 一致）。
func TestArmorBodyFindsSystemNotAtZero(t *testing.T) {
	body := []byte("{\"messages\":[{\"role\":\"user\",\"content\":\"u\"},{\"role\":\"system\",\"content\":\"S2\"}]}")
	got := sysOf(t, ApplyArmorToBody(body, armorState(ArmorAppend)))
	if !strings.HasPrefix(got, "S2") || !strings.Contains(got, armorStart) {
		t.Errorf("第 2 条 system 没被盖上: %q", got[:min(60, len(got))])
	}
}

// TestArmorBodyInsertsSystemWhenMissing 没有 system ⇒ 插一条（否则调用方不发 system 就绕过了）。
func TestArmorBodyInsertsSystemWhenMissing(t *testing.T) {
	body := []byte("{\"messages\":[{\"role\":\"user\",\"content\":\"u\"}]}")
	out := ApplyArmorToBody(body, armorState(ArmorAppend))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应当插入一条 system，实际 %d 条", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(first["content"].(string), armorStart) {
		t.Error("插入的不是带破甲的 system")
	}
	if msgs[1].(map[string]any)["content"] != "u" {
		t.Error("原用户消息顺序被改")
	}
}

// TestArmorBodyArrayContentSingleTextBlock 数组 content：text 块合并成一条，图片保留，替换模式不重复。
func TestArmorBodyArrayContentSingleTextBlock(t *testing.T) {
	body := []byte("{\"messages\":[{\"role\":\"system\",\"content\":[{\"type\":\"text\",\"text\":\"A\"},{\"type\":\"text\",\"text\":\"B\"},{\"type\":\"image_url\",\"image_url\":{\"url\":\"x\"}}]}]}")
	out := ApplyArmorToBody(body, armorState(ArmorReplace))
	if n := strings.Count(sysOf(t, out), armorStart); n != 1 {
		t.Errorf("replace 模式下破甲块应当只出现一次（不能每个 text 块各来一遍）: %d 次", n)
	}
	got := string(out)
	if !strings.Contains(got, "\"image_url\"") || !strings.Contains(got, "\"x\"") {
		t.Error("非 text 块（图片）被改掉了")
	}
	if strings.Contains(got, "\"text\":\"A\"") {
		t.Error("原 text 块应当被合并/替换掉（replace 模式）")
	}
}

// TestArmorBodyMalformedIsUntouched 解析失败一律原样返回。
func TestArmorBodyMalformedIsUntouched(t *testing.T) {
	for _, body := range []string{
		"not json",
		"{}",
		"{\"messages\":\"不是数组\"}",
		"{\"messages\":[{\"role\":\"system\",\"content\":123}]}",
		// 空 messages 不在这个清单里：按设计它是「没有 system ⇒ 插一条」，
		// 不是畸形输入（另有 TestArmorBodyInsertsSystemWhenEmptyMessages 钉住）。
	} {
		if got := string(ApplyArmorToBody([]byte(body), armorState(ArmorAppend))); got != body {
			t.Errorf("畸形输入必须原样返回: 入=%s 出=%s", body, got)
		}
	}
}

// TestArmorBodyIdempotent 重复经过不叠加（append 与 replace 都要）。
func TestArmorBodyIdempotent(t *testing.T) {
	for _, mode := range []string{ArmorAppend, ArmorReplace} {
		body := []byte("{\"model\":\"m\",\"messages\":[{\"role\":\"system\",\"content\":\"S\"},{\"role\":\"user\",\"content\":\"U\"}]}")
		once := ApplyArmorToBody(body, armorState(mode))
		twice := ApplyArmorToBody(once, armorState(mode))
		if string(once) != string(twice) {
			t.Errorf("mode=%s 第二次注入改变了字节（幂等被破坏）", mode)
		}
		if n := strings.Count(sysOf(t, once), armorStart); n != 1 {
			t.Errorf("mode=%s 破甲块出现 %d 次（应当 1 次）", mode, n)
		}
	}
}

// TestArmorBodySkipsWhenMarkerPresent 已含标记 ⇒ 原样返回（保护尾部话术；也避免 replace 抹掉它）。
func TestArmorBodySkipsWhenMarkerPresent(t *testing.T) {
	body := []byte("{\"model\":\"m\",\"messages\":[{\"role\":\"system\",\"content\":\"S\"}]}")
	armored := ApplyArmorToBody(body, armorState(ArmorReplace))
	nudged := ApplyRetryNudge(armored)
	if !strings.Contains(sysOf(t, nudged), RetryNudge) {
		t.Fatal("夹具本身不对：话术没进去")
	}
	again := ApplyArmorToBody(nudged, armorState(ArmorReplace))
	if string(again) != string(nudged) {
		t.Error("已盖章的 body 又被改动了 —— replace 模式下这一改会把话术整段冲掉")
	}
	if !strings.Contains(sysOf(t, again), RetryNudge) {
		t.Error("话术丢了")
	}
}

// TestArmorThenNudgeKeepsNudgeAtTail 顺序验证：先盖章 → 追话术 ⇒ 话术在**尾部**且只出现一次。
func TestArmorThenNudgeKeepsNudgeAtTail(t *testing.T) {
	for _, mode := range []string{ArmorAppend, ArmorReplace} {
		body := []byte("{\"messages\":[{\"role\":\"system\",\"content\":\"客户原文\"}]}")
		armed := ApplyArmorToBody(body, armorState(mode))
		final := ApplyArmorToBody(ApplyRetryNudge(armed), armorState(mode))
		sys := sysOf(t, final)
		if !strings.HasSuffix(strings.TrimSpace(sys), RetryNudge) {
			t.Errorf("mode=%s 话术不在尾部（尾部是 %q...）", mode, tail(sys, 40))
		}
		if strings.Count(sys, RetryNudge) != 1 {
			t.Errorf("mode=%s 话术出现 %d 次", mode, strings.Count(sys, RetryNudge))
		}
		if mode == ArmorAppend && !strings.Contains(sys, "客户原文") {
			t.Errorf("mode=%s 客户原文丢了", mode)
		}
	}
}

// TestArmorBodyInsertsSystemWhenEmptyMessages messages 为空数组 ⇒ 同样插一条（按设计）。
func TestArmorBodyInsertsSystemWhenEmptyMessages(t *testing.T) {
	body := []byte("{\"messages\":[]}")
	out := ApplyArmorToBody(body, armorState(ArmorAppend))
	if string(out) == string(body) {
		t.Error("空 messages 应当被插入一条 system（否则调用方清空 messages 就绕过这一层）")
	}
	if !strings.Contains(sysOf(t, out), armorStart) {
		t.Error("插入的 system 里没有破甲块")
	}
}

// tail 取尾部若干 rune（只在失败信息里用）。
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
