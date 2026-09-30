// admin.go TRAE 的管理端点（gateway.AdminExt 实现）。
//
// 端点全部 Hidden：它们不是面板入口，由账号池行内动作与顶部按钮调用。
//
//	POST /admin/trae/checkin           单账号签到（DailyAction OneURL）
//	POST /admin/trae/checkin/all       全量签到（DailyAction AllURL）
//	POST /admin/trae/credits           单账号额度（行内「额度」按钮的探测回执）
//	GET  /admin/trae/models            实时模型目录预览
//
// 登录不需要管理端点：authURL 直接是 TRAE 官方登录页，回调由本机监听
// （127.0.0.1:18080/authorize）自动接收，见 callback.go。
package trae

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

const (
	checkinPath    = "/admin/trae/checkin"
	checkinAllPath = "/admin/trae/checkin/all"
	creditsPath    = "/admin/trae/credits"
	modelsPath     = "/admin/trae/models"
)

// AdminRoutes 返回 trae 的管理端点（gateway.AdminExt）。
//
// ⚠ CapCheckin / CapQuotaProbe 都在 gateway.unverifiableCaps 里，
// 声明它们就必须有非空的路由（契约的 probeCapabilities 检查）。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method:     http.MethodPost,
			Path:       checkinPath,
			Handler:    p.handleCheckin,
			Capability: gateway.CapCheckin,
			Title:      "签到（单账号）",
			Hidden:     true,
		},
		{
			Method:     http.MethodPost,
			Path:       checkinAllPath,
			Handler:    p.handleCheckinAll,
			Capability: gateway.CapCheckin,
			Title:      "签到（全部账号）",
			Hidden:     true,
		},
		{
			Method:     http.MethodPost,
			Path:       creditsPath,
			Handler:    p.handleCredits,
			Capability: gateway.CapQuotaProbe,
			Title:      "额度查询",
			Hidden:     true,
		},
		{
			Method:     http.MethodGet,
			Path:       modelsPath,
			Handler:    p.handleModels,
			Capability: gateway.CapModels,
			Title:      "模型目录（实时）",
			Hidden:     true,
		},
	}
}

// writeTraeJSON 写 JSON 回执（本包唯一的 JSON 出口）。
func writeTraeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"序列化回执失败"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// accountRef 管理端点用的账号投影。
type accountRef struct {
	UID      string
	Nickname string
	Auth     *Auth
}

// accounts 列出本上游凭证目录里的全部账号。
func (p *Provider) accounts() []accountRef {
	list, err := LoadDir(p.authDir)
	if err != nil {
		return nil
	}
	out := make([]accountRef, 0, len(list))
	for _, a := range list {
		if a == nil || a.UID == "" || a.AccessToken == "" {
			continue
		}
		out = append(out, accountRef{UID: a.UID, Nickname: a.Nickname, Auth: a})
	}
	return out
}

// handleCheckin POST /admin/trae/checkin —— 单账号签到（结果写历史）。
//
// ⚠ 签到本体在 checkinOne（dailycheckin.go）：全量端点与跨上游入口
// 都调它。这里只做"找账号 + 组装 HTTP 回执"。
func (p *Provider) handleCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := strings.TrimSpace(req.UID)
	for _, a := range p.accounts() {
		if a.UID != uid {
			continue
		}
		ctx, cancel := contextWithTimeout(r, 30*time.Second)
		defer cancel()
		res := p.checkinOne(ctx, a, "manual")
		if res.status == checkinlog.StatusFail {
			writeTraeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": res.detail})
			return
		}
		writeTraeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": res.status, "credits": res.credits,
		})
		return
	}
	writeTraeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有这个 trae 账号: " + uid})
}

// handleCheckinAll POST /admin/trae/checkin/all —— 全量签到（结果写历史）。
//
// ⚠ 遍历复用 checkinAllOnce（与 gateway.DailyCheckinExt 同一段业务）：
// 顶部跨上游的「全部签到」和本按钮必须是同一个动作。
func (p *Provider) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 2*time.Minute)
	defer cancel()
	results := p.checkinAllOnce(ctx, "manual-all")
	var okN, already, failed int
	for _, res := range results {
		switch res.Status {
		case checkinlog.StatusOK:
			okN++
		case checkinlog.StatusAlready:
			already++
		default:
			failed++
		}
	}
	writeTraeJSON(w, http.StatusOK, map[string]any{
		"ok": failed == 0, "ok_count": okN, "already": already, "failed": failed,
		"results": results,
	})
}

// handleCredits POST /admin/trae/credits —— 单账号额度。
func (p *Provider) handleCredits(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := strings.TrimSpace(req.UID)
	for _, a := range p.accounts() {
		if a.UID != uid {
			continue
		}
		ctx, cancel := contextWithTimeout(r, 30*time.Second)
		defer cancel()
		remain, err := p.client.UserEntUsage(ctx, a.Auth)
		if err != nil {
			writeTraeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeTraeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "credits": remain})
		return
	}
	writeTraeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有这个 trae 账号: " + uid})
}

// handleModels GET /admin/trae/models —— 实时模型目录预览。
func (p *Provider) handleModels(w http.ResponseWriter, r *http.Request) {
	accts := p.accounts()
	if len(accts) == 0 {
		writeTraeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "没有 trae 账号"})
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	models, err := p.client.FetchModels(ctx, accts[0].Auth)
	if err != nil {
		writeTraeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeTraeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(models), "models": models})
}

// contextWithTimeout 从请求 ctx 派生带超时的 ctx（避免用 Background 丢取消传播）。
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	ctx := r.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, d)
}
