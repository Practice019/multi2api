package gateway

// 目录与倍率的中立词汇表。
//
// # 为什么这两个类型必须住在 gateway（而不是某个上游包里）
//
// 它们原先住在 `internal/upstream`（workbuddy 的私有 SDK）里，名字叫
// ModelCatalog / ModelCatalogEntry。但**消费它们的是核心**：
//
//	internal/admin  的 modelsPreview / modelMultipliers（倍率展示）
//	internal/server 的 modelCatalogFor（缓存 + 多上游合并）
//
// 于是核心包为了拿到"模型目录"这个跨上游共识的概念，被迫 import 了
// 某一个上游的 SDK —— 直接打破「加新上游核心零改动」。
//
// 判据是"概念归谁"：**"一个上游提供一份模型目录（含成本倍率）"是跨上游
// 共识**（每个上游都有，只是数据源不同：workbuddy 在 /v3/config，
// codearts 在官方下拉配置，loomy 在模型名后缀），所以类型属于接缝。
// 而"怎么从 /v3/config 解析出来"是 workbuddy 的事实，留在它自己包里。
//
// # 与 ModelMultiplierExt 的关系
//
// 两者是**同一件事的两个入口**：
//
//	ModelMultiplierExt.ModelMultipliers(ctx, cred)  → map[id]倍率（上游自报，轻量）
//	ModelCatalog                                     → 同数据的富形态（含展示名等）
//
// 核心优先走扩展点（更轻、且各上游数据源官方），拿不到才用目录。
// 这也解释了为什么上游**不需要**实现"返回 ModelCatalog"——
// 它只需报告倍率，富形态由核心按需拼装。

// ModelCatalogEntry 模型目录的一个条目。
//
// 字段取舍：只保留**跨上游有共识**的。展示名/厂商/标签在各上游口径不一，
// 因此都标了 omitempty；核心当前只消费 ID 与 Multiplier 两项
// （倍率展示与成本归因）。
type ModelCatalogEntry struct {
	ID string `json:"id"`
	// Name 展示名。上游没给时为空。
	Name string `json:"name,omitempty"`
	// Vendor 厂商。上游没给时为空。
	Vendor string `json:"vendor,omitempty"`
	// Tags 能力/分组标签。上游没给时为空。
	Tags []string `json:"tags,omitempty"`
	// CreditsRaw 上游原样倍率串（如 "x0.51 credits"），仅用于排查。
	//
	// ⚠ 与 Multiplier 并存是刻意的：当上游换了格式导致 Multiplier 解析成 0 时，
	// 日志里能看到原文，否则只能看到一排 0 猜原因。
	CreditsRaw string `json:"credits,omitempty"`
	// Multiplier 解析后的成本系数。0 表示"免费或未知"。
	//
	// ⚠ 0 有歧义（上游没下发 vs 真的免费），所以它**不进** MultiplierTable()
	// —— 见该方法的注释。需要区分时用 MultiplierKnown。
	Multiplier float64 `json:"-"`
	// MultiplierKnown 区分"上游明确给了系数（含 0=免费）"与"上游没给"。
	//
	// 这是把 Multiplier 的 0 歧义显式化的唯一手段。不导出到 JSON：
	// 它是来源侧的事实，不属于对外契约。
	MultiplierKnown bool `json:"-"`
	// MaxInputTokens 上下文窗口。未知为 0。
	MaxInputTokens int64 `json:"maxInputTokens,omitempty"`

	SupportsToolCall  bool `json:"supportsToolCall,omitempty"`
	SupportsImages    bool `json:"supportsImages,omitempty"`
	SupportsReasoning bool `json:"supportsReasoning,omitempty"`
}

// ModelCatalog 一份模型目录快照。
//
// 用命名类型（而不是裸 []ModelCatalogEntry）是为了能加字段
// （如 fetchedAt）而不破坏调用方签名。
type ModelCatalog struct {
	Models []ModelCatalogEntry
}

// MultiplierTable 返回 id → 系数的映射。
//
// # 只收 Multiplier > 0 的条目（**不是** MultiplierKnown）
//
// 让调用方用 map 的 comma-ok 区分"没这个模型"与"这个模型 0 系数"更容易出错，
// 所以表里只放可用系数。需要原始列表（含 0 与未知）时直接读 ModelCatalog.Models
// —— 这正是 admin 的 modelsPreview 保留 0 倍率（x0.00 是"免费"这个有意义的事实）
// 时走的路径。
//
// # 空结果一律返回 nil（统一契约）
//
// 曾经用 `len(mc.Models) == 0` 判空，于是"没有模型"返回 nil、
// "有模型但系数全是 0"返回非 nil 空 map —— 两个同样"空"的结果对
// `if tbl == nil` 含义不同，调用方会写出只在其中一种情况下正确的分支。
// 现在按**结果**判空：收完若一个都没进，返回 nil。
func (mc *ModelCatalog) MultiplierTable() map[string]float64 {
	if mc == nil {
		return nil
	}
	out := make(map[string]float64, len(mc.Models))
	for _, m := range mc.Models {
		if m.ID != "" && m.Multiplier > 0 {
			out[m.ID] = m.Multiplier
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Multiplier 查询单个模型的成本系数；不存在或系数为 0 时返回 (0, false)。
//
// 线扫 O(n)：真实目录只有几十个模型，且调用点在每次请求里至多几次，
// 不值得为它维护一份缓存表（那会引入「缓存与 Models 不同步」的新失败模式）。
// 需要批量查时用 MultiplierTable 一次建表。
func (mc *ModelCatalog) Multiplier(modelID string) (float64, bool) {
	if mc == nil {
		return 0, false
	}
	for _, m := range mc.Models {
		if m.ID == modelID {
			if m.Multiplier <= 0 {
				return 0, false
			}
			return m.Multiplier, true
		}
	}
	return 0, false
}

// CatalogOf 从倍率表拼一份最小目录（只有 ID 与 Multiplier）。
//
// 用途：核心从 ModelMultiplierExt 拿到 map 后，要喂给只认 ModelCatalog 的
// 既有路径（缓存、admin 的展示）。把这次拼装收在这里，避免每个调用方
// 各写一遍 —— 也避免"某处忘了填 MultiplierKnown"这类分叉。
func CatalogOf(m map[string]float64) *ModelCatalog {
	if len(m) == 0 {
		return nil
	}
	out := &ModelCatalog{Models: make([]ModelCatalogEntry, 0, len(m))}
	for id, mult := range m {
		out.Models = append(out.Models, ModelCatalogEntry{
			ID: id, Multiplier: mult, MultiplierKnown: true,
		})
	}
	return out
}
