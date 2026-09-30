// credits.go Qoder 的积分余额查询与每日领取。
//
// # 与「加密推理」的关系：**不需要 WASM**
//
// 这是本文件最重要的一条事实。Qoder 的推理走加密端点（需 WASM 签名），
// 但 `/sash/` 系列端点**只认 Bearer + Cosy-ClientType 头**（参照实现
// `Bx()` 那条路径）。早期参照实现因为只按 `/api/` 前缀搜索端点，
// 误判成「Qoder 无积分端点」—— 那是搜索范围问题，不是端点不存在。
//
// 所以本文件**完全独立于 internal/qoderwasm**：即使 WASM 桥坏了，
// 积分功能照常可用。
//
// # 端点（实测，2026-09-19 真实凭据）
//
//	GET  {openApiBase}/sash/api/v2/me/usage          用量（余额）
//	GET  {openApiBase}/sash/api/v1/me/campaigns      活动列表（签到状态）
//	POST {openApiBase}/sash/api/v1/me/campaigns/{id}/claim   领取（**body 空**）
//
// # ⚠ 余额不只在 userQuota 里
//
// 实测某账号 `userQuota.remaining = 0` 而 `addOnQuota.remaining = 100`
//（用户说的「资源包 100 积分」正是后者）。只读 userQuota 会显示 0 ——
// 与其它 provider 的「漏读某一层」是同一类缺陷。
//
// # ⚠ 两个头都必需，缺一服务端就不下发「可领取」的活动
//
//	Cosy-ClientType = 10（桌面 app 身份）
//	    用 client_type=5（CLI）时 /campaigns 恒返回 campaigns:[]
//	Cosy-MachineToken + Cosy-MachineType（**必须成对**）
//	    只用 ClientType=10 时服务端只回一条 VIEW_DETAILS，
//	    **没有** CLAIM_BENEFIT/CLAIMABLE → 会误判成「今天已领」
//
// 消融实验（同一账号、同一 token、只改头）：
//
//	仅 ClientType: 10                      → 1 条 VIEW_DETAILS，claimable:false
//	＋ MachineToken ＋ MachineType          → **2 条**，含 CLAIM_BENEFIT/CLAIMABLE/100
//	去掉 MachineToken 或 MachineType 任一    → 退回 1 条
//
// machine 值来自本机 IDE 的 `machine_token.json`（见 machine.go）。
package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 积分端点路径（挂 OpenAPIBase）。
const (
	usagePath     = "/sash/api/v2/me/usage"
	campaignsPath = "/sash/api/v1/me/campaigns"
)

// creditsMaxAttempts 一次 GET 最多尝试几次（含首次）。
//
// 取 3：实测上游 503 率约 20~30%，且是"临时不可用"。
// 3 次全撞上的概率约 1~3%，足够低；而最坏耗时 200+400ms 的退避，
// 对"每 30 分钟扫一次"的签到完全可接受，也不会让界面转圈。
const creditsMaxAttempts = 3

// creditsRetryDelayMS 重试退避的基数（毫秒）。第 n 次失败后等 n×base。
const creditsRetryDelayMS = 200

// creditsTimeoutMS 积分请求超时。
const creditsTimeoutMS = 15_000

// CreditPackage 一个额度包。
//
// 顺序即展示顺序：套餐额度 → 资源包 → 专用资源包。
type CreditPackage struct {
	// Name 展示名（"套餐额度" / "资源包" / 专用包的自报名）。
	Name string `json:"name"`
	// Unit 单位（"credits"）。
	Unit string `json:"unit"`
	// Remaining 剩余。
	Remaining float64 `json:"remaining"`
	// Total 总量。
	Total float64 `json:"total"`
	// Used 已用。
	Used float64 `json:"used"`
	// ExpiredTime 过期时刻（专用包才有）。
	ExpiredTime string `json:"expired_time,omitempty"`
}

// CreditBalance 积分余额视图。
//
// Total 是各包 remaining 之和 —— 与 IDE 顶部的 "Credits Balance" 同一口径，
// 用户可直接核对。
type CreditBalance struct {
	Total    float64         `json:"total"`
	Packages []CreditPackage `json:"packages"`
}

// CheckinStatus 签到状态（与其它 provider 共用的语义）。
type CheckinStatus struct {
	// Active 该上游当前有签到活动。
	//
	// ⚠ **拿到响应即 true**，不按"列表非空"判定 —— 服务端在
	// 「今天已领」时会把 campaigns 清空并回 showCampaign:false。
	// 若据此判 false，调用方会先命中「活动未开启」分支，
	// 把「今天已领」误报成「签到活动未开启」。
	Active bool `json:"active"`
	// TodayCheckedIn 今天是否已领。
	//
	// ⚠ **只有存在「领过」的领分类活动时才为 true**
	//（CLAIM_BENEFIT 且 claimStatus === 'CLAIMED'）。
	//
	// **不能**写成「没有可领活动即为 true」（真实缺陷，用户报障
	// 「没领过就显示已经领取，去 IDE 看还是可以领取的状态」）：
	// 「列表里没有可领项」不等于「今天领过了」—— 它还可能是
	// ① 未到刷新时间（每日 10:00 UTC+8）、② 请求头不完整导致服务端未下发、
	// ③ 该账号本就无此类活动。三者都不是「已领」。
	TodayCheckedIn bool `json:"today_checked_in"`
	// DailyCredit 可领活动的声明额度（实测 100）。
	DailyCredit float64 `json:"daily_credit"`
	// ActivityName 活动标识（campaignKey）。
	ActivityName string `json:"activity_name,omitempty"`
	// ActionRequired 需要用户去官方客户端登录一次（未开通每日领取）。
	ActionRequired bool `json:"action_required,omitempty"`
	// Message 可读说明。
	Message string `json:"message,omitempty"`
}

// ClaimOutcome 一次领取的结果。
type ClaimOutcome struct {
	// Kind claimed / already-claimed / inactive / failed
	Kind string `json:"kind"`
	// Credit 本次到账积分（kind=claimed 时有效）。
	Credit float64 `json:"credit,omitempty"`
	// Message 可读说明。
	Message string `json:"message,omitempty"`
	// ActionRequired 需要用户去官方客户端登录一次。
	ActionRequired bool `json:"action_required,omitempty"`
}

// notActivatedHint 账号尚未在 Qoder 侧开通每日领取时的提示。
//
// ⚠ 这条文案要**可操作** —— 用户看完应知道「去 IDE 登录一次」，
// 而不是面对「没有可领取的活动」无从下手。
//
// 真实缺陷（用户报障 2026-09-26）：经 GitHub 授权**新注册**的 Qoder 账号，
// 一键签到显示「当前没有可领取的活动」，用户以为是我们没做对。
// 实测证明不是设备身份问题，而是该账号在 Qoder 侧**确实没有**每日领取活动。
const notActivatedHint = "该账号尚未在 Qoder 侧开通每日领取（每日 100 Credits）。" +
	"请先用 Qoder 官方客户端登录一次该账号，开通后再回来领取。"

// creditsHeaders 构造 `/sash/` 端点的请求头。
//
// ⚠ 两个头都必需（见文件头注释），缺一都会让服务端不下发可领取的活动。
func (c *Client) creditsHeaders(a *Auth) map[string]string {
	h := map[string]string{
		"Accept":     "application/json",
		"User-Agent": "Qoder",
		// 桌面 app 身份（'10'）；服务端据此进入活动下发分支。
		// ⚠ 不是 clientMetadata 里的 client_type=5（那是设备码授权的 CLI 身份）。
		"Cosy-ClientType": c.Product.SashClientType,
	}
	if a != nil && a.AccessToken != "" {
		h["Authorization"] = "Bearer " + a.AccessToken
	}
	// machine 头族（成对）。拿不到就不带 —— 保守降级：宁可少一次可领活动，
	// 也不要因为读不到本机文件而让整个积分功能报错。
	if id := resolveMachineIdentity(); id != nil {
		h["Cosy-MachineToken"] = id.Token
		h["Cosy-MachineType"] = id.Type
	}
	return h
}

// getJSON 发一次带 creditsHeaders 的 GET；失败返回 (nil, 原因描述)。
//
// # 为什么返回"原因"而不是 bool（本轮改的，用户报障）
//
// 旧签名是 `bool`，于是调用方只能说"活动列表查询失败"——用户看到
// 「签到 失败」却完全不知道为什么（是凭据过期？限流？上游挂了？）。
// 这一轮排查里，我不得不**手工**打端点才知道真相是 503
// `DEPENDENCY_UNAVAILABLE`。把原因带出来，下次一眼可见。
//
// # ⚠ 503 是**可重试**的，不能一次定生死（本轮修的第二个缺陷）
//
// 实测（同一凭据、同一时刻交替打 10 次）：
//
//	不带 machine 头  200×7  503×3
//	带 machine 头    200×8  503×2
//
// 即 `campaign service is temporarily unavailable` 是**上游间歇性**故障
//（与请求头无关，`/usage` 端点同时刻恒 200）。而签到是每 30 分钟扫一次，
// 撞上 503 就记一次 "fail" —— 用户看到的「今日签到 失败」有相当比例
// 就是这么来的。所以对 5xx / 网络错误**退避重试**，只有全试完才认失败。
func (c *Client) getJSON(ctx context.Context, a *Auth, path string, out any) (bool, string) {
	var lastReason string
	for attempt := 0; attempt < creditsMaxAttempts; attempt++ {
		if attempt > 0 {
			// 退避：200ms / 400ms …（上游是"临时不可用"，等一小会儿往往就好）
			select {
			case <-ctx.Done():
				return false, "已取消"
			case <-time.After(time.Duration(attempt) * creditsRetryDelayMS * time.Millisecond):
			}
		}
		status, body, err := c.getJSONOnce(ctx, a, path)
		if err != nil {
			lastReason = "网络错误：" + err.Error()
			continue // 网络错误可重试
		}
		if status >= 200 && status < 300 {
			if jerr := json.Unmarshal(body, out); jerr != nil {
				// 2xx 却解不动：**不重试**（重试也是同样的字节），直接报形状问题。
				return false, fmt.Sprintf("响应无法解析（HTTP %d）：%s", status, snippet(body))
			}
			return true, ""
		}
		lastReason = describeNonJSON(status, string(body))
		if status < 500 {
			// 4xx 是确定性的（凭据失效 / 参数错），重试无意义。
			return false, lastReason
		}
		// 5xx：重试
	}
	return false, lastReason
}

// getJSONOnce 发一次 GET，返回 (状态码, 响应体)。网络失败时 err 非 nil。
func (c *Client) getJSONOnce(ctx context.Context, a *Auth, path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.openAPI()+path, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range c.creditsHeaders(a) {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(ctx, creditsTimeoutMS*time.Millisecond)
	defer cancel()
	resp, err := c.httpClient().Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// snippet 压平空白并截断（进错误文案用）。
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// FetchUsageRaw 拉原始用量响应（供「是否未开通」判据使用）。
//
// 单独抽出来是因为 `isNotActivated` 需要一个**原始**响应，而
// FetchCreditBalance 会把结果归一化成 CreditBalance（其中「查不到」与
// 「余额 0」都可能变成 nil，不足以区分开通与否）。
func (c *Client) FetchUsageRaw(ctx context.Context, a *Auth) map[string]any {
	var out map[string]any
	if ok, _ := c.getJSON(ctx, a, usagePath, &out); !ok {
		return nil
	}
	return out
}

// FetchCreditBalance 查积分余额。
//
// 返回 (余额, ok)：ok=false 表示**查不到**（网络失败 / 401 / 形状非法），
// 与「余额为 0」严格区分 —— 失败时界面应显示原因而不是 0。
func (c *Client) FetchCreditBalance(ctx context.Context, a *Auth) (*CreditBalance, bool) {
	var root map[string]any
	if ok, _ := c.getJSON(ctx, a, usagePath, &root); !ok {
		return nil, false
	}
	// 企业版：无额度数字，只有外部链接。返回 not-ok 而非 0（报 0 会误导）。
	if s, _ := root["displayMode"].(string); s == "enterprise" {
		return nil, false
	}
	usage, _ := root["qoderUsage"].(map[string]any)
	if usage == nil {
		return nil, false
	}

	pkgs := make([]CreditPackage, 0, 3)
	// 顺序即展示顺序：套餐额度 → 资源包 → 专用资源包。
	if p, ok := toPackage("套餐额度", usage["userQuota"]); ok {
		pkgs = append(pkgs, p)
	}
	if p, ok := toPackage("资源包", usage["addOnQuota"]); ok {
		pkgs = append(pkgs, p)
	}

	if arr, ok := usage["dedicatedResourcePackages"].([]any); ok {
		for _, item := range arr {
			name := firstNonEmpty(readString(item, "name"), readString(item, "id"), "专用资源包")
			p, ok := toPackage(name, item)
			if !ok {
				continue
			}
			p.ExpiredTime = firstNonEmpty(readString(item, "expiresAt"), readString(item, "expires_at"))
			pkgs = append(pkgs, p)
		}
	}
	// 一个包都没解析出来 → 视为「查不到」（响应形状与预期不符），
	// 而不是「余额为 0」—— 后者会让用户以为额度被清空了。
	if len(pkgs) == 0 {
		return nil, false
	}

	total := 0.0
	for _, p := range pkgs {
		total += p.Remaining
	}
	return &CreditBalance{Total: round2(total), Packages: pkgs}, true
}

// toPackage 把一个 quota 对象转成 CreditPackage。
//
// `remaining` 优先取服务端字段；缺失时按 `total - used` 计算。
// 负值一律 clamp 到 0：服务端在超额扣费/计量回滚下可能下发负值，
// 原样透出会让卡片显示「-12.5 积分」。
func toPackage(name string, quota any) (CreditPackage, bool) {
	total, hasTotal := readNumber(quota, "total")
	used, hasUsed := readNumber(quota, "used")
	remainingRaw, hasRemaining := readNumber(quota, "remaining")
	if !hasTotal && !hasUsed && !hasRemaining {
		return CreditPackage{}, false
	}
	tv := nonNeg(total)
	uv := nonNeg(used)
	r := nonNeg(remainingRaw)
	if !hasRemaining {
		r = nonNeg(tv - uv)
	}
	return CreditPackage{
		Name:      name,
		Unit:      firstNonEmpty(readString(quota, "unit"), "credits"),
		Remaining: r,
		Total:     tv,
		Used:      uv,
	}, true
}

// ── 活动（签到） ──

// campaign 一个可领取的活动条目。
type campaign struct {
	ID string
	// Key campaignKey（人类可读的活动标识，如 "act-20260921-308"）。
	//
	// ⚠ 与 ID 是两件事：ID 是 UUID（领取时要用），Key 是给用户看的名字。
	// 早期只留 ID 会让 activityName 显示成一串 UUID。
	Key string
	// ActionType CLAIM_BENEFIT = 可领取积分；VIEW_DETAILS = 仅跳转详情
	//（实测「Pro 首月翻倍」就是后者，**不该尝试领取**）。
	ActionType string
	// ClaimStatus CLAIMABLE / CLAIMED / … —— 领取前的权威判据。
	ClaimStatus string
	// Amount 可领积分（benefit.amount）。
	Amount float64
}

// campaignsResult 活动列表的一次解析结果。
type campaignsResult struct {
	Campaigns []campaign
}

// loadCampaigns 拉活动列表。
//
// 抽出来是因为签到状态与领取都要它 —— 早期两处各写一次会**重复发一次 GET**。
func (c *Client) loadCampaigns(ctx context.Context, a *Auth) (*campaignsResult, bool) {
	var root map[string]any
	if ok, _ := c.getJSON(ctx, a, campaignsPath, &root); !ok {
		return nil, false
	}
	out := &campaignsResult{}
	arr, _ := root["campaigns"].([]any)
	for _, item := range arr {
		id := readString(item, "campaignId")
		if id == "" {
			continue
		}
		out.Campaigns = append(out.Campaigns, campaign{
			ID:          id,
			Key:         readString(item, "campaignKey"),
			ActionType:  readString(item, "actionType"),
			ClaimStatus: readString(item, "claimStatus"),
			Amount:      readNumberAny(item, "benefit", "amount"),
		})
	}
	return out, true
}

// claimable 可领取的活动：CLAIM_BENEFIT 且当前 CLAIMABLE。
func (r *campaignsResult) claimable() []campaign {
	var out []campaign
	for _, c := range r.Campaigns {
		if c.ActionType == "CLAIM_BENEFIT" && c.ClaimStatus == "CLAIMABLE" {
			out = append(out, c)
		}
	}
	return out
}

// claimedBenefit 已领过的领分类活动。
func (r *campaignsResult) claimedBenefit() []campaign {
	var out []campaign
	for _, c := range r.Campaigns {
		if c.ActionType == "CLAIM_BENEFIT" && c.ClaimStatus == "CLAIMED" {
			out = append(out, c)
		}
	}
	return out
}

// FetchCheckinStatus 查签到状态。
//
// ok=false 表示**查不到**（网络失败 / 非 2xx / 形状非法），
// 与「无活动可领」严格区分。
func (c *Client) FetchCheckinStatus(ctx context.Context, a *Auth) (CheckinStatus, bool) {
	parsed, ok := c.loadCampaigns(ctx, a)
	if !ok {
		return CheckinStatus{}, false
	}
	claimable := parsed.claimable()
	benefit := benefitCampaigns(parsed)
	claimed := parsed.claimedBenefit()

	// 真有「领过」的领分类活动、且当前无可领项 ⇒ 今天已领。
	// 列表为空 / 仅 VIEW_DETAILS / 请求头不完整导致的空态，一律判**未领**。
	todayCheckedIn := len(claimed) > 0 && len(claimable) == 0

	st := CheckinStatus{
		// ⚠ 拿到响应即 true（见字段注释）。
		Active:         true,
		TodayCheckedIn: todayCheckedIn,
	}
	if len(claimable) > 0 {
		st.DailyCredit = claimable[0].Amount
	} else if len(benefit) > 0 {
		st.DailyCredit = benefit[0].Amount
	}
	if len(benefit) > 0 {
		// 优先 campaignKey（人类可读），回落 ID —— 只留 ID 会显示成一串 UUID。
		st.ActivityName = firstNonEmpty(benefit[0].Key, benefit[0].ID)
	}

	// 无可领项且非「已领」时，进一步判断是否**未开通**。
	// 判据与 ClaimDailyCheckin 完全一致 —— 两处必须同源，否则状态查询说
	// 「未领取」而领取时报「未开通」，用户会困惑。
	if !todayCheckedIn && len(claimable) == 0 {
		usage := c.FetchUsageRaw(ctx, a)
		if isNotActivated(parsed, usage) {
			st.ActionRequired = true
			st.Message = notActivatedHint
		}
	}
	return st, true
}

// benefitCampaigns 全部领分类活动（无论状态）。
func benefitCampaigns(r *campaignsResult) []campaign {
	var out []campaign
	for _, c := range r.Campaigns {
		if c.ActionType == "CLAIM_BENEFIT" {
			out = append(out, c)
		}
	}
	return out
}

// isNotActivated 判断账号是否「尚未开通每日领取」。
//
// # 判据（两条**同时**满足才算，避免误报）
//
//  1. 活动列表里**没有** CLAIM_BENEFIT（连已领的都没有）；
//  2. 用量响应里 `addOnQuota` 字段**不存在**（注意是缺失，不是 0 ——
//     已开通账号即使额度用尽也会有该字段，如 {total:100, remaining:0}）。
//
// ⚠ 第 2 条用「字段是否存在」而非「remaining 是否为 0」：后者对
// 「额度用光」与「从未开通」不可区分，会把用光额度的老账号误报成未开通。
//
// 取服务端信号而不读本机文件：本机目录信号虽也命中，但用户清过
// `~/.qoder` 缓存、或在另一台机器上跑时会误报。
func isNotActivated(c *campaignsResult, usageBody map[string]any) bool {
	// 取不到活动列表时无法判定 —— 保守返回 false（宁可少提示，不可误报）。
	if c == nil {
		return false
	}
	if len(benefitCampaigns(c)) > 0 {
		return false
	}
	if usageBody == nil {
		return false
	}
	usage, _ := usageBody["qoderUsage"].(map[string]any)
	if usage == nil {
		return false
	}
	_, hasAddOn := usage["addOnQuota"]
	return !hasAddOn
}

// ClaimDailyCheckin 领取该账号**当前所有**可领活动。
//
// 一个账号可能同时有多个 CLAIM_BENEFIT 活动，故逐个领取而非只领第一个。
//
// ⚠ **「无可领活动」必须是 inactive，不能报 already-claimed**
//（真实缺陷，用户报障「没领过就显示已经领取」）：旧实现在
// targets 为空时直接返回「今天已领取」，于是只要服务端没下发可领项
//（含请求头不完整、未到刷新时间、本就无活动三种情形），
// 界面就显示「今天已领取」，与 IDE 的「可领取」直接矛盾。
// 二者语义完全不同：inactive = 没东西可领；already-claimed = 领过了。
func (c *Client) ClaimDailyCheckin(ctx context.Context, a *Auth) ClaimOutcome {
	parsed, ok := c.loadCampaigns(ctx, a)
	if !ok {
		return ClaimOutcome{Kind: "failed", Message: "活动列表查询失败"}
	}

	targets := parsed.claimable()
	if len(targets) == 0 {
		// 区分三种「没领到」：
		//   ① 确实领过      → already-claimed
		//   ② 账号未开通    → inactive + 可操作提示
		//   ③ 只是暂时没活动 → inactive
		if len(parsed.claimedBenefit()) > 0 {
			return ClaimOutcome{Kind: "already-claimed", Message: "今天已领取"}
		}
		usage := c.FetchUsageRaw(ctx, a)
		if isNotActivated(parsed, usage) {
			// actionRequired 是给 UI 的**显式信号**（而非让它去猜文案）：
			// 这条 inactive 需要用户去官方客户端登录一次，必须单独醒目展示。
			return ClaimOutcome{Kind: "inactive", Message: notActivatedHint, ActionRequired: true}
		}
		return ClaimOutcome{Kind: "inactive", Message: "当前没有可领取的活动"}
	}

	total := 0.0
	firstErr := ""
	for _, t := range targets {
		out := c.claimOne(ctx, a, t.ID)
		switch out.Kind {
		case "claimed":
			total += out.Credit
		case "failed":
			if firstErr == "" {
				firstErr = out.Message
			}
		}
	}
	if total > 0 {
		return ClaimOutcome{Kind: "claimed", Credit: total}
	}
	if firstErr != "" {
		return ClaimOutcome{Kind: "failed", Message: firstErr}
	}
	return ClaimOutcome{Kind: "already-claimed", Message: "今天已领取"}
}

// claimOne 领取一个活动的积分。
//
// ⚠ **幂等判据是响应体的 `replayed`，不是 HTTP 状态码**：重复领取同样
// 返回 200，但 `replayed:true` 且**不含 `benefit`**、`claimedAt` 是旧时间。
// 只看状态码会把「今天已领」误报成「领取成功 +100」。
//
// ⚠ **请求体必须是空串**（抓包实测 `content-length: 0`）。源码里领取走
// POST 但无 payload；发 `{}` 之类未经验证的 body 属额外风险，故照实发空。
func (c *Client) claimOne(ctx context.Context, a *Auth, campaignID string) ClaimOutcome {
	u := c.openAPI() + campaignsPath + "/" + url.PathEscape(campaignID) + "/claim"
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(""))
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: err.Error()}
	}
	for k, v := range c.creditsHeaders(a) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(ctx, creditsTimeoutMS*time.Millisecond)
	defer cancel()
	resp, err := c.httpClient().Do(req.WithContext(ctx))
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ClaimOutcome{Kind: "failed", Message: describeNonJSON(resp.StatusCode, string(body))}
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ClaimOutcome{Kind: "failed", Message: describeNonJSON(resp.StatusCode, string(body))}
	}
	// `replayed:true` = 本次活动此前已领（服务端回放上次结果）。
	if b, _ := root["replayed"].(bool); b {
		return ClaimOutcome{Kind: "already-claimed", Message: "今天已领取"}
	}
	if s := readString(root, "status"); s != "" && s != "CLAIMED" {
		return ClaimOutcome{Kind: "failed", Message: "领取未成功（status=" + s + "）"}
	}
	return ClaimOutcome{Kind: "claimed", Credit: readNumberAny(root, "benefit", "amount")}
}

// describeNonJSON 把非 2xx 的响应压成一句可读原因。
//
// # ⚠ 名字与文案不一致（本轮修的一个小缺陷）
//
// 旧文案一律说「服务端返回了**非 JSON** 响应」，但它对被调用的场合太宽：
// 503 的错误体**本身就是 JSON**
//
//	{"errorCode":"DEPENDENCY_UNAVAILABLE","errorMessage":"campaign service…"}
//
// 于是界面报「非 JSON 响应」，而用户/排查者看到的是 JSON —— 这句文案
// 会把人往"解析器坏了"的方向带，而真相是"上游服务临时不可用"。
// 现在的判据是**看 body 到底是不是 JSON**，而不是假设。
func describeNonJSON(status int, text string) string {
	if status == 401 || status == 403 {
		return fmt.Sprintf("凭据已失效（HTTP %d），请重新登录该账号", status)
	}
	snippet := strings.Join(strings.Fields(text), " ")
	if len(snippet) > 120 {
		snippet = snippet[:120]
	}
	// 服务端的错误体是 JSON 时，把它的 errorCode/errorMessage 抠出来 ——
	// 那两个字段才是有诊断价值的（HTTP 状态码只说"哪一类"，它们是"为什么"）。
	if code, msg := jsonErrorCodeMessage(text); code != "" || msg != "" {
		if status == 503 {
			return fmt.Sprintf("上游服务暂时不可用（HTTP %d %s：%s）—— 稍后会自动重试",
				status, code, msg)
		}
		return fmt.Sprintf("服务端拒绝（HTTP %d %s：%s）", status, code, msg)
	}
	return fmt.Sprintf("服务端返回了无法识别的响应（HTTP %d）：%s", status, snippet)
}

// jsonErrorCodeMessage 从服务端错误体里取 errorCode / errorMessage。
//
// 取不到（不是 JSON / 没有这两个字段）时返回空串 —— 由调用方回落成截断文本。
func jsonErrorCodeMessage(text string) (code, msg string) {
	start := strings.Index(text, "{")
	if start < 0 {
		return "", ""
	}
	var rec struct {
		Code    string `json:"errorCode"`
		Message string `json:"errorMessage"`
	}
	if err := json.Unmarshal([]byte(text[start:]), &rec); err != nil {
		return "", ""
	}
	return rec.Code, rec.Message
}

// ── 安全读值（容忍字符串/数字/缺失） ──

// readString 安全读字符串字段。
func readString(source any, key string) string {
	m, ok := source.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// readNumber 安全读数字字段（容忍字符串与缺失）。
//
// 上游可能下发 float64（JSON 数字）或 string（"100"），两种都归一化。
func readNumber(source any, key string) (float64, bool) {
	m, ok := source.(map[string]any)
	if !ok {
		return 0, false
	}
	return numOf(m[key])
}

// readNumberAny 读嵌套一层的数字（如 benefit.amount）。
func readNumberAny(source any, outer, inner string) float64 {
	m, ok := source.(map[string]any)
	if !ok {
		return 0
	}
	sub, ok := m[outer].(map[string]any)
	if !ok {
		return 0
	}
	v, _ := numOf(sub[inner])
	return v
}

// numOf 把一个 JSON 值读成 float64。
func numOf(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		var f float64
		if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func nonNeg(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}

// round2 保留两位小数（积分展示精度）。
func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
