package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// 福利中心（= 用户口中的"签到"）。
//
// 契约来自 **IDE 运行日志实证**，不是从扩展源码推测 ——
// 扩展里拼的是 /PromptCenterService/v1/ops/delivery，实测 404；
// 生产环境 domain-switch 会改写成短前缀 /v1/ops/delivery。
//
//	GET  {engineBase}/v1/ops/delivery?channel=IDE
//	     Headers: Agent-Type: PromptCenter, X-Language, x-snap-traceid
//	POST {engineBase}/v1/ops/claim
//	     {"campaignId":N,"idempotentKey":"<hex>","channel":"IDE"}
//	POST {engineBase}/v1/ops/confirm
//	     {"campaignId":N}                      ← 见下「第二步确认」
//
// # 第二步确认（confirm）—— 本文件相对旧实现的**关键补齐**
//
// 来源：参照项目 `dsh-codearts-auth` 的 `src/codearts-credits.ts`（已实测跑通）。
// 它把这套流程写成四步，并明确标注：
//
//	5. 响应 `id !== null` 时补 `POST /v1/ops/confirm`（**漏掉会让积分停在待确认**）
//
// 我们原先只做到 claim，没有 confirm —— 也就是说领取动作**成功但未入账**，
// 积分停在「待确认」态。这是参照项目已经踩过并修好的坑，直接照搬其判据：
//
//	· 仅当 claim 响应的 `data.id` 非 null/undefined 时才补 confirm
//	  （服务端按这个字段表示"需要确认"；IDE 自身的判据就是 `benefit.id !== null`）
//	· **confirm 失败不把整体判为失败** —— 积分已进入待确认态，报 failed 会让
//	  用户以为没领到而重复点击。如实返回 claimed，把确认异常留在 Message 里。
//
// 实测存在三个 campaign：
//
//	1  每日签到领1000 积分      USER_LOGIN          1000 CREDIT  每日
//	2  学生认证领4000 积分      STUDENT_CERTIFIED   4000 CREDIT  一次性
//	4  新用户注册送 4000 积分    NEW_USER_REGISTER   4000 CREDIT  一次性
//
// 有效期 2026-09-08 → 2026-12-30（"限时"）。
const (
	welfareDeliveryPath = "/v1/ops/delivery"
	welfareClaimPath    = "/v1/ops/claim"
	welfareConfirmPath  = "/v1/ops/confirm"
)

// WelfareItem 是一个可领的福利活动。
type WelfareItem struct {
	CampaignID    int    `json:"campaignId"`
	Title         string `json:"title"`
	Type          string `json:"type"`
	BenefitAmount int    `json:"benefitAmount"`
	BenefitUnit   string `json:"benefitUnit"`
	Claimable     bool   `json:"claimable"`
	Extra         struct {
		StartTime string `json:"startTime"`
		EndTime   string `json:"endTime"`
		// ConsumePriority 越大越先消耗（决定额度用尽顺序）
		ConsumePriority int `json:"consumePriority"`
	} `json:"extra"`
}

// WelfareResult 是领取结果。
type WelfareResult struct {
	CampaignID int
	Title      string
	Claimed    bool
	Message    string
}

// welfareHeaders 构造福利接口所需的头。
//
// Agent-Type 必须是 **PromptCenter** —— 实测用 AgentCenter 会被路由拒绝
// （400 "未找到合法可路由api, agentType:AgentCenter"）。
func welfareHeaders() map[string]string {
	return map[string]string{
		"Agent-Type":     "PromptCenter",
		"X-Language":     "zh-cn",
		"x-snap-traceid": NewSessionID()[4:], // 复用随机 hex 生成
		"ide-name":       "CodeArts Agent",
		"ide-version":    "1.109.5",
		"plugin-name":    "snap_vscode",
		"plugin-version": "26.9.100",
	}
}

// FetchWelfare 列出当前账号的福利活动。
func (c *Client) FetchWelfare(a *Auth) ([]WelfareItem, error) {
	url := c.engineBase() + welfareDeliveryPath + "?channel=IDE"
	body, status, err := SignedCall(c, a, context.Background(), http.MethodGet, url, "", welfareHeaders())
	if err != nil {
		return nil, fmt.Errorf("请求福利列表: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("福利列表 HTTP %d: %s", status, truncate(body, 200))
	}

	var resp struct {
		Code int `json:"code"`
		Data struct {
			Items []WelfareItem `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("解析福利响应: %w", err)
	}
	return resp.Data.Items, nil
}

// ClaimWelfare 领取指定活动（两步：claim + 必要时 confirm）。
//
// 幂等：服务端按 idempotentKey 去重，重复调用不会重复发放。
// 已领过（claimable=false）时服务端返回业务错误，这里原样透出 message。
//
// # 为什么要补 confirm（照搬参照项目的实测判据）
//
// 服务端在 claim 响应里用 `data.id` 表示"这一笔需要二次确认"。
// 不补 confirm 的后果是**积分停在待确认态**，即"领了但没到账" ——
// 而界面与日志都显示成功，属于最难排查的一类不一致。
//
// 判据（与 IDE 自身一致）：`data.id` 非 null → 补 confirm。
//
// confirm 失败**不**让本函数返回错误：claim 已经成功、积分已进入待确认态，
// 报错会让用户以为没领到而重复点击。确认异常写进 Message 如实透出。
func (c *Client) ClaimWelfare(a *Auth, campaignID int) (*WelfareResult, error) {
	payload, _ := json.Marshal(map[string]any{
		"campaignId":    campaignID,
		"idempotentKey": NewSessionID()[4:],
		"channel":       "IDE",
	})
	url := c.engineBase() + welfareClaimPath
	body, status, err := SignedCall(c, a, context.Background(), http.MethodPost, url, string(payload), welfareHeaders())
	if err != nil {
		return nil, fmt.Errorf("请求领取: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("领取 HTTP %d: %s", status, truncate(body, 200))
	}

	// Data.ID 用 *json.RawMessage 承接：要区分「字段缺席/为 null」与「有值」。
	// 参照项目的判据是 `id !== null && id !== undefined`，两者都归入"不需要确认"。
	// 用 any 会丢掉这个区分（null 与缺席都解成 nil，但 0 会是 float64(0)），
	// 而服务端确实可能返回 id: 0 这种"需要确认但 id 恰为 0"的形态。
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			ID json.RawMessage `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &resp)

	// code==0 视为成功；非 0 时把 message 透出（已领取会走这里）
	ok := resp.Code == 0
	result := &WelfareResult{
		CampaignID: campaignID,
		Claimed:    ok,
		Message:    resp.Message,
	}

	// ── 第二步：按需确认 ──────────────────────────────────────────────
	//
	// 只在 claim 真的成功后才补 —— 失败时补 confirm 没有意义，
	// 且会对着一个不存在的 benefit 发请求。
	if ok && needsConfirm(resp.Data.ID) {
		if cerr := c.confirmWelfare(a, campaignID); cerr != nil {
			// 不返回错误：见函数注释。把异常留在 Message 里，让"领到了但确认失败"
			// 在界面上可见，而不是伪装成一次彻底的失败。
			result.Message = strings.TrimSpace(result.Message + " （已领取，但确认未完成: " + cerr.Error() + "）")
		}
	}
	return result, nil
}

// needsConfirm 报告 claim 响应是否需要补 confirm。
//
// 参照项目判据：`id !== null && id !== undefined` 才补。对应到 Go：
//
//	字段缺席        → raw == nil        → false
//	字段为 null     → raw == "null"     → false
//	其余（含 0、字符串、对象）→ true
//
// ⚠ 刻意**不**把 `id: 0` 当作"不需要确认"：ID 用 0 表示"无"是常见约定，
// 但服务端若真返回 0 且需要确认，漏掉 confirm 就是静默丢积分。
// 宁可多发一次幂等的 confirm（服务端按 campaignId 处理，重复确认无副作用），
// 也不要漏。这个方向的误判代价更低。
func needsConfirm(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// confirmWelfare 补一次 claim 确认（POST /v1/ops/confirm {campaignId}）。
func (c *Client) confirmWelfare(a *Auth, campaignID int) error {
	payload, _ := json.Marshal(map[string]any{"campaignId": campaignID})
	url := c.engineBase() + welfareConfirmPath
	body, status, err := SignedCall(c, a, context.Background(), http.MethodPost, url, string(payload), welfareHeaders())
	if err != nil {
		return fmt.Errorf("确认请求失败: %w", err)
	}
	if status >= 400 {
		return fmt.Errorf("确认 HTTP %d: %s", status, truncate(body, 120))
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &resp)
	if resp.Code != 0 {
		return fmt.Errorf("确认业务码 %d: %s", resp.Code, resp.Message)
	}
	return nil
}

// ClaimAllWelfare 领取所有当前可领的活动，返回每个的结果。
//
// 串行执行：福利接口没有并发收益，而串行能让日志与返回顺序对应。
func (c *Client) ClaimAllWelfare(a *Auth) ([]WelfareResult, error) {
	items, err := c.FetchWelfare(a)
	if err != nil {
		return nil, err
	}
	var out []WelfareResult
	for _, it := range items {
		if !it.Claimable {
			out = append(out, WelfareResult{
				CampaignID: it.CampaignID,
				Title:      it.Title,
				Claimed:    false,
				Message:    "今日已领",
			})
			continue
		}
		r, err := c.ClaimWelfare(a, it.CampaignID)
		if err != nil {
			out = append(out, WelfareResult{
				CampaignID: it.CampaignID, Title: it.Title,
				Claimed: false, Message: err.Error(),
			})
			continue
		}
		r.Title = it.Title
		out = append(out, *r)
	}
	return out, nil
}

// Subscription 是账号的套餐与额度快照。
//
// 与福利的关系：`IsTokenPackage == true` 时福利列表恒为空 ——
// 因为 token 套餐本身就是"限时额度"，不再叠加积分福利。
type Subscription struct {
	PackageNameCN string `json:"package_name_cn"`
	SpecCode      string `json:"spec_code"`
	IsCreditPack  bool   `json:"is_credit_package"`
	IsTokenPack   bool   `json:"is_token_package"`
	Status        string `json:"status"`

	EndDate   string `json:"end_date"`
	StartDate string `json:"start_date"`

	// Credits 是积分套餐的额度（实测体验版 6500）
	CreditTotal    float64
	CreditRemain   float64
	CreditUsed     float64
	CreditExpiring float64
	// TokenUsed 是 token 套餐的用量
	TokenUsed int64
}

// subscriptionRaw 对应 /snap-manager/v1/statistics/plugin 的响应。
type subscriptionRaw struct {
	EndDate   string `json:"end_date"`
	StartDate string `json:"start_date"`
	Package   struct {
		PackageNameCN string `json:"package_name_cn"`
		SpecCode      string `json:"spec_code"`
		IsCreditPack  bool   `json:"is_credit_package"`
		IsTokenPack   bool   `json:"is_token_package"`
		Status        string `json:"status"`
	} `json:"package"`
	Metrics []struct {
		Name           string  `json:"name"`
		CreditAmount   float64 `json:"package_credit_amount"`
		CreditRemain   float64 `json:"package_credit_remain"`
		CreditUsed     float64 `json:"package_credit_used"`
		CreditExpiring float64 `json:"package_credit_expiring_amount"`
		UsageTokenNum  int64   `json:"usage_token_num"`
	} `json:"metrics"`
}

// FetchSubscription 查账号套餐与额度。
//
// 走 /snap-manager/v1/statistics/plugin（实测签名方式即可，无需特制头）。
// 这是管理台展示"剩余积分"的来源 —— 此前一直显示 0，是因为没接这个接口。
func (c *Client) FetchSubscription(a *Auth) (*Subscription, error) {
	url := c.engineBase() + "/snap-manager/v1/statistics/plugin"
	body, status, err := SignedGet(c, a, url, map[string]string{"Agent-Type": "PromptCenter"})
	if err != nil {
		return nil, fmt.Errorf("请求订阅信息: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("订阅信息 HTTP %d: %s", status, truncate(body, 200))
	}

	var raw subscriptionRaw
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("解析订阅响应: %w", err)
	}

	s := &Subscription{
		PackageNameCN: raw.Package.PackageNameCN,
		SpecCode:      raw.Package.SpecCode,
		IsCreditPack:  raw.Package.IsCreditPack,
		IsTokenPack:   raw.Package.IsTokenPack,
		Status:        raw.Package.Status,
		EndDate:       raw.EndDate,
		StartDate:     raw.StartDate,
	}
	for _, m := range raw.Metrics {
		switch m.Name {
		case "usageTotalPackageCredit":
			s.CreditTotal = m.CreditAmount
			s.CreditRemain = m.CreditRemain
			s.CreditUsed = m.CreditUsed
			s.CreditExpiring = m.CreditExpiring
		case "usageTokenChatMessages":
			s.TokenUsed = m.UsageTokenNum
		}
	}
	return s, nil
}
