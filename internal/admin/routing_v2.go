package admin

import (
	"fmt"
	"net/http"
	"strings"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

func (h *Handler) credentialMetadata(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.principalFrom(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"admin_token": store.MaskTokenKey(h.currentAdminToken())})
}

func (h *Handler) credentialReveal(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.principalFrom(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	if !h.allowTokenReveal(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"full": h.currentAdminToken()})
}

func (h *Handler) unifiedChannelCatalog(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.cfg.Channels == nil {
		writeError(w, http.StatusServiceUnavailable, "channel store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, channelstore.BuildCatalog(h.cfg.Channels.List()))
}

func (h *Handler) channelProviders(w http.ResponseWriter, r *http.Request) {
	providers := []map[string]string{}
	if h.cfg.Registry != nil {
		for _, p := range h.cfg.Registry.All() {
			name := p.ID()
			if ext, ok := p.(gateway.DisplayNameExt); ok {
				name = ext.DisplayName()
			}
			providers = append(providers, map[string]string{"id": p.ID(), "name": name})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

func (h *Handler) validateChannelBinding(kind, provider, id string, keys []string) error {
	if kind == "" || kind == "standard" {
		if provider != "" {
			return fmt.Errorf("standard channel cannot bind provider_id")
		}
		return nil
	}
	if kind != "builtin" {
		return fmt.Errorf("unsupported channel kind")
	}
	if h.cfg.Registry == nil {
		return fmt.Errorf("provider registry unavailable")
	}
	if _, ok := h.cfg.Registry.Get(provider); !ok {
		return fmt.Errorf("unknown provider_id")
	}
	if len(keys) > 0 {
		return fmt.Errorf("builtin channel uses account credentials, not API keys")
	}
	for _, ch := range h.cfg.Channels.List() {
		if ch.ID != id && ch.EffectiveKind() == "builtin" && ch.ProviderID == provider {
			return fmt.Errorf("provider already bound to a channel")
		}
	}
	return nil
}

func (h *Handler) validateRoutingGroup(channels []string) error {
	if h.cfg.Channels == nil {
		return fmt.Errorf("channel store unavailable")
	}
	for _, id := range channels {
		if _, ok := h.cfg.Channels.Get(id); !ok {
			return fmt.Errorf("unknown channel: %s", id)
		}
	}
	return nil
}

func (h *Handler) routingGroupView(g *groupstore.Group) groupView {
	v := groupViewOf(g)
	if g.PolicyVersion == 2 && h.cfg.Channels != nil {
		result := channelstore.ResolvePolicy(h.cfg.Groups, h.cfg.Channels.List(), channelstore.RoutingPolicy{PolicyVersion: 2, Groups: []string{g.ID}})
		v.Models, v.ModelCount = result.Models, len(result.Models)
	}
	return v
}

func (h *Handler) tokenPolicyPreview(w http.ResponseWriter, r *http.Request) {
	var req tokenCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.PolicyVersion != 2 {
		writeError(w, http.StatusBadRequest, "preview requires policy_version 2")
		return
	}
	if h.cfg.Channels == nil || h.cfg.Groups == nil {
		writeError(w, http.StatusServiceUnavailable, "routing stores unavailable")
		return
	}
	noStore(w)
	channels := h.cfg.Channels.List()
	result := channelstore.ResolvePolicy(h.cfg.Groups, channels, channelstore.RoutingPolicy{PolicyVersion: 2, Groups: req.Groups, ModelLimitsEnabled: req.ModelLimitsEnabled, ModelLimits: normList(req.ModelLimits)})
	writeJSON(w, http.StatusOK, map[string]any{"models": result.Models, "excluded_models": result.ExcludedModels, "unresolved_groups": result.UnresolvedGroups, "ready": result.Ready, "snapshot_version": channelstore.BuildCatalog(channels).Version})
}

func (h *Handler) validateTokenPolicy(req *tokenCreateRequest) string {
	if req.PolicyVersion == 0 {
		return ""
	}
	if req.PolicyVersion != 2 {
		return "unsupported policy_version"
	}
	if req.Automatic || len(req.AutoGroups) != 0 {
		return "v2 requires explicit routing groups"
	}
	if len(req.Groups) == 0 {
		return "select at least one routing group"
	}
	if h.cfg.Groups == nil {
		return "routing groups unavailable"
	}
	for _, id := range req.Groups {
		g, ok := h.cfg.Groups.Get(strings.TrimSpace(id))
		if !ok || g.PolicyVersion != 2 {
			return "v2 tokens require v2 routing groups"
		}
	}
	return ""
}
