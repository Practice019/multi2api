// sse_usage_total_test.go 「自己加，不信上游 total_tokens」的守卫（2026-10-20）。
//
// # 这个文件钉的是什么
//
// 用户质问「我们下面那个 token 计算是不是太少了？我不可能只用了那么点啊」。
// 对照 new-api 的源码找到了答案（model/log.go:395）：
//
//	TokenUsed: params.PromptTokens + params.CompletionTokens
//
// **它自己加，从不信任上游给的 total_tokens。**
//
// 实测证据（loomy 一条真实请求）：
//
//	prompt_cache_hit_tokens = 63,360
//	prompt_cache_miss_tokens =    722
//	completion_tokens        =    483
//	total_tokens             =    483   ← 只报了输出，漏掉 6.4 万输入
//
// 信 total_tokens ⇒ 这条记 483；自己加 ⇒ 记 64,565。差 133 倍。
// 全库 Σ 差 4.3 倍（我们 1.84 亿 vs new-api 7.84 亿）就是这类漏算累积的。
package server

import "testing"

// TestSSEUsageTotalIgnoresBogusTotalTokens 上游 total_tokens 漏算时不影响我们。
func TestSSEUsageTotalIgnoresBogusTotalTokens(t *testing.T) {
	// 实测 loomy 形态：total_tokens 只等于 completion
	u := frameUsage(`{"usage":{"completion_tokens":483,"total_tokens":483,"prompt_cache_hit_tokens":63360,"prompt_cache_miss_tokens":722}}`)
	if u == nil {
		t.Fatal("frameUsage 返回 nil")
	}
	// ★ 核心断言：必须 63360+722+483 = 64565，不是上游报的 483
	if got := u.total(); got != 64565 {
		t.Fatalf("★ total 应为 64565（自己加），实际 %d —— 若等于 483 说明还在信上游的 total_tokens", got)
	}
}

// TestSSEUsageTotalUsesPromptPlusCompletion 标准帧：prompt+completion。
func TestSSEUsageTotalUsesPromptPlusCompletion(t *testing.T) {
	// 故意让 total_tokens 与 prompt+completion 不一致，验证我们用后者
	u := frameUsage(`{"usage":{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":9999}}`)
	if got := u.total(); got != 1200 {
		t.Fatalf("★ total 应为 prompt+completion = 1200（照 new-api），实际 %d —— 不能信 total_tokens=9999", got)
	}
}

// TestSSEUsageTotalFallsBackToTotalTokens 输入完全无法还原时才回退 total_tokens。
//
// ⚠ 这条防的是"为了不信 total_tokens 而把它彻底扔掉"：
// 有些上游只发 {total_tokens, completion_tokens}，不给 prompt 也不给缓存 ——
// 那种情况下 total_tokens 是唯一线索，扔掉会让总量退化成纯输出（更小）。
func TestSSEUsageTotalFallsBackToTotalTokens(t *testing.T) {
	u := frameUsage(`{"usage":{"completion_tokens":200,"total_tokens":5000}}`)
	if u == nil || !u.valid() {
		t.Fatal("有 total_tokens + completion 的帧应判有效")
	}
	if got := u.total(); got != 5000 {
		t.Fatalf("输入无从得知时应回退 total_tokens=5000，实际 %d", got)
	}
}

// TestSSEUsageTotalNoBogusSmaller 自己加出来的总是不小于上游 total_tokens（当上游漏算时）。
func TestSSEUsageTotalNoBogusSmaller(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"缓存全命中 + total 只报输出", `{"usage":{"completion_tokens":100,"total_tokens":100,"prompt_cache_hit_tokens":50000,"prompt_cache_miss_tokens":0}}`, 50100},
		{"缓存全未命中", `{"usage":{"completion_tokens":100,"total_tokens":100,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":3000}}`, 3100},
		{"标准 prompt", `{"usage":{"prompt_tokens":800,"completion_tokens":100,"total_tokens":100}}`, 900},
	}
	for _, c := range cases {
		u := frameUsage(c.body)
		if u == nil {
			t.Fatalf("%s: frameUsage nil", c.name)
		}
		if got := u.total(); got != c.want {
			t.Fatalf("%s: total 应为 %d（自己加），实际 %d", c.name, c.want, got)
		}
	}
}
