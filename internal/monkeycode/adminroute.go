// adminroute.go 本上游的管理端点。
//
// # 为什么必须有（不是"锦上添花"）
//
// 契约测试有一条硬判据：声明了 Caps 里的 CapModels / CapQuotaProbe / CapImport
// 就必须实现 AdminExt —— 因为这些能力的**可操作性**只能通过管理端点暴露。
// 不实现时契约测试直接判不合格（「声明了能力但没实现 AdminExt」）。
//
// # 只读优先
//
// 打开面板是高频路径，绝不碰写端点 —— Loomy 上踩过：
// 「打开面板即意外触发签到」。所以这里两条都是 GET。
package monkeycode

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/gateway"
)

// 管理端点路径。
const (
	adminQuotaPath  = "/admin/monkeycode/quota"
	adminModelsPath = "/admin/monkeycode/models"
	adminIssuePath  = "/admin/monkeycode/issue-key"
	// adminManualCompletePath 页内「添加账号」的粘贴完成端点。
	//
	// 由 ManualLogin() 报给前端（见 login.go），前端把
	// {state, code} POST 到这里。
	adminManualCompletePath = "/admin/monkeycode/login/complete"
	// 浏览器登录的进度查询与取消（前端轮询用）。
	adminLoginStatusPath = "/admin/monkeycode/login/status"
	adminLoginCancelPath = "/admin/monkeycode/login/cancel"
)

// AdminRoutes 本上游的管理端点。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			// 额度：账号池表格里已经有一列显示（QuotaExt），
			// 再单开一个导航页只是重复 —— 而且它渲染成原始 JSON 文本，
			// 用户实际不需要看这个形态。所以 Hidden。
			//
			// ⚠ Hidden **不影响端点可用**：路由照挂，能力位照留。
			// 它只回答"要不要为它生成一个左侧导航标签"。
			Method:     http.MethodGet,
			Path:       adminQuotaPath,
			Handler:    p.handleQuota,
			Capability: gateway.CapQuotaProbe,
			Title:      "额度",
			Hidden:     true,
		},
		{
			// 模型目录：`/v1/models` 与「可用模型」页已经列全了 45 条，
			// 这里再开一个页只是重复同一份数据（同样渲染成原始 JSON）。
			// 但端点要留 —— 排查"某个模型该走哪个协议"时它是唯一入口
			//（返回里有 interface_type）。
			Method:     http.MethodGet,
			Path:       adminModelsPath,
			Handler:    p.handleModels,
			Capability: gateway.CapModels,
			Title:      "模型目录",
			Hidden:     true,
		},
		{
			// ⚠ POST 是写操作，所以 Hidden：它不该在左侧导航占一个标签页。
			// 照网关统一约定的「纯动作端点」处理。
			Method:     http.MethodPost,
			Path:       adminIssuePath,
			Handler:    p.handleIssueKey,
			Capability: gateway.CapImport,
			Title:      "签发模型网关 key（用 session 换 api_key/signing_secret）",
			Hidden:     true,
		},
		{
			// 单账号签到：前端行内按钮 POST 它，body 带 uid。
			//
			// 走共享驱动的 HandlerOne —— 它负责"已签则早退 / 写历史 / 回结果"，
			// 本上游只管怎么签（见 checkin.go 的 Claim）。
			Method:     http.MethodPost,
			Path:       checkinDescriptor.OneURL,
			Handler:    p.handleCheckinOne,
			Capability: gateway.CapCheckin,
			Title:      "单账号签到",
			Hidden:     true,
		},
		{
			// 浏览器登录进度（前端每 2 秒轮询）。
			Method:     http.MethodGet,
			Path:       adminLoginStatusPath,
			Handler:    p.handleLoginStatus,
			Capability: gateway.CapImport,
			Title:      "登录进度",
			Hidden:     true,
		},
		{
			// 取消浏览器登录（关掉窗口 + 删临时 profile）。
			Method:     http.MethodPost,
			Path:       adminLoginCancelPath,
			Handler:    p.handleLoginCancel,
			Capability: gateway.CapImport,
			Title:      "取消登录",
			Hidden:     true,
		},
		{
			// 页内「添加账号」的粘贴完成端点。
			//
			// Hidden：它不是导航页 —— 入口是分组行上的「添加账号」按钮，
			// 那个按钮由 manifest 的 login 字段驱动（见 login.go 的 Configured）。
			Method:     http.MethodPost,
			Path:       adminManualCompletePath,
			Handler:    p.handleManualComplete,
			Capability: gateway.CapImport,
			Title:      "粘贴 session 完成添加账号",
			Hidden:     true,
		},
		{
			// 全量签到：分组标题上的「全部签到」按钮 POST 它，无参数。
			Method:     http.MethodPost,
			Path:       checkinDescriptor.AllURL,
			Handler:    p.handleCheckinAll,
			Capability: gateway.CapCheckin,
			Title:      "全部签到",
			Hidden:     true,
		},
	}
}

// resolveAuth 从请求里取 uid 并解析出凭证。
//
// 支持两种入口：`?uid=xxx` 查询参数，或 JSON body 里的 `uid`。
// 与 raccoon 的 resolveCred 同款（前端两种调用方式都在用）。
func (p *Provider) resolveAuth(r *http.Request) (*Auth, string, bool) {
	uid := r.URL.Query().Get("uid")
	if uid == "" && r.Body != nil {
		var body struct {
			UID string `json:"uid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			uid = body.UID
		}
	}
	if uid == "" {
		// ⚠ 不带 uid 时**回落到第一个账号**，而不是报错。
		//
		// # 为什么（实测）
		//
		// 前端打开这类只读面板时**不带 uid**，于是端点回 400
		// "缺少或找不到 uid"，界面上弹一条红字 —— 而模型目录本来
		// 就是上游级的东西（所有账号看到的是同一份），根本不需要
		// 指定账号。报错只会让人以为功能坏了。
		if p.uidEnumerator != nil {
			if all := p.uidEnumerator(); len(all) > 0 {
				uid = all[0]
			}
		}
		if uid == "" {
			return nil, "", false
		}
	}
	a := p.cred(uid)
	if a == nil {
		return nil, uid, false
	}
	return a, uid, true
}

// handleQuota 查额度。
//
// 单号（`?uid=`）或多号（不带 uid = 全部）都可。
// 多号走 uidEnumerator —— 没有它就是空数组（zcode 上踩过的"看起来没账号"）。
func (p *Provider) handleQuota(w http.ResponseWriter, r *http.Request) {
	type row struct {
		UID               string `json:"uid"`
		Nickname          string `json:"nickname,omitempty"`
		Region            string `json:"region"`
		Balance           int64  `json:"balance"`
		DailyTokenBalance int64  `json:"daily_token_balance"`
		DailyTokenLimit   int64  `json:"daily_token_limit"`
		Error             string `json:"error,omitempty"`
	}

	var uids []string
	if one := r.URL.Query().Get("uid"); one != "" {
		uids = []string{one}
	} else if p.uidEnumerator != nil {
		uids = p.uidEnumerator()
	}

	rows := make([]row, 0, len(uids))
	for _, uid := range uids {
		a := p.cred(uid)
		if a == nil {
			rows = append(rows, row{UID: uid, Error: "凭证不存在"})
			continue
		}
		out := row{UID: uid, Nickname: a.DisplayName(), Region: a.RegionOrDefault()}
		wallet, err := p.client.FetchWallet(r.Context(), a)
		if err != nil {
			out.Error = err.Error()
		} else {
			out.Balance = wallet.Balance
			out.DailyTokenBalance = wallet.DailyTokenBalance
			out.DailyTokenLimit = wallet.DailyTokenLimit
		}
		rows = append(rows, out)
	}

	writeJSON(w, http.StatusOK, map[string]any{"accounts": rows})
}

// handleModels 拉远端模型目录（含 interface_type，便于排查协议路由）。
func (p *Provider) handleModels(w http.ResponseWriter, r *http.Request) {
	a, uid, ok := p.resolveAuth(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "账号池里没有 monkeycode 账号（先把凭证放进 auths/monkeycode）"})
		return
	}
	list, err := p.client.FetchModels(r.Context(), a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "uid": uid})
		return
	}
	learnInterfaces(list)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "models": list})
}

// handleIssueKey 用 session 签一把模型网关 key，并就地更新内存里的凭证。
//
// ⚠ 只改内存，**不落盘**：落盘要经过凭证目录的原子写，
// 那是导入流程的责任。这里返回新 key，由调用方决定是否写回。
func (p *Provider) handleIssueKey(w http.ResponseWriter, r *http.Request) {
	a, uid, ok := p.resolveAuth(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少或找不到 uid"})
		return
	}
	name := r.URL.Query().Get("name")
	pk, err := p.client.CreateAPIKey(r.Context(), a, name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "uid": uid})
		return
	}
	p.mu.Lock()
	a.APIKey = pk.APIKey
	a.SigningSecret = pk.SigningSecret
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "api_key": pk.APIKey, "signing_secret": pk.SigningSecret,
		"note": "已更新内存中的凭证；如需持久化请写回凭证目录",
	})
}

// writeJSON 统一的 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleCheckinOne 单账号签到（转给共享驱动）。
func (p *Provider) handleCheckinOne(w http.ResponseWriter, r *http.Request) {
	if p.checkin == nil || !p.checkin.Ready() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "签到驱动未就绪"})
		return
	}
	p.checkin.HandlerOne(w, r)
}

// handleCheckinAll 全量签到（转给共享驱动）。
func (p *Provider) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	if p.checkin == nil || !p.checkin.Ready() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "签到驱动未就绪"})
		return
	}
	p.checkin.HandlerAll(w, r)
}

// handleLoginStatus 浏览器登录进度（前端轮询）。
func (p *Provider) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 id"})
		return
	}
	st := p.BrowserLoginStatusOf(id)
	if st == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "会话不存在或已过期"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": st})
}

// handleLoginCancel 取消浏览器登录。
func (p *Provider) handleLoginCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	_ = jsonDecode(r, &body)
	if body.ID == "" {
		body.ID = r.URL.Query().Get("id")
	}
	if body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 id"})
		return
	}
	if !p.CancelBrowserLogin(body.ID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "会话不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
