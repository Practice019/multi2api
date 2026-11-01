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
	"sync"
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
	// pathLoginEntry 登录入口。它 302 到上游自己的 OAuth authorize URL。
	pathLoginEntry = "/api/v1/users/login"
	pathResponses  = "/responses"
	pathMessages   = "/messages"
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
// 两站的网络前提不同：
//
//	国内站 (cn)    必须**直连** —— 它在国内，走海外代理既慢又触发风控
//	国际站 (intl)  **默认直连**；Proxy 配了才走代理（代理只在本机直连不通时才需要）
//
// ⚠ 2026-11-01 更正：「国际站必须走代理，否则连不上」是**错误前提**（见 egress.go 头注）。
// 用户实测零代理直连三发全通（status 401 / captcha 201 / 首页 200）——
// 把代理写成"设计上必须"，会让没有代理的用户误以为功能不可用。
//
// 踩过的坑（真机实测）：全局配了一个 proxy，国内站请求直接
// `Post "https://proxy.monkeycode-ai.com/v1/messages": EOF`，
// 表现为「账号不健康」——错误信息完全指不到代理上。
//
// 所以这里**按凭证的 region 选**，而不是全局一个。
type Client struct {
	// mu 保护下面两个 client 指针与两个出口字符串。
	//
	// # 为什么要可变（2026-11-01 用户报「代理配置是写死的，设置里设不了」）
	//
	// 原来这两个 client 是**构造时一次性定死**的：代理写在 config.json 里，
	// 改一次要重启，而设置页根本没有入口。用户的原话是「每个人都不一样」——
	// 出口地址天然是每台机器各不相同的，必须能在设置页改、并且**立刻生效**
	//（否则用户改完还得猜「重启了吗/生效了吗」）。
	//
	// 锁的粒度：只在取/换指针时加锁，请求本身不带锁（http.Client 自身并发安全）。
	mu        sync.RWMutex
	direct    *http.Client // 国内站
	proxied   *http.Client // 国际站
	userAgent string

	// Proxy 国际站出口（记下来仅供日志/自检显示）。
	Proxy string

	// DirectProxy 允许强制国内站也走代理（默认空 = 直连）。
	//
	// ⚠ 只用于「本机无法直连国内站」这类环境；正常部署**不要**设。
	DirectProxy string

	// Host 覆盖"网页/管理端点"的基址（留空 = 按凭证的 Region 取 hostCN/hostIntl）。
	//
	// # 为什么需要它（不是给生产用的旋钮）
	//
	// hostCN / hostIntl 是 **const**，而续期（renew.go）是自己拼 URL 的，
	// 所以没有覆盖点就做不出 hermetic 测试 —— 假上游起在 127.0.0.1，
	// 却只能往 monkeycode-ai.com 打。egress 自检那条路能把完整 URL 传进
	// probeEgress，所以它不需要这个；续期需要。
	//
	// ⚠ 生产代码**不要**设它：写了它，同进程所有 host 请求都会改道，
	// 而设置页里显示的还是真实站点 —— 那是最难查的一类"配了没用"。
	Host string
}

// httpFor 按凭证区域选 http.Client（读锁：SetProxy 可能正在换）。
func (c *Client) httpFor(a *Auth) *http.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if a.RegionOrDefault() == RegionIntl {
		return c.proxied
	}
	return c.direct
}

// SetProxy 热切换出口（空 = 直连）。
//
// # 它同时更新四样东西
//
//	两个 client（国内站直连 / 国际站走代理）+ 两个出口字符串（Proxy / DirectProxy）。
//	字符串必须一起更新：netretry 的错误文案与 egress 自检都读它们，只换 client 会让
//	「错误里写的出口」与「实际用的出口」不一致 —— 那正是这类自检最怕的谎报。
//
// ⚠ 旧 client 的**空闲连接**不会立刻关：Transport 里可能还有 keep-alive 连接，
//
//	这里不主动 CloseIdleConnections()，因为切换是低频动作，而关掉连接池会让
//	一笔在途请求失败。下次请求自然走新 transport。
func (c *Client) SetProxy(proxy, directProxy string) {
	proxy = strings.TrimSpace(proxy)
	directProxy = strings.TrimSpace(directProxy)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.direct = &http.Client{Transport: newTransport(directProxy)}
	c.proxied = &http.Client{Transport: newTransport(proxy)}
	c.Proxy = proxy
	c.DirectProxy = directProxy
}

// ProxyConfig 当前出口（proxy=国际站，directProxy=国内站强制代理）。
func (c *Client) ProxyConfig() (proxy, directProxy string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Proxy, c.DirectProxy
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
//
// ⚠ 空 session **不拼 Cookie 头**（不是拼一个空值）。
//
// # 为什么这条很重要（2026-11-01 修）
//
// 续期的三枪（captcha challenge / redeem / password-login）都是**公开端点**，
// 必须能在"session 已经死透"的情况下工作 —— 那正是它们存在的理由（见 renew.go）。
// 原先这里无条件 `h.Set("Cookie", "monkeycode_ai_session="+session)`，
// 于是**没有 session 时会发出 `Cookie: monkeycode_ai_session=`**：
//
//	① 语义错：那是"名为 session、值为空"的一条 cookie，不等于"没带 cookie"；
//	② 实测有害：上游见到这个名字会走进"已登录"分支，而不是匿名分支。
//
// 所以：有 session 才拼 Cookie；没有就只给 Accept。
func sessionHeaders(a *Auth) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	if a != nil && strings.TrimSpace(a.Session) != "" {
		h.Set("Cookie", "monkeycode_ai_session="+strings.TrimSpace(a.Session))
	}
	return h
}

// doJSON 发一个 JSON 请求，返回 (状态码, 原始体)。
//
// 非 2xx **不**当错误返回 —— 把原文交出去，由调用方按业务语义判。
// 只有网络层失败才返回 error（与 gateway 的契约一致）。
// # 2026-11-01：网络失败现在带出口信息 + 对 GET 做一次退避重试
//
// 用户实测到的原文是裸的 `net/http: TLS handshake timeout`，而它真正的原因很大概率是
// 「配了代理但代理出不了国」—— 错误信息指不到那里（见 netretry.go 的文件注释）。
// ⚠ 注意：这不是说国际站**必须**走代理；国际站默认直连，只有本机直连不通时才该配代理。
// 现在：GET 走一次退避重试（幂等，重复提交无副作用），POST 保持一发一次
// （重复提交在上游是错误码，会污染签到历史）。
func (c *Client) doJSON(ctx context.Context, a *Auth, method, rawURL string, headers http.Header, body []byte) (int, []byte, error) {
	if method == http.MethodGet {
		return c.doJSONWithRetry(ctx, a, method, rawURL, headers, body)
	}
	code, raw, err := c.doJSONOnce(ctx, a, method, rawURL, headers, body)
	if err != nil {
		return 0, nil, c.wrapNetErr(a, method, rawURL, err)
	}
	return code, raw, nil
}

// doJSONWithRetry 只在**传输层错误**上重试（HTTP 非 2xx 不重试：那是业务语义，
// 重试同样的请求只会得到同样的状态码 —— 与 qoder 的 getJSON 同一判据）。
func (c *Client) doJSONWithRetry(ctx context.Context, a *Auth, method, rawURL string, headers http.Header, body []byte) (int, []byte, error) {
	attempts := 1
	if method == http.MethodGet {
		attempts = getAttempts
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, nil, c.wrapNetErr(a, method, rawURL, ctx.Err())
			case <-time.After(retryDelay(attempt)):
			}
		}
		started := time.Now()
		code, raw, err := c.doJSONOnce(ctx, a, method, rawURL, headers, body)
		if err == nil {
			return code, raw, nil
		}
		lastErr = err
		if !transientNetErr(err) {
			break // 确定性失败（DNS 不存在 / TLS 证书错）再试也一样
		}
		if elapsed := time.Since(started); elapsed >= retryCostCeiling {
			// 已经付出一次完整超时的代价 ⇒ 这是断掉的路、不是抖动（见常量注释）
			break
		}
	}
	return 0, nil, c.wrapNetErr(a, method, rawURL, lastErr)
}

// doJSONOnce 发一次，不做任何重试（上面两个函数共用）。
func (c *Client) doJSONOnce(ctx context.Context, a *Auth, method, rawURL string, headers http.Header, body []byte) (int, []byte, error) {
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
