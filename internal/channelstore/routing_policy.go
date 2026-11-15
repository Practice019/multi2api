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
	Models           []string                      `json:"models"`
	ExcludedModels   []string                      `json:"excluded_models"`
	UnresolvedGroups []string                      `json:"unresolved_groups"`
	Ready            bool                          `json:"ready"`
	Candidates       map[string][]RoutingCandidate `json:"-"`
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
	excluded := map[string]bool{}
	for _, ch := range sorted {
		if !selected[ch.ID] || ch.Status != StatusEnabled {
			continue
		}
		names := PublicNames(ch)
		for public, real := range names {
			if policy.Endpoint != "" && !ch.SupportsEndpoint(real, policy.Endpoint) {
				continue
			}
			if policy.ModelLimitsEnabled && !limits[public] {
				excluded[public] = true
				continue
			}
			out.Candidates[public] = append(out.Candidates[public], RoutingCandidate{Channel: ch, UpstreamModel: real})
		}
	}
	for m := range out.Candidates {
		out.Models = append(out.Models, m)
	}
	for m := range excluded {
		out.ExcludedModels = append(out.ExcludedModels, m)
	}
	sort.Strings(out.Models)
	sort.Strings(out.ExcludedModels)
	out.Ready = len(out.Models) > 0
	return out
}
