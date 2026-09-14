// models.go Loomy 的模型目录。
//
// # 目录从哪来
//
// 手册第 4.1 节记录了一次完整的 `GET /v1/models` 实测（12 条）。本文件是
// 那份实测的快照，字段含义与 gateway.ModelInfo 对齐：
//
//	ID / ContextWindow / MaxOutputTokens
//
// # 为什么保留静态快照，而不是只靠实时拉取
//
// 两条理由，一条工程的一条现实的：
//
//  1. **可用性**：Models() 是 /v1/models 的数据源。若它只依赖网络，
//     一次上游抖动或 session 失效就会让 loomy 的整片模型从目录里消失，
//     客户端据此判定"模型不存在"并报错 —— 而真相只是这一轮没拉到。
//     静态表保证"目录至少是完整的"，实时结果只用来**更新**它。
//  2. **可核对**：静态表带着实测来源，是能被审计的（哪个模型、多少上下文、
//     当时实测可用还是 404）。纯实时拉取则没有任何"应该是什么"的基线。
//
// 实时优先、静态兜底的组合见 Client.ModelList 与 Provider.Models。
package loomy

import "strings"

// Model 目录里的一个模型。
type Model struct {
	// ID 上游认的模型名。**必须原样**发出去（含大小写与连字符形态：
	// `GLM-5.3-Flash` / `Kimi-k2.6` / `spark-x`）——自造 ID 会被上游拒绝，
	// 手册第 10 节的排错表里有一条正是
	// `Model "xxx" is not supported on this endpoint`。
	ID string
	// ContextWindow 上下文窗口（token）。
	ContextWindow int
	// MaxOutputTokens 单次输出上限（token）。用于裁剪超限的 max_tokens。
	MaxOutputTokens int
	// Unavailable 上游对这个模型返回 404「该模型暂未开放」。
	//
	// 手册实测的两个生图模型就是这种状态。它们仍然留在表里（而不是删掉）：
	// 用户看到 404 时能查到"这个是被上游自己下架的，不是我写错了 ID"，
	// 这个区别在排错时很值钱。
	Unavailable bool
}

// knownModels 实测快照（手册第 4.1 节，2026-09-14）。
//
// 顺序照抄手册的表格顺序，便于人工对照。
var knownModels = []Model{
	{ID: "deepseek-v4-flash-0731", ContextWindow: 1048576, MaxOutputTokens: 384000},
	{ID: "MiniMax-M3", ContextWindow: 1048576, MaxOutputTokens: 512000},
	{ID: "GLM-5.3-Flash", ContextWindow: 1048576, MaxOutputTokens: 131072},
	{ID: "qwen-3.8-max", ContextWindow: 1000000, MaxOutputTokens: 65536},
	{ID: "qwen3.8-flash", ContextWindow: 1000000, MaxOutputTokens: 131072},
	{ID: "qwen3.5-flash", ContextWindow: 1000000, MaxOutputTokens: 65536},
	{ID: "mimo-v2.5", ContextWindow: 1048576, MaxOutputTokens: 131072},
	{ID: "Kimi-k2.6", ContextWindow: 262144, MaxOutputTokens: 65536},
	{ID: "spark-x", ContextWindow: 1048576, MaxOutputTokens: 65536},
	{ID: "doubao-seed-2.0-mini", ContextWindow: 262144, MaxOutputTokens: 131072},

	// 下面两个实测返回 404「该模型暂未开放」——保留在表里但标 Unavailable，
	// 不进目录、也不参与 max_tokens 裁剪（见 Models() 与 clampMaxTokens）。
	{ID: "doubao-seedream-5-lite", ContextWindow: 128000, MaxOutputTokens: 131072, Unavailable: true},
	{ID: "qwen-image-3.0-pro", Unavailable: true},
}

// modelAliases 上游自己认的别名。
//
// 手册第 4.1 节实测：请求 `deepseek-v4.1-flash` → 服务端解析为
// `deepseek-v4-flash-0731`，返回成功。**所以别名不需要我们改写** ——
// 上游认它。本表只在"按模型查输出上限"时用来归一（否则别名请求会因为
// 查不到 entry 而跳过 max_tokens 裁剪），以及给 Models() 打标记。
var modelAliases = map[string]string{
	"deepseek-v4.1-flash": "deepseek-v4-flash-0731",
}

// KnownModels 返回全部已知模型（含不可用的）。
func KnownModels() []Model {
	out := make([]Model, len(knownModels))
	copy(out, knownModels)
	return out
}

// AvailableModels 返回**可用**模型（过滤掉上游 404 的那些）。
//
// cap 与 ContextWindow 为 0 表示未知 —— 契约允许（gateway.ModelInfo 的注释：
// "未知时为 0"）。qwen-image-3.0-pro 就是这样：手册里它的上下文是空的。
func AvailableModels() []Model {
	out := make([]Model, 0, len(knownModels))
	for _, m := range knownModels {
		if m.Unavailable {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ResolveModel 按 ID 或别名查一个模型。
//
// 第二返回值报告"是否经别名命中"，供日志与测试区分：
// 直接命中与别名命中的诊断含义不同（前者说明客户端用的规范名，后者说明
// 客户端在用上游的旧称）。
func ResolveModel(id string) (Model, bool, bool) {
	want := strings.TrimSpace(id)
	if want == "" {
		return Model{}, false, false
	}
	// 精确命中优先：别名表里可能有一个键**同时**是某个真实模型 ID 的情形，
	// 那时应当按真实 ID 解释。
	for _, m := range knownModels {
		if m.ID == want {
			return m, false, true
		}
	}
	if target, ok := modelAliases[want]; ok {
		for _, m := range knownModels {
			if m.ID == target {
				return m, true, true
			}
		}
	}
	return Model{}, false, false
}

// MaxOutputFor 返回某模型（含别名）允许的输出上限；未知返回 0。
func MaxOutputFor(id string) int {
	if m, _, ok := ResolveModel(id); ok {
		return m.MaxOutputTokens
	}
	return 0
}
