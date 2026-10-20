// public_name_test.go publicNameFor / resolveModelAlias 的方向与匹配（2026-10-21）
//
// # 用户报的现象
//
//	「我明明设置了对外名称为什么 csg 还是原来的模型名称？」
//
// 实测（用 key 666 查 /v1/models）：
//
//	001/DeepSeek-V4.1-Flash        ← 映射生效
//	csg/deepseek-v4.1-flash-1017   ← 没生效
//
// 而 model_map 里两条都在：
//
//	{"001/DeepSeek-V4.1-Flash":  "loomy/DeepSeek-V4.1-Flash",
//	 "002/deepseek-v4.1-flash-1017": "csg/deepseek-v4.1-flash-1017"}
//
// ⇒ 一个成一个不成，说明匹配逻辑对某个形态失效。
package server

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestPublicNameForExact 精确匹配：内部名与映射里的 target 逐字相同。
func TestPublicNameForExact(t *testing.T) {
	m := map[string]string{
		"001/DeepSeek-V4.1-Flash":      "loomy/DeepSeek-V4.1-Flash",
		"002/deepseek-v4.1-flash-1017": "csg/deepseek-v4.1-flash-1017",
	}
	for internal, want := range map[string]string{
		"loomy/DeepSeek-V4.1-Flash":    "001/DeepSeek-V4.1-Flash",
		"csg/deepseek-v4.1-flash-1017": "002/deepseek-v4.1-flash-1017",
	} {
		if got := publicNameFor(m, internal); got != want {
			t.Errorf("★ publicNameFor(%q) = %q，期望 %q", internal, got, want)
		}
	}
}

// TestPublicNameForNoMap 没有映射时原样返回。
func TestPublicNameForNoMap(t *testing.T) {
	if got := publicNameFor(nil, "csg/x"); got != "csg/x" {
		t.Errorf("nil 映射应原样返回，实际 %q", got)
	}
	if got := publicNameFor(map[string]string{}, "csg/x"); got != "csg/x" {
		t.Errorf("空映射应原样返回，实际 %q", got)
	}
}

// TestSplitModelOnChannelStyleName ⚠ 这条是诊断用的。
//
// `csg/deepseek-v4.1-flash-1017` 里的 `csg` 是**渠道名**，不是 gateway 认识的
// provider 前缀。`SplitModel` 对它的解析结果决定了"裸名兜底"能不能命中。
func TestSplitModelOnChannelStyleName(t *testing.T) {
	for _, name := range []string{
		"loomy/DeepSeek-V4.1-Flash",
		"csg/deepseek-v4.1-flash-1017",
		"deepseek-v4.1-flash-1017",
	} {
		p, m, hasPrefix := gateway.SplitModel(name)
		t.Logf("SplitModel(%q) = provider=%q model=%q hasPrefix=%v", name, p, m, hasPrefix)
	}
}

// TestPublicNameForBareFallback 裸名兜底：list 里的 id 与映射 target 前缀不同但裸名相同。
//
// 这是最常见的真实形态：上游目录给的 id 是裸名，而用户在映射里填的是带前缀的名字
// （或者反过来）。
func TestPublicNameForBareFallback(t *testing.T) {
	m := map[string]string{"002/x": "csg/deepseek-v4.1-flash-1017"}
	// 内部名是裸名 ⇒ 应能命中映射里带前缀的 target
	if got := publicNameFor(m, "deepseek-v4.1-flash-1017"); got != "002/x" {
		t.Errorf("★ 裸名兜底失败：publicNameFor(裸名) = %q，期望 002/x", got)
	}
}
