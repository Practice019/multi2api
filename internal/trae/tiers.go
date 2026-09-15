// tiers.go 模型分档 + 排队检测 + 自动降级。
//
// # 为什么要有它（用户本轮要求，复现自 trae-local-api 的 model-config.json）
//
// TRAE SOLO 的请求会**排队**：上游在 SSE 里发 `request_wait_in_queue`
// 事件（data 带 position），排队位置很高时响应要等很久。trae-local-api
// 的做法是：排队位置超过阈值 → 换同档/下一档模型重发，直到不排队。
//
// 本实现把它收敛成：
//
//	fallbackChain(model, n)  → [原模型, 同档其它, 下一档..., 兜底]，至多 n 项
//	convertSOLOWithQueue     → 转换 SSE 时看排队位置，超阈值且还没出内容就
//	                           放弃当前流（返回 position），由 Chat 的循环换模型重试
//
// 判据全部来自上游事件（position），核心零改动；档位表来自实测
// （trae-local-api 的 model-config.json，按 SOLO config_name）。
package trae

import "strings"

// defaultQueueThreshold / defaultMaxAttempts 排队降级的默认参数。
const (
	defaultQueueThreshold = 300
	defaultMaxAttempts    = 3
)

// tierOrder T1（最强）→ T5（最轻），与 trae-local-api 的 tiers 一致。
//
// 注意：这里是 **SOLO config_name**（llm_utils_chat 直接可用的模型名），
// 不是 Claude 别名 —— 别名映射是另一层（本网关不需要，客户端自己选模型）。
var tierOrder = []string{
	"glm-5.2",                                                  // T1 旗舰
	"glm-5.1", "qwen-3.7-plus", "kimi-k2.6", "DeepSeek-V4-Pro", // T2 强力
	"glm-5", "qwen-3.6-plus", "minimax-m3", "DeepSeek-V4-Flash", "glm-5v-turbo", // T3 中等
	"glm-4.7", "kimi-k2", "qwen3-coder", "minimax-m2.7", "Doubao-Seed-2.0-Code", // T4 轻量
	"glm-4.6", "Doubao_1_6", "minimax-m2.1", "minimax-m2", // T5 最轻
}

// tierRanges 各档在 tierOrder 里的下标区间 [start, end)（档位大小不一，不能均分）。
var tierRanges = [][]int{
	{0, 1},   // T1
	{1, 5},   // T2
	{5, 10},  // T3
	{10, 15}, // T4
	{15, 19}, // T5
}

// tierIndexOf 下标 → 档位（1..5），未知返回 0。
func tierIndexOf(i int) int {
	for t, r := range tierRanges {
		if i >= r[0] && i < r[1] {
			return t + 1
		}
	}
	return 0
}

// tierOf 返回模型所在档位（1..5），未知返回 0。
func tierOf(model string) int {
	for i, m := range tierOrder {
		if strings.EqualFold(m, model) {
			return tierIndexOf(i)
		}
	}
	return 0
}

// fallbackModel 所有档位都排队时的兜底模型（实测可用）。
const fallbackModel = "glm-5"

// fallbackChain 生成降级候选链：[原模型, 同档其它, 下一档…, 兜底]。
//
//   - 未知模型 → [model, 兜底]
//   - 档位内顺序按 tierOrder 的先后（同档里排在后面的当备选）
//   - 链长封顶 maxAttempts（>=1）
func fallbackChain(model string, maxAttempts int) []string {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	chain := make([]string, 0, maxAttempts)
	seen := map[string]bool{}
	add := func(m string) {
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		if len(chain) < maxAttempts {
			chain = append(chain, m)
		}
	}
	add(model)

	idx := indexOfModel(model)
	t := tierIndexOf(idx)
	if t == 0 {
		add(fallbackModel)
		return chain
	}
	r := tierRanges[t-1]
	// 同档其它模型（排在 model 后面的先试）。
	for i := idx + 1; i < r[1]; i++ {
		add(tierOrder[i])
	}
	// 下一档 → 更下一档。
	for nt := t; nt < len(tierRanges); nt++ {
		for i := tierRanges[nt][0]; i < tierRanges[nt][1]; i++ {
			add(tierOrder[i])
		}
	}
	// 兜底
	add(fallbackModel)
	return chain
}

// indexOfModel 返回模型在 tierOrder 里的下标（未知返回 -1）。
func indexOfModel(model string) int {
	for i, m := range tierOrder {
		if strings.EqualFold(m, model) {
			return i
		}
	}
	return -1
}

// modelOf 从请求体里取模型名（缺失回默认）。
func modelOf(body []byte) string {
	m := extractModel(body)
	if m == "" {
		return DefaultConfigName
	}
	return m
}
