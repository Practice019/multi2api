// sse_usage_prompt_test.go 「上游只发缓存不给 prompt_tokens」的守卫（2026-10-20）。
//
// # 这个文件钉的是什么
//
// 用户报「数据全都不对吧」（2026-10-20）。取证：日志里出现
//
//	toks=483 out=483 hit=63360 miss=722    ← loomy/DeepSeek-V4.1-Flash
//	toks=89  out=89  hit=64000 miss=657
//	toks=781 out=781 hit=64640 miss=759
//
// `toks` 与 `out` 完全相等，而缓存量是它的上百倍 —— 一眼矛盾。
//
// 根因：上游只发
//
//	{"completion_tokens":483,"prompt_cache_hit_tokens":63360,"prompt_cache_miss_tokens":722}
//
// **既没有 total_tokens 也没有 prompt_tokens**。旧实现两处都错：
//  1. valid() 要求 PromptTokens>0 ⇒ 整帧判"无效"丢弃 ⇒ total 退化成估算/输出
//  2. total() 兜底只有 Prompt+Completion ⇒ 63360 的缓存读取凭空消失
//
// ⇒ 现在 valid() 接受"缓存两项能还原总输入"的形态，total() 用它们兜底。
package server

import (
	"testing"
	"time"
)

// TestSSEUsageCacheOnlyFrame 只发缓存的帧：valid 为真，total 含缓存。
func TestSSEUsageCacheOnlyFrame(t *testing.T) {
	// 实测取自 loomy 上游的真实形态
	u := frameUsage(`{"usage":{"completion_tokens":483,"prompt_cache_hit_tokens":63360,"prompt_cache_miss_tokens":722}}`)
	if u == nil {
		t.Fatal("frameUsage 返回 nil，说明 JSON 没解析出来")
	}
	if !u.valid() {
		t.Fatalf("★ 只发缓存两项的帧必须判为有效（否则 total 会退化成估算），实际 valid=false")
	}
	// 真实 total = 缓存命中 63360 + 未命中 722 + 输出 483 = 64565
	if got := u.total(); got != 64565 {
		t.Fatalf("★ total 应为 64565（63360+722+483），实际 %d —— 这就是「缓存量是总量 131 倍」的来源", got)
	}
	if got := u.promptTotal(); got != 64082 {
		t.Fatalf("promptTotal 应为 64082（63360+722），实际 %d", got)
	}
}

// TestSSEUsageStandardFrameNotAffected 标准 OpenAI 帧的行为不变。
func TestSSEUsageStandardFrameNotAffected(t *testing.T) {
	u := frameUsage(`{"usage":{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":1200}}`)
	if !u.valid() {
		t.Fatal("标准帧必须有效")
	}
	if got := u.total(); got != 1200 {
		t.Fatalf("total 应为 total_tokens=1200，实际 %d", got)
	}
	if got := u.promptTotal(); got != 1000 {
		t.Fatalf("promptTotal 应为 1000，实际 %d", got)
	}
}

// TestSSEUsagePrefersPromptTokensOverCache prompt_tokens 存在时优先用它（它已含缓存）。
//
// ⚠ 这条防的是"两个来源同时存在时相加两次"——那会把输入算大一倍。
func TestSSEUsagePrefersPromptTokensOverCache(t *testing.T) {
	u := frameUsage(`{"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":200}}`)
	if got := u.promptTotal(); got != 1000 {
		t.Fatalf("★ 有 prompt_tokens 时应直接用 1000（它已含缓存），不能 800+200+1000 重复相加，实际 %d", got)
	}
	if got := u.total(); got != 1200 {
		t.Fatalf("total 应为 1000+200=1200，实际 %d", got)
	}
}

// TestSSEUsageHalfCacheIsNotSummed 只给一项缓存时不瞎加。
//
// ⚠ 缓存命中与未命中是**互斥的输入划分**，两项都齐才能还原总输入。
// 只给 hit 不给 miss 时相加会把未命中那部分当不存在（低估）。
func TestSSEUsageHalfCacheIsNotSummed(t *testing.T) {
	u := frameUsage(`{"usage":{"completion_tokens":200,"prompt_cache_hit_tokens":800}}`)
	if got := u.promptTotal(); got != 0 {
		t.Fatalf("只给 hit 时 promptTotal 应为 0（不猜），实际 %d", got)
	}
	// 但 total_tokens 缺失且输入未知 ⇒ total 只剩输出，这是诚实的下界
	if got := u.total(); got != 200 {
		t.Fatalf("输入未知时 total 应为输出 200，实际 %d", got)
	}
}

// TestSSEUsageNoOutputIsInvalid 没有输出的帧仍然无效（防半截快照）。
func TestSSEUsageNoOutputIsInvalid(t *testing.T) {
	for _, body := range []string{
		`{"usage":{"prompt_tokens":123}}`,             // 只有输入
		`{"usage":{"prompt_cache_hit_tokens":63360}}`, // 只有缓存，没输出
		`{"usage":{"total_tokens":500}}`,              // 只有 total，没输出
		`{"usage":{}}`,                                // 空对象
	} {
		if u := frameUsage(body); u != nil && u.valid() {
			t.Fatalf("★ 没有 completion 的帧必须判无效，实际 valid=true：%s", body)
		}
	}
}

// TestStreamUsageResultUsesCacheOnlyFrame 端到端：Result() 走末帧拿到含缓存的 total。
func TestStreamUsageResultUsesCacheOnlyFrame(t *testing.T) {
	s := newStreamUsage(time.Now().Add(-5 * time.Millisecond))
	s.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"usage\":{\"completion_tokens\":483,\"prompt_cache_hit_tokens\":63360,\"prompt_cache_miss_tokens\":722}}\n\n"))
	tok, _, _ := s.Result()
	if tok != 64565 {
		t.Fatalf("★ Result() 应返回含缓存的 64565，实际 %d —— 用户看到的「总量等于输出量」就是这个错", tok)
	}
	out, _ := s.Completion()
	if out != 483 {
		t.Fatalf("Completion() 应为纯输出 483，实际 %d", out)
	}
}
