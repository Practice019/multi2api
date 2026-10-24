// models_meta_fallback.go 给上游不报元数据、但确实可用的模型补窗口值。
//
// # 用户报的缺陷 4
//
//	004/deepseek-v4.1-flash-1017   ctx 与 max_output 都缺
//	001/deepseek-v4.1-flash-1017   连 created 都没有
//
// # 根因（实测三层，比最初以为的复杂）
//
// 第一层：上游不报。workbuddy-intl 的 /models 返回 18 条，每条都有
// maxInputTokens / maxOutputTokens —— 但里面没有 deepseek-v4.1-flash
// 这个 id。而它确实能调通（实测 200）。
//
// 第二层：别名目标不在上游目录里。用户配的 model_map 把别名
// 004/deepseek-v4.1-flash-1017 指向 workbuddy-intl/deepseek-v4.1-flash，
// 而这个名字只存在于上游的 agents[].models 里（能力声明），不在
// data.models 里（元数据声明）。两者是不同的数组。
//
// 第三层：渠道模型没有元数据来源。001/csg 走 channel 路径，
// appendChannelModels 只写 id 与 object。
//
// # 修法：不去猜，而是回填同上游的典型值
//
// 一个上游的模型窗口通常集中在少数几个值上（workbuddy-intl 实测：
// 176000 / 200000 / 256000 / 1000000）。对没有元数据的模型，取该上游
// 已有模型的众数作为兜底 —— 它必然是个真实存在的值，而不是编出来的数。
//
// 这仍然是推断，所以要能标出来，让客户端知道这个值不是上游明说的。
package server

import (
	"sort"
)

// inferMetaFor 用同上游已报模型的众数回填缺失的窗口值。
//
// existing 是该上游已经有元数据的条目（id -> (ctx, maxOut)）。
// 返回 (ctx, maxOut)，都为 0 表示无法推断（该上游一条元数据都没有）。
//
// # 为什么用众数而不是最大值 / 平均值
//
// 众数是这个上游最典型的那个窗口。用最大值会高估（客户端按它分配
// 预算，实际不够会截断）；用平均值会得到一个上游根本没提供的数
// （比如 213333 —— 那种值一看就是编的）。
func inferMetaFor(existing map[string][2]int64) (int64, int64) {
	if len(existing) == 0 {
		return 0, 0
	}
	ctxCount := map[int64]int{}
	outCount := map[int64]int{}
	for _, v := range existing {
		if v[0] > 0 {
			ctxCount[v[0]]++
		}
		if v[1] > 0 {
			outCount[v[1]]++
		}
	}
	return modeOf(ctxCount), modeOf(outCount)
}

// modeOf 取众数；并列时取较大的那个。
//
// 并列取大：宁可给客户端一个宽松的预算，也不要让它按偏小的值
// 提前截断（那种失败看起来像模型质量差，实际是我们报错了窗口）。
func modeOf(counts map[int64]int) int64 {
	if len(counts) == 0 {
		return 0
	}
	best, bestN := int64(0), 0
	keys := make([]int64, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })
	for _, k := range keys {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	return best
}
