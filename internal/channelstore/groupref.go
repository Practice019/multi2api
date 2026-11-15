// groupref.go 令牌的分组引用 → 分组 ID 的**显式兼容解析**（U-09）。
//
// # 它解决什么
//
// 令牌里存的"分组"有两种历史形态：
//
//	新形态：分组 **ID**（`g-a1b2c3`，创建后不变）
//	旧形态：分组 **名字**（界面早期就是这么存的，人手里的配置、迁移脚本、
//	        日志、工单里也全是名字）
//
// 改造前 `ResolvePolicy` 直接 `gs.Get(ref)`（只认 ID）⇒ 旧形态的令牌
// **一个模型都不许用**，而用户在界面上明明看到自己勾了分组。
//
// # 为什么独立成一个**纯函数**
//
// 判定链跑在请求热路径上，而"这个引用到底指谁"是个纯数据问题。
// 抽成纯函数之后，每条规则都能离线钉住 —— 尤其下面这条。
//
// # 「冲突不得任选」是本文件的核心
//
// 名字在 `Group` 上标着"全局唯一"，但**存量数据不一定满足**：
// 早期可以重名，后来才加的唯一约束。所以按名字解析时可能匹配到多个分组。
//
// 这时**不许挑一个**：挑错了就是"用户以为 A 组、实际是 B 组"，
// 而两个组的模型集合不同 —— 权限判错且毫无信号。
// 所以匹配到多个就记进 `Ambiguous`，由调用方阻塞/告警，绝不静默选一个。
package channelstore

import (
	"sort"
	"strings"

	"workbuddy2api/internal/groupstore"
)

// GroupResolution 一次"令牌分组引用"的解析结果。
type GroupResolution struct {
	// IDs 解析成功的分组 ID（**保输入顺序**，已去重）。
	IDs []string `json:"ids"`
	// Unresolved 既不是任何分组 ID、也不唯一匹配任何分组名字的引用。
	Unresolved []string `json:"unresolved,omitempty"`
	// Ambiguous 名字匹配到**多个**分组的引用。
	//
	// ⚠ 这些引用**没有**进 IDs —— 见文件头「冲突不得任选」。
	Ambiguous []string `json:"ambiguous,omitempty"`
}

// OK 是否全部解析成功且无歧义。
func (r GroupResolution) OK() bool {
	return len(r.Unresolved) == 0 && len(r.Ambiguous) == 0
}

// ResolveGroupRefs 把令牌里的分组引用解析成分组 ID。
//
// 规则（按优先级）：
//
//	① 精确命中分组 **ID** ⇒ 用 ID。ID 永远不歧义，所以它优先于名字。
//	② 否则按**名字**精确匹配：唯一 ⇒ 用它的 ID；多个 ⇒ Ambiguous；零个 ⇒ Unresolved。
//	③ 空白项直接丢弃（`["g-1",""]` 若保留空串，len>0 会让令牌进"受限"分支，
//	   而空串取不到分组 ⇒ 并集为空 ⇒ 一个模型都不许用）。
//
// 保序 + 去重：分组顺序对用户有意义（他在界面上按自己的顺序勾的）。
func ResolveGroupRefs(refs []string, groups []*groupstore.Group) GroupResolution {
	out := GroupResolution{IDs: []string{}}

	// 名字 → 该名字对应的全部分组 ID（排序保证遍历稳定）。
	byName := make(map[string][]string, len(groups))
	ids := make(map[string]bool, len(groups))
	for _, g := range groups {
		if g == nil || g.ID == "" {
			continue
		}
		ids[g.ID] = true
		name := strings.TrimSpace(g.Name)
		if name != "" {
			byName[name] = append(byName[name], g.ID)
		}
	}
	for name := range byName {
		sort.Strings(byName[name])
	}

	seen := make(map[string]bool, len(refs))
	ambSeen := make(map[string]bool)
	unSeen := make(map[string]bool)

	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		// ① ID 优先（永不歧义）。
		if ids[ref] {
			if !seen[ref] {
				seen[ref] = true
				out.IDs = append(out.IDs, ref)
			}
			continue
		}
		// ② 按名字。
		matches := byName[ref]
		switch len(matches) {
		case 1:
			id := matches[0]
			if !seen[id] {
				seen[id] = true
				out.IDs = append(out.IDs, id)
			}
		case 0:
			if !unSeen[ref] {
				unSeen[ref] = true
				out.Unresolved = append(out.Unresolved, ref)
			}
		default:
			// ★ 冲突不得任选。
			if !ambSeen[ref] {
				ambSeen[ref] = true
				out.Ambiguous = append(out.Ambiguous, ref)
			}
		}
	}
	return out
}

// ResolvePolicyGroups 用解析结果把 `RoutingPolicy.Groups` 换成 ID 列表。
//
// ⚠ 有歧义时**不降级成"部分解析"**：返回的 Groups 会把有歧义的那些**排除**
// （它们本来也没进 IDs），而调用方必须看 `Ambiguous` 非空来决定是否阻塞。
// 悄悄用"能解析的那部分"去判定权限，正是"任选"的另一种形态 ——
// 而且更难发现：它不报错，只是少给了几个模型。
func ResolvePolicyGroups(policy RoutingPolicy, groups []*groupstore.Group) (RoutingPolicy, GroupResolution) {
	res := ResolveGroupRefs(policy.Groups, groups)
	if len(res.Ambiguous) == 0 && len(res.Unresolved) == 0 {
		policy.Groups = res.IDs
	}
	return policy, res
}
