// models.go Raccoon 的模型目录：远端优先 + 兜底表。
//
// 兜底表来源：2026-09-26 实测 GET /api/web/llm/v2/model_catalog 取 visible:true 的 6 条。
// **顺序照抄远端返回顺序，不重排**。
//
// ⚠ 兜底表的 name 已含倍率后缀，且**必须与 DisplayName 的输出形态一致** ——
// 否则远端/回退两条路径会显示不同形态。
//
// ⚠ 表里**不含 `Raccoon-Auto`**：它是客户端 i18n 条目渲染的"自动选模"入口，
// 不在远端 model_catalog 里 —— 直接发给 chat/completions 会 **404**。
package raccoon

// Model 目录里的一个模型。
type Model struct {
	// ID 上游认的模型名。
	ID string
	// Name 展示名（**已含倍率后缀**）。
	Name string
	// ContextWindow 上下文窗口（token）。0 = 未知。
	ContextWindow int
	// MaxTokens 单次输出上限（token）。0 = 未知。
	MaxTokens int
	// SupportsImage 是否接受图片输入。
	SupportsImage bool
}

// fallbackModels 兜底静态表（参照 raccoon-product.ts:99-144）。
var fallbackModels = []Model{
	{ID: "sn-sensenova-6-8-flash", Name: "SenseNova-6.8-Flash · 免费",
		ContextWindow: 256_000, MaxTokens: 63_999, SupportsImage: true},
	{ID: "sn-sensenova-6-8-flash-lite", Name: "SenseNova-6.8-Flash-Lite · 免费",
		ContextWindow: 256_000, MaxTokens: 63_999, SupportsImage: true},
	{ID: "sn-glm-5-3", Name: "GLM-5-3 · x0.75",
		ContextWindow: 1_000_000, MaxTokens: 100_000, SupportsImage: true},
	{ID: "sn-kimi-k3", Name: "Kimi-K3 · x1",
		ContextWindow: 1_000_000, MaxTokens: 100_000, SupportsImage: true},
	{ID: "sn-glm-5-3-flash", Name: "GLM-5-3-Flash · x0.2→x0.1",
		ContextWindow: 1_000_000, MaxTokens: 100_000, SupportsImage: false},
	{ID: "sn-deepseek-v4-1-flash", Name: "DeepSeek-V4.1-Flash · x0.25",
		ContextWindow: 1_000_000, MaxTokens: 100_000, SupportsImage: false},
}

// FallbackModels 导出兜底表副本（供装配层与测试使用）。
func FallbackModels() []Model {
	out := make([]Model, len(fallbackModels))
	copy(out, fallbackModels)
	return out
}

// mergeModels 合并远端与兜底（远端优先）。
//
// 远端失败（空切片）时直接用兜底；否则远端条目的元数据覆盖兜底，
// 兜底里有而远端没有的也保留（上游临时少下发不该让模型消失）。
func mergeModels(remote []RemoteModel) []Model {
	if len(remote) == 0 {
		return FallbackModels()
	}
	byID := map[string]Model{}
	order := make([]string, 0, len(remote)+len(fallbackModels))
	// 先放兜底（提供元数据），再让远端覆盖
	for _, f := range fallbackModels {
		byID[f.ID] = f
		order = append(order, f.ID)
	}
	for _, r := range remote {
		base, hasBase := byID[r.ID]
		m := Model{ID: r.ID}
		if hasBase {
			m = base
		}
		// 展示名**总是**由 DisplayName 生成（与兜底表形态一致）
		eff := 0.0
		hasEff := false
		if r.HasEffectiveMult {
			eff = r.EffectiveMult
			hasEff = true
		}
		if hasEff {
			m.Name = DisplayName(r.ID, r.Name, r.BaseMultiplier, eff)
		} else if !hasBase || m.Name == "" {
			// 没有倍率信息：不加后缀（那才是"取不到"的正确形态）
			m.Name = DisplayName(r.ID, r.Name, 0, -1)
		}
		if r.ContextWindow > 0 {
			m.ContextWindow = r.ContextWindow
		}
		if r.MaxTokens > 0 {
			m.MaxTokens = r.MaxTokens
		}
		m.SupportsImage = r.SupportsImage
		if !hasBase {
			// 远端新增的模型：插到末尾，不打扰兜底表的顺序
			order = append(order, r.ID)
		}
		byID[r.ID] = m
	}
	out := make([]Model, 0, len(order))
	seen := map[string]bool{}
	for _, id := range order {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, byID[id])
	}
	return out
}
