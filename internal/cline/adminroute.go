// adminroute.go Cline 自己的管理端点（余额）。
//
// # 为什么需要它（一个**悬空承诺**的修复）
//
// provider.go 的 Caps 注释写着「余额是另一套（/users/{id}/balance，
// 见 FetchBalance），**由 AdminExt 暴露**」—— 而本包此前**没有** AdminRoutes。
// 也就是说那个余额查询虽然实现了、却**没有任何入口能调到它**：
// 前端看不到、命令行调不到、只有测试能碰。
//
// 这不是"少写一个端点"那么轻：它让一句注释变成了**不成立的事实**。
// 代码里的承诺与代码实际做的事不符，比没有承诺更糟 ——
// 后来者读到那句注释会以为"已经接好了"，于是不去查。
//
// # Cline 只有余额，没有签到
//
// 对整个 sidecar 做字符串扫描，checkin / check-in / daily / campaign
// 均无 Cline 业务端点命中（campaign 的命中是 PostHog 的 UTM 参数与
// feature-flag 事件属性；daily 是 YAML cron 别名与 Blob 导出频率枚举）。
//
// 故这里只有一条余额路由，且 Caps **不声明** CapCheckin ——
// 声明了等于给前端画一个点了必然失败的面板。
// （与 WorkBuddy 国际版的先例一致：那边也显式禁用了玩法类能力。）
package cline

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/gateway"
)

// balancePath 余额端点路径。
const balancePath = "/admin/cline/balance"

// credentialSource 由装配层注入的"按 uid 取凭证"访问器。
//
// # 为什么用注入而不是让本包依赖 pool
//
// 上游包**不得** import internal/pool（架构判据 3）。核心只把
// "给我某个 uid 的凭证"这个动作适配进来 —— 与其它上游同一模式。
type credentialSource func(uid string) (gateway.Credential, bool)

// SetCredentialSource 注入凭证访问器（装配层调用）。
func (p *Provider) SetCredentialSource(fn credentialSource) { p.creds = fn }

// AdminRoutes 实现 gateway.AdminExt。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method:  http.MethodGet,
			Path:    balancePath,
			Handler: p.handleBalance,
			Title:   "账户余额",
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

// handleBalance 查账户余额。
func (p *Provider) handleBalance(w http.ResponseWriter, r *http.Request) {
	cred, uid, ok := p.resolveCred(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "error": "账号不在池里（uid=" + uid + "）",
		})
		return
	}
	a, err := authOf(cred)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": err.Error(),
		})
		return
	}
	res := p.client.FetchBalance(r.Context(), a)
	if res.Error != "" {
		// 查不到时**如实报原因**，不要显示 0（0 是"已用光"的语义）。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": res.Error,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		// total 是换算后的量级（÷ BalanceScale）。
		"total": normalizedBalance(res.Raw),
		"unit":  "USD",
		// raw 一并返回：单位换算是本模块**唯一的不确定点**，
		// 把原始值透出来，便于用真实凭据核对（见 balanceScale 的注释）。
		"raw": res.Raw,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// 编译期断言：AdminExt。
var _ gateway.AdminExt = (*Provider)(nil)
