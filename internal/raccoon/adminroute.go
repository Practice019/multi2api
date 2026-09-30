// adminroute.go Raccoon 自己的管理端点（余额 + 一次性登录奖励）。
//
// # 为什么需要它
//
// `FetchBalance` 与 `ClaimLoginGrant` 此前都已实现、也有测试覆盖，
// 但本包**没有** AdminRoutes —— 也就是说这两件事**没有任何入口能调到**
// （前端看不到、命令行调不到，只有测试能碰）。
// cline 的余额端点踩过同一个坑（见那边的注释），这里一并补上。
//
// # ⚠ 登录奖励**不是**每日签到（这个区别必须保住）
//
// 三个积分来源的语义完全不同：
//
//	新人注册礼包  3000  注册时服务端自动发放      不涉及（用户注册即有）
//	桌面端登录奖励 3000  POST …/login/points/grant  ✅ 本文件的 claim 端点
//	每日积分发放   300   **服务端按日自动发放，无端点**  ❌ 不实现
//
// ⚠ **每日 300 没有签到端点** —— 实测该账号 13:30 注册、13:31 就收到
// `daily_grant` 账单（`biz_type: 'daily_grant'`）。故**不能**把它实现成
// 签到按钮：不存在可调用的端点，按钮必然失败。
//
// ⚠ **登录奖励是幂等一次性的**（已领过返回 `granted:false`），语义与
// 「新手任务」同构，**不是**「每日签到」—— 后者会让用户以为每天都真的
// 加了额度。所以：
//
//	Caps **不声明** CapCheckin（避免前端画一个"每天可领"的按钮）
//	端点用 onboarding 语义命名（/admin/raccoon/login-reward）
package raccoon

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/gateway"
)

// 管理端点路径。
//
// ⚠ 名字带 admin 前缀是为了与**上游 API 路径**区分：本包已有
// `balancePath`（= `/api/web/points/v1/balance`，见 raccoon.go），
// 那是发给上游的；这里是核心挂给前端的。两者重名会让读者以为
// 是同一个东西。
const (
	adminBalancePath     = "/admin/raccoon/balance"
	adminLoginRewardPath = "/admin/raccoon/login-reward"
	adminOnboardingPath  = "/admin/raccoon/onboarding"
)

// credentialSource 由装配层注入的"按 uid 取凭证"访问器。
//
// 上游包**不得** import internal/pool（架构判据 3）。
type credentialSource func(uid string) (gateway.Credential, bool)

// SetCredentialSource 注入凭证访问器（装配层调用）。
func (p *Provider) SetCredentialSource(fn credentialSource) { p.creds = fn }

// AdminRoutes 实现 gateway.AdminExt。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method:  http.MethodGet,
			Path:    adminBalancePath,
			Handler: p.handleBalance,
			Title:   "积分余额",
		},
		{
			// ⚠ POST：这是**写**操作（领取）。只读优先原则：
			// 打开面板这类高频路径绝不碰写端点（Loomy 的 first-login 踩过：
			// 打开面板即意外触发签到）。
			Method:  http.MethodPost,
			Path:    adminLoginRewardPath,
			Handler: p.handleLoginReward,
			Title:   "领取桌面端登录奖励（每号一次）",
		},
		{
			Method:  http.MethodGet,
			Path:    adminOnboardingPath,
			Handler: p.handleOnboarding,
			Title:   "登录奖励是否已领",
		},
	}
}

// resolveCred 从请求里取 uid 并解析出凭证。
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

// handleBalance 查积分余额。
//
// ⚠ 各池**分开作 package**，让用户看出「奖励 / 每日 / 会员 / 充值」
// 是独立来源 —— 它们的有效期与回补规则都不同
// （每日积分每日刷新、充值积分长期有效）。
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
			"ok": false, "error": "凭证结构不对",
		})
		return
	}
	b, err := p.client.FetchBalance(r.Context(), a)
	if err != nil {
		// 查不到时**如实报原因**，不要显示 0（0 是"已用光"的语义）
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": err.Error(),
		})
		return
	}
	items := make([]map[string]any, 0, 4)
	add := func(name string, v float64) {
		if v != 0 {
			items = append(items, map[string]any{"name": name, "remaining": v, "unit": "积分"})
		}
	}
	add("奖励积分", b.Reward)
	add("每日积分", b.Daily)
	add("会员积分", b.Monthly)
	add("充值积分", b.Topup)
	if len(items) == 0 {
		// 服务端只给 total 不给分项时也要有至少一个包，否则 UI 空列表。
		items = append(items, map[string]any{
			"name": "可用积分", "remaining": b.Total, "unit": "积分",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": b.Total, "unit": "积分", "items": items,
	})
}

// handleLoginReward 领取一次性登录奖励。
func (p *Provider) handleLoginReward(w http.ResponseWriter, r *http.Request) {
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
			"ok": false, "error": "凭证结构不对",
		})
		return
	}
	res, err := p.client.ClaimLoginGrant(r.Context(), a)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": err.Error(),
		})
		return
	}
	if !res.Claimed {
		// ⚠ 幂等判据是 `granted`（重复领取同样 HTTP 200 + granted:false），
		// 映射成 already-claimed 而**不是** claimed ——
		// 后者会让用户以为每次都真的加了额度。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"kind":    "already-claimed",
			"message": "该账号已领取过桌面端登录奖励（每号一次）",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "kind": "claimed", "credit": res.Points,
	})
}

// handleOnboarding 查登录奖励是否已领。
func (p *Provider) handleOnboarding(w http.ResponseWriter, r *http.Request) {
	cred, uid, ok := p.resolveCred(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "账号不在池里（uid=" + uid + "）",
		})
		return
	}
	a, ok := authFor(cred)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "凭证结构不对"})
		return
	}
	st := p.client.FetchOnboardingStatus(r.Context(), a)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "claimed": st.Claimed, "points": st.Points,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// 编译期断言：AdminExt。
var _ gateway.AdminExt = (*Provider)(nil)
