package channelstore

import (
	"sort"
	"strings"
	"workbuddy2api/internal/groupstore"
)

type RoutingPolicy struct {
	Endpoint           string   `json:"endpoint,omitempty"`
	PolicyVersion      int      `json:"policy_version"`
	Groups             []string `json:"groups"`
	ModelLimitsEnabled bool     `json:"model_limits_enabled"`
	ModelLimits        []string `json:"model_limits"`
}

type RoutingCandidate struct {
	Channel       *Channel
	UpstreamModel string
}

type PolicyResult struct {
	Models         []string `json:"models"`
	ExcludedModels []string `json:"excluded_models"`
	// ExcludedDetail 逐条原因。与 ExcludedModels 同序对应，但**覆盖更全** ——
	// 见 ExcludedModel 的注释（改造前除白名单外的三种原因都是静默消失的）。
	ExcludedDetail   []ExcludedModel               `json:"excluded_detail,omitempty"`
	UnresolvedGroups []string                      `json:"unresolved_groups"`
	Ready            bool                          `json:"ready"`
	Candidates       map[string][]RoutingCandidate `json:"-"`
}

// ExcludedModel 一个"没有出现在候选里"的模型，以及**为什么**。
//
// # 为什么需要它
//
// 改造前只有白名单那一种原因会被记录，其余三种（端点不支持、渠道停用、
// 不在分组内）都是**静默消失**：模型既不在 `models` 也不在 `excluded_models` 里。
// 运维看到"这个模型用不了"，但拿不到任何原因 —— 只能猜。
//
// 而"猜"在这个系统里代价很高：分组、白名单、端点能力、渠道状态四层叠加，
// 猜错一次就会去改一个本来没问题的地方。
type ExcludedModel struct {
	Public string `json:"public"`
	// Reason 见下面的 Excluded* 常量。
	Reason string `json:"reason"`
	// ChannelIDs 证据渠道（端点原因 = 这些渠道都不支持；停用原因 = 这些停了）。
	ChannelIDs []string `json:"channel_ids,omitempty"`
}

// 排除原因。**优先级从高到低**（一个模型同时满足多条时，报最高的那条）。
const (
	// ExcludedWhitelist 白名单启用但没包含它。优先级最高：这是用户自己配的。
	ExcludedWhitelist = "whitelist"
	// ExcludedChannelDisabled 它所在的渠道都停用了。
	ExcludedChannelDisabled = "channel_disabled"
	// ExcludedEndpoint 它所在的渠道都不支持请求的端点。
	ExcludedEndpoint = "endpoint_unsupported"
	// ⚠ 刻意**没有** "not_in_group_scope" 这一档：
	// 那份"不在分组里的模型"是全库的其余模型（几百个），对这把令牌毫无意义。
	// 报出来只会把真正的三条原因淹掉。分组范围本身由分组编辑器回答。
)

// excludedPriority 原因 → 优先级（越大越优先报）。
var excludedPriority = map[string]int{
	ExcludedEndpoint:        1,
	ExcludedChannelDisabled: 2,
	ExcludedWhitelist:       3,
}

func ResolvePolicy(gs *groupstore.Store, channels []*Channel, policy RoutingPolicy) PolicyResult {
	out := PolicyResult{Models: []string{}, ExcludedModels: []string{}, UnresolvedGroups: []string{}, Candidates: map[string][]RoutingCandidate{}}
	if policy.PolicyVersion != 2 || gs == nil || len(policy.Groups) == 0 {
		return out
	}
	selected := map[string]bool{}
	for _, id := range policy.Groups {
		g, ok := gs.Get(id)
		if !ok || g.PolicyVersion != 2 {
			out.UnresolvedGroups = append(out.UnresolvedGroups, id)
			continue
		}
		for _, ch := range g.ChannelIDs {
			selected[ch] = true
		}
	}
	if len(out.UnresolvedGroups) > 0 {
		sort.Strings(out.UnresolvedGroups)
		return out
	}
	limits := map[string]bool{}
	for _, m := range policy.ModelLimits {
		limits[strings.TrimSpace(m)] = true
	}
	sorted := []*Channel{}
	for _, ch := range channels {
		if ch != nil {
			sorted = append(sorted, ch)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	// 原因收集：public → 原因（按 excludedPriority 取最高的）+ 证据渠道。
	// 打分而不是"先到先得"：同一个模型可能同时满足多条，而报哪条决定了
	// 运维去哪里改。取最可操作的那条（见常量注释里的优先级）。
	reasonScore := map[string]int{}
	reasonOf := map[string]string{}
	reasonChans := map[string][]string{}
	note := func(public, reason, chID string) {
		if excludedPriority[reason] > reasonScore[public] {
			reasonScore[public] = excludedPriority[reason]
			reasonOf[public] = reason
			reasonChans[public] = nil
		}
		if reasonOf[public] == reason && chID != "" {
			reasonChans[public] = append(reasonChans[public], chID)
		}
	}

	for _, ch := range sorted {
		if ch == nil || !selected[ch.ID] {
			// 只报**在令牌分组内**的渠道上的模型。分组外的模型是全库其余模型
			// （几百个），报出来只会把真正的原因淹掉 —— 见原因常量那段注释。
			continue
		}
		enabled := ch.Status == StatusEnabled
		for public, real := range PublicNames(ch) {
			if !enabled {
				note(public, ExcludedChannelDisabled, ch.ID)
				continue
			}
			if policy.Endpoint != "" && !ch.SupportsEndpoint(real, policy.Endpoint) {
				note(public, ExcludedEndpoint, ch.ID)
				continue
			}
			if policy.ModelLimitsEnabled && !limits[public] {
				note(public, ExcludedWhitelist, ch.ID)
				continue
			}
			out.Candidates[public] = append(out.Candidates[public], RoutingCandidate{Channel: ch, UpstreamModel: real})
		}
	}
	for m := range out.Candidates {
		out.Models = append(out.Models, m)
	}
	for m, reason := range reasonOf {
		if _, ok := out.Candidates[m]; ok {
			continue // 有候选就不算被排除（别的渠道能服务它）
		}
		out.ExcludedModels = append(out.ExcludedModels, m)
		ids := append([]string{}, reasonChans[m]...)
		sort.Strings(ids)
		out.ExcludedDetail = append(out.ExcludedDetail, ExcludedModel{
			Public: m, Reason: reason, ChannelIDs: dedupeStrings(ids),
		})
	}
	sort.Strings(out.Models)
	sort.Strings(out.ExcludedModels)
	sort.Slice(out.ExcludedDetail, func(i, j int) bool {
		return out.ExcludedDetail[i].Public < out.ExcludedDetail[j].Public
	})
	out.Ready = len(out.Models) > 0
	return out
}

// dedupeStrings 去重（已排序的输入）。
func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for i, s := range in {
		if i > 0 && in[i-1] == s {
			continue
		}
		out = append(out, s)
	}
	return out
}
