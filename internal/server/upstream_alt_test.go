package server

import "testing"

// ⚠ 钉住"跨上游模型名能对上"这条**可行性前提** ——
// 整个"换一个上游试同一个模型"的方案都建立在它上面。
//
// 实测来源（2026-10-18，request-log 里真实出现过的模型名）：
//
//	loomy/DeepSeek-V4.1-Flash          · workbuddy/deepseek-v4.1-flash
//	workbuddy-intl/deepseek-v4.1-flash
//
// 三家的差别**只在**大小写与 `.`/`-` ⇒ 归一后同一个键。
// ⚠ 若哪天某家把模型名改得对不上，这条测试会红 —— 那是**该知道**的事
//   （意味着自动降级对它失效，而不是静默不生效）。
func TestNormModelKeyMatchesAcrossUpstreams(t *testing.T) {
	same := []string{
		"loomy/DeepSeek-V4.1-Flash",
		"workbuddy/deepseek-v4.1-flash",
		"workbuddy-intl/deepseek-v4.1-flash",
	}
	want := "deepseekv41flash"
	for _, m := range same {
		if got := normModelKey(m); got != want {
			t.Errorf("normModelKey(%q) = %q，想要 %q —— ⚠ 对不上就意味着换上游降级对它失效", m, got, want)
		}
	}
}

// TestNormModelKeyKeepsModelSuffixAfterFirstSlash ⚠ 只吃**第一个**斜杠。
//
// 有的上游模型名自带斜杠（如 `cline/cline-free/deepseek-v4.1-flash`）——
// 去掉第一段之后，后面的 `cline-free/deepseek-v4.1-flash` 仍要参与归一，
// 否则它会被误判成"和 deepseek-v4.1-flash 同一个模型"。
func TestNormModelKeyKeepsModelSuffixAfterFirstSlash(t *testing.T) {
	got := normModelKey("cline/cline-free/deepseek-v4.1-flash")
	if got != "clinefreedeepseekv41flash" {
		t.Errorf("normModelKey = %q，想要 clinefreedeepseekv41flash（只吃第一个斜杠）", got)
	}
	// ⚠ 而它与纯 deepseek 那批**不该**相等 —— 这正是不吃第二个斜杠的意义
	if got == normModelKey("loomy/DeepSeek-V4.1-Flash") {
		t.Error("带渠道段的模型名不该与纯模型名归一成同一个键（那会导致换错上游）")
	}
}

// TestNormModelKeyEmptyOnGarbage 归一不出东西时返回空串 ⇒ 调用方放弃匹配。
//
// ⚠ 宁可"不动"，也不要"乱配"：把 `-` 这种垃圾名匹配到某个真模型上，
//   会让我们把请求发到一个它并不支持的上游 —— 比不高可用更糟。
func TestNormModelKeyEmptyOnGarbage(t *testing.T) {
	for _, m := range []string{"", "-", "/", "///", "---"} {
		if got := normModelKey(m); got != "" {
			t.Errorf("normModelKey(%q) = %q，想要空串（调用方据此放弃匹配）", m, got)
		}
	}
}
