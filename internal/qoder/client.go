// client.go Qoder 的 HTTP 出口层。
//
// 对应参照项目：
//
//	qoder-oauth.ts   → 设备码轮询 / 续期 / 用户信息
//	qoder-auth.ts    → 模型目录（我们用静态表，不发请求）
//	qoder-adapter.ts → 聊天转发
package qoder

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

// Client Qoder 的 HTTP 客户端。
type Client struct {
	HTTP *http.Client
	// Product 本实例对应的产品配置。
	Product Product
	// Signer 加密推理的请求构造器（由 wasm 层实现）。
	//
	// nil = 未接 WASM → 加密推理不可用（Chat 会返回明确错误，
	// 而不是静默走公开端点 —— 那条路不认目录 key，必然失败）。
	Signer RequestSigner
}

// RequestSigner 把一次推理请求编成加密端点的 (URL, 请求体, 签名头)。
//
// 这是 WASM 与网络层的接缝：WASM 部分（wasm*.go）实现它，
// 本文件只负责把它产出的东西发出去。这样"加密怎么做"与"怎么发请求"
// 各自可独立测试。
type RequestSigner interface {
	// BuildInferRequest 构造加密推理请求。
	//
	// 返回 (完整 URL 路径的 query 部分, 请求体字节, 额外请求头, error)。
	BuildInferRequest(model string, body []byte) (path string, payload []byte, headers map[string]string, err error)
}

// New 生产默认值（国际版）。
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 0}, Product: Qoder}
}

// NewWithProduct 按产品构造。
func NewWithProduct(p Product) *Client {
	return &Client{HTTP: &http.Client{Timeout: 0}, Product: p}
}

// NewWithBase 测试构造（所有基址指向同一个假上游）。
func NewWithBase(base string) *Client {
	p := Qoder
	p.AuthBase = base
	p.OpenAPIBase = base
	p.InferBase = base
	p.EncryptedInferBase = base
	p.SashBase = base
	return &Client{HTTP: &http.Client{Timeout: 0}, Product: p}
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: requestTimeoutMS * time.Millisecond}
}

func (c *Client) openAPI() string {
	if c != nil && c.Product.OpenAPIBase != "" {
		return strings.TrimRight(c.Product.OpenAPIBase, "/")
	}
	return Qoder.OpenAPIBase
}

func (c *Client) authBase() string {
	if c != nil && c.Product.AuthBase != "" {
		return strings.TrimRight(c.Product.AuthBase, "/")
	}
	return Qoder.AuthBase
}

func (c *Client) encryptedBase() string {
	if c != nil && c.Product.EncryptedInferBase != "" {
		return strings.TrimRight(c.Product.EncryptedInferBase, "/")
	}
	return Qoder.EncryptedInferBase
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// do 发一次请求。
func (c *Client) do(ctx context.Context, req *http.Request, timeoutMS int) (int, []byte, error) {
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

// userAgent 出站 UA。
func (c *Client) userAgent() string {
	prefix := Qoder.UserAgentPrefix
	if c != nil && c.Product.UserAgentPrefix != "" {
		prefix = c.Product.UserAgentPrefix
	}
	return prefix + "/1.0.0"
}

// ── 设备码登录（qoder-oauth.ts）─────────────────────────────────────────

// DeviceSelectURL 返回设备码授权入口 URL（用户在浏览器里打开它）。
//
// ⚠ 该路径对**任一** client_id（含全零 UUID）都返回 302 ——
// 故 GET 它的状态码**不能**用来校验 client_id。
// client_id 的错误要到**授权回调阶段**才被服务端校验出来。
//
// 这也是为什么本函数是纯 URL 构造、不发请求：发一次 GET 也验不出什么。
func (c *Client) DeviceSelectURL(state string) string {
	// 路径与查询参数按参照实现拼装。
	q := []string{
		"client_id=" + c.Product.ClientID,
		"response_type=code",
		"state=" + state,
	}
	for k, v := range clientMetadata {
		q = append(q, k+"="+v)
	}
	return c.authBase() + deviceSelectPath + "?" + strings.Join(q, "&")
}

// PollResult 一次轮询的结果。
type PollResult struct {
	// Pending 用户尚未完成授权（HTTP 404），继续轮询。
	Pending bool
	// AccessToken / RefreshToken 授权完成时的令牌。
	AccessToken  string
	RefreshToken string
}

// PollDeviceToken 轮询一次设备码。
//
// # ⚠ 判据是 **HTTP 404**，不是响应体
//
// 实测依据：该端点返回 404 而任意不存在的路径返回 401 ——
// 说明它被网关豁免认证、由业务层报「会话未就绪」。
//
// 轮询 host 是 **openapi.qoder.sh**（qoder.com 的同名路径返回 401）。
//
// 业务码 11217 / 12151（token/account not ready）同样表示"继续等"。
func (c *Client) PollDeviceToken(ctx context.Context, state string) (PollResult, error) {
	payload, _ := json.Marshal(map[string]any{
		"state":     state,
		"client_id": c.Product.ClientID,
	})
	req, err := http.NewRequest(http.MethodPost, c.openAPI()+devicePollPath, bytes.NewReader(payload))
	if err != nil {
		return PollResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return PollResult{}, err // 网络失败由调用方计数
	}
	if status == http.StatusNotFound {
		return PollResult{Pending: true}, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return PollResult{}, fmt.Errorf("qoder: 轮询被拒绝（HTTP %d）", status)
	}
	if status < 200 || status >= 300 {
		return PollResult{}, fmt.Errorf("qoder: 轮询 HTTP %d: %s", status, truncate(string(body), 160))
	}

	var rec struct {
		Code         int    `json:"code"`
		Message      string `json:"message"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Data         struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return PollResult{}, fmt.Errorf("qoder: 轮询响应无法解析: %w", err)
	}
	// 业务码「未就绪」也继续轮询
	if rec.Code == codeTokenNotReady || rec.Code == codeAccountNotReady {
		return PollResult{Pending: true}, nil
	}
	at := rec.AccessToken
	rt := rec.RefreshToken
	if at == "" {
		at = rec.Data.AccessToken
	}
	if rt == "" {
		rt = rec.Data.RefreshToken
	}
	if at == "" {
		return PollResult{Pending: true}, nil // 无 token 视为未就绪（不产出半截凭据）
	}
	return PollResult{AccessToken: at, RefreshToken: rt}, nil
}

// ── 续期 ────────────────────────────────────────────────────────────────

// RefreshCredential 续期一份凭证。
//
// ⚠ 请求体**必须带 machine_id** —— 这是 Qoder 特有的要求
//（多数 OAuth 实现不带机器标识）。丢了它续期会被服务端拒绝。
func (c *Client) RefreshCredential(ctx context.Context, a *Auth) (*Auth, error) {
	if a == nil || !a.Renewable() {
		return nil, ErrRefreshExpired
	}
	body := map[string]any{
		"refresh_token": a.RefreshToken,
		"client_id":     c.Product.ClientID,
	}
	if a.MachineID != "" {
		body["machine_id"] = a.MachineID
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.openAPI()+refreshPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	status, raw, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return nil, err // 网络失败：可重试
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, ErrRefreshExpired
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("qoder: 续期 HTTP %d: %s", status, truncate(string(raw), 160))
	}

	var rec struct {
		Code         int    `json:"code"`
		Message      string `json:"message"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Data         struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &rec)

	at := firstNonEmpty(rec.AccessToken, rec.Data.AccessToken)
	rt := firstNonEmpty(rec.RefreshToken, rec.Data.RefreshToken)
	exp := rec.ExpiresIn
	if exp == 0 {
		exp = rec.Data.ExpiresIn
	}
	if at == "" {
		return nil, fmt.Errorf("%w：续期响应缺少 access_token", ErrRefreshExpired)
	}

	next := *a
	next.AccessToken = at
	if rt != "" {
		next.RefreshToken = rt
	}
	if exp > 0 {
		next.ExpiresIn = exp
	}
	// machine_id 必须保留（续期请求体要用）
	return &next, nil
}

// ── 用户信息 ────────────────────────────────────────────────────────────

// UserInfo 账号信息。
type UserInfo struct {
	UID      string
	Nickname string
}

// FetchUserInfo 查用户信息（失败返回零值，不影响主流程）。
func (c *Client) FetchUserInfo(ctx context.Context, a *Auth) UserInfo {
	req, err := http.NewRequest(http.MethodGet, c.openAPI()+userInfoPath, nil)
	if err != nil {
		return UserInfo{}
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil || status < 200 || status >= 300 {
		return UserInfo{}
	}
	var rec struct {
		UID      string `json:"uid"`
		ID       string `json:"id"`
		Nickname string `json:"nickname"`
		Name     string `json:"name"`
		Data     struct {
			UID      string `json:"uid"`
			ID       string `json:"id"`
			Nickname string `json:"nickname"`
			Name     string `json:"name"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &rec)
	return UserInfo{
		UID:      firstNonEmpty(rec.UID, rec.ID, rec.Data.UID, rec.Data.ID),
		Nickname: firstNonEmpty(rec.Nickname, rec.Name, rec.Data.Nickname, rec.Data.Name),
	}
}

// ── 推理 ────────────────────────────────────────────────────────────────

// ErrNoSigner 未接 WASM 签名器 —— 加密推理不可用。
//
// ⚠ **刻意不回落公开端点**：公开端点不认目录 key（`qfmodel` 等一律
// `Unsupported model`），回落只会把"加密不可用"伪装成"模型不存在"，
// 让排查方向完全跑偏。明确报错比静默降级诚实。
var ErrNoSigner = fmt.Errorf("qoder: 未接入 WASM 签名器，无法构造加密推理请求" +
	"（公开端点不认目录 key，故不回落）")

// ChatStream 发一次流式对话（走**加密端点**）。
//
// ⚠ 必须走加密端点：目录 key（qfmodel / dmodel 等）只有这条路能用。
//
// ⚠ 中国版**没有**可用的公开端点（gateway/openapi.qoder.com.cn 的
// /model/v1/chat/completions 都回 503）—— 对中国版更是只有这一条路。
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	if c == nil || c.Signer == nil {
		return nil, 0, nil, ErrNoSigner
	}
	model := modelOf(body)
	path, payload, headers, err := c.Signer.BuildInferRequest(model, body)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("qoder: 构造加密请求失败: %w", err)
	}
	url := c.encryptedBase() + encryptedInferPath
	if path != "" {
		url += "?" + strings.TrimPrefix(path, "?")
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", c.userAgent())
	if a != nil {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	}
	// ⚠ 签名头**必须原样透传**，用普通 Bearer 覆盖会被判签名无效。
	for k, v := range headers {
		req.Header.Set(k, v)
	}

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

// modelOf 从请求体里取 model 字段（不解析时返回空串）。
func modelOf(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &obj)
	return obj.Model
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
