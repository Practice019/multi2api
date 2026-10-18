package server

import (
	"context"
	"strings"
)

// upstream_alt.go 「同一个模型、换一个上游」的支持代码（2026-10-18）。
//
// # 为什么需要它（用户原话与实测数据）
//
// 用户："我的要求很简单…保证多账号正常的使用，自动轮询，保证性能请求速度" +
//      "我希望可以让轮询变得和使用单个账号一样就行了"。
// ⚠ 而实测把问题指得很清楚：
//
//	loomy        失败率 15.22%（10-17 那天 26.1%）
//	workbuddy    失败率  0.13%
//	⚠ 而**两家提供的是同一个模型**（见 normModelKey 的实测记录）
//
// ⇒ 也就是说：**我们要的东西就在隔壁那家上游**，只是请求被模型前缀钉在了坏的那家上
//   （`loomy/DeepSeek-V4.1-Flash` 里的 `loomy/` 就是路由记号）。
// ⇒ 所以"上游整体不可用时，自动去试另一个有同一模型的上游"是这里**收益最大**的一步：
//   它把一个 15% 失败率的请求变成 0.13% 失败率的请求（约 **117 倍**）。
//
// ⚠ 与"上游级熔断"（pool 的 provState）的分工：
//   熔断负责**即时知道"这家不通"**（不必等 44 个号逐个熔断）；
//   本文件负责**既然这家不通，那就换一家**。
//   ⚠ 只有熔断没有本文件，等于"更快地失败" —— 那不是用户要的。

// normModelKey 把模型名归一成**跨上游可比**的键。
//
// 规则（实测三家都能对上）：**去上游前缀 → 转小写 → 只保留字母与数字**。
//
// 实测记录（2026-10-18，取自 request-log 里出现过的模型名）：
//
//	loomy/DeepSeek-V4.1-Flash            → deepseekv41flash
//	workbuddy/deepseek-v4.1-flash        → deepseekv41flash
//	workbuddy-intl/deepseek-v4.1-flash   → deepseekv41flash
//
// ⚠ 注意大小写与分隔符都要吃掉：三家的差异**只在**大小写和 `.`/`-` 上。
// ⚠ 归一失败（空串）时返回空串，调用方据此放弃匹配 —— 宁可不动，也不要乱配。
func normModelKey(model string) string {
	// 去掉上游前缀（`loomy/xxx` → `xxx`）。⚠ 只用**第一个**斜杠：
	// 有些上游的模型名里自带斜杠（如 `cline/cline-free/deepseek-v4.1-flash`），
	// 去前缀后还要保留后面那段。
	if i := strings.IndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	var b strings.Builder
	b.Grow(len(model))
	for _, r := range strings.ToLower(model) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// alternateUpstream 找一个"有同一个模型"的**其它**上游，返回它以及该上游的模型名。
//
// ⚠ 返回值第二个必须是**那个上游的真实模型名**：换上游之后请求体里的 model 也得换
//
//	（`workbuddy/deepseek-v4.1-flash` 与 `loomy/DeepSeek-V4.1-Flash` 是两个不同的字符串）。
//	带着别人的名字发过去，上游会按"未知模型"拒绝 —— 那正是 providerFor 的注释里
//	记着的那类白跑（"带着前缀发过去，上游按未知模型拒绝，表现为一次白跑的 503 + 换号"）。
//
// ⚠ 找不到就返回空串对，调用方**保持原样**（不要瞎换一家不认识的）。
func (h *Handler) alternateUpstream(model, current string) (string, string) {
	if h.cfg.Provider == nil {
		return "", ""
	}
	key := normModelKey(model)
	if key == "" {
		return "", ""
	}
	for _, id := range h.otherProviders(current) {
		infos, ok := h.cfg.Provider.Models(context.Background(), id)
		if !ok || len(infos) == 0 {
			continue // 这个上游给不出目录：跳过（与 modelList 的同一条取舍）
		}
		for _, mi := range infos {
			if normModelKey(mi.ID) == key {
				return id, mi.ID
			}
		}
	}
	return "", ""
}
