// catalog_impact.go 删掉一条渠道会影响哪些公开模型（U-11 验收「删除影响明确」）。
//
// # 它回答什么问题
//
// 运维要删一条渠道时，真正的问题是：**"删了之后有哪些模型会消失？"**
//
// 改造前这个问题的答案散在三处：渠道详情页（它声称能服务哪些模型）、
// 分组编辑器（它在哪些组里）、令牌预览（哪些令牌因此少了模型）。
// 三处都要自己点一遍、并在脑子里求交集 —— 而漏掉一处就是"删完才发现
// 某个客户端挂了"。
//
// # 两类影响必须分开
//
//	LostModels     删掉它就**再也没有供给** ⇒ 该公开模型从目录里消失
//	DegradedModels 还有别的供给，但少了一条 ⇒ 仍可用，轮询变少 / 冗余降低
//
// 混成一句"影响 12 个模型"是没用的：前者要去补供给，后者通常什么都不用做。
//
// # 不逐一列出"不受影响"的模型
//
// 那可能是几百条，只会把真正的两类淹掉。只给一个计数。
package channelstore

import "sort"

// CatalogDeletionImpact 删掉一条渠道的影响。
type CatalogDeletionImpact struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	// Lost 删掉后**没有任何供给**的公开模型（会从目录消失）。
	Lost []string `json:"lost"`
	// Degraded 仍有别的供给、但少了一条的公开模型。
	Degraded []string `json:"degraded"`
	// Unaffected 不受影响的公开模型数量（不逐一列出，避免几百条噪音）。
	Unaffected int `json:"unaffected"`
	// Note 给人看的一句说明。
	Note string `json:"note,omitempty"`
}

// CatalogDeletionImpactOf 计算删掉 `channelID` 的影响。
//
// `catalog` 用 `BuildCatalog` 的结果 —— 这样"影响"与"目录里实际有什么"
// 是**同一份数据**，不会因为两套算法而给出互相矛盾的答案。
func CatalogDeletionImpactOf(catalog UnifiedCatalog, channelID string) CatalogDeletionImpact {
	out := CatalogDeletionImpact{
		ChannelID: channelID,
		Lost:      []string{},
		Degraded:  []string{},
	}

	for _, m := range catalog.Models {
		// 这条渠道能为该模型提供几条**启用的**供给？
		//
		// ⚠ 只数 `p.Enabled`：`BuildCatalog` 把停用渠道也列进 Providers
		// （带 Enabled 标记，供管理面展示"这条渠道配了但没开"），
		// 但**停用的渠道不服务流量**。把它算成供给会让"删掉一个停用渠道"
		// 被报成"少了一条供给"，而实际零影响 —— 那会把运维引向错误的方向。
		providers := 0
		fromThis := 0
		for _, p := range m.Providers {
			if !p.Enabled {
				continue
			}
			providers++
			if p.ChannelID == channelID {
				fromThis++
				if out.ChannelName == "" {
					out.ChannelName = p.Name
				}
			}
		}
		// 名字在停用时也要能取到（否则"未知渠道"与"停用渠道"分不开）。
		if fromThis == 0 && out.ChannelName == "" {
			for _, p := range m.Providers {
				if p.ChannelID == channelID {
					out.ChannelName = p.Name
					break
				}
			}
		}
		if fromThis == 0 {
			out.Unaffected++
			continue
		}
		// 删掉之后还剩几条？
		if providers-fromThis == 0 {
			out.Lost = append(out.Lost, m.ID)
		} else {
			out.Degraded = append(out.Degraded, m.ID)
		}
	}

	sort.Strings(out.Lost)
	sort.Strings(out.Degraded)

	switch {
	case len(out.Lost) == 0 && len(out.Degraded) == 0:
		out.Note = "删掉这条渠道不会影响任何公开模型"
	case len(out.Lost) == 0:
		out.Note = "删掉后没有模型消失，但这些模型少了一条供给（轮询变少、冗余降低）：" +
			"确认没有客户端强依赖这条渠道即可"
	default:
		out.Note = "⚠ 删掉后这些模型会**从目录里消失**（没有别的渠道能服务它们）：" +
			"先补供给，或确认没有客户端在用"
	}
	return out
}
