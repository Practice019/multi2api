// plaza.go 模型广场：**所有分组里的对外模型**的全集（U-11 多提供者详情）。
//
// # 它与目录快照的区别（这是关键）
//
//	`BuildCatalog`   按**渠道**算：每条渠道声明了哪些模型、映射成什么对外名。
//	                 它回答"这条渠道能服务什么"。
//	本文件           按**分组**算：每个分组里配了哪些对外模型。
//	                 它回答"我们**对外承诺**了哪些模型"。
//
// 两者不是同一个集合，差集恰好是最需要看见的东西：
//
//	分组里配了、渠道没供给  ⇒ **对外承诺了一个没人能服务的模型**（客户端会 404/503）
//	渠道能供给、分组里没配  ⇒ 只是内部可达，客户端拿不到（除非别的分组配了）
//
// 所以广场的每一行都带 `in_catalog`：它是不是真的有渠道能服务。
//
// # 对外名字怎么算
//
//	分组的 ModelMap 是「对外名 → 一组内部名」。键就是对外名。
//	分组声明的 Models 是**内部名**；没有为它配映射时，内部名本身就是对外名
//	（与 `PublicNames` 同一约定）。
//
// ⇒ 对外模型全集 = ∪ 每个分组的 (ModelMap 的键 ∪ 未被映射覆盖的 Models)
package channelstore

import (
	"sort"
	"strings"

	"workbuddy2api/internal/groupstore"
)

// PlazaModel 广场里的一行：一个对外模型。
type PlazaModel struct {
	// Public 对外名（客户端要发的那个名字）。
	Public string `json:"public"`
	// Reals 它背后映射到的真实模型（无映射时等于 Public 自己）。
	Reals []string `json:"reals,omitempty"`
	// Groups 出现在哪些分组里（分组 ID，排序）。
	Groups []string `json:"groups"`
	// GroupNames 分组的展示名（与 Groups 同序对应，**供人读**）。
	GroupNames []string `json:"group_names,omitempty"`
	// Endpoints 端点能力（来自渠道目录；目录里没有它时为空）。
	Endpoints []string `json:"endpoints,omitempty"`
	// Channels 能提供它的渠道 ID（来自渠道目录）。
	Channels []string `json:"channels,omitempty"`
	// InCatalog 渠道目录里**真的有**渠道能服务它。
	//
	// false 意味着"分组里承诺了，但没有渠道供给" —— 客户端发这个模型会失败。
	InCatalog bool `json:"in_catalog"`
}

// ModelPlaza 广场全集。
type ModelPlaza struct {
	// Models 按 Public 排序。
	Models []PlazaModel `json:"models"`
	// Total 条数。
	Total int `json:"total"`
	// Orphaned 分组里配了、但**没有渠道能服务**的对外模型数（应当为 0）。
	Orphaned int `json:"orphaned"`
	// OrphanedList 上面那些模型的名单（同样应当为空）。
	OrphanedList []string `json:"orphaned_list,omitempty"`
	// Note 给人看的一句说明。
	Note string `json:"note,omitempty"`
}

// BuildModelPlaza 从**分组**算对外模型全集，再用**渠道目录**补端点/渠道/可用性。
//
// `channels` 用于 `BuildCatalog`；`groups` 是 `groupstore.List()` 的结果。
func BuildModelPlaza(groups []*groupstore.Group, channels []*Channel) ModelPlaza {
	catalog := BuildCatalog(channels)

	// 目录侧索引：对外名 → 该模型的目录行。
	type catEntry struct {
		endpoints []string
		channels  []string
	}
	inCatalog := map[string]catEntry{}
	for _, m := range catalog.Models {
		e := catEntry{endpoints: append([]string{}, m.Endpoints...)}
		for _, p := range m.Providers {
			// 只算**启用的**供给：停用渠道不服务流量，把它算进来等于骗人。
			if p.Enabled {
				e.channels = append(e.channels, p.ChannelID)
			}
		}
		sort.Strings(e.channels)
		inCatalog[m.ID] = e
	}

	// 分组侧：对外名 → 聚合信息。
	type acc struct {
		reals      map[string]bool
		groupIDs   map[string]bool
		groupNames map[string]string
	}
	byPublic := map[string]*acc{}
	get := func(pub string) *acc {
		if byPublic[pub] == nil {
			byPublic[pub] = &acc{
				reals:      map[string]bool{},
				groupIDs:   map[string]bool{},
				groupNames: map[string]string{},
			}
		}
		return byPublic[pub]
	}

	for _, g := range groups {
		if g == nil {
			continue
		}
		mapped := map[string]bool{} // 已经被 ModelMap 覆盖的内部名
		// ① ModelMap 的键 = 对外名。
		for public, reals := range g.ModelMap {
			public = strings.TrimSpace(public)
			if public == "" {
				continue
			}
			a := get(public)
			a.groupIDs[g.ID] = true
			a.groupNames[g.ID] = g.Name
			for _, real := range reals {
				real = strings.TrimSpace(real)
				if real == "" {
					continue
				}
				a.reals[real] = true
				mapped[real] = true
			}
		}
		// ② 未被映射覆盖的 Models：内部名就是对外名。
		for _, m := range g.Models {
			m = strings.TrimSpace(m)
			if m == "" || mapped[m] {
				continue
			}
			a := get(m)
			a.groupIDs[g.ID] = true
			a.groupNames[g.ID] = g.Name
			a.reals[m] = true
		}
	}

	out := ModelPlaza{Models: make([]PlazaModel, 0, len(byPublic))}
	names := make([]string, 0, len(byPublic))
	for pub := range byPublic {
		names = append(names, pub)
	}
	sort.Strings(names)

	for _, pub := range names {
		a := byPublic[pub]
		row := PlazaModel{Public: pub, Groups: []string{}}
		for r := range a.reals {
			row.Reals = append(row.Reals, r)
		}
		sort.Strings(row.Reals)
		for id := range a.groupIDs {
			row.Groups = append(row.Groups, id)
		}
		sort.Strings(row.Groups)
		for _, id := range row.Groups {
			row.GroupNames = append(row.GroupNames, a.groupNames[id])
		}
		if e, ok := inCatalog[pub]; ok && len(e.channels) > 0 {
			row.InCatalog = true
			row.Endpoints = e.endpoints
			row.Channels = e.channels
		}
		out.Models = append(out.Models, row)
	}

	out.Total = len(out.Models)
	for _, m := range out.Models {
		if !m.InCatalog {
			out.Orphaned++
			out.OrphanedList = append(out.OrphanedList, m.Public)
		}
	}
	switch {
	case out.Total == 0:
		out.Note = "还没有任何分组配置对外模型。先在分组里勾选模型或配置对外名，这里才会出现。"
	case out.Orphaned > 0:
		out.Note = "⚠ 有模型**在分组里配了但没有渠道能服务**（见 orphaned_list）——" +
			"客户端发它们会失败。要么给它们补渠道供给，要么从分组里移除。"
	default:
		out.Note = "所有对外模型都有渠道能服务。"
	}
	return out
}
