// adminroute.go LobsterAI 自己的管理端点。
//
// # 为什么必须有它（不是可选的装饰）
//
// gateway 的契约测试强制："声明了的能力必须真的可达"。本 Provider 声明了
// CapCheckin，而签到能力**只能通过管理端点暴露** —— 所以没有 AdminRoutes
// 就会被契约测试判不合格（这正是它跑起来抓到的第一条）。
//
// # 两条路由的分工
//
//	POST /admin/lobsterai/checkin  一键签到（三步流程，见 client.ClaimDailyCheckin）
//	GET  /admin/lobsterai/balance  积分余额（profile-summary，含活动积分）
//
// 都**不是** Hidden：它们是面板上的真实入口（LobsterAI 有签到与余额两项能力）。
//
// # 请求/响应约定（与其它上游的管理端点一致）
//
// 请求体/查询参数里带 `uid`（账号池主键），端点自己从账号池取凭证 ——
// 前端不认识凭证结构，只认识 uid。凭证怎么取见 credentialSource 的注释。
package lobsterai

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/gateway"
)

// 管理端点路径。
const (
	checkinPath = "/admin/lobsterai/checkin"
	balancePath = "/admin/lobsterai/balance"
)

// credentialSource 由装配层注入的"按 uid 取凭证"访问器。
//
// # 为什么用注入而不是让本包依赖 pool
//
// 上游包**不得** import internal/pool（架构判据 3）。核心只把
// "给我某个 uid 的凭证"这个动作适配进来 —— 与其它上游同一模式。
//
// 返回 nil 表示该 uid 不在池里（已删除/未并入），端点据此回报 404。
type credentialSource func(uid string) (gateway.Credential, bool)

// SetCredentialSource 注入凭证访问器（装配层调用）。
func (p *Provider) SetCredentialSource(fn credentialSource) { p.creds = fn }

// AdminRoutes 实现 gateway.AdminExt。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method:     http.MethodPost,
			Path:       checkinPath,
			Handler:    p.handleCheckin,
			Capability: gateway.CapCheckin,
			Title:      "每日签到",
		},
		{
			Method:  http.MethodGet,
			Path:    balancePath,
			Handler: p.handleBalance,
			Title:   "积分余额",
		},
	}
}

// resolveCred 从请求里取 uid 并解析出凭证。
//
// uid 从 query 或 JSON body 里取（前端两种都用过）。
func (p *Provider) resolveCred(r *http.Request) (gateway.Credential, string, bool) {
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		var body struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		uid = body.UID
	}
	if uid == "" {
		return gateway.Credential{}, "", false
	}
	if p.creds == nil {
		return gateway.Credential{}, uid, false
	}
	cred, ok := p.creds(uid)
	return cred, uid, ok
}

// handleCheckin 一键签到。
func (p *Provider) handleCheckin(w http.ResponseWriter, r *http.Request) {
	cred, uid, ok := p.resolveCred(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "账号不在池里（uid=" + uid + "）",
		})
		return
	}
	out := p.ClaimDailyCheckin(r.Context(), cred)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      out.Kind != "failed",
		"kind":    out.Kind,
		"credit":  out.Credit,
		"message": out.Message,
	})
}

// handleBalance 查积分余额。
func (p *Provider) handleBalance(w http.ResponseWriter, r *http.Request) {
	cred, uid, ok := p.resolveCred(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "账号不在池里（uid=" + uid + "）",
		})
		return
	}
	b, err := p.Balance(r.Context(), cred)
	if err != nil {
		// 查不到时**如实报原因**，不要显示 0（0 是"已用光"的语义）
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": err.Error(),
		})
		return
	}
	items := make([]map[string]any, 0, len(b.Items))
	for _, it := range b.Items {
		items = append(items, map[string]any{
			"type":       it.Type,
			"remaining":  it.Remaining,
			"expires_at": it.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"total": b.Total,
		"unit":  creditBalanceUnitLabel,
		"items": items,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// 编译期断言：AdminExt。
var _ gateway.AdminExt = (*Provider)(nil)
