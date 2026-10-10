// client.go ZCode 的 HTTP 客户端（对话 + 计量）。
//
// # 两条通道走不同的端点，本文件负责选路
//
//	api-key → {origin}/api/paas/v4/chat/completions        OpenAI 协议，本就原生
//	          {origin}/api/coding/paas/v4/chat/completions Coding Plan 的 Key 通道
//	jwt     → zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages
//	          **Anthropic 协议** —— 需要协议转换（见 anthropic.go）
//
// # 为什么 JWT 通道不能也用 OpenAI 端点（实测否证的假设）
//
// 一开始我推断"Coding Plan 应该也有 OpenAI 形态"，实测**否证**：
//
//	/api/v1/zcode-plan/paas/v4/chat/completions   → 404
//	/api/v1/zcode-plan/v1/chat/completions        → 404
//	/api/v1/zcode-plan/anthropic/v1/messages      → 401  ← 只有这个
//
// 所以 JWT 通道必须做协议转换。这个代价是实打实的（参照项目为此写了 27KB），
// 但它换来的是**订阅额度可用**，而不是按量付费。
package zcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workbuddy2api/internal/anthroconv"
)

// DefaultOriginZAI Z.ai 开放平台（国际版）默认基址。
const DefaultOriginZAI = originZAI

// DefaultOriginBigModel 智谱开放平台（国内版）默认基址。
const DefaultOriginBigModel = originBigModel

// Config 本上游的构造配置。
type Config struct {
	// Origin 基址覆盖。空 = 按账号的 AuthDir 自动选（见 clientFor）。
	Origin string
	// OAuthBase 登录端点的基址覆盖（空 = 官方线上 `https://zcode.z.ai/api/v1`）。
	//
	// # 为什么它必须可覆盖（两个理由，都不是为了测试方便）
	//
	//  1. **官方自己会改写端点**：`official-coding-plan-gateway.ts` 揭示
	//     Coding Plan 的请求会被服务端动态改写到 `/api/v1/ultra[-zai]/...`。
	//     硬编码等于把"上游改架构"变成我们的故障。
	//  2. **内网/私有部署**：官方 `.env.example` 里有 `ZCODE_DEPS_BASE_URL` /
	//     `INTRANET_MACHINE_HOST` 这类内网覆盖项，说明存在自建部署形态。
	//
	// （顺带的好处：测试能把假上游指进来，而不必真去打线上 ——
	// 之前硬编码时测试真的把请求发出去了，那是不可接受的副作用。）
	OAuthBase string
	// BillingOrigin 计量端点的 origin 覆盖（空 = 官方线上 zcode.z.ai）。
	//
	// ⚠ **不能与 OAuthBase 复用同一个字段**（第一版就是那样，结果 404）：
	//
	//	CLI OAuth 的路径  /oauth/cli/init          ← 不含 /api/v1 前缀
	//	计量端点的路径    /api/v1/zcode-plan/…     ← **自带** /api/v1
	//
	// 两边前缀约定不同，共用一个 base 会拼出
	// `https://zcode.z.ai/api/v1/api/v1/zcode-plan/…`（实测 404）。
	// 所以它们是两个独立的注入点。
	BillingOrigin string
	// AuthDir 凭证目录。
	AuthDir string
	// HTTPClient 注入的 HTTP 客户端（测试用；空 = 默认）。
	HTTPClient *http.Client
	// Captcha 验证码求解器（JWT 通道必需；nil = 不支持 JWT 通道）。
	Captcha CaptchaSolver
}

// Client 一个 ZCode 上游实例的 HTTP 客户端。
type Client struct {
	origin    string
	oauthBase string
	// billingOrigin 计量端点的 origin（与 oauthBase 分开的原因见 Config）。
	billingOrigin string
	http          *http.Client
	captcha       CaptchaSolver
	appVer        string
	accounts      map[string]*Auth // UID → 活凭证（由 Provider 装配时注入）
}

// NewClient 构造客户端。
func NewClient(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		// 超时是**必须的**：上游偶发挂起时不设超时会拖死整个网关的调度。
		// 但**不能设 WriteTimeout 级别的短超时** —— 流式对话可能持续数分钟，
		// 所以这里只用 Transport 级的连接超时，不用 Client.Timeout。
		hc = &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.OAuthBase), "/")
	if base == "" {
		base = cliOAuthBase
	}
	billBase := strings.TrimRight(strings.TrimSpace(cfg.BillingOrigin), "/")
	if billBase == "" {
		billBase = planOrigin
	}
	return &Client{
		origin:        strings.TrimRight(strings.TrimSpace(cfg.Origin), "/"),
		oauthBase:     base,
		billingOrigin: billBase,
		http:          hc,
		captcha:       cfg.Captcha,
		appVer:        defaultAppVersion,
	}
}

// originFor 选该凭证该用的 origin。
//
// # 优先级：凭证 > 全局配置 > 默认 Z.ai
//
// 为什么**凭证优先**而不是全局配置优先：Z.ai（国际，api.z.ai）与
// BigModel（国内，open.bigmodel.cn）是两套独立平台，同一个 Key 不能跨用。
// 一个网关同时有两边的 Key 是常见情形 —— 此时"每个账号属于哪个平台"
// 是**账号的属性**，全局配置只能表达"默认值"。
//
// 全局配置仍然有用：单平台部署时可以一配到底，不必每份凭证都写 origin。
//
// ⚠ 早先的实现只读 `c.origin`，**完全没看凭证上的 Origin 字段** ——
// 于是 `Auth.Origin` 是个永远不生效的字段（导入时解析了、落盘了、
// 加载了，然后在选路时被无视）。症状是"BigModel 的 Key 被打到 Z.ai"
// 从而鉴权失败，而凭证文件里明明写着正确的平台。
func (c *Client) originFor(a *Auth) string {
	if a != nil {
		if o := strings.TrimRight(strings.TrimSpace(a.Origin), "/"); o != "" {
			return o
		}
	}
	if c.origin != "" {
		return c.origin
	}
	return originZAI
}

// chatURL 该凭证的对话端点 URL。
//
// 两条通道走**不同协议**，这是本上游最需要注意的地方：
//
//	API Key → OpenAI 协议（本网关 wire 层原生支持，零转换）
//	JWT     → Anthropic 协议（必须转换，见 anthropic.go）
//
// 为什么 API Key 不用 Anthropic 端点：官方 builtin config 里
// coding-plan 的 apiKey 通道声明的确实是 `api.z.ai/api/anthropic`，
// 但**实测 OpenAI 端点也在**（`/api/paas/v4` 与 `/api/coding/paas/v4`
// 都返回 401 而非 404）。走 OpenAI 端点可以**完全省掉协议转换**，
// 与其它 10 个上游保持同一形状 —— 代价只是不能用 Anthropic 独有字段
// （如 cache_control），那些字段本网关的调用方也发不出来。
func (c *Client) chatURL(a *Auth) string {
	if a.UsesJWT() {
		return planOrigin + planAnthropicPath
	}
	base := c.originFor(a)
	if a.CodingPlan {
		// Coding Plan 的 Key 与普通 Key 走不同端点，官方文档明示
		// 两者**不可互换、不互相消耗额度**。
		return base + pathCodingPaaS + "/chat/completions"
	}
	return base + pathPaaS + "/chat/completions"
}

// Chat 发一次对话，返回原始响应（调用方负责 Close Body）。
//
// 契约对齐 gateway.Provider.Chat：**status 非 2xx 也算正常返回**，
// 把上游错误体带回给调用方按业务码判断。只有网络层失败才返回 error。
func (c *Client) Chat(ctx context.Context, a *Auth, body []byte, stream bool) (*http.Response, error) {
	if a == nil || !a.Usable() {
		return nil, fmt.Errorf("zcode: 凭证缺失或不可用")
	}
	target := c.chatURL(a)
	payload, err := c.buildBody(a, body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	h := chatHeaders(a, nil)
	if stream {
		h.Set("Accept", "text/event-stream")
	}
	req.Header = h
	if err := c.attachCaptcha(ctx, a, req); err != nil {
		return nil, err
	}
	return c.http.Do(req)
}

// buildBody 把出口层的 OpenAI 请求体编成该通道要发的形态。
//
//	api-key → 原样透传（本就是 OpenAI 格式）
//	jwt     → 做 Anthropic 转换（见 anthropic.go）
func (c *Client) buildBody(a *Auth, openaiBody []byte) ([]byte, error) {
	if !a.UsesJWT() {
		return openaiBody, nil
	}
	return anthroconv.OpenAIToAnthropic(openaiBody)
}

// attachCaptcha 给 JWT 通道的请求挂上验证码参数。
//
// ⚠ 这是 JWT 通道**每请求必需**的一步（API Key 通道不需要）。
// 官方仓库搜 captcha/aliyun **零命中** —— 说明这是纯第三方逆向出来的机制，
// 所以它的失效风险高于其它部分，失败时要如实报错而不是静默去掉。
func (c *Client) attachCaptcha(ctx context.Context, a *Auth, req *http.Request) error {
	if !a.UsesJWT() {
		return nil
	}
	if c.captcha == nil {
		return fmt.Errorf("zcode: JWT 通道需要验证码求解器，但未装配（" +
			"请改用 API Key 通道，或配置验证码求解）")
	}
	region := strings.TrimSpace(a.CaptchaRegion)
	if region == "" {
		// 实测的线上配置默认是 cn（见 /api/v1/client/configs 的 captcha.region）。
		region = "cn"
	}
	param, err := c.captcha.Solve(ctx, region)
	if err != nil {
		return fmt.Errorf("zcode: 获取阿里云验证码参数失败: %w", err)
	}
	req.Header.Set(hdrCaptchaParam, param)
	req.Header.Set(hdrCaptchaRegion, region)
	return nil
}

// BillingCurrent 取当前套餐信息（GET，实测修正：不是 POST）。
func (c *Client) BillingCurrent(ctx context.Context, a *Auth) ([]byte, error) {
	return c.billingGet(ctx, a, planBillingCurrentPath)
}

// BillingBalance 取余额/配额桶。
func (c *Client) BillingBalance(ctx context.Context, a *Auth) ([]byte, error) {
	return c.billingGet(ctx, a, planBillingBalancePath)
}

func (c *Client) billingGet(ctx context.Context, a *Auth, path string) ([]byte, error) {
	if a == nil || !a.Usable() {
		return nil, fmt.Errorf("zcode: 凭证缺失或不可用")
	}
	// ⚠ 必须带 `?app_version=` —— 参照实现两个端点都带它
	//（config.BillingClientVersion）。实测 current 不带也能通，
	// 但照抄版本头/查询串能让请求与官方客户端一致，少一个被风控的理由。
	target := c.billingBase() + path + "?app_version=" + c.appVer
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header = billingHeaders(a)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 限读：错误页可能是几十 KB 的 HTML，全读进来只是浪费内存。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return raw, fmt.Errorf("zcode: 计量端点 HTTP %d: %s",
			resp.StatusCode, firstLine(errMessage(raw)))
	}
	return raw, nil
}

// billingBase 计量端点的基址。
//
// 为什么做成方法而不是直接用常量：测试要能把假上游指进来，
// 否则"主备顺序"与"端点选择"这两件事**无法被断言** ——
// 而它们正是用户报的那个 bug 的核心（我打了 balance 而它报参数错）。
//
// 生产上它恒等于 planOrigin（除非将来上游改写端点）。
func (c *Client) billingBase() string {
	if c != nil && c.billingOrigin != "" {
		return c.billingOrigin
	}
	return planOrigin
}

// firstLine 取字符串第一行（塞进错误信息时避免把整个 JSON 打进去）。
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ProbeOrigin 探测该凭证属于哪个平台（Z.ai 还是 BigModel）。
//
// 判据是**实测过的行为差异**：同一个 Key 在错误的平台上会得到鉴权错误，
// 在正确的平台上会通过鉴权（然后可能因其它原因失败）。
//
// 返回选定的 origin。探测失败时返回默认 Z.ai —— 不阻断装配。
func (c *Client) ProbeOrigin(ctx context.Context, a *Auth) string {
	if c.origin != "" {
		return c.origin
	}
	if a == nil || !a.Usable() {
		return originZAI
	}
	for _, candidate := range []string{originZAI, originBigModel} {
		if c.probeOne(ctx, a, candidate) {
			return candidate
		}
	}
	return originZAI
}

// probeOne 在指定 origin 上探一次鉴权。
//
// "通过"的判据：**不是 401**。用 401 而不是 200，是因为：
//   - 真实 Key 也可能因额度耗尽返回 402/429，那**不算**平台错
//   - 只要不是 401（鉴权被拒），就说明这个平台的鉴权头认这把 Key
func (c *Client) probeOne(ctx context.Context, a *Auth, origin string) bool {
	body := []byte(`{"model":"GLM-5.3","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		origin+pathPaaS+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header = chatHeaders(a, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode != http.StatusUnauthorized
}

// EscapeQuery 供调试端点构造查询串。
func EscapeQuery(kv map[string]string) string {
	q := url.Values{}
	for k, v := range kv {
		q.Set(k, v)
	}
	return q.Encode()
}
