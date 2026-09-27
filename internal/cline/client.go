// client.go Cline 的 HTTP 出口层（推理 / 模型 / 续期 / 余额 / 设备码登录）。
//
// # 与参照项目的对应关系
//
//	cline-oauth.ts     → 本文件的设备码三步
//	cline-auth.ts      → 本文件的 refreshCredential
//	cline-models.ts    → 本文件的 fetchRecommended / fetchModels
//	cline-credits.ts   → 本文件的 FetchBalance
//	cline-adapter.ts   → 本文件的 ChatStream
package cline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client Cline 的 HTTP 客户端。两个基址都可注入（测试用 httptest）。
type Client struct {
	HTTP *http.Client
	// APIBase 推理/账号端点基址。
	APIBase string
	// WorkOSBase 设备码授权基址。
	//
	// ⚠ 与 APIBase 是两个不同的域，不能合并。
	WorkOSBase string
}

// New 生产默认值。
func New() *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: 0}, // 逐请求设超时（SSE 不能有总时长）
		APIBase:    DefaultAPIBase,
		WorkOSBase: DefaultWorkOSBase,
	}
}

// NewWithBase 测试构造：把两个基址都指向同一个假上游。
func NewWithBase(base string) *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: 0},
		APIBase:    base,
		WorkOSBase: base,
	}
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: httpTimeoutMS * time.Millisecond}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (c *Client) apiBase() string {
	if c != nil && c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) workOSBase() string {
	if c != nil && c.WorkOSBase != "" {
		return strings.TrimRight(c.WorkOSBase, "/")
	}
	return DefaultWorkOSBase
}

// truncate 截断响应体用于日志/错误信息。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// applyClientHeaders 注入客户端标识头。
func applyClientHeaders(req *http.Request) {
	for k, v := range clientHeaders {
		req.Header.Set(k, v)
	}
}

// clineAuthHeaders 构造带鉴权的头集合（参照 cline.ts:296-307）。
//
// ⚠ Authorization 的值必须先过 clineBearerValue 补 workos: 前缀。
func clineAuthHeaders(req *http.Request, accessToken string) {
	req.Header.Set("Authorization", "Bearer "+clineBearerValue(accessToken))
	req.Header.Set("Accept", "application/json")
	applyClientHeaders(req)
}

// doJSON 发一次带超时的 JSON 请求，返回 (状态码, 响应体)。
func (c *Client) doJSON(ctx context.Context, req *http.Request, timeoutMS int) (int, []byte, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	resp, err := c.httpClient().Do(req.WithContext(cctx))
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// byteReader 把 []byte 包成 io.Reader。
//
// 契约要求 ChatStream.Body 可 Close；bytes.Reader 本身没有 Close，
// 调用方用 io.NopCloser 包一层。
func byteReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}

// saveAuthFile 把凭证原子写回磁盘。
//
// 先写临时文件再 rename：进程在写一半时被杀，rename 之前旧文件仍然完好，
// 不会留下半截 JSON（那会让下次启动解析失败、账号消失）。
func saveAuthFile(a *Auth) error {
	if a == nil || a.FilePath == "" {
		return nil
	}
	raw, err := MarshalAuthFile(a)
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// ── 设备码登录（cline-oauth.ts）───────────────────────────────────────────

// DeviceAuthorization 设备码授权的响应。
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresInMS             int64
	IntervalMS              int64
}

// LoginURL 返回应该给用户打开的地址。
//
// 优先 verification_uri_complete（用户少一步手输）——参照 cline-oauth.ts:200-203。
func (d DeviceAuthorization) LoginURL() string {
	return firstNonEmpty(d.VerificationURIComplete, d.VerificationURI)
}

// StartDeviceAuth 申请设备码（第 1 步）。
//
// body 逐字节等于 `client_id=<workOSClientID>`（无其它字段）。
// device_code / user_code / verification_uri **三字段齐备才算有效响应**。
func (c *Client) StartDeviceAuth(ctx context.Context) (DeviceAuthorization, error) {
	form := url.Values{}
	form.Set("client_id", workOSClientID)

	req, err := http.NewRequest(http.MethodPost, c.workOSBase()+deviceAuthorizationPath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceAuthorization{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	status, body, err := c.doJSON(ctx, req, httpTimeoutMS)
	if err != nil {
		return DeviceAuthorization{}, fmt.Errorf("登录服务返回异常：设备码授权请求失败 - %w", err)
	}
	if status < 200 || status >= 300 {
		return DeviceAuthorization{}, fmt.Errorf("登录服务返回异常：设备码授权失败（HTTP %d） - %s",
			status, describeWorkOSError(body))
	}

	var raw struct {
		DeviceCode              string  `json:"device_code"`
		UserCode                string  `json:"user_code"`
		VerificationURI         string  `json:"verification_uri"`
		VerificationURIComplete string  `json:"verification_uri_complete"`
		ExpiresIn               float64 `json:"expires_in"`
		Interval                float64 `json:"interval"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return DeviceAuthorization{}, fmt.Errorf("登录服务返回异常：设备码授权响应无法解析 - %w", err)
	}
	if raw.DeviceCode == "" || raw.UserCode == "" || raw.VerificationURI == "" {
		return DeviceAuthorization{}, fmt.Errorf("登录服务返回异常：设备码授权响应缺少必要字段")
	}

	expires := int64(deviceAuthExpiresMS)
	if raw.ExpiresIn > 0 {
		expires = int64(raw.ExpiresIn) * 1000
	}
	interval := int64(deviceAuthIntervalMS)
	if raw.Interval > 0 {
		interval = int64(raw.Interval) * 1000
	}
	if interval < pollMinIntervalMS {
		interval = pollMinIntervalMS
	}
	return DeviceAuthorization{
		DeviceCode:              raw.DeviceCode,
		UserCode:                raw.UserCode,
		VerificationURI:         raw.VerificationURI,
		VerificationURIComplete: raw.VerificationURIComplete,
		ExpiresInMS:             expires,
		IntervalMS:              interval,
	}, nil
}

// PollResult 一次轮询的结果。
type PollResult struct {
	// Pending 用户尚未完成授权，调用方应继续轮询。
	Pending bool
	// SlowDown 服务端要求降速（调用方应累积增加间隔）。
	SlowDown bool
	// Token 授权完成时的 WorkOS token（access + refresh）。
	AccessToken  string
	RefreshToken string
}

// PollDeviceAuth 轮询一次（第 2 步）。
//
// # ⚠ 判据在响应体的 error 字段，不是 HTTP 状态码
//
//	authorization_pending → **不是错误**，继续轮询
//	slow_down             → **不是错误**，累积退避后继续
//	access_denied / expired_token / invalid_grant → 终态
//
// 早期若按状态码判失败，会把"用户还没点授权"误报成登录失败
// （参照 cline-oauth.ts:40-43）。这与 Qoder 的"404 表示尚未授权"是同一类
// 语义，但**判据形态完全不同**（Qoder 看状态码，Cline 看响应体）。
func (c *Client) PollDeviceAuth(ctx context.Context, deviceCode string) (PollResult, error) {
	form := url.Values{}
	form.Set("grant_type", deviceGrantType)
	form.Set("device_code", deviceCode)
	form.Set("client_id", workOSClientID)

	req, err := http.NewRequest(http.MethodPost, c.workOSBase()+deviceAuthenticatePath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return PollResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	status, body, err := c.doJSON(ctx, req, httpTimeoutMS)
	if err != nil {
		return PollResult{}, err // 网络失败由调用方计数
	}

	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		Desc         string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &raw)

	switch raw.Error {
	case "authorization_pending":
		return PollResult{Pending: true}, nil
	case "slow_down":
		return PollResult{SlowDown: true}, nil
	case "access_denied", "expired_token", "invalid_grant":
		return PollResult{}, fmt.Errorf("登录服务返回异常：%s", firstNonEmpty(raw.Desc, "WorkOS 授权失败"))
	case "":
		// 无 error 字段：看是不是成功
	default:
		return PollResult{}, fmt.Errorf("登录服务返回异常：WorkOS token 轮询失败（HTTP %d） - %s",
			status, firstNonEmpty(raw.Desc, raw.Error))
	}

	// 2xx 但缺字段 → 视为**服务端异常**，不是"继续等用户"，避免死循环
	// （参照 cline-oauth.ts:256-264）。
	if raw.AccessToken == "" || raw.RefreshToken == "" {
		return PollResult{}, fmt.Errorf("登录服务返回异常：WorkOS token 响应缺少必要字段")
	}
	if status < 200 || status >= 300 {
		return PollResult{}, fmt.Errorf("登录服务返回异常：WorkOS token 轮询失败（HTTP %d） - %s",
			status, firstNonEmpty(raw.Desc, raw.Error))
	}
	return PollResult{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken}, nil
}

// RegisterTokens 用 WorkOS token 换 Cline 自己的 token（第 3 步）。
//
// ⚠ 与 refresh **不同**：本调用点带全部 clientHeaders，且字段名是驼峰。
func (c *Client) RegisterTokens(ctx context.Context, accessToken, refreshToken string) (*Auth, error) {
	payload, _ := json.Marshal(map[string]any{
		"accessToken":  accessToken,
		"refreshToken": refreshToken,
	})
	req, err := http.NewRequest(http.MethodPost, c.apiBase()+registerPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyClientHeaders(req)

	status, body, err := c.doJSON(ctx, req, httpTimeoutMS)
	if err != nil {
		return nil, fmt.Errorf("登录服务返回异常：token 注册请求失败 - %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("登录服务返回异常：token 注册失败（HTTP %d） - %s",
			status, truncate(string(body), 200))
	}

	p, err := parseTokenPayload(body)
	if err != nil {
		return nil, fmt.Errorf("登录服务返回异常：%w", err)
	}
	if p.AccessToken == "" {
		return nil, fmt.Errorf("登录服务返回异常：token 注册响应缺少访问令牌")
	}
	return buildCredential(p, nil), nil
}

// RefreshCredential 续期（参照 cline-auth.ts:352-396）。
//
// # 终态 vs 可重试的分界（决定调度器是否停止续期）
//
//	HTTP 401/403               → ErrRefreshExpired（终态，需重新登录）
//	2xx 但响应缺 token          → ErrRefreshExpired（终态）
//	其它（网络抖动 / 5xx / 429） → 普通 error（可重试）
//
// ⚠ "拿到 200 却没有 token"是**终态**而不是可重试的瞬时故障 ——
// 把它当可重试会让调度器无限重试一个永远不会成功的请求。
func (c *Client) RefreshCredential(ctx context.Context, a *Auth) (*Auth, error) {
	if a == nil || !a.Renewable() {
		return nil, ErrRefreshExpired
	}
	// ⚠ 字段名是驼峰 refreshToken / grantType，不是 OAuth 标准的
	// refresh_token / grant_type。写错时服务端回一个泛化的认证失败，极难定位。
	payload, _ := json.Marshal(map[string]any{
		"refreshToken": a.RefreshToken,
		"grantType":    "refresh_token",
	})
	req, err := http.NewRequest(http.MethodPost, c.apiBase()+refreshPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// ⚠ refresh 调用点**没有** clientHeaders、**没有** Authorization
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	status, body, err := c.doJSON(ctx, req, httpTimeoutMS)
	if err != nil {
		// 网络抖动不该让用户重新登录 → 普通 error（可重试）
		return nil, fmt.Errorf("cline 续期网络失败：%w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, fmt.Errorf("%w（HTTP %d）", ErrRefreshExpired, status)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("cline 续期失败（HTTP %d）：%s", status, truncate(string(body), 200))
	}

	p, perr := parseTokenPayload(body)
	if perr != nil {
		return nil, fmt.Errorf("cline 续期响应不是 JSON（HTTP %d）", status)
	}
	if p.AccessToken == "" {
		return nil, fmt.Errorf("%w：续期响应缺少访问令牌", ErrRefreshExpired)
	}
	next := buildCredential(p, a)
	// ⚠ 续期**不更新 nickname**：buildCredential 已保留 fallback 的值
	return next, nil
}

// ── 模型目录（cline-models.ts）───────────────────────────────────────────

// RecommendedModels recommended-models 的三段内容。
type RecommendedModels struct {
	Recommended []RemoteModelRef
	Free        []RemoteModelRef
	ClinePass   []RemoteModelRef
}

// RemoteModelRef 远端模型条目（只有 id/name/description 可用）。
type RemoteModelRef struct {
	ID          string
	Name        string
	Description string
}

// FetchRecommendedModels 拉 recommended-models（**不需要认证**）。
//
// ⚠ clinePass **不是免费集合**：它是 Cline Pass 订阅制模型，按订阅额度计费。
// 实测 14 个，误判为免费会误导用户。
func (c *Client) FetchRecommendedModels(ctx context.Context) (RecommendedModels, error) {
	req, err := http.NewRequest(http.MethodGet, c.apiBase()+recommendedModelsPath, nil)
	if err != nil {
		return RecommendedModels{}, err
	}
	req.Header.Set("Accept", "application/json")
	applyClientHeaders(req)

	status, body, err := c.doJSON(ctx, req, modelsTimeoutMS)
	if err != nil {
		return RecommendedModels{}, err
	}
	if status < 200 || status >= 300 {
		return RecommendedModels{}, fmt.Errorf("recommended-models HTTP %d", status)
	}

	var raw struct {
		Recommended []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"recommended"`
		Free []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"free"`
		ClinePass []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"clinePass"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return RecommendedModels{}, fmt.Errorf("recommended-models 响应无法解析: %w", err)
	}
	conv := func(in []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}) []RemoteModelRef {
		out := make([]RemoteModelRef, 0, len(in))
		for _, m := range in {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue // 无 id 的项丢弃
			}
			out = append(out, RemoteModelRef{ID: id, Name: m.Name, Description: m.Description})
		}
		return out
	}
	return RecommendedModels{
		Recommended: conv(raw.Recommended),
		Free:        conv(raw.Free),
		ClinePass:   conv(raw.ClinePass),
	}, nil
}

// FetchModelIDs 拉全量模型 id（需认证）。
//
// ⚠ 返回的 460 个 id 里 **cline-free/* 零命中** —— 免费模型只由
// recommended-models 下发。这正是"必须打两个端点"的理由：
// 只调这个端点会看不到任何免费模型。
func (c *Client) FetchModelIDs(ctx context.Context, a *Auth) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, c.apiBase()+modelsPath, nil)
	if err != nil {
		return nil, err
	}
	clineAuthHeaders(req, a.AccessToken)

	status, body, err := c.doJSON(ctx, req, modelsTimeoutMS)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("models HTTP %d", status)
	}
	var raw struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("models 响应无法解析: %w", err)
	}
	out := make([]string, 0, len(raw.Data))
	for _, m := range raw.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// ── 余额（cline-credits.ts）─────────────────────────────────────────────

// BalanceResult 余额查询结果。失败时 Error 非空、Raw 为 0。
type BalanceResult struct {
	Raw   float64
	Error string
}

// FetchBalance 查账户余额。
//
// ⚠ accountId 必须用凭据里的 account_id（usr-…），**不是** JWT 的 sub（user_…）。
// 实测传 sub 返回 400 {"error":"Invalid request format"}。
//
// ⚠ 失败形态有两种，**必须都认**（真实缺陷）：
//
//	{success:false, error:"…"}  业务层失败（HTTP 200）
//	{error:"Unauthorized: …"}   网关层失败（HTTP 401，**没有 success 字段**）
//
// 只判第一种时，401 会落到"响应缺少 data 字段"这个误导性文案，
// 而服务端真正给的原因被丢掉 —— 那正是排查鉴权问题唯一有用的线索。
func (c *Client) FetchBalance(ctx context.Context, a *Auth) BalanceResult {
	if a == nil || a.AccountID == "" {
		return BalanceResult{Error: "凭证缺少 account_id（余额查询必须用它，不是 JWT 的 sub）"}
	}
	u := c.apiBase() + fmt.Sprintf(balancesPathFmt, url.PathEscape(a.AccountID))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return BalanceResult{Error: err.Error()}
	}
	clineAuthHeaders(req, a.AccessToken)

	status, body, err := c.doJSON(ctx, req, creditsTimeoutMS)
	if err != nil {
		return BalanceResult{Error: "余额查询网络失败：" + err.Error()}
	}

	var rec struct {
		Success *bool   `json:"success"`
		Error   string  `json:"error"`
		Data    *struct {
			Balance *float64 `json:"balance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return BalanceResult{Error: fmt.Sprintf("余额响应不是 JSON（HTTP %d）", status)}
	}

	// 先看服务端给的 error 文案 —— HTTP 401 的响应体没有 success 字段
	serverErr := strings.TrimSpace(rec.Error)
	if (rec.Success != nil && !*rec.Success) || (serverErr != "" && (rec.Success == nil || !*rec.Success)) {
		if serverErr != "" {
			return BalanceResult{Error: serverErr}
		}
		return BalanceResult{Error: "服务端返回失败"}
	}
	if status < 200 || status >= 300 {
		if serverErr != "" {
			return BalanceResult{Error: fmt.Sprintf("余额查询失败（HTTP %d）：%s", status, serverErr)}
		}
		return BalanceResult{Error: fmt.Sprintf("余额查询失败（HTTP %d）", status)}
	}
	if rec.Data == nil {
		return BalanceResult{Error: "响应缺少 data 字段"}
	}
	if rec.Data.Balance == nil {
		return BalanceResult{Error: "响应缺少 balance 字段"}
	}
	return BalanceResult{Raw: *rec.Data.Balance}
}

// Total 把原始余额换算成展示值（÷ balanceScale）。
//
// ⚠ 不把"查不到"显示成 0（0 是"已用光"的语义），调用方应据 Error 区分。
func (r BalanceResult) Total() float64 {
	return round2(r.Raw / balanceScale)
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ── 推理（cline-adapter.ts）─────────────────────────────────────────────

// ChatStream 发一次流式对话。
//
// 返回 (流, 状态码, 错误体, error)。语义与其它上游对齐：
// status 非 2xx 时仍返回流（把上游错误体带回来给调用方判断），
// 只有网络层失败才返回 error。
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.apiBase()+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	clineAuthHeaders(req, a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	// 本适配器只支持 SSE（上游仅流式）
	req.Header.Set("Accept", "text/event-stream")

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

// VerifyToken 用 /users/me 验证令牌是否有效。
func (c *Client) VerifyToken(ctx context.Context, a *Auth) error {
	req, err := http.NewRequest(http.MethodGet, c.apiBase()+mePath, nil)
	if err != nil {
		return err
	}
	clineAuthHeaders(req, a.AccessToken)
	status, body, err := c.doJSON(ctx, req, httpTimeoutMS)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("cline: 令牌校验 HTTP %d: %s", status, truncate(string(body), 120))
	}
	return nil
}

// describeWorkOSError 从 WorkOS 错误体里取 error_description。
func describeWorkOSError(body []byte) string {
	var raw struct {
		Error string `json:"error"`
		Desc  string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &raw)
	return strings.TrimSpace(firstNonEmpty(raw.Desc, raw.Error, truncate(string(body), 120)))
}
