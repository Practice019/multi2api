// models.go 静态模型兜底表 + 别名归一。
//
// # 为什么需要静态兜底
//
// Models() 实时拉 GET {base}/models；网络抖动/账号临时不可用时若返回空，
// /v1/models 整片消失、客户端判定"模型不存在"（loomy/trae 同一条判据）。
//
// 清单权威=活查 models.dev + 官方定价页（评审报告 §2.1，2026-09 快照）：
// v2.5 系 2026-10-21 弃用 → 动态目录优先，本表只做断网兜底并保留注记。
// 长上下文是**字面 id** `mimo-v2.5-pro[1m]`（不是后缀参数）。
package mimo

import "strings"

// staticModels 静态快照（paid 面 chat 模型；TTS/ASR 非 chat 不进本表）。
var staticModels = []ModelInfo{
	{ID: "mimo-v2.6-pro", Name: "MiMo V2.6 Pro"},
	{ID: "mimo-v2.6-flash", Name: "MiMo V2.6 Flash"},
	{ID: "mimo-v2.6-pro-ultraspeed", Name: "MiMo V2.6 Pro UltraSpeed"},
	{ID: "mimo-v2.5-pro[1m]", Name: "MiMo V2.5 Pro (1M ctx)"},
	{ID: "mimo-v2.5-pro", Name: "MiMo V2.5 Pro"}, // 2026-10-21 弃用
	{ID: "mimo-v2.5", Name: "MiMo V2.5"},         // 2026-10-21 弃用
}

// aliasModel 三方生态里常见的错误写法 → 字面正确 id（导入与请求都归一）。
//
// 只做**目录确有对应物**的别名；不做 gpt-4o→MiMo-X-Pro-Preview 那类
// 劫持别名（MiMo2API 私货，官方无据且毁语义）。
func aliasModel(id string) string {
	lower := strings.ToLower(strings.TrimSpace(id))
	switch lower {
	case "mimo-2.5-pro", "mimo-v2.5-pro-1m":
		return "mimo-v2.5-pro[1m]"
	}
	if strings.HasSuffix(lower, "-1m") {
		if base := strings.TrimSuffix(lower, "-1m"); strings.HasPrefix(base, "mimo-") {
			return base + "[1m]"
		}
	}
	return id
}
