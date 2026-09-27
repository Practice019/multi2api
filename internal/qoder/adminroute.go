// adminroute.go Qoder 自己的管理端点（签到 + 积分余额）。
//
// # 为什么必须有它（不是可选的装饰）
//
// gateway 的契约测试强制："声明了的能力必须真的可达"。本 Provider 现在
// 声明 CapCheckin，而签到能力**只能通过管理端点暴露** —— 没有 AdminRoutes
// 就会被契约测试判不合格（lobsterai 接入时正是这样被第一条抓到的）。
//
// # 两条路由的分工
//
//	POST /admin/qoder/checkin   一键签到（查活动 → 逐个领取，见 credits.go）
//	GET  /admin/qoder/balance   积分余额（usage，含资源包）
//
// # 请求/响应约定（与其它上游的管理端点一致）
//
// 请求体/查询参数里带 `uid`（账号池主键），端点自己从账号池取凭证 ——
// 前端不认识凭证结构，只认识 uid。凭证怎么取见 credentialSource 的注释。
//
// ⚠ 两个产品（qoder / qodercn）共用同一套端点：它们同协议族、
// 同一份凭证目录（靠 product_id 区分）。端点前缀按**实例 ID** 生成，
// 所以 qodercn 实例会挂 `/admin/qodercn/checkin`。
package qoder

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/gateway"
)

// credentialSource 由装配层注入的"按 uid 取凭证"访问器。
//
// # 为什么用注入而不是让本包依赖 pool
//
// 上游包**不得** import internal/pool（架构判据 3）。核心只把
// "给我某个 uid 的凭证"这个动作适配进来 —— 与其它上游同一模式。
//
// 返回 false 表示该 uid 不在池里（已删除/未并入），端点据此回报 404。
type credentialSource func(uid string) (gateway.Credential, bool)

// SetCredentialSource 注入凭证访问器（装配层调用）。
func (p *Provider) SetCredentialSource(fn credentialSource) { p.creds = fn }

// checkinPath / balancePath 按实例 ID 生成端点路径。
//
// ⚠ 用 p.ID() 而不是包级常量：两个产品是**同类型的两份实例**，
// 路径必须按实例区分，否则 qodercn 的签到会作用在 qoder 的账号上
//（前端按 uid 调用，而两个产品的账号池是分开的 —— 但路径相同会让
// 路由表里两条同路径注册冲突）。
func (p *Provider) checkinPath() string { return "/admin/" + p.ID() + "/checkin" }
func (p *Provider) balancePath() string { return "/admin/" + p.ID() + "/balance" }

// AdminRoutes 实现 gateway.AdminExt。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method:     http.MethodPost,
			Path:       p.checkinPath(),
			Handler:    p.handleCheckin,
			Capability: gateway.CapCheckin,
			Title:      "每日签到",
		},
		{
			Method:  http.MethodGet,
			Path:    p.balancePath(),
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

// authFor 从核心凭证里取出本包认识的 Auth。
func authFor(cred gateway.Credential) (*Auth, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return nil, false
	}
	return a, true
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
	a, ok := authFor(cred)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": "凭证结构不对（可能来自另一个产品的目录）",
		})
		return
	}
	out := p.client.ClaimDailyCheckin(r.Context(), a)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      out.Kind != "failed",
		"kind":    out.Kind,
		"credit":  out.Credit,
		"message": out.Message,
		// actionRequired 是给 UI 的**显式信号**（而非让它去猜文案）：
		// 这条 inactive 需要用户去官方客户端登录一次，必须单独醒目展示。
		"action_required": out.ActionRequired,
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
	a, ok := authFor(cred)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": "凭证结构不对（可能来自另一个产品的目录）",
		})
		return
	}
	b, ok := p.client.FetchCreditBalance(r.Context(), a)
	if !ok {
		// 查不到时**如实报原因**，不要显示 0（0 是"已用光"的语义）
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": "查询失败（网络 / 凭据 / 响应形状异常）",
		})
		return
	}
	pkgs := make([]map[string]any, 0, len(b.Packages))
	for _, pkg := range b.Packages {
		pkgs = append(pkgs, map[string]any{
			"name":      pkg.Name,
			"unit":      pkg.Unit,
			"remaining": pkg.Remaining,
			"total":     pkg.Total,
			"used":      pkg.Used,
			"expires_at": pkg.ExpiredTime,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    b.Total,
		"unit":     "credits",
		"packages": pkgs,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// 编译期断言：AdminExt。
var _ gateway.AdminExt = (*Provider)(nil)
