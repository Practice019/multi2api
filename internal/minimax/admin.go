// admin.go 本上游的管理端点。
//
// # 端点清单与它们的可见性
//
//	GET  /admin/minimax/quota        积分余额（CapQuotaProbe）
//	POST /admin/minimax/checkin      单账号签到（DailyAction.OneURL）
//	POST /admin/minimax/checkin-all  本上游全部账号签到（DailyAction.AllURL）
//	GET  /admin/minimax/diagnose     令牌诊断（排障，Hidden）
//
// ⚠ quota 那条**必须是 Hidden**（用户明确要求过 zcode 的同类问题）：
// 前端会为"非 hidden 的 GET 路由"生成一个标签页，而余额已经在账号池的
// 「额度」列里了 —— 多一个空壳标签页只是重复入口。
// 另两条是 **POST**，前端按 routesFor 的判据本来就只把 GET 当面板入口，
// 所以它们天然不生成标签页。
package minimax

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// AdminRoutes 本上游的管理端点。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method: http.MethodGet,
			Path:   "/admin/minimax/quota",
			// Hidden：余额已经在账号池的「额度」列，不需要额外的标签页。
			Hidden:     true,
			Handler:    p.handleQuota,
			Capability: gateway.CapQuotaProbe,
			Title:      "MiniMax 积分",
		},
		{
			Method:     http.MethodPost,
			Path:       "/admin/minimax/checkin",
			Handler:    p.handleCheckin,
			Capability: gateway.CapCheckin,
			Title:      "MiniMax 签到（单账号）",
		},
		{
			Method:     http.MethodPost,
			Path:       "/admin/minimax/checkin-all",
			Handler:    p.handleCheckinAll,
			Capability: gateway.CapCheckin,
			Title:      "MiniMax 签到（本上游全部）",
		},
		{
			Method:     http.MethodGet,
			Path:       "/admin/minimax/diagnose",
			Hidden:     true,
			Handler:    p.handleDiagnose,
			Capability: gateway.CapChat,
			Title:      "MiniMax 诊断",
		},
	}
}

// handleQuota 返回某账号（或全部账号）的积分视图。
//
// 查询参数 uid（缺省则返回全部）。
//
// ⚠ 取不到时报 `has_data:false` 而不是 0 —— 本仓的三态约定：
// 查不到（`—`）/ 真的是 0（`0`）/ 有余额（数字）。
// 把"查不到"报成 0 会让用户以为积分用尽，引发一轮无意义的排查。
func (p *Provider) handleQuota(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	uids := []string{uid}
	if uid == "" {
		uids = p.knownUIDs()
	}

	type row struct {
		UID       string `json:"uid"`
		Channel   string `json:"channel"`
		Remaining int64  `json:"remaining"`
		HasData   bool   `json:"has_data"`
		Note      string `json:"note,omitempty"`
	}
	// ⚠ 必须是**非 nil 空切片**：nil 会被序列化成 `null`，
	// 而前端 `data.accounts.length` 会因此抛 TypeError（本仓踩过）。
	rows := make([]row, 0, len(uids))
	for _, u := range uids {
		res := row{UID: u, Channel: "MiniMax Code"}
		if view, ok := p.RefreshQuota(u); ok && view.HasData {
			res.Remaining = view.Remaining
			res.HasData = true
		} else if !ok {
			res.Note = "账号不存在或凭证不可用"
		} else {
			res.Note = "上游没有返回余额"
		}
		rows = append(rows, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": providerID,
		"accounts": rows,
	})
}

// handleCheckin 单账号签到（DailyAction 的 OneURL）。
//
// 请求体 `{"uid":"…"}`（与核心的通用约定一致）。
func (p *Provider) handleCheckin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID string `json:"uid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON（需要 {uid}）")
		return
	}
	uid := strings.TrimSpace(body.UID)
	if uid == "" {
		writeError(w, http.StatusBadRequest, "缺少 uid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	res := p.checkinOne(ctx, uid)
	status := http.StatusOK
	if res.Status == "fail" {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, res)
}

// handleCheckinAll 本上游全部账号签到。
func (p *Provider) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	rep := p.CheckinAll(ctx)
	writeJSON(w, http.StatusOK, rep)
}

// handleDiagnose 令牌诊断（**只给长度与尾部，绝不回显完整令牌**）。
//
// 为什么要有它：令牌坏了时用户需要知道"是根本没有 / 还是过期了 /
// 还是格式不对"，而这些从账号池的列里看不出来。
//
// ⚠ 永不回显完整 token —— 它会被写进日志/截图/工单。
func (p *Provider) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	uids := []string{uid}
	if uid == "" {
		uids = p.knownUIDs()
	}
	type row struct {
		UID         string `json:"uid"`
		TokenLen    int    `json:"token_len"`
		TokenTail   string `json:"token_tail"`
		Usable      bool   `json:"usable"`
		Refreshable bool   `json:"refreshable"`
		ExpiresAtMS int64  `json:"expires_at_ms"`
		Expired     bool   `json:"expired"`
		Scope       string `json:"scope,omitempty"`
		Note        string `json:"note,omitempty"`
	}
	rows := make([]row, 0, len(uids))
	for _, u := range uids {
		a := p.cred(u)
		if a == nil {
			rows = append(rows, row{UID: u, Note: "账号不存在"})
			continue
		}
		tok := a.Token()
		rows = append(rows, row{
			UID:         u,
			TokenLen:    len(tok),
			TokenTail:   tail(tok, 4),
			Usable:      a.Usable(),
			Refreshable: a.Refreshable(),
			ExpiresAtMS: a.ExpiresAtMS(),
			Expired:     a.Expired(),
			Scope:       a.Scope,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": providerID,
		"accounts": rows,
	})
}

// tail 取字符串末尾 n 个字符（诊断用，配合长度足够定位问题）。
func tail(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return ""
	}
	return s[len(s)-n:]
}

// writeJSON 统一的 JSON 回执。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 统一的错误回执。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
