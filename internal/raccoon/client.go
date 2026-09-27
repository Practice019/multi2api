// client.go Raccoon 的 HTTP 出口层。
//
// 对应参照项目：
//
//	raccoon-oauth.ts   → 扫码轮询 / 续期 / 用户信息
//	raccoon-auth.ts    → 模型目录
//	raccoon-credits.ts → 余额 / 登录奖励
//	raccoon-adapter.ts → 聊天转发
package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client Raccoon 的 HTTP 客户端。基址可注入（测试用 httptest）。
type Client struct {
	HTTP *http.Client
	// APIBase 上游基址。
	APIBase string
}

// New 生产默认值。
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 0}, APIBase: DefaultAPIBase}
}

// NewWithBase 测试构造。
func NewWithBase(base string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 0}, APIBase: base}
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: requestTimeoutMS * time.Millisecond}
}

func (c *Client) base() string {
	if c != nil && c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

// truncate 截断响应体用于日志。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// raccoonHeaders 基础业务头（参照 raccoon.ts:271-293）。
//
// withPlatform 为真时额外带 X-Client-Platform / X-Client-Version
// （billing 与 desktop 端点需要，catalog 不需要）。
func raccoonHeaders(req *http.Request, a *Auth, withPlatform bool) {
	req.Header.Set("Accept", "application/json")
	if a != nil {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
		// ⚠ X-Org-Code **总是发**（个人账号为空串也照发）
		req.Header.Set("X-Org-Code", a.OfficeIdentity)
		if a.DeviceID != "" {
			req.Header.Set("X-Client-Device-ID", a.DeviceID)
		}
	} else {
		req.Header.Set("X-Org-Code", "")
	}
	req.Header.Set("X-Raccoon-Language", "zh")
	if withPlatform {
		req.Header.Set("X-Client-Platform", clientPlatform)
		req.Header.Set("X-Client-Version", clientVersion)
	}
}

// doJSON 发一次 JSON 请求并解析信封。
//
// ⚠ 返回的 envelope 里 code 才是成功判据 —— **HTTP 200 + 非 0 code 表示失败**。
func (c *Client) doJSON(ctx context.Context, method, url string, body any,
	timeoutMS int, decorate func(*http.Request)) (envelope, int, error) {

	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return envelope{}, 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return envelope{}, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if decorate != nil {
		decorate(req)
	}

	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	resp, err := c.httpClient().Do(req.WithContext(cctx))
	if err != nil {
		return envelope{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return envelope{}, resp.StatusCode, err
	}
	var env envelope
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &env); uerr != nil {
			return envelope{}, resp.StatusCode,
				fmt.Errorf("raccoon: 响应不是 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 160))
		}
	}
	return env, resp.StatusCode, nil
}

// ── 扫码登录（raccoon-oauth.ts:140-180）──────────────────────────────────

// QRPollResult 一次扫码轮询的结果。
type QRPollResult struct {
	Status       QRStatus
	ExpiredAt    string
	AccessToken  string
	RefreshToken string
}

// PollQRCode 轮询扫码状态。
//
// ⚠ 本端点**不带 Authorization**（唯一必需头是 Content-Type）。
//
// ⚠ 任何异常（网络抖动、响应畸形、未知 status）→ 调用方应**降级为 pending
// 继续轮询**，而不是当成失败。见 NormalizeQRStatus 的注释。
func (c *Client) PollQRCode(ctx context.Context, code string) (QRPollResult, error) {
	env, _, err := c.doJSON(ctx, http.MethodPost, c.base()+qrcodeLoginPath,
		map[string]string{"qrcode_code": code}, requestTimeoutMS, nil)
	if err != nil {
		return QRPollResult{}, err
	}
	if env.Code != 0 {
		// 业务失败也降级为 pending（调用方继续轮询），但把原因带出来便于日志。
		return QRPollResult{Status: QRStatusPending}, envelopeError(env)
	}

	var data struct {
		Status       string `json:"status"`
		ExpiredAt    string `json:"expired_at"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(env.Data, &data)

	st := NormalizeQRStatus(data.Status)
	res := QRPollResult{Status: st, ExpiredAt: data.ExpiredAt}
	// ⚠ success 但 access_token 为空 → **视为 pending**（不产出半截凭据）
	if st == QRStatusSuccess && strings.TrimSpace(data.AccessToken) == "" {
		res.Status = QRStatusPending
		return res, nil
	}
	if st == QRStatusSuccess {
		res.AccessToken = data.AccessToken
		res.RefreshToken = data.RefreshToken
	}
	return res, nil
}

// ── 续期（raccoon-oauth.ts:294-318）─────────────────────────────────────

// RefreshCredential 续期一份凭证。
//
// 合并规则（**必须保留服务端不返回的字段**）：
//
//	access_token   ← 新值
//	refresh_token  ← 新值非空才覆盖，否则**保留旧值**
//	expires_at     ← 由新 token 的 JWT exp 重算（解析不到则沿用旧值）
//	其余字段        ← 全部保留（office_identity / user_id / nickname / phone / device_id）
//
// ⚠ "服务端可能只返回新 access_token"是实测形态。不保留旧 refresh_token
// 会让续期一次就把账号变成不可续期。
func (c *Client) RefreshCredential(ctx context.Context, a *Auth) (*Auth, error) {
	if a == nil || !a.Renewable() {
		return nil, ErrRefreshExpired
	}
	env, status, err := c.doJSON(ctx, http.MethodPost, c.base()+refreshPath,
		map[string]string{"refresh_token": a.RefreshToken}, requestTimeoutMS,
		func(req *http.Request) { req.Header.Set("Content-Type", "application/json") })
	if err != nil {
		return nil, err // 网络失败：可重试
	}
	// ⚠ HTTP 401 或 code 200003 = 登录态已过期（终态，不重试）
	if status == http.StatusUnauthorized || env.Code == 200003 {
		return nil, ErrRefreshExpired
	}
	if env.Code != 0 {
		return nil, envelopeError(env)
	}

	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(env.Data, &data)
	if strings.TrimSpace(data.AccessToken) == "" {
		return nil, fmt.Errorf("%w：续期响应缺少 access_token", ErrRefreshExpired)
	}

	next := *a // 逐字段保留（含 FilePath 等）
	next.AccessToken = data.AccessToken
	if strings.TrimSpace(data.RefreshToken) != "" {
		next.RefreshToken = data.RefreshToken
	}
	if ms := decodeJWTExpMS(data.AccessToken); ms > 0 {
		next.ExpiresAt = fmt.Sprintf("%d", ms)
	}
	return &next, nil
}

// ── 用户信息（raccoon-oauth.ts:336-358）─────────────────────────────────

// UserInfo 补全后的账号信息。
type UserInfo struct {
	ID             string
	Name           string
	OfficeIdentity string
	Phone          string
}

// FetchUserInfo 查用户信息。
//
// ⚠ 失败/非 0 code/网络异常 → **返回零值，不抛错**：
// 它是"尽力补全"，不该影响登录成功。
func (c *Client) FetchUserInfo(ctx context.Context, a *Auth) UserInfo {
	env, _, err := c.doJSON(ctx, http.MethodGet, c.base()+userInfoPath, nil,
		requestTimeoutMS, func(req *http.Request) { raccoonHeaders(req, a, false) })
	if err != nil || env.Code != 0 {
		return UserInfo{}
	}
	var data struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		OfficeIdentity string `json:"office_identity"`
		Phone          string `json:"phone"`
	}
	_ = json.Unmarshal(env.Data, &data)
	return UserInfo{ID: data.ID, Name: data.Name, OfficeIdentity: data.OfficeIdentity, Phone: data.Phone}
}

// EnrichCredential 尽力补全凭证里缺失的展示字段（参照 raccoon-auth.ts:221-239）。
//
// ⚠ **只在字段缺失时补**（credential.X == "" 才写）：已有值不被覆盖。
// 失败不影响登录成功。
func (c *Client) EnrichCredential(ctx context.Context, a *Auth) {
	if a == nil {
		return
	}
	if a.Nickname != "" && a.UserID != "" && a.Phone != "" {
		return // 都齐了，不必打上游
	}
	ui := c.FetchUserInfo(ctx, a)
	if ui.ID == "" && ui.Name == "" && ui.Phone == "" && ui.OfficeIdentity == "" {
		return // 没拿到任何东西
	}
	if a.UserID == "" {
		a.UserID = ui.ID
	}
	if a.Nickname == "" {
		a.Nickname = ui.Name
	}
	if a.Phone == "" {
		a.Phone = ui.Phone
	}
	if a.OfficeIdentity == "" {
		a.OfficeIdentity = ui.OfficeIdentity
	}
}

// ── 模型目录（raccoon-auth.ts:627-656）──────────────────────────────────

// RemoteModel 远端模型条目。
type RemoteModel struct {
	ID                string
	Name              string
	ContextWindow     int
	MaxTokens         int
	SupportsImage     bool
	BaseMultiplier    float64
	EffectiveMult     float64
	HasBaseMultiplier bool
	HasEffectiveMult  bool
}

// FetchModelCatalog 拉模型目录。
//
// 解析路径：code==0 → data.categories[] → type=="chat" 的那一项 → .models[]
//
// ⚠ 失败（非 2xx / 网络异常 / code!=0）→ **返回空切片**，
// 由调用方回退兜底表。
//
// ⚠ 本端点**不发 Content-Type、不发 X-Client-Platform**（参照 raccoon-auth.ts:484-489）。
func (c *Client) FetchModelCatalog(ctx context.Context, a *Auth) []RemoteModel {
	env, _, err := c.doJSON(ctx, http.MethodGet, c.base()+modelCatalogPath, nil,
		catalogTimeoutMS, func(req *http.Request) {
			req.Header.Set("Accept", "application/json")
			if a != nil {
				req.Header.Set("Authorization", "Bearer "+a.AccessToken)
				req.Header.Set("X-Org-Code", a.OfficeIdentity)
			}
			req.Header.Set("X-Raccoon-Language", "zh")
		})
	if err != nil || env.Code != 0 {
		return nil
	}

	var payload struct {
		Categories []struct {
			Type   string `json:"type"`
			Models []struct {
				Name        string          `json:"name"` // ⚠ 这个字段是**模型 id**，不是展示名
				Description string          `json:"description"`
				Visible     *bool           `json:"visible"`
				Params      json.RawMessage `json:"params"`
				// 回退字段（params 里没有时用）
				ContextWindow int `json:"context_window"`
				// 倍率
				BillingEffectiveMultiplier *float64 `json:"billing_effective_multiplier"`
				BillingMultiplier          *float64 `json:"billing_multiplier"`
				Tags                       []string `json:"tags"`
			} `json:"models"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return nil
	}

	var chatModels []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Visible     *bool           `json:"visible"`
		Params      json.RawMessage `json:"params"`
		ContextWindow int           `json:"context_window"`
		BillingEffectiveMultiplier *float64 `json:"billing_effective_multiplier"`
		BillingMultiplier          *float64 `json:"billing_multiplier"`
		Tags                       []string `json:"tags"`
	}
	for _, cat := range payload.Categories {
		if cat.Type == "chat" {
			chatModels = cat.Models
			break
		}
	}
	if chatModels == nil {
		return nil
	}

	out := make([]RemoteModel, 0, len(chatModels))
	seen := map[string]bool{}
	for _, m := range chatModels {
		id := strings.TrimSpace(m.Name)
		if id == "" || seen[id] {
			continue
		}
		// ⚠ visible 缺省视为可见（保守：不因字段缺失而隐藏模型）
		if m.Visible != nil && !*m.Visible {
			continue
		}
		seen[id] = true

		rm := RemoteModel{ID: id, Name: m.Description}
		// params 里优先，回退顶层
		var params struct {
			ContextWindow *int `json:"context_window"`
			MaxTokens     *int `json:"max_tokens"`
		}
		if len(m.Params) > 0 {
			_ = json.Unmarshal(m.Params, &params)
		}
		if params.ContextWindow != nil {
			rm.ContextWindow = *params.ContextWindow
		} else {
			rm.ContextWindow = m.ContextWindow
		}
		if params.MaxTokens != nil {
			rm.MaxTokens = *params.MaxTokens
		}
		if m.BillingEffectiveMultiplier != nil {
			rm.EffectiveMult = *m.BillingEffectiveMultiplier
			rm.HasEffectiveMult = true
		}
		if m.BillingMultiplier != nil {
			rm.BaseMultiplier = *m.BillingMultiplier
			rm.HasBaseMultiplier = true
		}
		for _, t := range m.Tags {
			lt := strings.ToLower(strings.TrimSpace(t))
			if lt == "vision" || lt == "image" || lt == "image-understanding" {
				rm.SupportsImage = true
			}
		}
		out = append(out, rm)
	}
	return out
}

// ── 积分（raccoon-credits.ts）────────────────────────────────────────────

// Balance 积分余额。
type Balance struct {
	Total   float64
	Reward  float64
	Daily   float64
	Monthly float64
	Topup   float64
}

// FetchBalance 查积分余额。
//
// ⚠ `available_points` 缺失即判失败（**不编造 0**）：0 是"已用光"的语义。
func (c *Client) FetchBalance(ctx context.Context, a *Auth) (*Balance, error) {
	env, _, err := c.doJSON(ctx, http.MethodGet, c.base()+balancePath, nil,
		requestTimeoutMS, func(req *http.Request) { raccoonHeaders(req, a, true) })
	if err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, envelopeError(env)
	}
	var data struct {
		AvailablePoints *float64 `json:"available_points"`
		RewardPoints    float64  `json:"reward_points"`
		DailyPoints     float64  `json:"daily_points"`
		MonthlyPoints   float64  `json:"monthly_points"`
		TopupPoints     float64  `json:"topup_points"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("raccoon: 余额响应无法解析: %w", err)
	}
	if data.AvailablePoints == nil {
		return nil, fmt.Errorf("raccoon: 余额响应缺少 available_points")
	}
	return &Balance{
		Total:   *data.AvailablePoints,
		Reward:  data.RewardPoints,
		Daily:   data.DailyPoints,
		Monthly: data.MonthlyPoints,
		Topup:   data.TopupPoints,
	}, nil
}

// LoginGrantResult 领取一次性登录奖励的结果。
type LoginGrantResult struct {
	// Claimed 本次真的领到了。
	Claimed bool
	// Points 领到的积分（granted=false 时为 0）。
	Points float64
}

// ClaimLoginGrant 领取一次性登录奖励（**无 body**）。
//
// ⚠ `X-Client-Platform` 是**准入条件**：它标识"来自桌面端"，缺了会被拒。
//
// 判据：code==0 且 data.granted===true → claimed；
// granted!==true（含 false，**HTTP 200**）→ 已经领过。
func (c *Client) ClaimLoginGrant(ctx context.Context, a *Auth) (LoginGrantResult, error) {
	env, _, err := c.doJSON(ctx, http.MethodPost, c.base()+loginGrantPath, nil,
		requestTimeoutMS, func(req *http.Request) { raccoonHeaders(req, a, true) })
	if err != nil {
		return LoginGrantResult{}, err
	}
	if env.Code != 0 {
		return LoginGrantResult{}, envelopeError(env)
	}
	var data struct {
		Granted bool `json:"granted"`
		Popup   *struct {
			Source string  `json:"source"`
			Points float64 `json:"points"`
		} `json:"popup"`
	}
	_ = json.Unmarshal(env.Data, &data)
	if !data.Granted {
		return LoginGrantResult{Claimed: false}, nil // 已经领过（不是错误）
	}

	pts := float64(loginRewardPoints)
	if data.Popup != nil && data.Popup.Points > 0 {
		pts = data.Popup.Points
	}
	return LoginGrantResult{Claimed: true, Points: pts}, nil
}

// OnboardingStatus 「桌面端登录奖励」是否已领。
type OnboardingStatus struct {
	// Claimed 已领过（每号一次）。
	Claimed bool
	// Points 已领到的积分（未领或账单未给出时为默认额度）。
	Points float64
}

// FetchOnboardingStatus 查登录奖励是否已领（供管理端点显示）。
//
// # 为什么必须查账单明细
//
// ⚠ **不能靠 balance 推断** —— 余额是多个来源（注册礼包 / 每日 / 充值）
// 的合计，无法区分某一项是否已领。
//
// ⚠ **不能只按 `biz_type === 'reward_grant'` 判定** —— 「新人注册礼包」
// 也是 reward_grant，把它算作登录奖励会让**新用户一开始就显示「已领取」**。
// 必须同时匹配 `event_name === '桌面端登录奖励'`。
//
// ⚠ **服务端没有单独的奖励状态端点**（实测），故只能查账单明细。
//
// ⚠ 查询失败时**保守返回 Claimed:false** —— 宁可让用户多点一次
//（服务端幂等，无害），也不要误报「已领」而让他真的错过。
func (c *Client) FetchOnboardingStatus(ctx context.Context, a *Auth) OnboardingStatus {
	fallback := OnboardingStatus{Claimed: false, Points: float64(loginRewardPoints)}
	u := c.base() + pointsPrefix + "/bills?paging.limit=50&paging.offset=0"
	env, _, err := c.doJSON(ctx, http.MethodGet, u, nil,
		requestTimeoutMS, func(req *http.Request) { raccoonHeaders(req, a, true) })
	if err != nil || env.Code != 0 {
		return fallback
	}
	var data struct {
		Items []struct {
			BizType   string  `json:"biz_type"`
			EventName string  `json:"event_name"`
			Points    float64 `json:"points"`
		} `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return fallback
	}
	for _, it := range data.Items {
		// ⚠ 两个条件都要判（见函数注释：只判 biz_type 会让新用户误报已领）。
		if it.BizType != "reward_grant" || it.EventName != loginRewardEventName {
			continue
		}
		out := OnboardingStatus{Claimed: true, Points: fallback.Points}
		if it.Points > 0 {
			out.Points = it.Points
		}
		return out
	}
	return fallback
}

// ── 推理（raccoon-adapter.ts）───────────────────────────────────────────

// ChatStream 发一次流式对话。
//
// 标准 OpenAI 兼容 + 标准 SSE，**无加密、无信封、无格式转换**。
//
// ⚠ 本端点带 X-Client-Platform，但**不带** X-Client-Version / X-Client-Device-ID
// （参照 raccoon-adapter.ts:297-304 的头集合逐字）。
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.base()+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	if a != nil {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
		req.Header.Set("X-Org-Code", a.OfficeIdentity)
	}
	req.Header.Set("X-Raccoon-Language", "zh")
	req.Header.Set("X-Client-Platform", clientPlatform)

	resp, err := c.httpClient().Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}
