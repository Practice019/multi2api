// admin.go 本上游的管理端点（gateway.AdminExt）。
//
// # 为什么必须有这个文件（契约测试逼出来的）
//
// 契约测试有一条判定：**声明了能力位就必须有 AdminExt** ——
// 因为本仓所有能力（chat / models / quota-probe / import …）都是
// 通过管理端点暴露给前端的。没有 AdminRoutes 就等于"声明了但没实现"。
//
// 这条判定在本上游身上抓到了一个真实缺口：我一开始只实现了
// AccountImportExt（导入解析）却没有 AdminExt —— 于是"导入"这个能力
// 在界面上**没有任何入口**。解析逻辑写得再对，用户也碰不到它。
package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// AdminRoutes 本上游的管理端点。
//
// 两个端点，都对应我声明的能力位：
//
//	/admin/zcode/quota    余额/配额查询（CapQuotaProbe）
//	/admin/zcode/diagnose 令牌生命周期诊断（排障用）
//
// # ⚠ 两个都是 Hidden（用户要求："这个 zcode 不需要有额外的标签页"）
//
// Hidden 的语义是「这条路由存在、但**不是面板入口**」——
// 前端 panelRoutesFor 会把它从"该生成一个标签页吗"的判据里排掉，
// 而**路由本身照挂**（curl / 调试仍可用）。
//
// 我原来让 quota 这条**可见**，于是左侧导航多出一个「额度探测」标签页。
// 那个标签页是本上游独有的 —— 对照其它上游：
//
//	codearts  声明 CapQuotaProbe，但端点是 **POST**（§routesFor 只认 GET）
//	           → 不生成面板
//	trae      声明 CapQuotaProbe，**没有任何路由**
//	           → 不生成面板
//	workbuddy 声明 CapQuotaProbe，没有独立 GET 额度端点
//	           → 不生成面板
//
// 也就是说：**zcode 是唯一因为这个而多出一个空标签页的上游**。
// 而那个面板里其实什么都没有 —— 额度已经在账号池的「额度」列、
// 以及分组行的「刷新本上游额度」按钮上（两者都走 core 的
// `POST /admin/accounts/quota/refresh`，与本路由无关）。
//
// 所以隐藏它不损失任何功能，只是去掉一个重复的空壳入口。
//
// 刻意**不做**的端点：
//
//	签到相关      Z.ai 没有签到概念（额度由订阅周期决定）
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method: http.MethodGet,
			Path:   "/admin/zcode/quota",
			// Hidden：见上面那段 —— 额度已经由账号池的「额度」列与
			// 分组行的「刷新本上游额度」按钮承担，不需要额外的标签页。
			Hidden:     true,
			Handler:    p.handleQuota,
			Capability: gateway.CapQuotaProbe,
			Title:      "ZCode 额度",
		},
		{
			Method: http.MethodGet,
			Path:   "/admin/zcode/diagnose",
			// Hidden：这不是面板入口，而是排障用的诊断端点。
			// 本仓约定用 Hidden 标记"存在但不是给人点的按钮"。
			Hidden:     true,
			Handler:    p.handleDiagnose,
			Capability: gateway.CapChat,
			Title:      "ZCode 诊断",
		},
	}
}

// handleQuota 返回某账号的额度视图。
//
// 查询参数 uid（缺省则返回全部账号的）。
//
// ⚠ 额度取不到时报 `has_data: false` 而不是 0 —— 本仓的三态约定：
// 查不到（—）/ 真的是 0（0）/ 有额度（数字）。把"查不到"报成 0
// 会让用户以为额度用尽，引发一轮无意义的排查。
func (p *Provider) handleQuota(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))

	type row struct {
		UID       string           `json:"uid"`
		Kind      string           `json:"kind"`
		Channel   string           `json:"channel"`
		Platform  string           `json:"platform"`
		Remaining int64            `json:"remaining"`
		HasData   bool             `json:"has_data"`
		ByModel   map[string]int64 `json:"by_model,omitempty"`
		Note      string           `json:"note,omitempty"`
	}

	var rows []row
	uids := []string{uid}
	if uid == "" {
		uids = p.knownUIDs()
	}
	for _, u := range uids {
		a := p.cred(u)
		if a == nil {
			continue
		}
		view, _ := p.RefreshQuota(u)
		rw := row{
			UID:       u,
			Kind:      view.Kind,
			Channel:   channelName(a),
			Platform:  platformName(p.client.originFor(a)),
			Remaining: view.Remaining,
			HasData:   view.HasData,
			ByModel:   view.ByModel,
		}
		if !view.HasData {
			// 说清**为什么**没有数据 —— 否则用户会以为是我们坏了。
			if a.UsesJWT() {
				rw.Note = "Coding Plan 的计量端点没返回可用数据（可能凭证已过期，JWT 不可刷新，需重新登录）"
			} else {
				rw.Note = "API Key 通道没有额度端点（Z.ai 不为按量付费的 Key 提供余额查询）"
			}
		}
		rows = append(rows, rw)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider": ProviderID,
		"accounts": nonNilRows(rows),
	})
}

// handleDiagnose 令牌生命周期诊断。
//
// # 为什么需要它（本上游特有的坑）
//
// 本上游的失败有三种容易混淆的形态，而现象都是"用不了"：
//
//	401 + code 1001   根本没带鉴权参数   → 头拼错了
//	401 + code 401    token 无效/过期    → 凭证问题
//	3012 风控          请求头形态可疑     → **多发了一个头**
//
// 光看"401"分不出前两种；而 3012 看起来根本不像鉴权问题。
// 这个端点把凭证形态、通道、有效期、选定的端点全部列出来，
// 让排障从"猜"变成"看"。
//
// ⚠ 它**不回显令牌内容**（只回长度与尾 4 位）——
// 诊断端点的输出常被贴到 issue 里求助，回显令牌等于泄露密钥。
func (p *Provider) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))

	type diag struct {
		UID       string `json:"uid"`
		Channel   string `json:"channel"`
		Platform  string `json:"platform"`
		Endpoint  string `json:"endpoint"`
		Protocol  string `json:"protocol"`
		TokenLen  int    `json:"token_len"`
		TokenTail string `json:"token_tail"`
		HasDevice bool   `json:"has_device_mid"`
		ExpiresAt string `json:"expires_at,omitempty"`
		Expired   bool   `json:"expired"`
		CaptchaOK bool   `json:"captcha_solver"`
		Note      string `json:"note,omitempty"`
	}

	var rows []diag
	uids := []string{uid}
	if uid == "" {
		uids = p.knownUIDs()
	}
	for _, u := range uids {
		a := p.cred(u)
		if a == nil {
			continue
		}
		tok := a.Token()
		d := diag{
			UID:       u,
			Channel:   channelName(a),
			Platform:  platformName(p.client.originFor(a)),
			Endpoint:  p.client.chatURL(a),
			Protocol:  protocolName(a),
			TokenLen:  len(tok),
			HasDevice: strings.TrimSpace(a.DeviceMid) != "",
			Expired:   a.Expired(time.Now()),
			CaptchaOK: p.client.captcha != nil,
		}
		if len(tok) > 4 {
			d.TokenTail = "…" + tok[len(tok)-4:]
		}
		if a.ExpiresAt > 0 {
			d.ExpiresAt = time.Unix(a.ExpiresAt, 0).Local().Format(time.RFC3339)
		}
		// 把最容易踩的坑直接写在诊断结果里。
		switch {
		case !a.Usable():
			d.Note = "凭证没有令牌 —— 无法发起任何请求"
		case a.UsesJWT() && p.client.captcha == nil:
			d.Note = "JWT 通道需要验证码求解器但未装配 —— 请求会直接报错（不会静默去掉验证码头）"
		case a.UsesJWT() && a.Expired(time.Now()):
			d.Note = "JWT 已过期且**不可刷新**（官方无 refresh 接口）—— 必须重新走 OAuth 登录"
		case !a.UsesJWT() && a.CodingPlan:
			d.Note = "Coding Plan 的 Key 走 " + pathCodingPaaS + "（与普通 Key 的额度不互通，打错端点会报无额度）"
		}
		rows = append(rows, d)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider": ProviderID,
		"accounts": nonNilRows(rows),
	})
}

// knownUIDs 返回本上游**账号池里**的全部 uid。
//
// # ⚠ 不能只读 p.creds（实测发现的空列表缺陷）
//
// 装配层把凭证装进 `pool`，而 Provider 侧只拿到一个
// `credSrc(uid) → Credential` 的**按名查询**访问器 —— 它没有"列出全部"的能力。
// 于是 `p.creds` 在真实运行时**是空的**（只有测试才会往里塞），
// 诊断端点会把 accounts 返回成 `[]`。
//
// 症状是：端点返回 200 + 空数组，看起来"没有账号"，而实际账号好好的
// —— 一个静默的空列表比报错更难查。
//
// 修法：优先用装配层注入的**枚举器**；没有它时回落到本地表
// （测试路径）。两者都空才真实地表示"没有账号"。
func (p *Provider) knownUIDs() []string {
	p.mu.RLock()
	enum := p.uidEnumerator
	local := p.creds
	p.mu.RUnlock()

	if enum != nil {
		if uids := enum(); len(uids) > 0 {
			return uids
		}
	}
	out := make([]string, 0, len(local))
	for u := range local {
		out = append(out, u)
	}
	return out
}

// SetUIDEnumerator 装配层注入"列出本上游全部 uid"的能力。
//
// 为什么不让 Provider 依赖 pool：本仓的架构判据是
// **上游包不得依赖核心的业务包**（arch_test.go 守着）。
// 所以池子的枚举能力以**函数**形式注入，与 SetCredentialSource 同一模式。
func (p *Provider) SetUIDEnumerator(f func() []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.uidEnumerator = f
}

// channelName 人类可读的通道名（控制台与诊断都用它）。
func channelName(a *Auth) string {
	if a.UsesJWT() {
		return "Coding Plan (JWT)"
	}
	if a.CodingPlan {
		return "API Key (Coding Plan)"
	}
	return "API Key"
}

// platformName 人类可读的平台名。
func platformName(origin string) string {
	if strings.Contains(origin, "bigmodel") {
		return "BigModel"
	}
	if strings.Contains(origin, "z.ai") {
		return "Z.ai"
	}
	return origin
}

// protocolName 该凭证走哪种协议。
func protocolName(a *Auth) string {
	if a.UsesJWT() {
		// 实测：JWT 通道**只有** Anthropic 形态（OpenAI 形态 404）。
		return "anthropic-messages（需要协议转换）"
	}
	return "openai-chat-completions"
}

// writeJSON 统一的 JSON 响应写法。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// nonNilRows 把可能为 nil 的行切片收敛成**空数组**。
//
// # 为什么必需（实测发现的 `accounts: null` 缺陷）
//
// Go 的 nil 切片序列化成 `null` 而不是 `[]`：
//
//	{"accounts":null}   ← nil 切片
//	{"accounts":[]}     ← 空切片
//
// 两者对前端的差别是**致命的**：`data.accounts.length` 在 null 上会抛
// `TypeError`，而后端返回的是 200 —— 于是界面什么都不显示、控制台一个异常，
// 看起来像"接口坏了"。
//
// 实测触发路径：账号池里还没有本上游的账号时（也就是**第一次配置**时），
// 两个诊断端点都返回 null。而那正是用户最需要看到"我该做什么"的时刻。
//
// 用泛型是因为两个端点的行类型不同（quotaRow / diagRow）。
func nonNilRows[T any](rows []T) []T {
	if rows == nil {
		return []T{}
	}
	return rows
}

// ProbeOriginAll 给装配层用：把没有显式平台的凭证探测一遍并回填。
//
// 为什么要在装配时做：API Key 的凭证常常是"粘贴进来的"，
// 用户未必知道它属于哪个平台（z.ai 与 bigmodel 的 Key 形态一样）。
// 探测一次填好，之后就按凭证走，不用每次请求都猜。
//
// 返回被回填的凭证（装配层负责落盘）。
func (p *Provider) ProbeOriginAll(ctx context.Context) []*Auth {
	p.mu.RLock()
	list := make([]*Auth, 0, len(p.creds))
	for _, a := range p.creds {
		list = append(list, a)
	}
	p.mu.RUnlock()

	var changed []*Auth
	for _, a := range list {
		if a == nil || !a.Usable() || a.UsesJWT() {
			// JWT 通道的平台是固定的（planOrigin），不需要探测。
			continue
		}
		if strings.TrimSpace(a.Origin) != "" {
			continue
		}
		org := p.client.ProbeOrigin(ctx, a)
		if org != "" {
			a.Origin = org
			changed = append(changed, a)
		}
	}
	return changed
}
