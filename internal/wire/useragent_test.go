package wire

import (
	"strings"
	"testing"
)

// TestPickUAPrefersFirstMatch 先命中先返回（顺序有意义，不能排序）。
//
// 参照实现是 `for (const rule of product.userAgentByModelFamily ?? [])` +
// 立即 return —— 表序即优先级。若这里改成"最长前缀优先"或排序，
// 将来加一条更宽的前缀（如 `g`）时结果会与参照不一致。
func TestPickUAPrefersFirstMatch(t *testing.T) {
	rules := []UAModelFamilyRule{
		{Match: "glm-", UA: "CN-UA"},
		{Match: "gl", UA: "WIDE-UA"}, // 更宽但排在后面 → 不该赢
	}
	if got := PickUA(rules, "glm-5.2", "FALLBACK"); got != "CN-UA" {
		t.Errorf("PickUA = %q，want CN-UA —— "+
			"先命中先返回（表序即优先级），改成最长前缀优先会与参照不一致", got)
	}
}

// TestPickUAFallsBack 无命中时用 fallback。
func TestPickUAFallsBack(t *testing.T) {
	rules := []UAModelFamilyRule{{Match: "gpt-", UA: "INTL-UA"}}
	if got := PickUA(rules, "unknown-model", "FALLBACK"); got != "FALLBACK" {
		t.Errorf("PickUA = %q，want FALLBACK", got)
	}
}

// TestPickUAEmptyModelUsesFallback 模型名为空时**不猜**分档。
//
// 空模型名意味着"不知道这次是什么模型"（如非 chat 路径的公共请求）。
// 此时按任一条规则匹配都是错的 —— 它们的前缀可能恰好匹配空串。
func TestPickUAEmptyModelUsesFallback(t *testing.T) {
	rules := []UAModelFamilyRule{
		{Match: "", UA: "EMPTY-MATCH-UA"}, // 空前缀会匹配一切
		{Match: "gpt-", UA: "INTL-UA"},
	}
	if got := PickUA(rules, "", "FALLBACK"); got != "FALLBACK" {
		t.Errorf("PickUA(空模型) = %q，want FALLBACK —— "+
			"空模型名不该被任何规则命中（空前缀会匹配一切）", got)
	}
}

// TestPickUASkipsEmptyMatch 空 Match 的规则被跳过（防御畸形配置）。
func TestPickUASkipsEmptyMatch(t *testing.T) {
	rules := []UAModelFamilyRule{
		{Match: "", UA: "BAD"},
		{Match: "glm-", UA: "CN-UA"},
	}
	if got := PickUA(rules, "glm-5.2", "FALLBACK"); got != "CN-UA" {
		t.Errorf("PickUA = %q，want CN-UA（空 Match 应被跳过）", got)
	}
}

// TestPickUANilRules 无规则表时返回 fallback（未配置该特性）。
func TestPickUANilRules(t *testing.T) {
	if got := PickUA(nil, "gpt-6", "FALLBACK"); got != "FALLBACK" {
		t.Errorf("PickUA(nil) = %q，want FALLBACK", got)
	}
}

// TestReferenceTableShape 参照的 intl 分档表形状（逐条核对）。
//
// 这张表是**协议事实**（参照 product.ts:376-388），不是我们发明的：
//
//	gpt- / gemini- / claude-  → 国际版形态
//	glm- / hy / kimi- / minimax- → 国内形态
//
// ⚠ 注意 `hy` 没有连字符 —— 它同时匹配 `hy3` / `hy3-x` / `hy4-preview`
//（那些模型名都没有 `hy-` 前缀）。改成 `hy-` 会让它们全部落到 fallback。
func TestReferenceTableShape(t *testing.T) {
	const intlUA = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	const cnUA = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"
	rules := []UAModelFamilyRule{
		{Match: "gpt-", UA: intlUA},
		{Match: "gemini-", UA: intlUA},
		{Match: "claude-", UA: intlUA},
		{Match: "glm-", UA: cnUA},
		{Match: "hy", UA: cnUA},
		{Match: "kimi-", UA: cnUA},
		{Match: "minimax-", UA: cnUA},
	}

	cases := []struct {
		model string
		want  string
	}{
		{"gpt-6-astra", intlUA},
		{"gemini-3.5-flash", intlUA},
		{"claude-4.5", intlUA},
		{"glm-5.2", cnUA},
		{"hy3", cnUA},          // ⚠ 无连字符也匹配
		{"hy3-x", cnUA},        // ⚠
		{"hy4-preview", cnUA},  // ⚠
		{"kimi-k3", cnUA},
		{"minimax-m3", cnUA},
		{"default-model", intlUA}, // 无命中 → 国际版默认形态
	}
	for _, c := range cases {
		if got := PickUA(rules, c.model, intlUA); got != c.want {
			short := func(s string) string {
				if s == intlUA {
					return "INTL"
				}
				return "CN"
			}
			t.Errorf("PickUA(%q) = %s，want %s", c.model, short(got), short(c.want))
		}
	}
}

// TestTwoUAFormsDifferOnlyInSecondSegment 两个形态只差品牌段（逐字核对）。
//
// ⚠ 这条钉住一个**很容易被"整理"掉**的差异：
//
//	国际版  WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2
//	国内版  WorkBuddy/5.5.2 WorkBuddy/5.5.2    CLI/5.5.2
//	                       ^^^^^^^^^^^ 只有这里不同
//
// 把两者合并成一个（"都是 WorkBuddy 啊"）会让国际版模型的账单归因变成 `-`。
//
// ⚠ 判据**不能**用 `strings.Split(s, " ")` 数段数：国际版的品牌段是
// `WorkBuddy AI`，**自带一个空格**，所以按空格切是 4 段而不是 3 段。
// 我第一版就是这么写的，被自己的测试抓出来了 —— 那恰好说明
// "这段 UA 里有个空格"是个真实且容易踩的细节。
//
// 改用"归一品牌段后应当逐字相同"作判据：把 `WorkBuddy AI` 换成
// `WorkBuddy` 之后两者必须**完全相等** —— 这精确表达了
// "唯一的区别就是那个 AI 后缀"。
func TestTwoUAFormsDifferOnlyInSecondSegment(t *testing.T) {
	const intl = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	const cn = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"

	if intl == cn {
		t.Fatal("两个形态不得相同")
	}
	// 把国际版的品牌段归一成国内形态后，两者必须逐字相同。
	normalized := strings.Replace(intl, "WorkBuddy AI", "WorkBuddy", 1)
	if normalized != cn {
		t.Errorf("去掉 `AI` 后缀后两者应完全相同：\n  归一后 %q\n  国内版 %q\n"+
			"（说明差异不止在品牌段 —— 那是与参照不一致的）", normalized, cn)
	}
	// 且差异**确实**只是那个后缀。
	if !strings.Contains(intl, "WorkBuddy AI") {
		t.Error("国际版品牌段必须含 `WorkBuddy AI`（含空格）")
	}
	if strings.Contains(cn, "WorkBuddy AI") {
		t.Error("国内版品牌段不得含 `WorkBuddy AI`")
	}
}
