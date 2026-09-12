package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// ClaimWelfare 领取指定活动。
//
// 幂等：服务端按 idempotentKey 去重，重复调用不会重复发放。
// 已领过（claimable=false）时服务端返回业务错误，这里原样透出 message。
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

	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &resp)

	// code==0 视为成功；非 0 时把 message 透出（已领取会走这里）
	return &WelfareResult{
		CampaignID: campaignID,
		Claimed:    resp.Code == 0,
		Message:    resp.Message,
	}, nil
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
