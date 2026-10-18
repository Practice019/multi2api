package wire

import (
	"encoding/json"
	"testing"
)

// ⚠ 本文件是 `ParseUsageExtras` 缓存字段的**单测**（2026-10-18 新建）。
//
// # 为什么先写它
//
// 账本 E071~E073 的教训：三轮都是"先有现象、后有测试"，返工三遍。
// 而这轮的根因**已经实测到确定**（`loomy` 的真实 usage），所以先把断言钉下来，
// 再改实现 —— 改完若断言红，说明改错了。
//
// ⚠ 最要紧的一条是 `TestParseUsageExtrasNestedCachedTokens`：
// 它就是那个让缓存命中率**整整 92% 的请求读成 0** 的键位。
func TestParseUsageExtrasNestedCachedTokens(t *testing.T) {
	// ⚠ 这是**实测抄回来的真实响应**（loomy/DeepSeek-V4.1-Flash，max_tokens:1）。
	//   不要"润色"它 —— 它存在的意义就是原样复现线上那一份。
	const raw = `{"completion_tokens":1,
	 "completion_tokens_details":{"reasoning_tokens":1},
	 "points_consumed":1,
	 "prompt_tokens":651,
	 "prompt_tokens_details":{"cached_tokens":640},
	 "total_tokens":652}`

	var u map[string]any
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("测试数据本身不合法: %v", err)
	}
	got := ParseUsageExtras(u)

	if got.CacheHitTokens != 640 {
		t.Errorf("CacheHitTokens = %d，想要 640（嵌套 prompt_tokens_details.cached_tokens）", got.CacheHitTokens)
	}
	// ⚠ 上游不报 miss ⇒ 必须是 651 - 640 = 11，而不是 0（0 会被读成"全部未命中"）
	if got.CacheMissTokens != 11 {
		t.Errorf("CacheMissTokens = %d，想要 11（prompt_tokens - hit）", got.CacheMissTokens)
	}
	if got.ThinkTokens != 1 {
		t.Errorf("ThinkTokens = %d，想要 1（嵌套 completion_tokens_details.reasoning_tokens）", got.ThinkTokens)
	}
}

func TestParseUsageExtrasCacheKeyPrecedence(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		hit  int
		miss int
	}{
		// 顶层显式名优先
		{"顶层显式", `{"prompt_cache_hit_tokens":10,"cached_tokens":99,"prompt_tokens":100}`, 10, 90},
		// 顶层简写（workbuddy 的形态）
		{"顶层简写", `{"cached_tokens":7,"prompt_tokens":100}`, 7, 93},
		// 嵌套（OpenAI 兼容的标准位置）
		{"嵌套", `{"prompt_tokens_details":{"cached_tokens":30},"prompt_tokens":100}`, 30, 70},
		// 显式 miss 存在就用它，不推算
		{"显式 miss", `{"prompt_cache_hit_tokens":10,"prompt_cache_miss_tokens":5,"prompt_tokens":100}`, 10, 5},
		// ⚠ 一个都没有 ⇒ 两者都为 0（"没有缓存信息"，而不是"全部未命中"）
		{"无缓存字段", `{"prompt_tokens":100}`, 0, 0},
		// ⚠ 合法地为 0：键在、值是 0 ⇒ hit=0 且**不推算** miss（0 hit 时推算没有依据）
		{"命中为 0", `{"prompt_cache_hit_tokens":0,"prompt_tokens":100}`, 0, 0},
	}
	for _, c := range cases {
		var u map[string]any
		if err := json.Unmarshal([]byte(c.raw), &u); err != nil {
			t.Fatalf("[%s] 测试数据不合法: %v", c.name, err)
		}
		got := ParseUsageExtras(u)
		if got.CacheHitTokens != c.hit || got.CacheMissTokens != c.miss {
			t.Errorf("[%s] hit/miss = %d/%d，想要 %d/%d",
				c.name, got.CacheHitTokens, got.CacheMissTokens, c.hit, c.miss)
		}
	}
}

// TestParseUsageExtrasMissNotFabricated 把"不许编数据"单独钉一条。
//
// ⚠ 若哪天有人把 miss 改成"总是 prompt_tokens - hit"，那么**没有缓存信息的请求**
// 会被算成"全部未命中" ⇒ 命中率被恒报为 0% ⇒ 前端会把那个 0% 读成一个结论
// （而它其实该显示 N/A）。这条断言就是为了拦住那种"看起来更完整"的改动。
func TestParseUsageExtrasMissNotFabricated(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":500,"completion_tokens":10}`), &u); err != nil {
		t.Fatal(err)
	}
	if got := ParseUsageExtras(u); got.CacheMissTokens != 0 || got.CacheHitTokens != 0 {
		t.Errorf("无缓存字段时应为 0/0，得到 %d/%d", got.CacheHitTokens, got.CacheMissTokens)
	}
}
