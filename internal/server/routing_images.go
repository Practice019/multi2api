package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"workbuddy2api/internal/channelroute"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
)

func (h *Handler) routingImages(w http.ResponseWriter, r *http.Request, body []byte, model string, policy channelstore.PolicyResult) {
	candidates := policy.Candidates[model]
	if len(candidates) == 0 {
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed", "令牌未获该生图模型的端点权限")
		return
	}
	ids := []string{}
	byID := map[string]channelstore.RoutingCandidate{}
	for _, candidate := range candidates {
		ids = append(ids, candidate.Channel.ID)
		byID[candidate.Channel.ID] = candidate
	}
	sort.Strings(ids)
	h.routingOnce.Do(func() { h.routingRR = channelroute.NewRoundRobin() })
	excluded := map[string]bool{}
	ctx, cancel := context.WithTimeout(r.Context(), imageGenTimeout)
	defer cancel()
	for ctx.Err() == nil {
		id, ok := h.routingRR.Next(model+"\x00images\x00"+strings.Join(ids, "\x00"), ids, excluded)
		if !ok {
			break
		}
		candidate := byID[id]
		stream, used, err := h.routingImageAttempt(ctx, candidate, body)
		if !used {
			excluded[id] = true
			if err != nil {
				continue
			}
		}
		if err != nil {
			h.bumpKeyUsage(r, 0, false)
			if ctx.Err() == nil {
				writeOpenAIError(w, http.StatusBadGateway, "image_upstream_error", "生图调用失败；为避免重复扣费，不重试执行")
			}
			return
		}
		if stream.Body == nil {
			h.bumpKeyUsage(r, 0, false)
			writeOpenAIError(w, http.StatusBadGateway, "image_upstream_error", "上游未返回响应体")
			return
		}
		defer stream.Body.Close()
		stop := context.AfterFunc(ctx, func() { _ = stream.Body.Close() })
		defer stop()
		status := stream.Status
		if status < 200 || status > 599 {
			status = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Channel-Id", id)
		w.WriteHeader(status)
		_, copyErr := io.Copy(w, stream.Body)
		h.bumpKeyUsage(r, 0, status < 300 && copyErr == nil && ctx.Err() == nil)
		return
	}
	if ctx.Err() == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "routing_unavailable", "没有可用的生图渠道凭据")
	}
}

func (h *Handler) routingImageAttempt(ctx context.Context, candidate channelstore.RoutingCandidate, body []byte) (gateway.ChatStream, bool, error) {
	empty := gateway.ChatStream{}
	ch, ok := h.cfg.Channels.Get(candidate.Channel.ID)
	if !ok || ch.Status != channelstore.StatusEnabled || ch.EffectiveKind() != candidate.Channel.EffectiveKind() || ch.ProviderID != candidate.Channel.ProviderID || !ch.SupportsEndpoint(candidate.UpstreamModel, "images") {
		return empty, false, fmt.Errorf("channel changed")
	}
	rewritten, err := withUpstreamModel(body, candidate.UpstreamModel)
	if err != nil {
		return empty, false, err
	}
	if ch.EffectiveKind() == "standard" {
		if h.cfg.ChannelProvider == nil {
			return empty, false, fmt.Errorf("image provider unavailable")
		}
		provider := h.cfg.ChannelProvider(ch.ID, ch.BaseURL)
		imageProvider, supported := provider.(interface {
			Images(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error)
		})
		if !supported {
			return empty, false, fmt.Errorf("images not supported")
		}
		_, key := h.cfg.Channels.PickKeyIdx(ch)
		if key == "" {
			return empty, false, fmt.Errorf("no available key")
		}
		stream, err := imageProvider.Images(ctx, gateway.Credential{Provider: ch.ID, UID: ch.ID, Secret: map[string]any{"api_key": key}}, rewritten)
		return stream, true, err
	}
	generator, supported := h.cfg.Provider.(interface {
		ImageGen(context.Context, string, string, []byte) ([]byte, int, bool, error)
	})
	if !supported || h.cfg.Pool == nil {
		return empty, false, fmt.Errorf("builtin images unavailable")
	}
	account := h.cfg.Pool.PickRoundRobinFor(ch.ProviderID, candidate.UpstreamModel, nil)
	if account == nil {
		return empty, false, fmt.Errorf("no available account")
	}
	defer h.cfg.Pool.Release(account.UID)
	if h.needsRefreshVia(ch.ProviderID, account) {
		if err = h.refreshCredential(ctx, ch.ProviderID, account); err != nil {
			return empty, false, err
		}
	}
	raw, status, available, err := generator.ImageGen(ctx, ch.ProviderID, account.UID, rewritten)
	if !available {
		return empty, false, fmt.Errorf("builtin image endpoint not supported")
	}
	if err != nil {
		return empty, true, err
	}
	return gateway.ChatStream{Status: status, Body: io.NopCloser(bytes.NewReader(raw))}, true, nil
}
