// anthro_limit.go 把「模型输出上限」喂给 Anthropic 转换层。
//
// # 为什么需要这一层
//
// Anthropic 协议的 max_tokens 是必填，而 OpenAI 请求可以不传。填补时需要
// **模型真能输出的上限**，不是任意「够用就好」的小值 ——
// 写死小值会让长输出被静默截断，而推理模型的 reasoning 也算在
// completion_tokens 里，于是表现为「想了很久、正文为空」（用户实测踩过）。
//
// 数据来源就在本包：fetchDynamicModels 的缓存里每个模型都带 MaxTokens。
// 但 anthroconv 是**共享协议转换层**，不能反向依赖 server（会成环）。
// 所以由本包在装配时把查询函数**注入**过去（与 DefaultUpstream 同一手法）。
package server

import (
	"strings"

	"workbuddy2api/internal/anthroconv"
)

// RegisterAnthroOutputLimit 把本 Handler 的模型上限查询注入 anthroconv。
//
// 必须在 Handler 构造完成之后调用（装配层负责）。
//
// 为什么不自动在 New() 里做：New 里还没有模型目录（要等第一轮拉取），
// 而注入的语义是「问这个 Handler」，调用时机由装配层决定更清楚。
func (h *Handler) RegisterAnthroOutputLimit() {
	anthroconv.SetOutputLimitLookup(func(model string) int {
		return h.modelMaxOutput(model)
	})
}

// modelMaxOutput 按模型名查输出上限（0 = 未知）。
//
// 匹配顺序（宽松到严格）：
//
//	① 完整名（带上游前缀，如 workbuddy/deepseek-v4.1-flash）
//	② 裸名（剥掉前缀后的那一截）
//
// 为什么两种都要试：调用方传进来的可能是任一种 —— 出口层改写过 model
// 字段（转成上游真名），而上游自己也可能带前缀（monkeycode-basic/xxx）。
// 只试一种会漏掉另一类，表现为「有的模型有上限、有的永远走兜底」。
func (h *Handler) modelMaxOutput(model string) int {
	model = strings.TrimSpace(model)
	if model == "" {
		return 0
	}
	infos := h.fetchDynamicModels()
	if len(infos) == 0 {
		return 0
	}
	bare := model
	if i := strings.IndexByte(model, '/'); i >= 0 {
		bare = model[i+1:]
	}
	// ① 完整名
	for _, mi := range infos {
		if mi.ID == model && mi.MaxTokens > 0 {
			return int(mi.MaxTokens)
		}
	}
	// ② 裸名（也接受「上游名/裸名」这种上游侧的完整形态）
	for _, mi := range infos {
		id := mi.ID
		if j := strings.IndexByte(id, '/'); j >= 0 {
			id = id[j+1:]
		}
		if id == bare && mi.MaxTokens > 0 {
			return int(mi.MaxTokens)
		}
	}
	return 0
}
