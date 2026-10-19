// Package modelvis 计算"对外可见的模型列表"。
//
// 这是 R1（对外模型可见性）的规则本身，刻意做成**纯函数**：不碰 HTTP、不读配置、不查库，
// 输入四个值 → 输出一个列表。理由是这个规则将来会被三处用到（/v1/models、管理端的"对外模型"
// 页签、以及"这条请求的模型允不允许"的准入判断），如果把它写进 handler 里，
// 三处会各写一遍、然后各偏一点。
//
// # 规则（对标 new-api 的两段式，默认收紧）
//
//  1. 候选 = **能力索引**里"至少有一个启用渠道能服务"的模型（new-api 的 GetGroupEnabledModels）。
//     空 ⇒ 对外**空**（不是"全部"）：宁可少给，不能漏给 —— 漏给的后果是"我不想公开的模型被扫出来"。
//  2. 令牌维度：未设白名单 / 白名单为空 = 不限制；非空 = **只允许**列表里的（三态语义，
//     与 internal/server/apikey_models.go 的既有口径一致）。
//  3. 名字用**公开名**：能力索引里存的是上游真实名，输出前用反向映射换成公开名；
//     没有反向项的模型**原样输出**（new-api 也是这个行为：映射只对表里列出的名字生效）。
//
// ⚠ 白名单按**公开名**解释（用户是在控制台里挑的对外名字）。这条口径必须与前端"对外模型"
// 页签展示的名字一致，否则会出现"勾了却看不到"。
package modelvis

import "sort"

// PublicModels 计算对外可见的模型名（已应用反向映射、去重、升序）。
//
//   - abilities：能力索引里的模型名（上游真实名），即"启用渠道声明并且真有能力"的那些
//   - whitelist：令牌的模型白名单（按**公开名**解释）
//   - restricted：true = 只允许 whitelist 里的；false = 不限制（未设或为空）
//   - reverse：上游真实名 → 公开名 的反向映射（可为 nil）
func PublicModels(abilities, whitelist []string, restricted bool, reverse map[string]string) []string {
	if len(abilities) == 0 {
		// 规则 1：没有任何启用渠道能服务 ⇒ 对外空。**这一条是"默认收紧"的本体**，
		// 不能写成"没有能力记录就放行全部"。
		return []string{}
	}

	allow := map[string]bool{}
	if restricted {
		for _, w := range whitelist {
			if w != "" {
				allow[w] = true
			}
		}
		if len(allow) == 0 {
			// restricted 但白名单实际为空：这是调用方传错了（三态语义下"空"应表达为 restricted=false）。
			// 按最保守的方式处理 —— 不放行任何东西。
			return []string{}
		}
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(abilities))
	for _, real := range abilities {
		if real == "" {
			continue
		}
		name := real
		if pub, ok := reverse[real]; ok && pub != "" {
			name = pub // 规则 3：有反向项就用公开名
		}
		if restricted && !allow[name] {
			// 注意这里比对的是**公开名**：白名单是用户按对外名字挑的。
			continue
		}
		if seen[name] {
			continue // 两个上游模型映射到同一个公开名 ⇒ 只列一次
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// VisibleForRequest 回答"这次请求要的模型名（公开名）允不允许"。
//
// 与 PublicModels 同一套规则，但只判一个名字 —— 请求路径上不该为了判一个模型去算整张列表。
// 返回 false 时调用方应回 404/403（与 new-api 的 channel filter 语义一致：模型不在可见集内直接拒）。
func VisibleForRequest(abilities, whitelist []string, restricted bool, reverse map[string]string, want string) bool {
	if want == "" {
		return false
	}
	for _, m := range PublicModels(abilities, whitelist, restricted, reverse) {
		if m == want {
			return true
		}
	}
	return false
}
