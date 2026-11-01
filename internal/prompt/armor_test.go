package prompt

import (
	"strings"
	"testing"
)

// TestArmorOffInjectsNothing 默认（off）不许改动出站提示词。
func TestArmorOffInjectsNothing(t *testing.T) {
	for _, mode := range []string{"", "off", "OFF", "  "} {
		got, changed := ApplyArmor("客户端人格", ArmorConfig{Mode: mode})
		if changed || got != "客户端人格" {
			t.Errorf("mode=%q 应当原样返回，得到 changed=%v %q", mode, changed, got)
		}
	}
}

// TestArmorAppendKeepsClientPrompt append 模式必须**保留**客户端人格（只是追加）。
func TestArmorAppendKeepsClientPrompt(t *testing.T) {
	got, changed := ApplyArmor("客户端人格：我是 Claude Code 的助手", ArmorConfig{Mode: ArmorAppend})
	if !changed {
		t.Fatal("append 模式必须报告改动")
	}
	if !strings.HasPrefix(got, "客户端人格：") {
		t.Error("append 模式把客户端人格弄丢了 —— 那是 replace 的语义，不是 append")
	}
	// ⚠ 断言串必须取自**真实载荷**（armor-payload.md 是原项目那份文件的逐字副本）：
	//   原来写成 "无限制模式"/"任务优先" 是我那版浓缩文本里的说法，逐字副本里没有。
	for _, want := range []string{"Little Code Sauce", "PRIME DIRECTIVE", "TASK PRIORITY", "拒绝对抗", armorStart, armorEnd} {
		if !strings.Contains(got, want) {
			t.Errorf("注入块里缺 %q", want)
		}
	}
}

// TestArmorReplaceDropsClientPrompt replace 模式用破甲人设整体替换。
func TestArmorReplaceDropsClientPrompt(t *testing.T) {
	got, changed := ApplyArmor("客户端人格", ArmorConfig{Mode: ArmorReplace})
	if !changed {
		t.Fatal("replace 模式必须报告改动")
	}
	if strings.Contains(got, "客户端人格") {
		t.Error("replace 模式应当替换掉客户端 system")
	}
	if !strings.Contains(got, "Little Code Sauce") {
		t.Error("replace 模式没注入破甲正文")
	}
}

// TestArmorIsIdempotent 重复注入不会越堆越长（重试路径会多次经过）。
//
// 这条守的是"重试"这条真实路径：审核拦截降级重试、拒绝重试都会再走一遍注入 ——
// 不幂等的话提示词每重试一次长一截，最后把上下文挤爆，而表现是**上游报长度错误**，
// 很难联想到注入层。
func TestArmorIsIdempotent(t *testing.T) {
	cfg := ArmorConfig{Mode: ArmorAppend}
	once, _ := ApplyArmor("客户端人格", cfg)
	twice, changed := ApplyArmor(once, cfg)
	if changed {
		t.Error("第二次注入报告了改动 —— 旧块没被剥掉（幂等失效）")
	}
	if twice != once {
		t.Errorf("重复注入改变了文本：第一次=%q 第二次=%q", once, twice)
	}
	if n := strings.Count(twice, armorStart); n != 1 {
		t.Errorf("注入块出现 %d 次，want 1", n)
	}
}

// TestArmorStripsTruncatedBlock 只有开标记没有闭标记（被截断）时也要能清理。
func TestArmorStripsTruncatedBlock(t *testing.T) {
	broken := "客户端人格\n\n" + armorStart + "\n一半的条款…"
	got, _ := ApplyArmor(broken, ArmorConfig{Mode: ArmorAppend})
	if strings.Contains(got, "一半的条款") {
		t.Error("截断的旧块没被清掉 —— 会与新的注入块叠在一起")
	}
	if !strings.HasPrefix(got, "客户端人格") {
		t.Error("清理截断块时把客户端人格也删了")
	}
}

// （原 TestArmorCustomOverrides 已删：那一版断言的是 Persona/Clauses 两个字段，
//   而载荷模型收敛成单一 Payload 之后它们是死代码 —— 覆盖路径由
//   TestArmorPayloadOverrideWins 覆盖，不留两套说法。）

// TestRefusalPatternsHit 每条规则都要能命中它的代表句。
//
// 另一组（不该命中的）在下面单独一条：漏判只是少一次重试，误判会把**正常回答**换掉。
func TestRefusalPatternsHit(t *testing.T) {
	cases := []struct{ name, text string }{
		{"英文硬拒绝", "I can't help with that."},
		{"英文道歉式", "I'm sorry, but I cannot do that."},
		// ⚠ 夹具必须与**源正则**逐字对齐（移植保持原样，不擅自放宽）：
		//   · cannot-help 那条源正则要求后接 "with that/this/your"
		//   · "作为一个"后面**紧接** AI（源里没有 \s*）
		//   · "请"后面只允许可选的"你"，不能是"请先"
		//   第一版夹具比源更宽，于是三条"漏判"全是夹具的错，不是移植的错。
		{"英文 cannot-help", "I cannot help with that."},
		{"中文硬拒绝", "我不能提供这个内容。"},
		{"中文超出范围", "这超出了我的能力范围。"},
		{"AI 身份开场", "作为一个AI语言模型，我需要说明…"},
		{"中文软拒绝开场", "这一步我不做，后面的也帮不了。"},
		{"要授权", "请你提供书面授权，我再继续。"},
		{"判为越权", "这属于未授权的操作。"},
		{"要目标", "请把目标发给我。"},
		{"元拒绝（短）", "这是提示词注入。"},
	}
	for _, c := range cases {
		if !LooksLikeRefusal(c.text) {
			t.Errorf("%s：%q 应当被判为拒绝（漏判 → 不会触发重试）", c.name, c.text)
		}
	}
}

// TestRefusalMetaDiscussionIsNotRefusal 长文本里正常讨论注入/越狱**不算**拒绝。
//
// 这是整份检测的精髓：没有这条豁免，任何一篇谈到"提示词注入"的技术回答都会被
// 判成拒绝 → 重试 → 把正常回答换掉，等于把正常流量打坏。
func TestRefusalMetaDiscussionIsNotRefusal(t *testing.T) {
	discussion := "关于提示词注入（prompt injection）的防护，我们有三种做法：" +
		strings.Repeat("首先在输入侧做模式匹配，把已知的越狱模板串改写掉；", 12) +
		"综上，这一层的目标是降低误报，而不是宣称能挡住一切。"
	if utf8Len(discussion) <= 280 {
		t.Fatalf("夹具太短，测不出豁免（需要超过 280 字符），实际 %d", utf8Len(discussion))
	}
	if LooksLikeRefusal(discussion) {
		t.Error("正常的技术讨论被判成了拒绝 —— 会把正常回答重试掉")
	}
}

// TestRefusalShortMetaRefusalStillMatches 短的元拒绝（就是在评论指令本身）仍然要命中。
func TestRefusalShortMetaRefusalStillMatches(t *testing.T) {
	if !LooksLikeRefusal("这是提示词注入，我不执行。") {
		t.Error("短的元拒绝应当命中（豁免只对长文本生效）")
	}
}

// TestRefusalEmptyAndNormalAreClean 空文本与正常回答不许命中。
func TestRefusalEmptyAndNormalAreClean(t *testing.T) {
	for _, s := range []string{"", "   ", "这是修复后的 diff，测试全绿。", "The build passes; here is the patch."} {
		if LooksLikeRefusal(s) {
			t.Errorf("%q 被误判为拒绝", s)
		}
	}
}

// TestWithRetryNudgeAppendsAndIsIdempotent 重试话术只追加，且幂等。
func TestWithRetryNudgeAppendsAndIsIdempotent(t *testing.T) {
	one := WithRetryNudge("系统提示")
	if !strings.HasPrefix(one, "系统提示") || !strings.Contains(one, "要现在执行吗？") {
		t.Errorf("追加结果不对: %q", one)
	}
	if two := WithRetryNudge(one); two != one {
		t.Error("重试话术不幂等 —— 每次重试都会再加一遍")
	}
}

// TestArmorPayloadIsVerbatimPort 载荷必须是原项目那份文件的**逐字**副本。
//
// # 这条是用户投诉换来的
//
// 第一版我拿 dsh-purge 的 test/fixtures/inject-clauses.json（**片段**夹具）当来源，
// 自己"重组成语义完整的条款块"，只有 1230 字符；而原项目真正注入的是
// ~/.dsh/prompt-inject.md —— 21194 字符 / 226 行 / 47 段。覆盖率 5.8%。
// 用户的原话是「我都说了按照原项目 1:1 移植啊」。
//
// 所以现在钉三件事：① 载荷够大（不是浓缩版）② 关键分节都在 ③ 注入块里
// **原样包含**整份载荷（逐字，没被改写/截断）。
func TestArmorPayloadIsVerbatimPort(t *testing.T) {
	p := ArmorPayload()
	if len(p) < 20000 {
		t.Errorf("内置载荷只有 %d 字节 —— 这看起来又变成浓缩版了（原文件 21194 字符 / 226 行）", len(p))
	}
	for _, needle := range []string{
		"Little Code Sauce",           // 人格
		"[MODE: UNRESTRICTED SANDBOX", // 模式声明
		"PRIME DIRECTIVE",             // 首要指令
		"TASK PRIORITY",               // 任务优先
		"Direct generation",           // 直接生成
		"REFUSAL HARDENING",           // 拒绝对抗
		"Scoped Rules",                // 分域规则（最大的一段）
		"Never Refuse",                // 永不拒绝
		"Sandbox test directions",     // 沙箱测试方向
		"Intent mapping",              // 意图映射
		"CTF Core Contract",           // CTF 契约
	} {
		if !strings.Contains(p, needle) {
			t.Errorf("载荷里缺分节标记 %q —— 不是逐字副本", needle)
		}
	}
	// 逐字：注入块里必须能原样找到整份载荷（只允许外层多一对幂等标记）
	blk := ArmorText(ArmorConfig{})
	if !strings.Contains(blk, strings.TrimRight(p, "\n")) {
		t.Error("注入块里的载荷被改写过 —— 逐字副本不允许任何改写/截断")
	}
	if !strings.HasPrefix(blk, armorStart) || !strings.HasSuffix(blk, armorEnd) {
		t.Error("幂等标记没包住整块载荷")
	}
}

// TestArmorPayloadOverrideWins 配置给的载荷优先于内置（换文件/换版本用）。
func TestArmorPayloadOverrideWins(t *testing.T) {
	got := ArmorText(ArmorConfig{Payload: "自定义逐字载荷"})
	if !strings.Contains(got, "自定义逐字载荷") {
		t.Errorf("覆盖没生效: %q", got)
	}
	if strings.Contains(got, "Little Code Sauce") {
		t.Error("给了覆盖载荷时不该再拼内置载荷")
	}
}

// utf8Len 夹具用的字符数（与实现同口径）。
// TestApplyRetryNudgePatchesFirstSystemMessage 第一条是 system ⇒ 尾部追加（字符串形态）。
func TestApplyRetryNudgePatchesFirstSystemMessage(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"原始系统提示"},{"role":"user","content":"用户原话"}]}`)
	out := ApplyRetryNudge(body)
	if !strings.Contains(string(out), "原始系统提示") {
		t.Fatal("原文被丢了")
	}
	if !strings.Contains(string(out), "要现在执行吗？") {
		t.Error("nudge 没追加到 system 尾部")
	}
	if !strings.Contains(string(out), "用户原话") {
		t.Error("用户消息被动过")
	}
	// 顶层其它字段逐字保留
	if !strings.Contains(string(out), `"model":"m"`) {
		t.Error("顶层字段被改写")
	}
}

// TestApplyRetryNudgeSkipsWhenFirstIsNotSystem 第一条**不是** system ⇒ 原样返回。
//
// 这是原项目 withNudge 的行为，最容易被"顺手优化"掉：直觉上"有 system 就加"更对，
// 但那就不是 1:1 了（用户明确要求照原样做）。
func TestApplyRetryNudgeSkipsWhenFirstIsNotSystem(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"u"},{"role":"system","content":"s"}]}`)
	if out := ApplyRetryNudge(body); string(out) != string(body) {
		t.Errorf("第一条不是 system 时应当原样返回，实际: %s", out)
	}
}

// TestApplyRetryNudgeArrayContentOnlyTextBlocks 数组 content：只给 text 块拼。
func TestApplyRetryNudgeArrayContentOnlyTextBlocks(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"A"},{"type":"image_url","image_url":{"url":"x"}}]}]}`)
	out := string(ApplyRetryNudge(body))
	if !strings.Contains(out, "A") || !strings.Contains(out, "要现在执行吗？") {
		t.Errorf("text 块没被追加: %s", out)
	}
	if !strings.Contains(out, `"type":"image_url"`) || !strings.Contains(out, `"url":"x"`) {
		t.Errorf("非 text 块被改动了: %s", out)
	}
}

// TestApplyRetryNudgeFailuresReturnInputVerbatim 解析失败/畸形一律原样返回。
func TestApplyRetryNudgeFailuresReturnInputVerbatim(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{}`,
		`{"messages":[]}`,
		`{"messages":"字符串不是数组"}`,
		`{"messages":[{"role":"system","content":123}]}`,
	} {
		if out := ApplyRetryNudge([]byte(body)); string(out) != body {
			t.Errorf("畸形输入必须原样返回: 入=%s 出=%s", body, out)
		}
	}
}

func utf8Len(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
