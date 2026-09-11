package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 账号存活探测与额度预警
//
// 两个端点都用 Bearer token（与聊天同域），已用真实账号实测（见下方各方法注释）。
// 它们**不改变**账号状态，只回答两个问题：
//
//	CheckAccountAlive  「这个号还能不能用？」
//	DosageNotify      「上游怎么解释当前额度？（告急时才有内容）」
// ---------------------------------------------------------------------------

// accountsPath 是存活探测端点。
//
// 实测（真实账号）：
//
//	有效 token → HTTP 200  {"code":0,...,"data":{"accounts":[{...,"pluginEnabled":true}]}}
//	伪造 token → HTTP 401  HTML（openresty/APISIX 的错误页，**不是 JSON**）
//	空 token   → HTTP 401  HTML
//
// 两个要点决定了实现：
//  1. 失效时返回的是 HTML 而非 JSON —— 所以判定不能依赖"解出 JSON"，
//     必须先看状态码（doJSON 在 >=400 时直接返回 *Error，正好覆盖）。
//  2. 但也不能只看状态码：上游若改成"200 带业务错误码"，只判状态码会把死号
//     判成活的。因此额外校验 code==0 与 data.accounts 非空。
const accountsPath = "/v2/plugin/accounts"

// AccountLiveness 一次存活探测的结果。
type AccountLiveness struct {
	Alive bool
	// Accounts 是上游返回的账号条目数（>0 说明该 token 确实绑定着账号）。
	Accounts int
	// PluginEnabled 该账号的插件能力是否开启（实测字段；关闭时聊天会失败）。
	PluginEnabled bool
	// DeployMsg 上游给的部署状态说明（实测为空；非空时常是"为什么不能用"的答案）。
	DeployMsg string
	// Nickname / Phone 便于日志与界面直接显示，省得调用方再查一次。
	Nickname string
	Phone    string
}

// CheckAccountAlive 探测账号是否仍然可用（GET /v2/plugin/accounts）。
//
// 定位：这是**轻量存活性**判定，不代替余额查询 —— 它不返回积分。
// 实测它比余额端点慢（约 690ms vs 290ms），所以**不要**用它做高频轮询；
// 它的价值在"失败了想知道是死号还是限流"这类**诊断**场景。
//
// 失败即返回 error（含 401 之类的 *Error），调用方据此判定不健康。
func (c *Client) CheckAccountAlive(a *auth.Auth) (*AccountLiveness, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+accountsPath, nil)
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	// 实测该端点要求 UA（与 /v3/config 同）：缺了会被上游按未知客户端拒绝。
	req.Header.Set("User-Agent", clientUA)

	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}

	// 空 data：code==0 但没给 accounts。视为**不可用**而不是错误 ——
	// 没有账号条目意味着这个 token 不代表任何可用账号。
	if len(data) == 0 {
		return &AccountLiveness{Alive: false}, nil
	}

	var parsed struct {
		Accounts []struct {
			UID           string `json:"uid"`
			Nickname      string `json:"nickname"`
			Phone         string `json:"phoneNumber"`
			PluginEnabled bool   `json:"pluginEnabled"`
			DeployStatus  struct {
				StatusMsg string `json:"statusMsg"`
				DetailMsg string `json:"detailMsg"`
			} `json:"deployStatus"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("accounts parse: %w", err)
	}
	if len(parsed.Accounts) == 0 {
		return &AccountLiveness{Alive: false}, nil
	}

	first := parsed.Accounts[0]
	out := &AccountLiveness{
		Alive:         true,
		Accounts:      len(parsed.Accounts),
		PluginEnabled: first.PluginEnabled,
		Nickname:      first.Nickname,
		Phone:         first.Phone,
	}
	// 说明文案优先取 detailMsg（更具体），回落 statusMsg。
	out.DeployMsg = strings.TrimSpace(first.DeployStatus.DetailMsg)
	if out.DeployMsg == "" {
		out.DeployMsg = strings.TrimSpace(first.DeployStatus.StatusMsg)
	}
	return out, nil
}

// dosagePath 是额度预警端点。
const dosagePath = "/v2/billing/meter/get-dosage-notify"

// DosageNotify 查询上游对当前额度的**告警说明**（POST /v2/billing/meter/get-dosage-notify）。
//
// 实测（3 个真实账号，额度均正常）：
//
//	HTTP 200 {"code":0,"data":{"dosageNotifyCode":0,"dosageNotifyZh":"","dosageNotifyEn":""}}
//	无效 token → HTTP 401 HTML
//
// 关键性质：正常时**完全静默**（code=0、文案为空）。它只在额度告急/受限时
// 才带内容 —— 所以它的定位是「**失败时的一句人话解释**」，不是定时任务。
// 平时查它只是多打一次上游、什么都得不到。
//
// 返回 nil 表示"上游没给任何告警"（正常情况），调用方无需区分 nil 与空结构。
func (c *Client) DosageNotify(a *auth.Auth) (*DosageWarning, error) {
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+dosagePath,
		strings.NewReader(`{"timeout":5000}`))
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	req.Header.Set("Content-Type", "application/json")

	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	// ⚠ doJSON 返回的**已经是 data 段**（client.go 里 `return env.Data`），
	// 这里不能再套一层 {"code":..,"data":{...}} —— 否则取不到值且**不报错**，
	// 表现成"永远没有告警"。同一个坑在 models_catalog.go 里踩过一次（评审抓出），
	// 这是第二次，故把结论写在这里。
	var parsed struct {
		Code int    `json:"dosageNotifyCode"`
		Zh   string `json:"dosageNotifyZh"`
		En   string `json:"dosageNotifyEn"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("dosage parse: %w", err)
	}
	// 无告警：返回 nil，让调用方用 `if w != nil` 一个判断就够。
	if parsed.Code == 0 && strings.TrimSpace(parsed.Zh) == "" && strings.TrimSpace(parsed.En) == "" {
		return nil, nil
	}
	return &DosageWarning{
		Code: parsed.Code,
		Zh:   strings.TrimSpace(parsed.Zh),
		En:   strings.TrimSpace(parsed.En),
	}, nil
}

// DosageWarning 上游给出的额度告警。
type DosageWarning struct {
	Code int    `json:"code"`
	Zh   string `json:"zh,omitempty"`
	En   string `json:"en,omitempty"`
}

// Message 返回最适合展示的一句话：优先中文，回落英文；都没有则按码给兜底文案。
func (w *DosageWarning) Message() string {
	if w == nil {
		return ""
	}
	if w.Zh != "" {
		return w.Zh
	}
	if w.En != "" {
		return w.En
	}
	return fmt.Sprintf("上游额度告警（code=%d）", w.Code)
}
