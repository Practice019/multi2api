package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/channelroute"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
)

func (h *Handler) routingPolicyFor(r *http.Request) (channelstore.PolicyResult, int) {
	if h.cfg.APIKeys == nil {
		return channelstore.PolicyResult{}, 0
	}
	id, _ := r.Context().Value(apikeyCtxKey).(string)
	version, groups, limited, models, ok := h.cfg.APIKeys.RoutingPolicyOf(id)
	if !ok || version == 0 {
		return channelstore.PolicyResult{}, 0
	}
	channels := []*channelstore.Channel{}
	if h.cfg.Channels != nil {
		channels = h.cfg.Channels.List()
	}
	endpoint := ""
	if r.URL.Path == "/v1/chat/completions" {
		endpoint = "chat"
	}
	if r.URL.Path == "/v1/images/generations" {
		endpoint = "images"
	}
	return channelstore.ResolvePolicy(h.cfg.Groups, channels, channelstore.RoutingPolicy{PolicyVersion: version, Groups: groups, ModelLimitsEnabled: limited, ModelLimits: models, Endpoint: endpoint}), version
}

type routingObservedBody struct {
	io.ReadCloser
	failure error
}

func (b *routingObservedBody) Read(buffer []byte) (int, error) {
	n, err := b.ReadCloser.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		b.failure = err
	}
	return n, err
}

type routingBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *routingBody) Close() error {
	var err error
	b.once.Do(func() { defer b.release(); err = b.ReadCloser.Close() })
	return err
}

func (h *Handler) routingChat(w http.ResponseWriter, r *http.Request, body []byte, model string, policy channelstore.PolicyResult) {
	candidates := policy.Candidates[model]
	if len(candidates) == 0 {
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed", "模型不在令牌路由组与模型白名单的交集中")
		return
	}
	ids := make([]string, 0, len(candidates))
	byID := map[string]channelstore.RoutingCandidate{}
	for _, candidate := range candidates {
		ids = append(ids, candidate.Channel.ID)
		byID[candidate.Channel.ID] = candidate
	}
	sort.Strings(ids)
	h.routingOnce.Do(func() { h.routingRR = channelroute.NewRoundRobin() })
	scope := model + "\x00chat\x00" + strings.Join(ids, "\x00")
	exhausted := map[string]bool{}
	resources := map[string]bool{}
	var lastErr error
	started := time.Now()
	attempts := 0
	for attempts < 3 && r.Context().Err() == nil {
		id, ok := h.routingRR.Next(scope, ids, exhausted)
		if !ok {
			break
		}
		fw, used, err := h.routingAttempt(r.Context(), byID[id], model, body, resources)
		if !used {
			exhausted[id] = true
		} else {
			attempts++
		}
		if err != nil {
			lastErr = err
			if upstream, ok := gateway.AsUpstreamHTTPError(err); ok {
				if upstream.Status != 429 && upstream.Status != 502 && upstream.Status != 503 && upstream.Status != 504 {
					h.bumpKeyUsage(r, 0, false)
					status := upstream.Status
					if status < 400 || status > 599 {
						status = http.StatusBadGateway
					}
					writeOpenAIError(w, status, "upstream_error", "上游拒绝了请求")
					return
				}
				continue
			}
			if rej := armorRejectionOf(err); rej != nil {
				h.bumpKeyUsage(r, 0, false)
				writeOpenAIError(w, rej.Status(), armorBlockedCode, rej.Message())
				return
			}
			if errors.Is(err, gateway.ErrConvertFailed) {
				h.bumpKeyUsage(r, 0, false)
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求无法转换为上游协议")
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			if used && !routingRetryError(err) {
				break
			}
			continue
		}
		if fw.Stream.Body == nil {
			lastErr = fmt.Errorf("empty upstream response")
			exhausted[id] = true
			continue
		}
		if fw.Stream.Status >= 300 {
			status := fw.Stream.Status
			raw, _ := io.ReadAll(io.LimitReader(fw.Stream.Body, 1<<20))
			_ = fw.Stream.Body.Close()
			if byID[id].Channel.EffectiveKind() == "builtin" {
				kind := h.classifyErr(byID[id].Channel.ProviderID, status, raw)
				h.applyErrorPolicy(fw.KeyMask, kind)
				if kind == gateway.ErrKindFatalClient || kind == gateway.ErrKindClient || kind == gateway.ErrKindContentBlocked {
					h.bumpKeyUsage(r, 0, false)
					writeOpenAIError(w, status, "upstream_error", "上游拒绝了请求")
					return
				}
			}
			lastErr = fmt.Errorf("upstream status %d", status)
			if status != 429 && status != 502 && status != 503 && status != 504 {
				h.bumpKeyUsage(r, 0, false)
				writeOpenAIError(w, status, "upstream_error", "上游拒绝了请求")
				return
			}
			continue
		}
		capture := newStreamUsage(started)
		observed := &routingObservedBody{ReadCloser: fw.Stream.Body}
		fw.Stream.Body = observed
		h.writeChannelStream(w, r, fw, capture, true)
		tokens, ttfb, source := capture.Result()
		success := r.Context().Err() == nil && observed.failure == nil
		if success && byID[id].Channel.EffectiveKind() == "builtin" {
			h.cfg.Pool.NoteSuccess(fw.KeyMask)
		}
		logChatRowProvider(ttfb, time.Since(started), model, "unified", fw.KeyMask, "channel:"+fw.ChannelName, fw.Stream.Status, tokens, nil, "usage:"+string(source), "", "", h.cfg.ReqDump, 0, gateway.ExtractClientIP(r), r.UserAgent(), fw.ChannelID, fw.UpstreamModel)
		h.bumpKeyUsage(r, tokens, success)
		return
	}
	h.bumpKeyUsage(r, 0, false)
	if r.Context().Err() != nil {
		return
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用渠道凭据")
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "routing_unavailable", "统一渠道调用失败或资源耗尽")
}

func routingRetryError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, gateway.ErrConvertFailed) {
		return false
	}
	var ce *chatError
	if errors.As(err, &ce) {
		return ce.Kind == gateway.ErrKindServer || ce.Kind == gateway.ErrKindSoftRate
	}
	var ne net.Error
	return errors.As(err, &ne) && !ne.Timeout()
}

func (h *Handler) routingAttempt(ctx context.Context, candidate channelstore.RoutingCandidate, public string, body []byte, tried map[string]bool) (channelForward, bool, error) {
	ch := candidate.Channel
	empty := channelForward{}
	current, exists := h.cfg.Channels.Get(ch.ID)
	if !exists || current.Status != channelstore.StatusEnabled || current.EffectiveKind() != ch.EffectiveKind() || current.ProviderID != ch.ProviderID {
		return empty, false, fmt.Errorf("channel configuration changed")
	}
	ch = current
	rewritten, err := withUpstreamModel(body, candidate.UpstreamModel)
	if err != nil {
		return empty, false, err
	}
	if ch.EffectiveKind() != "builtin" {
		current, ok := h.cfg.Channels.Get(ch.ID)
		if !ok || current.Status != channelstore.StatusEnabled {
			return empty, false, fmt.Errorf("channel unavailable")
		}
		for range current.Keys {
			index, key := h.cfg.Channels.PickKeyIdx(current)
			if index < 0 || key == "" {
				break
			}
			identity := ch.ID + "\x00" + key
			if tried[identity] {
				continue
			}
			tried[identity] = true
			fw, err := h.tryChannelKeyOnce(ctx, channelroute.Candidate{Channel: current, UpstreamModel: candidate.UpstreamModel}, public, public, body, index, key)
			return fw, true, err
		}
		return empty, false, fmt.Errorf("channel credentials exhausted")
	}
	if h.cfg.Pool == nil || h.cfg.Provider == nil {
		return empty, false, fmt.Errorf("builtin runtime unavailable")
	}
	excluded := map[string]bool{}
	prefix := ch.ProviderID + "\x00"
	for id := range tried {
		if strings.HasPrefix(id, prefix) {
			excluded[strings.TrimPrefix(id, prefix)] = true
		}
	}
	account := h.cfg.Pool.PickRoundRobinFor(ch.ProviderID, candidate.UpstreamModel, excluded)
	if account == nil {
		return empty, false, fmt.Errorf("builtin credentials exhausted")
	}
	tried[prefix+account.UID] = true
	release := func() { h.cfg.Pool.Release(account.UID) }
	if h.needsRefreshVia(ch.ProviderID, account) {
		if err = h.refreshCredential(ctx, ch.ProviderID, account); err != nil {
			release()
			return empty, true, err
		}
	}
	stream, status, err := h.chatVia(ctx, ch.ProviderID, account, rewritten)
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		release()
		return empty, true, err
	}
	if stream == nil {
		release()
		return empty, true, fmt.Errorf("empty builtin stream")
	}
	return channelForward{Stream: gateway.ChatStream{Status: status, Body: &routingBody{ReadCloser: stream, release: release}}, ChannelID: ch.ID, ChannelName: ch.Name, UpstreamModel: candidate.UpstreamModel, PublicModel: public, KeyMask: account.UID}, true, nil
}
