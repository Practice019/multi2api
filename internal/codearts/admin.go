// admin.go CodeArts 自己的管理端点（gateway.AdminExt 实现）。
//
// # 为什么这四条端点在**本包**而不是核心的 admin
//
// 它们是**平台特殊**的：workbuddy 一个都没有。
//
//	GET  /admin/welfare             福利活动列表
//	POST /admin/welfare/claim       领取所有可领福利
//	GET  /admin/subscription        套餐与额度
//	POST /admin/models/quota-probe  主动探测按模型额度
//
// 改造前它们硬编码在 core 的 internal/admin/admin.go 里，靠 nil 判断降级
// （h.cfg.Welfare == nil → 501）。那样有两个问题：
//
//  1. 加第 3 个上游时，核心又得为它加一批路由与字段（核心在**跟着上游长**）
//  2. nil 判断是**运行期**的：配错了不会报错，只会静默 501
//
// 现在改为上游自注册：核心遍历 registry 取出各 Provider 的 AdminRoutes() 挂载，
// **加新上游时 core 的 admin 包零改动**（判据 1）。
//
// # 为什么不持有账号池
//
// 上游包**不得**依赖 internal/pool（架构约束 arch_test.go 强制）。
// 需要"池里有哪些账号"时，由核心通过 AdminEnv.Accounts 以**函数**形式喂进来，
// 方向是"核心给上游数据"，不是"上游去读核心"。
//
// # uid 的语义
//
// 四条端点都接受可选 `uid`（query 或 body）。未指定时取账号列表的第一个 ——
// 与改造前 admin 的行为一致（"未指定账号时取池中第一个"）。
package codearts

import (
	"encoding/json"
	"log"
	"net/http"

	"workbuddy2api/internal/gateway"
)

// AdminEnv 核心为上游管理端点提供的只读依赖。
//
// 全部可缺省：nil 时各端点降级（返回明确错误），**不 panic**。
type AdminEnv struct {
	// Accounts 返回当前账号的 uid 列表（核心注入；nil 时本包按 AuthDir 现场扫描）。
	//
	// 为什么是 uid 字符串而不是凭证对象：管理端点只需要"选哪个账号"，
	// 由本包自己按 uid 解析出凭证。核心不需要理解 CodeArts 凭证的结构。
	Accounts func() []string
	// Resolve 按 uid 取凭证（核心注入；nil 时本包按 AuthDir 现场扫描）。
	Resolve func(uid string) (*Auth, error)
}

// AdminRoutes 返回 CodeArts 专属的管理端点（gateway.AdminExt）。
//
// 每条路由都标了 Capability —— 前端据此决定是否显示入口，
// 能力由**后端下发**，前端不硬编码（见 gateway.AdminRoute 的注释）。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method: "GET", Path: "/admin/welfare",
			Handler: p.handleWelfareList, Capability: gateway.CapWelfare, Title: "福利中心",
		},
		{
			Method: "POST", Path: "/admin/welfare/claim",
			Handler: p.handleWelfareClaim, Capability: gateway.CapWelfare, Title: "领取福利",
		},
		{
			Method: "GET", Path: "/admin/subscription",
			Handler: p.handleSubscription, Capability: gateway.CapWelfare, Title: "套餐与额度",
		},
		{
			Method: "POST", Path: "/admin/models/quota-probe",
			Handler: p.handleQuotaProbe, Capability: gateway.CapQuotaProbe, Title: "额度探测",
		},
	}
}

// ---------------------------------------------------------------- 端点实现

// handleWelfareList 列出当前账号的福利活动。
func (p *Provider) handleWelfareList(w http.ResponseWriter, r *http.Request) {
	a, err := p.accountFor(r.URL.Query().Get("uid"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := p.client.FetchWelfare(a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "拉取福利失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":   a.UID,
		"items": items,
		"count": len(items),
	})
}

// handleWelfareClaim 领取所有当前可领的福利。
func (p *Provider) handleWelfareClaim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	a, err := p.accountFor(body.UID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	results, err := p.client.ClaimAllWelfare(a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "领取失败: "+err.Error())
		return
	}
	claimed := 0
	for _, x := range results {
		if x.Claimed {
			claimed++
		}
	}
	log.Printf("codearts: 福利领取 uid=%s 本次领到 %d/%d", a.UID, claimed, len(results))
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": a.UID, "results": results, "claimed": claimed,
	})
}

// handleSubscription 查套餐与额度。
func (p *Provider) handleSubscription(w http.ResponseWriter, r *http.Request) {
	a, err := p.accountFor(r.URL.Query().Get("uid"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub, err := p.client.FetchSubscription(a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "拉取订阅失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": a.UID, "subscription": sub})
}

// handleQuotaProbe 主动探测所有模型的额度状态。
//
// 为什么是**手动**而不是定时：探测要给每个模型发一次真实上游请求
// （约 7 次），既消耗额度又占会话名额（每账号只有 3 个并发会话）。
// 定时跑会在后台持续挤占配额，得不偿失。
//
// 探测是**串行**的（ProbeAllQuota 内部）：并发探测会互相挤掉并发名额，
// 拿到的就是"并发上限"而不是真实额度状态 —— 那比不探测更误导。
func (p *Provider) handleQuotaProbe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	a, err := p.accountFor(body.UID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	states := p.client.ProbeAllQuota(a)

	res := make(map[string]any, len(states))
	exhausted := 0
	for k, v := range states {
		if v.Exhausted {
			exhausted++
		}
		res[k] = map[string]any{
			"exhausted":  v.Exhausted,
			"reason":     v.Reason,
			"checked_at": v.CheckedAt,
		}
	}
	log.Printf("codearts: 额度探测完成，共 %d 个模型，%d 个耗尽", len(res), exhausted)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":       a.UID,
		"models":    res,
		"exhausted": exhausted,
		"total":     len(res),
	})
}

// ---------------------------------------------------------------- 账号解析

// accountFor 按 uid 取凭证；uid 为空时取第一个账号。
//
// 为什么要有这条回落：管理台的面板上多数操作是"对当前唯一账号做点什么"，
// 每次都要用户先选账号会很啰嗦。改造前 core 的行为就是这个语义，
// 这里保持一致（避免搬运后行为漂移）。
func (p *Provider) accountFor(uid string) (*Auth, error) {
	if p.adminEnv.Resolve != nil {
		if uid == "" {
			uid = p.firstUID()
		}
		if uid == "" {
			return nil, errNoAccount
		}
		return p.adminEnv.Resolve(uid)
	}

	// 未接线：按 AuthDir 现场扫描（测试与"没有账号池"的部署路径）。
	list, err := p.localAccounts()
	if err != nil {
		return nil, err
	}
	if uid == "" {
		if len(list) == 0 {
			return nil, errNoAccount
		}
		return list[0], nil
	}
	for _, a := range list {
		if a.UID == uid || a.AccessKey == uid {
			return a, nil
		}
	}
	return nil, errUnknownAccount(uid)
}

// firstUID 取账号列表的第一个 uid（核心注入的访问器优先）。
func (p *Provider) firstUID() string {
	if p.adminEnv.Accounts != nil {
		if ids := p.adminEnv.Accounts(); len(ids) > 0 {
			return ids[0]
		}
		return ""
	}
	list, err := p.localAccounts()
	if err != nil || len(list) == 0 {
		return ""
	}
	return list[0].UID
}

// localAccounts 按 AuthDir 现场扫描凭证（未注入访问器时的回落路径）。
//
// 每次都重新扫盘：管理端点是低频操作，而"用户刚跑完 cmd/login 就点开面板"
// 是最常见的用法 —— 缓存会让他看不到新账号。
func (p *Provider) localAccounts() ([]*Auth, error) {
	if p.accounts != nil {
		return p.accounts(), nil
	}
	if p.authDir == "" {
		return nil, nil
	}
	return LoadDir(p.authDir)
}

// ---------------------------------------------------------------- HTTP 辅助

// writeJSON 写一个 JSON 响应。
//
// 本包不 import 核心的 admin（架构约束），所以这两个小助手在本地各留一份。
// 它们只有三行，重复的代价远小于让上游反向依赖核心。
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeError 写一个 {"error": "..."} 响应（与核心 admin 的形态一致，
// 前端可以用同一套错误处理）。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// ---------------------------------------------------------------- 错误

// errNoAccount 账号池为空（或未接线）。
var errNoAccount = errAdmin("账号池为空（未配置 CodeArts 凭证或账号池未接线）")

// errUnknownAccount 按 uid 找不到账号。
func errUnknownAccount(uid string) error { return errAdmin("账号不存在: " + uid) }

// errAdmin 管理端点的错误类型（让错误信息带上"这是管理面"的上下文）。
type errAdmin string

func (e errAdmin) Error() string { return string(e) }
