// models.go Qoder 的模型表（**静态表，恒不发网络请求**）。
//
// # 为什么是静态表（与其它上游相反）
//
// 远端 `GET /algo/api/v2/model/list` **需要 WASM 签名**，故 listModels
// **不发网络请求** —— 表就是权威。参照项目的注释也写明这一点。
//
// 表里是**目录 key**（`qfmodel` / `dmodel` 这类），它们只在**加密端点**可用：
//
//	加密端点  认目录 key        ← 我们走这条
//	公开端点  认通用名（qwen-flash），目录 key 一律 `Unsupported model`
//
// ⚠ 改表必须逐个实发验证可推理，不能只照抄目录 key。
//
// # ⚠ 中国版**不能沿用**国际版这张表
//
// 实测差异（2026-09-27，本机 CN catalog-v6 的 chat 场景）：
//
//	CN 独有 q37fmodel / gm51model
//	CN 没有 ultimate / performance / efficient / smodel / cmodel
//	  —— 沿用会让菜单出现 5 个 CN 端点根本不认的模型，点了就报错
//	5 条上下文窗口、4 条思考标记、1 条 vl 标记不同
//	mmodel 在 CN 是 MiniMax-M2.7（国际版 M3）
//
// ⚠ 国际版曾因「手工估值 + 单测只断言 id 列表」让价格漂移长期未被发现
// （14 个模型有偏差，用户报障）。改本表必须重新跑探针对照。
package qoder

// Model 一个目录条目。
type Model struct {
	// ID 目录 key（**加密端点认这个**）。
	ID string
	// Name 展示名。
	Name string
	// ContextWindow 上下文窗口。
	ContextWindow int
	// SupportsImage 是否支持图片。
	SupportsImage bool
	// SupportsThinking 是否支持思考。
	SupportsThinking bool
	// PriceFactor 当前价（0 = 免费，**是合法值**，不能用 > 0 过滤）。
	PriceFactor float64
	// OriginalPriceFactor 原价（有促销时才有意义）。
	OriginalPriceFactor float64
	// IsFree 免费额度模型（e2e 探针默认用它们以免消耗积分）。
	IsFree bool
	// Efforts 支持的思考档位。
	Efforts []string
}

// qoderModels 国际版目录（17 条）。
var qoderModels = []Model{
	{ID: "auto", Name: "Auto", ContextWindow: 200_000, SupportsImage: true, PriceFactor: 0.5},
	{ID: "ultimate", Name: "Ultimate", ContextWindow: 1_000_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 2, Efforts: []string{"xhigh", "high", "low", "max", "medium"}},
	{ID: "performance", Name: "Performance", ContextWindow: 1_000_000, SupportsImage: true,
		PriceFactor: 1.1, Efforts: []string{"xhigh", "high", "low", "max", "medium"}},
	{ID: "efficient", Name: "Efficient", ContextWindow: 200_000, SupportsImage: true, PriceFactor: 0.3},
	{ID: "smodel", Name: "Sonus", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 8, Efforts: []string{"xhigh", "high", "low", "max", "medium"}},
	{ID: "cmodel", Name: "Cantus", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 4, Efforts: []string{"xhigh", "high", "low", "max", "medium"}},
	{ID: "qmodel_38max", Name: "Qwen3.8-Max", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, IsFree: true, PriceFactor: 0.2,
		Efforts: []string{"xhigh", "low", "medium"},
		// 错峰 4 折（窗口 22:00–08:00 Asia/Singapore）
		OriginalPriceFactor: 0.5},
	{ID: "qfmodel", Name: "Qwen3.8-Flash", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, IsFree: true,
		// ⚠ priceFactor: 0 是**免费**，不是缺失
		PriceFactor: 0, OriginalPriceFactor: 0.1, Efforts: []string{"xhigh", "low", "medium"}},
	{ID: "qmodel_latest", Name: "Qwen3.7-Max", ContextWindow: 1_000_000, SupportsImage: true},
	{ID: "qmodel", Name: "Qwen3.7-Plus", ContextWindow: 1_000_000, SupportsImage: true},
	{ID: "kmodel_latest", Name: "Kimi-K3", ContextWindow: 180_000, SupportsImage: true,
		PriceFactor: 1.4, Efforts: []string{"high", "low", "max"}},
	{ID: "kmodel", Name: "Kimi-K2.8-Preview", ContextWindow: 200_000, SupportsImage: true,
		PriceFactor: 0.8, Efforts: []string{"high", "low", "max"}},
	{ID: "gmodel", Name: "GLM-5.3", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.8, Efforts: []string{"high", "low", "max"}},
	{ID: "gfmodel", Name: "GLM-5.3-Flash", ContextWindow: 1_000_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.1, Efforts: []string{"high", "max"}},
	{ID: "dmodel", Name: "DeepSeek-V4-Pro", ContextWindow: 1_000_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.5, Efforts: []string{"high", "max"}},
	{ID: "dfmodel", Name: "DeepSeek-Flash", ContextWindow: 1_000_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.1, Efforts: []string{"high", "max", "low"}},
	{ID: "mmodel", Name: "MiniMax-M3", ContextWindow: 180_000, SupportsImage: true, PriceFactor: 0.2},
}

// qoderCNModels 中国版目录（14 条）。
//
// ⚠ 与国际版的差异见本文件头注释 —— 这是**实测**数据，不是推断。
var qoderCNModels = []Model{
	{ID: "auto", Name: "Auto", ContextWindow: 200_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.5},
	{ID: "qmodel_38max", Name: "Qwen3.8-Max", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, IsFree: true, PriceFactor: 0.2,
		OriginalPriceFactor: 0.5, Efforts: []string{"xhigh", "low", "medium"}},
	{ID: "qfmodel", Name: "Qwen3.8-Flash", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, IsFree: true,
		PriceFactor: 0, OriginalPriceFactor: 0.1, Efforts: []string{"xhigh", "low", "medium"}},
	// ⚠ CN 的上下文窗口是 180_000（国际版是 1_000_000），思考标记也不同
	{ID: "qmodel_latest", Name: "Qwen3.7-Max", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true},
	{ID: "qmodel", Name: "Qwen3.7-Plus", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true},
	// CN 独有
	{ID: "q37fmodel", Name: "Qwen3.7-Flash", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.1},
	// ⚠ CN 的 dmodel 窗口是 96_000（国际版 1_000_000）
	{ID: "dmodel", Name: "DeepSeek-V4-Pro", ContextWindow: 96_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.5, Efforts: []string{"high", "max"}},
	{ID: "dfmodel", Name: "DeepSeek-Flash", ContextWindow: 180_000, SupportsImage: true,
		PriceFactor: 0.1, Efforts: []string{"high", "max", "low"}},
	{ID: "gmodel", Name: "GLM-5.3", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.8, Efforts: []string{"high", "low", "max"}},
	{ID: "gfmodel", Name: "GLM-5.3-Flash", ContextWindow: 1_000_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.1, Efforts: []string{"high", "max"}},
	// CN 独有
	{ID: "gm51model", Name: "GLM-5.2", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.6, Efforts: []string{"high", "max"}},
	{ID: "kmodel_latest", Name: "Kimi-K3", ContextWindow: 180_000, SupportsImage: true,
		PriceFactor: 1.4, Efforts: []string{"high", "low", "max"}},
	// ⚠ CN 的 kmodel 窗口是 180_000（国际版 200_000），且支持思考
	{ID: "kmodel", Name: "Kimi-K2.8-Preview", ContextWindow: 180_000, SupportsImage: true,
		SupportsThinking: true, PriceFactor: 0.8, Efforts: []string{"high", "low", "max"}},
	// ⚠ CN 是 MiniMax-M2.7（国际版是 M3）
	{ID: "mmodel", Name: "MiniMax-M2.7", ContextWindow: 180_000, PriceFactor: 0.2},
}

// ModelsFor 返回某个产品的目录（副本，调用方可安全改写）。
func ModelsFor(productID string) []Model {
	src := qoderModels
	if productID == ProviderIDCN {
		src = qoderCNModels
	}
	out := make([]Model, len(src))
	copy(out, src)
	return out
}

// FindModel 按目录 key 找模型。
func FindModel(productID, modelID string) (Model, bool) {
	for _, m := range ModelsFor(productID) {
		if m.ID == modelID {
			return m, true
		}
	}
	return Model{}, false
}
