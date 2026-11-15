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

// catalogSnapshots 返回本实例的目录快照持有者（惰性建）。
//
// ⚠ 惰性建而不是构造函数里建：`&Handler{}` 直接构造的测试也要能调
// （与 adminToken / patterns 同一模式）。
func (h *Handler) catalogSnapshots() *channelstore.CatalogSnapshotStore {
	if s := h.catalogSnap.Load(); s != nil {
		return s
	}
	s := channelstore.NewCatalogSnapshotStore()
	if h.catalogSnap.CompareAndSwap(nil, s) {
		return s
	}
	return h.catalogSnap.Load()
}

// publishCatalog 从**当前渠道配置**算一份目录并发布为快照。
//
// 这是「获取」这一侧：只有它读渠道库。失败时用 Fail() 记一笔，
// 当前快照原样保留（catalog_snapshot.go 的硬不变量）。
func (h *Handler) publishCatalog() (channelstore.CatalogSnapshot, error) {
	store := h.catalogSnapshots()
	if h.cfg.Channels == nil {
		err := fmt.Errorf("channel store unavailable")
		return store.Fail(err), err
	}
	return store.Publish(channelstore.BuildCatalog(h.cfg.Channels.List())), nil
}

// unifiedChannelCatalog 读取**已发布的**目录快照。
//
// # U-04：为什么这里不再现算
//
// 改造前这里 `BuildCatalog(h.cfg.Channels.List())` —— 每次请求现算，
// 于是"你看到的这份目录"与"上次刷新成功的那份"没有任何绑定关系，
// 运维无法回答「这份是第几次刷的、有没有失败过」。
//
// 现在：
//
//	读      永远返回上一份成功的快照，顺带给出 Sequence / Attempts / LastError
//	首次    库里一份都没有时**惰性发布一次**（否则这个端点永远 503）
//	刷新    显式 POST /admin/channels/catalog/refresh 才重新发布
//
// ⇒ 渠道改了但没刷新时，这里仍然显示旧快照 —— 这是**特性**：
// 它会明确告诉你 sequence/attempts 是多少，而不是悄悄替你把两个时间点混一起。
func (h *Handler) unifiedChannelCatalog(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.cfg.Channels == nil {
		writeError(w, http.StatusServiceUnavailable, "channel store unavailable")
		return
	}
	snap := h.catalogSnapshots().Read()
	if !snap.Ready() {
		// 首次访问：惰性发布。失败也照样回 200 —— 把失败原因放进 last_error，
		// 让面板能显示"取目录失败"，而不是一个光秃秃的 5xx。
		snap, _ = h.publishCatalog()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"catalog":      snap.Catalog,
		"sequence":     snap.Sequence,
		"attempts":     snap.Attempts,
		"failed":       snap.FailedAttempts(),
		"published_at": snap.PublishedAt,
		"last_error":   snap.LastError,
	})
}

// refreshUnifiedChannelCatalog 显式重新发布目录快照（POST）。
//
// 与读端点分开的唯一理由：**刷新是会失败的，读不会**。
// 混在一起就会出现"读一次请求顺带改一次状态"的隐式写。
func (h *Handler) refreshUnifiedChannelCatalog(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.principalFrom(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	snap, err := h.publishCatalog()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":      err.Error(),
			"catalog":    snap.Catalog,
			"sequence":   snap.Sequence,
			"attempts":   snap.Attempts,
			"last_error": snap.LastError,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"catalog":      snap.Catalog,
		"sequence":     snap.Sequence,
		"attempts":     snap.Attempts,
		"published_at": snap.PublishedAt,
	})
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
