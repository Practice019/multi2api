// client.go MonkeyCode 的 HTTP 面：三组端点，三套认证。
//
// # 为什么一个上游要碰两个域名
//
//	网页/管理端点  <host>          Cookie: monkeycode_ai_session=…
//	模型网关       <proxyBase>    Bearer + X-OhMyAgent-Signature
//
// 两者**不共用认证**：session 换不了网关的通行证，网关也不认 cookie。
// 所以 Chat 走 proxyBase，Models/Quota/Checkin 走 host。
//
// # 区域
//
//	cn     host=https://monkeycode-ai.com      proxy=https://proxy.monkeycode-ai.com
//	intl   host=https://monkeycode-ai.net      proxy=https://proxy.monkeycode-ai.net
//
// 两站的**模型目录不同**（国内 45 / 国际 11），选错基址直接 404。
package monkeycode

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 站点基址。
const (
	hostCN        = "https://monkeycode-ai.com"
	hostIntl      = "https://monkeycode-ai.net"
	proxyBaseCN   = "https://proxy.monkeycode-ai.com/v1"
	proxyBaseIntl = "https://proxy.monkeycode-ai.net/v1"
)

// 端点路径。
const (
	pathStatus    = "/api/v1/users/status"
	pathModels    = "/api/v1/users/models"
	pathWallet    = "/api/v1/users/wallet"
	pathCheckin   = "/api/v1/users/wallet/checkin"
	pathAPICreate = "/api/v1/users/ohmyagent/api-keys"
	pathResponses = "/responses"
	pathMessages  = "/messages"
)

// 协议类型（模型目录里 interface_type 的取值）。
const (
	// ifaceResponses OpenAI Responses（/v1/responses）。
	ifaceResponses = "openai_responses"
	// ifaceAnthropic Anthropic Messages（/v1/messages）。
	ifaceAnthropic = "anthropic"
)

// defaultUserAgent 与官方客户端同款（部分端点会看它）。
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// Client MonkeyCode HTTP 客户端。
//
// # 为什么持有**两个** http.Client
//
// 两站的网络前提**相反**：
//
//	国内站 (cn)    必须**直连** —— 它在国内，走海外代理既慢又触发风控
//	国际站 (intl)  必须**走代理** —— 否则连不上
//
// 踩过的坑（真机实测）：全局配了一个 proxy，国内站请求直接
// `Post "https://proxy.monkeycode-ai.com/v1/messages": EOF`，
// 表现为「账号不健康」——错误信息完全指不到代理上。
//
// 所以这里**按凭证的 region 选**，而不是全局一个。
type Client struct {
	direct    *http.Client // 国内站
	proxied   *http.Client // 国际站
	userAgent string

	// Proxy 国际站出口（记下来仅供日志/自检显示）。
	Proxy string

	// DirectProxy 允许强制国内站也走代理（默认空 = 直连）。
	//
	// ⚠ 只用于「本机无法直连国内站」这类环境；正常部署**不要**设。
	DirectProxy string
}

// httpFor 按凭证区域选 http.Client。
func (c *Client) httpFor(a *Auth) *http.Client {
	if a.RegionOrDefault() == RegionIntl {
		return c.proxied
	}
	return c.direct
}

// NewClient 构造客户端（国际站直连 —— 仅测试用）。
//
// ⚠ 刻意叫 NewClient 而不是 New：本包的 New 是 Provider 构造函数，
// 同名会编译冲突（AGENTS.md §9 第一条就是这个坑）。
func NewClient() *Client { return NewClientWithProxy("") }

// newTransport 造一个 Transport（proxy 为空 = 直连）。
func newTransport(proxy string) *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if fn := proxyFunc(p); fn != nil {
			tr.Proxy = fn
		}
	}
	return tr
}

// NewClientWithProxy 构造客户端：国内站直连，国际站走 proxy。
//
// ⚠ **不设 http.Client.Timeout**：流式对话可能很长，超时靠 ctx 控制。
func NewClientWithProxy(proxy string) *Client {
	proxy = strings.TrimSpace(proxy)
	return &Client{
		direct:    &http.Client{Transport: newTransport("")},
		proxied:   &http.Client{Transport: newTransport(proxy)},
		userAgent: defaultUserAgent,
		Proxy:     proxy,
	}
}

// ---- 管理端点（Cookie 认证）----

// sessionHeaders 管理端点的请求头。
func sessionHeaders(a *Auth) http.Header {
	h := http.Header{}
	h.Set("Cookie", "monkeycode_ai_session="+strings.TrimSpace(a.Session))
	h.Set("Accept", "application/json")
	return h
}

// doJSON 发一个 JSON 请求，返回 (状态码, 原始体)。
//
// 非 2xx **不**当错误返回 —— 把原文交出去，由调用方按业务语义判。
// 只有网络层失败才返回 error（与 gateway 的契约一致）。
func (c *Client) doJSON(ctx context.Context, a *Auth, method, rawURL string, headers http.Header, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return 0, nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpFor(a).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// apiEnvelope 上游管理端点的统一信封 {"code":0,"message":"success","data":…}。
type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// unwrap 解信封，取出 data。
//
// 有的端点（/users/status 是直接给 data）没有信封，此时原样返回。
func unwrap(raw []byte) (json.RawMessage, error) {
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw, nil
	}
	if env.Code != 0 && env.Message != "" {
		return nil, fmt.Errorf("monkeycode: 上游返回 %d：%s", env.Code, env.Message)
	}
	if len(env.Data) == 0 {
		return raw, nil
	}
	return env.Data, nil
}

// StatusPayload GET /users/status 的 data。
type StatusPayload struct {
	User struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
		Status    string `json:"status"`
		IsBlocked bool   `json:"is_blocked"`
	} `json:"user"`
}

// FetchStatus 查账号信息。
//
// 用途有二：① 导入时校验 session 有效 ② 取 user.id / name 落盘。
func (c *Client) FetchStatus(ctx context.Context, a *Auth) (*StatusPayload, error) {
	code, raw, err := c.doJSON(ctx, a, http.MethodGet, a.Host()+pathStatus, sessionHeaders(a), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("monkeycode: status 返回 %d：%s", code, truncate(raw, 200))
	}
	body, err := unwrap(raw)
	if err != nil {
		return nil, err
	}
	var p StatusPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("monkeycode: 解析 status 失败：%w", err)
	}
	return &p, nil
}

// ModelEntry 模型目录里的一条。
type ModelEntry struct {
	Model         string `json:"model"`
	AccessLevel   string `json:"access_level"`
	InterfaceType string `json:"interface_type"`
	IsFree        bool   `json:"is_free"`
	IsHidden      bool   `json:"is_hidden"`
	ContextLimit  int    `json:"context_limit"`
	OutputLimit   int    `json:"output_limit"`
	ThinkingOn    bool   `json:"thinking_enabled"`
	SupportImage  bool   `json:"support_image"`
}

// ModelsPayload GET /users/models 的 data。
type ModelsPayload struct {
	Models []ModelEntry `json:"models"`
}

// FetchModels 拉远端模型目录。
//
// ⚠ 返回的是**全量**（含 is_hidden）：隐藏只控制官方 UI 展示，
// 不代表不能用 —— 实测 monkeycode-basic/deepseek-v4-flash 标着 hidden
// 但 basic 号能跑。所以这里**不**过滤，让调用方决定。
func (c *Client) FetchModels(ctx context.Context, a *Auth) ([]ModelEntry, error) {
	code, raw, err := c.doJSON(ctx, a, http.MethodGet, a.Host()+pathModels+"?limit=200", sessionHeaders(a), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("monkeycode: models 返回 %d：%s", code, truncate(raw, 200))
	}
	body, err := unwrap(raw)
	if err != nil {
		return nil, err
	}
	var p ModelsPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("monkeycode: 解析 models 失败：%w", err)
	}
	return p.Models, nil
}

// WalletPayload GET /users/wallet 的 data。
type WalletPayload struct {
	Balance           int64 `json:"balance"`
	DailyTokenBalance int64 `json:"daily_token_balance"`
	DailyTokenLimit   int64 `json:"daily_token_limit"`
}

// FetchWallet 查额度。
func (c *Client) FetchWallet(ctx context.Context, a *Auth) (*WalletPayload, error) {
	code, raw, err := c.doJSON(ctx, a, http.MethodGet, a.Host()+pathWallet, sessionHeaders(a), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("monkeycode: wallet 返回 %d：%s", code, truncate(raw, 200))
	}
	body, err := unwrap(raw)
	if err != nil {
		return nil, err
	}
	var p WalletPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("monkeycode: 解析 wallet 失败：%w", err)
	}
	return &p, nil
}

// APIKeyPayload 签发接口的 data。
type APIKeyPayload struct {
	ID            string `json:"id"`
	APIKey        string `json:"api_key"`
	SigningSecret string `json:"signing_secret"`
	CreatedAt     int64  `json:"created_at"`
}

// CreateAPIKey 用 session 签一把模型网关的 key。
//
// ⚠ **可重复调用**：实测同一账号能签任意多把，旧的不会失效。
// 所以调用方要**先查本地是否已有**，别每次启动都签一把（会越堆越多）。
func (c *Client) CreateAPIKey(ctx context.Context, a *Auth, name string) (*APIKeyPayload, error) {
	if strings.TrimSpace(name) == "" {
		name = "multi2api"
	}
	payload, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	h := sessionHeaders(a)
	h.Set("Origin", a.Host())
	h.Set("Referer", a.Host()+"/console/tasks")
	code, raw, err := c.doJSON(ctx, a, http.MethodPost, a.Host()+pathAPICreate, h, payload)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return nil, fmt.Errorf("monkeycode: 签发 key 返回 %d：%s", code, truncate(raw, 200))
	}
	body, err := unwrap(raw)
	if err != nil {
		return nil, err
	}
	var p APIKeyPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("monkeycode: 解析签发结果失败：%w", err)
	}
	if strings.TrimSpace(p.APIKey) == "" || strings.TrimSpace(p.SigningSecret) == "" {
		return nil, fmt.Errorf("monkeycode: 签发结果缺 api_key 或 signing_secret")
	}
	return &p, nil
}

// ---- 模型网关 ----

// ChatRequest 一次对话要发的东西。调用方负责把 OpenAI 请求转成目标协议。
type ChatRequest struct {
	// Interface openai_responses 或 anthropic。
	Interface string
	// Payload 已序列化的请求体。
	Payload []byte
	// Instructions 用于签名的静态提示词。
	//
	// ⚠ 必须与 Payload 里那段字符串**逐字相同**，否则 403。
	Instructions string
}

// DoChat 发一次对话，返回原始响应（流式/非流式都由调用方处理）。
//
// 非 2xx 也正常返回（把上游错误体带回去），只有网络层失败才返回 error。
func (c *Client) DoChat(ctx context.Context, a *Auth, req ChatRequest) (*http.Response, error) {
	if !a.ChatUsable() {
		return nil, fmt.Errorf("monkeycode: 凭证缺 api_key 或 signing_secret，无法对话")
	}
	path := pathResponses
	if req.Interface == ifaceAnthropic {
		path = pathMessages
	}
	base := strings.TrimSuffix(a.ProxyBase(), "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(req.Payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(a.APIKey))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(signatureHeader, Sign(a.SigningSecret, req.Instructions))
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("User-Agent", c.userAgent)
	if req.Interface == ifaceAnthropic {
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	}
	return c.httpFor(a).Do(httpReq)
}

// ---- 杂项 ----

// truncate 截断错误体，避免把几 MB 塞进错误信息。
func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// proxyFunc 把代理 URL 转成 Transport.Proxy。
//
// 支持 http/https/socks5。解析失败返回 nil（退化为直连，上层日志里会提示）。
func proxyFunc(proxy string) func(*http.Request) (*url.URL, error) {
	u, err := url.Parse(proxy)
	if err != nil || u.Scheme == "" {
		return nil
	}
	return http.ProxyURL(u)
}
