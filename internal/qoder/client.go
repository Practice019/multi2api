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
	"net/url"
	"strconv"
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

// SignIdentity 加密签名所需的账号身份。
//
// # 为什么用**独立结构体**而不是直接传 *Auth
//
// 实现方（internal/qoderwasm）**不能 import 本包** ——
// 本包依赖 internal/gateway，而 arch_test 的 discoverUpstreams 用的是
// `go list -deps`（**传递**依赖）：一旦 qoderwasm 依赖本包，
// 它就会被判成一个"消费契约"的上游，而它没有 Provider 方法 → 判据红。
//
// 所以这里用**纯标量**把身份传过去，实现方不需要认识本包的任何类型。
// 适配由装配层（cmd/server）做 —— 那里可以同时 import 两边。
type SignIdentity struct {
	// UID 账号主键（进 Cosy-User 与鉴权字段）。
	UID string
	// AccessToken 访问令牌（WASM 用它算 encrypt_user_info）。
	AccessToken string
	// MachineID 机器标识（进 Cosy-MachineId / Cosy-MachineToken）。
	MachineID string
}

// RequestSigner 把一次推理请求编成加密端点的 (URL, 请求体, 签名头)。
//
// 这是 WASM 与网络层的接缝：WASM 部分实现它，本文件只负责把它产出的
// 东西发出去。这样"加密怎么做"与"怎么发请求"各自可独立测试。
type RequestSigner interface {
	// BuildInferRequest 构造加密推理请求。
	//
	// id 是**本账号**的身份：签名与鉴权字段都由它派生，
	// 所以同一个 Signer 实例可以服务多个账号（实现方自己缓存）。
	//
	// 返回 (URL 的 query 部分, 请求体字节, 额外请求头, error)。
	BuildInferRequest(id SignIdentity, model string, body []byte) (path string, payload []byte, headers map[string]string, err error)
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

// DeviceSession 一次设备码登录会话。
//
// # 三个字段缺一不可（本轮修的缺陷就是"只带了 state"）
//
//	Pkce       授权时提交 challenge，轮询时提交 verifier —— 服务端据此配对
//	Nonce      一次性随机串，授权与轮询两处必须是**同一个**
//	MachineID  设备标识，授权 URL 要带；且**必须随凭据持久化**
//	           （续期请求体也要它，见 RefreshCredential）
//
// 参照实现的 createQoderDeviceSession 也是这三个字段，
// 其中 machineId 由客户端生成并持久化（不是硬件指纹）。
type DeviceSession struct {
	Pkce      Pkce
	Nonce     string
	MachineID string
}

// newDeviceSession 生成一次设备码登录会话。
func newDeviceSession() (DeviceSession, error) {
	p, err := newPkce()
	if err != nil {
		return DeviceSession{}, err
	}
	nonce, err := randomNonce()
	if err != nil {
		return DeviceSession{}, err
	}
	// machine_id 与 nonce 同形（16 字节 hex）。参照用 UUID；
	// 两者都是"客户端生成的随机标识"，服务端不做格式校验
	//（参照的注释明确说它不复制 Qoder 的硬件指纹逻辑）。
	machineID, err := randomNonce()
	if err != nil {
		return DeviceSession{}, err
	}
	return DeviceSession{Pkce: p, Nonce: nonce, MachineID: machineID}, nil
}

// ── 设备码登录（qoder-oauth.ts）─────────────────────────────────────────
//
// # ⚠ 本轮修的缺陷：此前**完全没有 PKCE**
//
// 授权 URL 只带了 `client_id` / `response_type=code` / `state`，
// 轮询也只提交一个 `state`。而参照实现（已实测跑通）是标准 PKCE：
//
//	授权 URL  {authBase}/device/selectAccounts
//	          ?challenge=<base64url(sha256(verifier))>&challenge_method=S256
//	          &nonce=<uuid>&machine_id=<uuid>&client_id=<product.ClientID>
//
//	轮询      **GET** {openAPIBase}/api/v1/deviceToken/poll
//	          ?nonce=<同一个>&verifier=<明文 verifier>&challenge_method=S256
//
// 服务端靠 `verifier` 校验授权时提交的 `challenge`。不发 PKCE 时，
// 授权页照常打开、用户照常点授权，但服务端没有可校验的东西 ——
// **永远拿不到 token**，直到 5 分钟超时。用户报的"添加账号有问题"就是这个。
//
// 三个细节见 pkce.go 的文件头（长度、去 padding、GET 而非 POST）。

// DeviceSelectURL 返回设备码授权入口 URL（用户在浏览器里打开它）。
//
// # 形态（逐字照抄参照 buildQoderAuthUrl）
//
//	{authBase}/device/selectAccounts
//	  ?challenge=<pkce.Challenge>&challenge_method=S256
//	  &nonce=<session.Nonce>&machine_id=<session.MachineID>
//	  &client_id=<product.ClientID>
//
// ⚠ `client_id` 必须用 **prod** 那个（Qoder.ClientID）。
// 参照的注释写明：用错成 test 的 id 时，"授权页 302 正常、点击授权后
// 报参数无效" —— 故**不能**靠探测入口验证，只有真实登录闭环才暴露。
//
// ⚠ 该路径对**任一** client_id（含全零 UUID）都返回 302 ——
// 故 GET 它的状态码**不能**用来校验 client_id。
// 这也是为什么本函数是纯 URL 构造、不发请求：发一次 GET 也验不出什么。
func (c *Client) DeviceSelectURL(s *DeviceSession) string {
	if s == nil {
		return ""
	}
	q := []string{
		"challenge=" + url.QueryEscape(s.Pkce.Challenge),
		"challenge_method=S256",
		"nonce=" + url.QueryEscape(s.Nonce),
		"machine_id=" + url.QueryEscape(s.MachineID),
		"client_id=" + url.QueryEscape(c.Product.ClientID),
	}
	return c.authBase() + deviceSelectPath + "?" + strings.Join(q, "&")
}

// DevicePollURL 轮询取 token 的完整 URL（**GET** + query 参数）。
//
// # 为什么是 GET 而不是 POST（本轮修的缺陷之一）
//
// 参照实现：`fetcher(pollUrl, { method: 'GET' })`，参数全在 query。
// 我们此前发的是 POST + JSON body（只有 state / client_id）——
// 服务端既拿不到 verifier（PKCE 校验失败），方法也不对。
//
// ⚠ 挂的是 **openAPIBase**，不是 authBase：
// 实测 `qoder.com` 的同名路径返回 401，而 `openapi.qoder.sh` 返回 404
// （= 无待授权会话，应继续轮询）。写错 host 会让登录永远失败。
func (c *Client) DevicePollURL(s *DeviceSession) string {
	if s == nil {
		return ""
	}
	q := []string{
		"nonce=" + url.QueryEscape(s.Nonce),
		"verifier=" + url.QueryEscape(s.Pkce.Verifier),
		"challenge_method=S256",
	}
	return c.openAPI() + devicePollPath + "?" + strings.Join(q, "&")
}

// PollResult 一次轮询的结果。
type PollResult struct {
	// Pending 用户尚未完成授权（HTTP 404 / 2xx 但无 token），继续轮询。
	Pending bool
	// AccessToken / RefreshToken 授权完成时的令牌。
	AccessToken  string
	RefreshToken string
	// UID / Nickname 设备码响应里的 user_id / user_name。
	//
	// ⚠ 必须读出来：**加密推理需要 uid**（WASM 用它派生 encrypt_user_info）。
	// 早期漏读导致只能走公开端点 —— 而公开端点不认目录 key（见 qoder.go）。
	UID      string
	Nickname string
}

// PollDeviceToken 轮询一次设备码（**GET**，参数在 query）。
//
// # ⚠ 判据是 **HTTP 404**，不是响应体
//
// 实测依据：该端点返回 404 而任意不存在的路径返回 401 ——
// 说明它被网关豁免认证、由业务层报「会话未就绪」。
//
// 轮询 host 是 **openapi.qoder.sh**（qoder.com 的同名路径返回 401）。
//
// 业务码 11217 / 12151（token/account not ready）同样表示"继续等"。
//
// # ⚠ 2xx 但无 token 也要继续（不能当失败）
//
// 参照实现的判据是 `payload.accessToken.length > 0` 才返回；
// 否则 sleep 后重试。把它当错误会让"刚点完授权、服务端还在处理"
// 这个正常中间态变成失败。
func (c *Client) PollDeviceToken(ctx context.Context, s *DeviceSession) (PollResult, error) {
	req, err := http.NewRequest(http.MethodGet, c.DevicePollURL(s), nil)
	if err != nil {
		return PollResult{}, err
	}
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
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		UserName     string `json:"user_name"`
		Data         struct {
			Token        string `json:"token"`
			DeviceToken  string `json:"device_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			UserID       string `json:"user_id"`
			UserName     string `json:"user_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return PollResult{}, fmt.Errorf("qoder: 轮询响应无法解析: %w", err)
	}
	// 业务码「未就绪」也继续轮询
	if rec.Code == codeTokenNotReady || rec.Code == codeAccountNotReady {
		return PollResult{Pending: true}, nil
	}
	// ⚠ 字段名有四种（参照 parseQoderTokenPayload 逐字照抄）：
	//   登录响应用 `token` / `device_token`，续期响应用 `access_token`。
	// 只认 access_token 会让**登录响应永远解析不出 token** →
	// 表现同样是"授权成功但一直 pending 到超时"。
	at := firstNonEmpty(rec.Token, rec.DeviceToken, rec.AccessToken,
		rec.Data.Token, rec.Data.DeviceToken, rec.Data.AccessToken)
	rt := firstNonEmpty(rec.RefreshToken, rec.Data.RefreshToken)
	uid := firstNonEmpty(rec.UserID, rec.Data.UserID)
	nick := firstNonEmpty(rec.UserName, rec.Data.UserName)
	if at == "" {
		return PollResult{Pending: true}, nil // 无 token 视为未就绪（不产出半截凭据）
	}
	return PollResult{AccessToken: at, RefreshToken: rt, UID: uid, Nickname: nick}, nil
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

	// ⚠ 字段名与形状**逐字照抄参照 parseQoderTokenPayload**（本轮修的缺陷）。
	//
	// 实测真实续期响应（2026-09-30）：
	//
	//	{"device_token":"dt-9PpG1q39MXpdm8CCYa6smmXl",
	//	 "refresh_token":"drt-EEpc518l3XX9AdEMCVMcvZ2y",
	//	 "token_type":"Bearer",
	//	 "expires_at":"2026-10-30T06:56:55Z",          ← ISO 绝对时刻
	//	 "refresh_token_expires_at":"2027-09-25T06:56:55Z",
	//	 "created_at":"2026-09-30T06:56:55Z"}
	//
	// 而旧实现读的是 `access_token` + `expires_in`（相对秒）—— **两个都不存在**：
	//
	//	access_token 不存在 → at=="" → 报"续期响应缺少 access_token"
	//	                       （即"续期永远失败"，即使服务端回了 200）
	//	expires_in   不存在 → 过期时刻从来没被存下来 → 界面 Token 列恒显示 `—`
	//
	// token 本身是 **`dt-` 前缀的不透明串**（27 字符，不是 JWT），
	// 所以"解 JWT exp"那条路也走不通 —— 过期时刻**只能**从这里存。
	var rec struct {
		Code         int    `json:"code"`
		Message      string `json:"message"`
		// 登录响应用 `token`、续期响应用 `device_token`，两者都认（参照同）。
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		// ExpiresAt 是 **ISO 字符串**（不是秒数），见 parseQoderTimeMS。
		ExpiresAt string `json:"expires_at"`
		// RefreshTokenExpiresAt refresh_token 的过期时刻（ISO 字符串）。
		RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
		Data                  struct {
			Token        string `json:"token"`
			DeviceToken  string `json:"device_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresAt    string `json:"expires_at"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &rec)

	at := firstNonEmpty(rec.Token, rec.DeviceToken, rec.AccessToken,
		rec.Data.Token, rec.Data.DeviceToken, rec.Data.AccessToken)
	rt := firstNonEmpty(rec.RefreshToken, rec.Data.RefreshToken)
	expISO := firstNonEmpty(rec.ExpiresAt, rec.Data.ExpiresAt)
	if at == "" {
		return nil, fmt.Errorf("%w：续期响应缺少访问令牌", ErrRefreshExpired)
	}

	next := *a
	next.AccessToken = at
	if rt != "" {
		next.RefreshToken = rt
	}
	// 过期时刻存**绝对毫秒**（parseQoderTimeMS 兼容 ISO 字符串与秒/毫秒数字）。
	//
	// ⚠ 不存"相对秒数"：相对值一离开响应就没有参照点了 ——
	// 存下来再读只会得到"签发时的那一刻剩余多久"，与现在无关。
	if ms := parseQoderTimeMS(expISO); ms > 0 {
		next.ExpiresAt = ms
	}
	if ms := parseQoderTimeMS(rec.RefreshTokenExpiresAt); ms > 0 {
		next.RefreshTokenExpiresAt = ms
	}
	// machine_id 必须保留（续期请求体要用）
	return &next, nil
}

// parseQoderTimeMS 把上游的时间值解析成**毫秒时间戳**；解不出返回 0。
//
// 兼容两种形态（与参照 readTimestamp 逐字对齐）：
//
//	ISO 字符串  "2026-10-30T06:56:55Z"   ← 实测续期响应用的就是它
//	数字        10 位视为秒、13 位视为毫秒
//
// ⚠ **解不出返回 0，绝不填当前时间**：「没有过期时间」与「刚过期」
// 是两回事，后者会让界面显示"已过期"并误导用户去重新登录。
func parseQoderTimeMS(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	// 纯数字：按位数判秒/毫秒
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		if n < 1e12 {
			return n * 1000
		}
		return n
	}
	// ISO 8601 / RFC3339
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli()
	}
	// 秒带小数点的 ISO（如 "2026-10-30T06:56:55.123Z"）由 RFC3339 覆盖；
	// 这里再兜一次不带时区的形态。
	if t, err := time.Parse("2006-01-02T15:04:05", v); err == nil {
		return t.UnixMilli()
	}
	return 0
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
	path, payload, headers, err := c.Signer.BuildInferRequest(SignIdentity{
		UID:         a.UIDValue(),
		AccessToken: a.AccessToken,
		MachineID:   a.MachineID,
	}, model, body)
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
	// ⚠ 必须解包：加密端点的每帧都多一层信封（见 envelope.go 的文件头）。
	// 不解包时下游看到的每帧都没有 choices 字段 —— 表现为**静默无输出**
	//（流正常结束、不报错、用户什么都没看到，是最坏的一类故障）。
	return unwrapEnvelopeStream(resp.Body), resp.StatusCode, nil, nil
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
