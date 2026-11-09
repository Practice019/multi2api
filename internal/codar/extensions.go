package codar

// extensions.go —— P0 扩展点（T-317）。
//
// 全部**只读**：4 条管理端点只查不改（签到/领取的写操作是 P1，本轮刻意不碰）。
// 端点响应结构全部来自 2026-11-08 的真机实测（各 1 次 GET/查询 POST）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：P0 四个小扩展点 + 管理端点确实注册得上去。
var (
	_ gateway.DisplayNameExt        = (*Provider)(nil)
	_ gateway.NoticeExt             = (*Provider)(nil)
	_ gateway.CredentialTokenExt    = (*Provider)(nil)
	_ gateway.CredentialLifetimeExt = (*Provider)(nil)
	_ gateway.AdminExt              = (*Provider)(nil)
)

// SetCredentialSource 注入"按 uid 取活凭证"（装配层给；上游不得 import pool）。
func (p *Provider) SetCredentialSource(fn func(uid string) (gateway.Credential, bool)) {
	p.credSrc = fn
}

// DisplayName 界面中文名（不实现就显示裸 id "codar"）。
func (p *Provider) DisplayName() string { return "科搭 Codar" }

// Notice 分组里的一句提醒。
//
// 两句都是**实测踩出来的**，不是客套话：
//  1. 凭证是桌面端会话 token 的副本 ⇒ 会过期（界面能看到到期时间，到期需重新导入）。
//  2. 上游已有风控记录（T-307：社区出现 "API access is restricted"）⇒ 别高频打。
func (p *Provider) Notice() string {
	return "凭证取自桌面端登录态（会过期，见到期时间）；上游已有风控记录，请低频使用。"
}

// HasToken 这份凭证有没有 access token（token 是唯一鉴权材料）。
func (p *Provider) HasToken(cred gateway.Credential) bool {
	a, err := authOf(cred)
	return err == nil && a.Token != ""
}

// NeverExpires 本上游凭证**设计上**会过期（JWT 带 exp）⇒ 恒 false。
//
// ⚠ 契约要求"拿不准必须返回 false"：报 true 会让核心不再续期，
// 而 token 一过期整个账号就废（这正是 T-315 修 expiresAt 的同一个坑）。
func (p *Provider) NeverExpires(cred gateway.Credential) bool { return false }

// ── 管理端点（4 条，全只读）──────────────────────────────────────────

// AdminRoutes 端点。
//
//	GET /admin/codar/usage  → 用量（/v1/codar/usage/daily）
//	GET /admin/codar/quota  → 余额（POST {Pay}/v1/agent/balance，**查询**不是转账）
//	GET /admin/codar/sign   → 签到状态（POST {Pay}/v1/daily-sign/status，**只查状态**）
//
// ⚠ T-465：原有一条只读端点（查上游赠送模型活动的状态）已删除，连它的 Title 一起 ——
// 那正是用户在账号池里看到的那个按钮的来源。理由见 p1.go 的 DailyActions()。
//
// Capability 一律给 0（通用端点）：我们没声明 CapQuotaProbe/CapCheckin 等能力位，
// 若在这里报一个没声明过的能力位，前端会画出点了必然失败的按钮（本仓明确禁止多报）。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{Method: http.MethodGet, Path: "/admin/codar/usage", Handler: p.handleUsage, Title: "用量"},
		{Method: http.MethodGet, Path: "/admin/codar/quota", Handler: p.handleQuota, Title: "余额"},
		{Method: http.MethodGet, Path: "/admin/codar/sign", Handler: p.handleSignStatus, Title: "签到状态"},
		{Method: http.MethodGet, Path: "/admin/codar/invite", Handler: p.handleInvite, Capability: gateway.CapInvite, Title: "邀请码"},
		{Method: http.MethodPost, Path: "/admin/codar/invite/bind", Handler: p.handleInviteBind, Capability: gateway.CapInvite, Title: "绑定邀请码"},
		{Method: http.MethodGet, Path: "/admin/codar/invite-page", Handler: p.handleInvitePage, Capability: gateway.CapInvite, Title: "邀请页"},
		{Method: http.MethodPost, Path: "/admin/codar/invite-page/bind", Handler: p.handleInvitePageBind, Capability: gateway.CapInvite, Title: "邀请页绑定", Hidden: true},
		// ── T-364：微信绑定（邀请奖励第二段；Capability 沿用 CapInvite —— 它属于邀请体系）──
		{Method: http.MethodGet, Path: wechatQRPath, Handler: p.handleWechatQR, Capability: gateway.CapInvite, Title: "微信二维码"},
		{Method: http.MethodGet, Path: wechatStatusPath, Handler: p.handleWechatStatus, Capability: gateway.CapInvite, Title: "微信绑定状态", Hidden: true},
		{Method: http.MethodGet, Path: wechatPagePath, Handler: p.handleWechatPage, Capability: gateway.CapInvite, Title: "微信绑定页"}, // ── T-324：手机号登录（自带页面 + 发码/验码；用户明确要手机号，不要微信）──
		{Method: http.MethodGet, Path: loginPagePath, Handler: p.handleLoginPage, Title: "手机号登录"},
		{Method: http.MethodPost, Path: loginSMSPath, Handler: p.handleLoginSMS, Title: "发送验证码", Hidden: true},
		{Method: http.MethodPost, Path: loginVerPath, Handler: p.handleLoginVerify, Title: "验证码登录", Hidden: true},
		{Method: http.MethodPost, Path: loginIPPath, Handler: p.handleLoginIPVerify, Title: "IP 验证", Hidden: true},
		// ── P1：写端点（只在用户点按钮时执行；没有自动循环，理由见 p1.go）──
		{Method: http.MethodPost, Path: signOnePath, Handler: p.handleSignOne, Title: "签到", Hidden: true},
		{Method: http.MethodPost, Path: checkinAllPath, Handler: p.handleSignAll, Title: "全部签到"},
	}
}

// pickAuth 取一个可用凭证：优先 uid 参数，其次**唯一的/第一个**。
func (p *Provider) pickAuth(uid string) (*Auth, error) {
	if p.credSrc == nil {
		return nil, fmt.Errorf("codar: 未注入凭证源（装配层漏了 SetCredentialSource）")
	}
	if uid != "" {
		cred, ok := p.credSrc(uid)
		if !ok {
			return nil, fmt.Errorf("codar: 找不到 uid=%s 的凭证", uid)
		}
		return authOf(cred)
	}
	for _, cred := range p.loadAll() {
		if a, err := authOf(cred); err == nil {
			return a, nil
		}
	}
	return nil, fmt.Errorf("codar: 没有任何可用凭证")
}

// loadAll 通过凭证源拿"全部凭证"—— 用 authDir 扫目录（与 LoadCredentials 同源）。
//
// ⚠ 取的是 `CredentialSecret.Credential`（内嵌的结构体），不是整个 CredentialSecret：
// 调用方（authOf/AdminRoute）只吃 gateway.Credential，而 Secret 已经在这一份里
// （`LoadCredentialsWithSecrets` 把上游私有凭证塞进 `Credential.Secret` 之外的
//
//	`CredentialSecret.Secret`，这里取内嵌值即"投影 + 同一个 Secret 指针"）。
//
// 注：本方法此前直接 `return creds` —— 在 `LoadCredentialsWithSecrets` 的返回类型
// 被修正成接口要求的 `[]gateway.CredentialSecret` 之后，这里必须同步取内嵌值。
func (p *Provider) loadAll() []gateway.Credential {
	items, err := p.LoadCredentialsWithSecrets(p.authDir)
	if err != nil {
		return nil
	}
	out := make([]gateway.Credential, 0, len(items))
	for _, it := range items {
		c := it.Credential
		c.Secret = it.Secret // 上游私有凭证要跟着走，否则 authOf 断言失败
		out = append(out, c)
	}
	return out
}

// callJSON 打一次上游（只读用）。返回原始 body。
func (p *Provider) callJSON(ctx context.Context, method, url string, a *Auth, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	ua := p.userAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	// ⚠ 扫码的 qrcode / status 端点**不需要鉴权**（T-322 实测）⇒ a 可能为 nil。
	// 不判空会空指针 panic（本仓契约测试当场抓住过一次）。
	if a != nil && a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	resp, err := p.client.http().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

// writeJSON 统一的响应出口（上游状态码原样带回，便于排障）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleUsage GET /admin/codar/usage?uid=&start=&end=
//
// 实测响应（2026-11-08）：
//
//	{"success":true,"data":{"timezone":"Asia/Shanghai","start_date":..,"end_date":..,
//	  "models":[...],"summary":{"calls":11,"input_tokens":56378,"output_tokens":7434,
//	   "total_tokens":63812,"used_models":3},"daily":[{"date":"..","calls":0,...}]}}
func (p *Provider) handleUsage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	a, err := p.pickAuth(r.URL.Query().Get("uid"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	end := r.URL.Query().Get("end")
	if end == "" {
		end = time.Now().Format("2006-01-02")
	}
	start := r.URL.Query().Get("start")
	if start == "" {
		start = time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	}
	u := p.apiBase + "/v1/codar/usage/daily?start_date=" + url.QueryEscape(start) + "&end_date=" + url.QueryEscape(end)
	raw, status, err := p.callJSON(ctx, http.MethodGet, u, a, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// handleQuota GET /admin/codar/quota?uid=
//
// T-348 定稿：余额 = 三桶之和（agent + general + rewrite，有 T-347 消费实验支撑）。
// ⚠ 不含 used / limit —— `limit - balance` 的恒等式被实验否决（新账号会算出假的"已用 500"）。
// gettoken 拿不到时：general/rewrite 为 **null**（不是 0）并标 partial=true，
// 因为"缺一个桶"和"那个桶是 0"是两件事。
func (p *Provider) handleQuota(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	a, err := p.pickAuth(r.URL.Query().Get("uid"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	rawBal, statusBal, errBal := p.callJSON(ctx, http.MethodPost, p.payBase+"/v1/agent/balance", a,
		map[string]any{"username": a.UID})
	if errBal != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": errBal.Error()})
		return
	}
	var bal struct {
		Data struct {
			AgentBalance int64 `json:"agent_balance"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rawBal, &bal)
	agent := bal.Data.AgentBalance

	rawGen, statusGen, errGen := p.callJSON(ctx, http.MethodPost, p.payBase+"/v1/gettoken", a,
		map[string]any{"username": a.UID, "token": 0})
	var gen struct {
		Token        int64 `json:"token"`
		AgentToken   int64 `json:"agent_token"`
		RewriteToken int64 `json:"rewrite_token"`
	}
	haveGen := errGen == nil && statusGen == http.StatusOK && json.Unmarshal(rawGen, &gen) == nil

	detail := map[string]any{
		"agent":       map[string]any{"balance": agent},
		"general":     map[string]any{"balance": nil},
		"rewrite":     map[string]any{"balance": nil},
		"total":       agent,
		"partial":     true,
		"spend_order": "先扣 agent，agent 为空才扣 general（T-347 消费实验）",
		"source":      map[string]any{"agent": "/v1/agent/balance", "general": "/v1/gettoken"},
		"note":        "余额 = agent + general + rewrite（三桶都能花）；缺失的桶为 null 而非 0",
	}
	total := agent
	if haveGen {
		total = agent + gen.Token + gen.RewriteToken
		detail["general"] = map[string]any{"balance": gen.Token}
		detail["rewrite"] = map[string]any{"balance": gen.RewriteToken}
		detail["total"] = total
		detail["partial"] = false
		detail["agent_token_crosscheck"] = gen.AgentToken // 与 agent/balance 同值，可交叉验证
	}
	writeJSON(w, statusBal, map[string]any{
		"ok": true, "uid": a.UID, "upstream_status": statusBal,
		"agent_balance": total, // 字段名不变（前端兼容）；现在是**可花总和**
		"quota_detail":  detail,
		"raw":           map[string]any{"balance": json.RawMessage(rawBal), "gettoken": json.RawMessage(rawGen)},
	})
}

// handleSignStatus GET /admin/codar/sign?uid=
//
// ★ 只**查**签到状态，不签到（写操作是 P1）。
// 实测：{"code":200,"message":"Already signed","data":{"isSigned":true,"retry_after":79529}}
// ⇒ retry_after 秒 = 距下次可签的秒数，可直接给界面显示"下次可签时间"。
func (p *Provider) handleSignStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	a, err := p.pickAuth(r.URL.Query().Get("uid"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	raw, status, err := p.callJSON(ctx, http.MethodPost, p.payBase+"/v1/daily-sign/status", a, map[string]any{"username": a.UID})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var parsed struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			IsSigned   bool  `json:"isSigned"`
			RetryAfter int64 `json:"retry_after"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &parsed)
	next := ""
	if parsed.Data.RetryAfter > 0 {
		next = time.Now().Add(time.Duration(parsed.Data.RetryAfter) * time.Second).Format("2006-01-02 15:04")
	}
	writeJSON(w, status, map[string]any{
		"ok": true, "uid": a.UID, "upstream_status": status,
		"is_signed": parsed.Data.IsSigned, "retry_after_sec": parsed.Data.RetryAfter,
		"next_signable_at": next, "upstream_message": parsed.Message,
	})
}

// ⚠ T-465：这里原有两条 handler —— 查上游"赠送模型活动"状态的只读端点，与它的
// 领取动作（在 p1.go）。整套已删除：上游活动已结束（实测 visible=false、
// status=unavailable），端点唯一的作用是把"不可领"三个字回显给用户。
//
// 墓碑注释刻意**不写**被删代码里的标识符与文案（理由见同包防复活守卫测试的注释）。

// inviteAccountView 列表型响应里的**每账号**一项（字段名是冻结契约，一字不许改）。
//
// # 为什么要把这些字段下放到每账号（T-357）
//
// 旧形状是**单对象**（顶层直接放 invite_code / page_url / invited …），前端只能渲染一个账号。
// 面板要**一行一个账号**，所以顶层收窄成 {ok,total,page,pages,accounts}，其余全部下放。
//
// ⚠ raw 被删掉了：它装着整包上游 body，列表化后会放大 N 倍（N=账号数），
//
//	而它从来没有过前端消费者 —— 详见本文件顶部 T-357 的注释。
type inviteAccountView struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	OK       bool   `json:"ok"`
	MyCode   string `json:"my_code,omitempty"`
	MyLink   string `json:"my_link,omitempty"`
	// ⚠ T-368：invited 与 invited_known 必须成对看 —— invited 的**零值 false 有两义**：
	// "查到了且没被邀请" 与 "这次没查到"。只看 invited 会把后者当成前者。
	// 所以 ok 只表示"拿到了 my_code"（唯一必要条件），其余步骤各自用 *_known 标记可信度。
	Invited        bool   `json:"invited"`
	InvitedKnown   bool   `json:"invited_known"`
	InvitedMessage string `json:"invited_message,omitempty"`
	// Warning 非致命问题的一句话摘要（如「邀请状态未取到」）。
	// 为什么要有它：容错之后 ok 可能是 true 而某些列其实没查到 ——
	// 不给一句话说明，用户会以为"这张表就是全部真相"。
	Warning      string `json:"warning,omitempty"`
	Balance      int64  `json:"balance"`
	BalanceKnown bool   `json:"balance_known"`
	// BalancePartial T-371：余额**只取到部分桶**（general/rewrite 未取到，数就只是 agent 桶）。
	//
	// 为什么必须单独一个字段：实测老号 4206 = agent 1706 + general 2500 + rewrite 0。
	// gettoken 挂掉时只会看到 1706 —— 数字对不上用户预期，而 balance_known=true 又表示"可信"。
	// 两者都不撒谎，但缺了"这是部分桶"这个中间态，界面就只能显示一个会让人误判的数。
	BalancePartial bool             "json:\"balance_partial,omitempty\""
	BindPage       string           `json:"bind_page,omitempty"`
	BindRequires   []string         `json:"bind_requires,omitempty"`
	InviteRecords  []map[string]any `json:"invite_records"`
	// ── T-364：微信绑定（邀请奖励的第二段：被邀请人绑微信后邀请人才入账）──
	WechatBound      bool   `json:"wechat_bound"`
	WechatBoundKnown bool   `json:"wechat_bound_known"`
	WechatPage       string `json:"wechat_page,omitempty"`
	Error            string `json:"error,omitempty"`
}

// inviteBindRequires 绑定所需的字段（来自前端 JS 逐字；与 BindInvite 的入参一致）。
var inviteBindRequires = []string{"invite_code", "lot_number", "captcha_output", "pass_token", "gen_time"}

// handleInvite GET /admin/codar/invite[?uid=&page=&page_size=]
//
// 列表型（与 loomy 的 /admin/loomy/invite 同形）：面板一行一个账号。
//
//	不带 uid → 全部账号（按 loadAll() 的顺序，**不用 map 遍历** ⇒ 顺序稳定）
//	带 uid   → 只回该账号（uid 不存在时给一条 ok=false 的条目，而不是空数组）
//	page/page_size → 分页（page 从 1 起；page_size 缺省 25、上限 100；page 越界按 1）
//
// 单账号失败（凭证解不出 / promotion/info 报错）**不拖垮整体**：该条目 ok=false + error，
// 其余照常返回，顶层 ok 仍为 true —— 否则一个坏账号会让整个面板空白。
//
// 只读：查**自己**的邀请码 / 链接 / 邀请记录 / 余额。
//
// # ⚠ 本上游没有"绑定别人的邀请码"的**独立**端点
//
// 绑定走的是 POST /v1/user/add-invite（字段名见 BindInvite），它要求阿里云人机验证的 4 个字段
// ⇒ 必须由页面采集（/admin/codar/invite-page）。本端点只负责"把码/链接给别人"这一半。
func (p *Provider) handleInvite(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	q := r.URL.Query()
	wantUID := strings.TrimSpace(q.Get("uid"))

	// ── 账号集合：带 uid 就只它一个；否则全部（顺序 = loadAll() 顺序）──
	type target struct {
		uid  string
		nick string
		auth *Auth
	}
	var targets []target
	if wantUID != "" {
		a, err := p.pickAuth(wantUID)
		if err != nil {
			// uid 不存在（或凭证解不开）：按契约给**一条** ok=false 的条目，而不是空数组
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "total": 1, "page": 1, "pages": 1,
				"accounts": []inviteAccountView{{UID: wantUID, OK: false, Error: err.Error()}},
			})
			return
		}
		targets = append(targets, target{uid: a.UID, nick: a.Nickname, auth: a})
	} else {
		for _, cred := range p.loadAll() {
			a, err := authOf(cred)
			if err != nil {
				// 解不开的凭证也留一条（否则"为什么少了一个号"无从排查）
				targets = append(targets, target{uid: cred.UID, nick: cred.Nickname})
				continue
			}
			targets = append(targets, target{uid: a.UID, nick: a.Nickname, auth: a})
		}
	}

	total := len(targets)
	page, pageSize := invitePaging(q.Get("page"), q.Get("page_size"), total)
	pages := 1
	if total > 0 {
		pages = (total + pageSize - 1) / pageSize
	}
	lo, hi := (page-1)*pageSize, page*pageSize
	if lo > total {
		lo = total
	}
	if hi > total {
		hi = total
	}

	// ★ T-361：账号级**有界并发**。
	//
	// 为什么改：每账号要打 5 次上游（promotion/info + check-invited + promotion/invites +
	// agent/balance + gettoken），25 个账号串行 = 125 次往返 ⇒ 必然撞上本函数的 60s ctx 超时，
	// 用户看到的是整页「查询失败」。
	//
	// 为什么有界（inviteConcurrency=6）而不是全并发：125 条同时在飞会被上游限流，
	// 也会让我们自己被判定为异常流量。
	//
	// ⚠ 顺序：结果按**下标**写回 views[i]，与 targets[lo:hi] 一一对应 ⇒
	//   "谁先返回"不影响顺序（分页顺序必须稳定）。
	views := make([]inviteAccountView, hi-lo)
	sem := make(chan struct{}, inviteConcurrency)
	var wg sync.WaitGroup
	for i, t := range targets[lo:hi] {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			views[i] = p.inviteViewFor(ctx, t.uid, t.nick, t.auth)
		}(i, t)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "total": total, "page": page, "pages": pages, "accounts": views,
	})
}

// invitePaging 解析分页参数（缺省/非法都按契约落到安全值）。
func invitePaging(pageRaw, sizeRaw string, total int) (page, size int) {
	// T-366：缺省从 25 降到 10 —— 每账号要打 6 次上游（invite 5 次 + wechat-bound 1 次），
	// 25 账号 × 6 = 150 次；6 并发也要 25 轮，60s 兜底容易撞。
	// 10 账号 ⇒ 60 次 ⇒ 10 轮，留足余量；前端本来就有分页器，面板是"按行看"的。
	size = 10
	if v, err := strconv.Atoi(strings.TrimSpace(sizeRaw)); err == nil && v > 0 {
		if v > 100 {
			v = 100
		}
		size = v
	}
	page = 1
	if v, err := strconv.Atoi(strings.TrimSpace(pageRaw)); err == nil && v > 1 {
		page = v
	}
	// page 越界按 1（契约原文）—— 注意"total=0"时不调这里的分支
	if total > 0 && (page-1)*size >= total {
		page = 1
	}
	return page, size
}

// inviteConcurrency 一页并发的账号数上限。
//
// 为什么需要有界而不是全并发：每个账号要打 5 次上游（见 inviteViewFor），
// 25 个账号全部并发 = 125 条同时在飞 ⇒ 上游会限流、我们自己也会被判定为异常流量。
// 为什么不能继续串行：125 次串行在真实网络下会撞上本函数的 60s ctx 超时
// （用户看到的是整页「查询失败」，而不是慢一点）。
// 取 6：与仓库其它地方的有界并发口径一致（既压住并发量，又把 25 账号压到约 5 轮）。
const inviteConcurrency = 6

// inviteAccountTimeout **单个账号**的查询上限。
//
// # 为什么要单账号超时（T-366）
//
// 一个账号要打 6 次上游；只要其中一次挂住（上游抖动/DNS/限流），
// 旧实现会让**整页**都等它，直到顶层的 60s ctx 兜底 —— 用户看到的是"整页失败"。
// 派生一个更短的子超时后，卡住的账号只让自己那一行 ok=false，
// 其余账号照常返回（这也是列表型相对单对象型最大的好处：**故障可隔离到行**）。
//
// 12s 的依据：6 次调用 × 2s 的宽松上限。做成**包级变量**是为了测试能压到毫秒级
// （否则测一次要真等 12 秒）。
var inviteAccountTimeout = 12 * time.Second

// inviteViewFor 查一个账号的邀请码视图；**任何**失败都收敛成 ok=false + error，不向上抛。
func (p *Provider) inviteViewFor(ctx context.Context, uid, nick string, a *Auth) inviteAccountView {
	// T-366：派生**单账号**超时 —— 卡住的账号只拖自己那一行（见 inviteAccountTimeout 的注释）。
	// 签名不变（调用方不用改）；顶层 60s 仍保留作兜底。
	cctx, cancel := context.WithTimeout(ctx, inviteAccountTimeout)
	defer cancel()
	ctx = cctx

	v := inviteAccountView{
		UID: uid, Nickname: nick, BindRequires: inviteBindRequires,
		// invite_records 显式给**空数组**而不是让它序列化成 null：
		// 前端会直接 .map()/.length，null 会当场抛（契约里它是数组）。
		InviteRecords: []map[string]any{},
		// InvitedKnown 的零值就是 false，但**故意显式写出来**：
		// 契约是"必须先置 false，成功取到才置 true" —— 漏掉这一步，
		// invited=false 会被界面读成"确认没被邀请"（而实际是"这次没查到"）。
		InvitedKnown: false,
	}
	if a == nil {
		v.Error = "凭证解不开（点一次「重载 auths」或重新登录）"
		return v
	}
	v.BindPage = invitePagePath + "?uid=" + uid
	// T-364：wechat_page **恒填**（失败也要能点进去自己看错误）。
	v.WechatPage = wechatPagePath + "?uid=" + uid

	// ── T-368：容错语义 ──
	//
	// ok 只表示**一件事**：拿到了 my_code（这张表的唯一必要条件 —— 邀请码就是它的产物）。
	// 其余步骤失败一律**不再**把整行报废，而是各自标记可信度 + 拼一句 warning：
	//   invited → invited_known；记录 → 空数组；余额 → balance_known。
	//
	// 为什么必须这样：余额步超时不该把已经拿到的邀请码一起作废 ——
	// 用户看不到邀请码、只看到"查询失败"，是把次要步骤的错误升级成了整行错误。
	budgetNote := "（超出单账号 " + inviteAccountTimeout.String() + " 预算）"
	warn := func(what string) {
		if v.Warning == "" {
			v.Warning = what
		} else {
			v.Warning += "；" + what
		}
	}

	// ② 我的邀请码 —— **唯一必要条件**：拿不到就没有可显示的产物 ⇒ ok=false + return
	body := map[string]any{"username": a.UID, "token": 0}
	rawInfo, st1, e1 := p.callJSON(ctx, http.MethodPost, p.payBase+"/v1/promotion/info", a, body)
	if e1 != nil {
		// 把"ctx 被超时掐断"翻译成人话（上游可能只报 context deadline exceeded）
		if ctx.Err() == context.DeadlineExceeded {
			v.Error = "单账号查询超时（" + inviteAccountTimeout.String() + "）"
			return v
		}
		v.Error = e1.Error()
		return v
	}
	// ⚠ callJSON 对**非 2xx 不返回 error**（它只报传输层失败）⇒ 必须自己看状态码。
	// 否则上游 500 的 {"code":500,...} 会被当成"查到了但码是空"，条目显示 ok=true + 空码，
	// 用户看到的是"这个号没有邀请码"而不是"这次查询失败了"。
	if st1 != http.StatusOK {
		v.Error = "上游 " + strconv.Itoa(st1) + "：" + truncateStr(string(rawInfo), 160)
		return v
	}
	var info struct {
		Data struct {
			InviteCode string `json:"invite_code"`
			InviteLink string `json:"invite_link"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rawInfo, &info)
	v.MyCode, v.MyLink = info.Data.InviteCode, info.Data.InviteLink
	if strings.TrimSpace(v.MyCode) == "" {
		// 200 但没给码：同样按"这次查不到"处理（列表里显示得出来原因，而不是一个空码）
		v.Error = "上游未返回邀请码：" + truncateStr(string(rawInfo), 160)
		return v
	}
	// ★ 到这里就可以判成功了 —— 后面任何一步失败都不再改 ok（T-368 的核心）。
	//
	// ⚠ 顺序上把 promotion/info 放在**最前**（T-368 实跑后调整）：单账号预算是固定的一段，
	// 若先花在非致命步骤上（如微信状态挂住 400ms 吃光预算），唯一必要条件就拿不到 ⇒
	// 整行反而报废 —— 与"容错"的目标正好相反。预算必须优先给唯一必要条件。
	v.OK = true

	// ① 微信绑定状态（非致命：只影响展示，见 T-364 的口径）
	if wb, werr := p.wechatBoundCtx(ctx, a.UID); werr == nil {
		v.WechatBound, v.WechatBoundKnown = wb, true
	} else if ctx.Err() != nil {
		warn("微信绑定状态未取到" + budgetNote)
	} else {
		warn("微信绑定状态未取到")
	}
	// ③ 邀请状态（非致命）：成功才把 invited_known 置真 —— invited 的零值有两义，
	//    没有这个标志就无法区分"没被邀请"与"这次没查到"。
	if ctx.Err() != nil {
		warn("邀请状态未取到" + budgetNote)
	} else if invited, msg, cerr := p.CheckInvited(ctx, a.UID); cerr == nil {
		v.Invited, v.InvitedMessage, v.InvitedKnown = invited, msg, true
	} else if ctx.Err() != nil {
		warn("邀请状态未取到" + budgetNote)
	} else {
		warn("邀请状态未取到")
	}

	// ④ 邀请记录（非致命）：拿不到就保留空数组（契约里它是数组，不能序列化成 null）。
	// ⚠ 只在拿到非 nil 时才覆盖 —— 直接赋值会把 nil 写回去。
	if ctx.Err() != nil {
		warn("邀请记录未取到" + budgetNote)
	} else if recs, rerr := p.inviteRecords(ctx, a); rerr == nil && recs != nil {
		v.InviteRecords = recs
	} else if ctx.Err() != nil {
		warn("邀请记录未取到" + budgetNote)
	} else {
		warn("邀请记录未取到")
	}

	// ⑤ 余额（非致命）：refreshQuotaCtx 自己挑凭证并打 /v1/agent/balance + /v1/gettoken。
	if ctx.Err() != nil {
		warn("余额未取到" + budgetNote)
	} else if agent, general, rewrite, partial, ok := p.refreshQuotaDetailCtx(ctx, uid); ok {
		if partial {
			// 只有 agent 桶：数字**没错**（agent 桶确实可信 ⇒ known=true），
			// 但它只是"部分" ⇒ 同时给 partial + 一句话，界面才能区分 1706 与 4206。
			v.Balance, v.BalanceKnown, v.BalancePartial = agent, true, true
			warn("余额只取到部分桶（general/rewrite 未取到）")
		} else {
			v.Balance, v.BalanceKnown = agent+general+rewrite, true
		}
	} else if ctx.Err() != nil {
		warn("余额未取到" + budgetNote)
	} else {
		warn("余额未取到")
	}

	// ⚠ T-368：这里**不再**有"末尾预算检查把 ok 置 false"。
	// 预算耗尽的后果已经被上面各步如实记进 warning + 各自的 *_known，
	// 而 ok 只由 my_code 决定 —— 已经拿到码的行必须把码给出去。
	return v
}
