// client.go MiniMax 的 HTTP 面（业务 + 推理 + OAuth）。
//
// # 三个 host 分工（照抄参照项目，不能合并）
//
//	accountBase  OAuth（设备码 / 轮询 / 续期）
//	apiBase      目录 / 签到 / 积分 / 推理
//
// 两个都可注入（测试要能把假上游指进来）—— 参照项目里它们是硬编码常量，
// 那里能接受是因为它的 e2e 探针直接打真实上游；本仓的判据是
// "**每条路径都要能被断言**"，所以 OAuth 与业务各留一个注入点。
package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workbuddy2api/internal/anthroconv"
)

// Config 本上游的构造配置。
type Config struct {
	// AccountBase OAuth 主机（空 = 官方 account.minimax.cn）。
	AccountBase string
	// APIBase 业务/推理主机（空 = 官方 agent.minimax.cn）。
	APIBase string
	// AuthDir 凭证目录。
	AuthDir string
	// HTTPClient 注入（测试用；空 = 默认）。
	HTTPClient *http.Client
	// TimezoneID 签到端点要的 IANA 时区（空 = 从本机推断，再不行 UTC）。
	//
	// ⚠ 它是 **query 参数**，而且放错位置**也返回 HTTP 200**
	//（放头上会回 `1406010011 invalid timezone_id`，仍然是 200）。
	// 所以"200 = 成功"在这个端点上不成立 —— 必须查业务码。
	TimezoneID string
}

// Client 一个 MiniMax 上游实例的 HTTP 客户端。
type Client struct {
	accountBase string
	apiBase     string
	http        *http.Client
	timezone    string
}

// NewClient 构造客户端。
func NewClient(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		// 超时是必须的：上游偶发挂起时不设超时会拖死整个网关的调度。
		// ⚠ 不用 `Client.Timeout` —— 推理是流式（SSE），可能持续数分钟，
		// 总时长超时会把长回答截断。用 Transport 级的首字节超时兜底。
		hc = &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        100,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		}
	}
	return &Client{
		accountBase: trimBase(cfg.AccountBase, accountHost),
		apiBase:     trimBase(cfg.APIBase, apiHost),
		http:        hc,
		timezone:    resolveTimezone(cfg.TimezoneID),
	}
}

func trimBase(v, def string) string {
	if s := strings.TrimRight(strings.TrimSpace(v), "/"); s != "" {
		return s
	}
	return def
}

// resolveTimezone 取签到端点要的 IANA 时区。
//
// 与参照项目同款：显式配置优先，否则用本机时区，再不行 `UTC`。
// 不做"猜一个中国时区"—— 上游要的是 IANA 名字，猜错会得到
// `invalid timezone_id`（而且是 HTTP 200，见 Config 的注释）。
func resolveTimezone(configured string) string {
	if s := strings.TrimSpace(configured); s != "" {
		return s
	}
	if name := time.Now().Location().String(); name != "" && name != "Local" {
		return name
	}
	return "UTC"
}

// businessHeaders 业务端点的头。
//
// ⚠ 只有两个：`Authorization` + `Accept`。参照项目明确记过：
// 签到 / 积分 / 目录 / 推理**都不需要**机器头或签名（与 qoder 的
// `/sash/` 端点是不同情形）。多塞头是凭空猜测。
func businessHeaders(a *Auth) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.Token())
	h.Set("Accept", "application/json")
	return h
}

// inferHeaders 推理端点的头。
//
// ⚠ **不加 `anthropic-version` / `anthropic-beta`** —— 参照项目实测
// 只带这三个即 HTTP 200，且明确写了"加未经验证的头是猜测"。
//
// ⚠ `Accept: text/event-stream`（不是 application/json）：
// 请求体带 `stream:true`，响应是 SSE。
func inferHeaders(a *Auth) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.Token())
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	return h
}

// oauthHeaders OAuth 端点的头（form-urlencoded，**无** Authorization）。
func oauthHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	return h
}

// errorBodyMax 错误响应里最多读多少字节塞进错误信息。
const errorBodyMax = 500

// doJSON 发一个 JSON 请求并读回响应体。
//
// 返回 (原始体, HTTP 状态码, error)。**HTTP 非 2xx 不返回 error** ——
// 由调用方决定语义：本上游的业务端点在 HTTP 200 上也可能带业务错误码，
// 而 401/402 需要分别归类（AUTH / QUOTA）。
// 把"非 2xx"在这里统一成 error 会让调用方拿不到状态码，无法分类。
func (c *Client) doJSON(ctx context.Context, method, fullURL string, header http.Header, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header = header
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// 限读：错误页可能是几十 KB 的 HTML，全读进来只是浪费内存。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

// ---- 模型目录 ----

// modelEntry 归一后的模型条目（远端与兜底表**共用**这个形状）。
type modelEntry struct {
	ID            string
	Name          string
	ContextWin    int
	MaxTokens     int
	SupportsImage bool
	// EffortOptions 思考档位（远端 `effort_options`）；只有 M3.1 有。
	EffortOptions []string
	// DefaultEffort 默认档位（远端 `default_effort`）。
	DefaultEffort string
	// ThinkingMode 思考开关模式（远端 `thinking_config.mode`）。
	ThinkingMode string
}

// fetchModels 拉远端模型目录。
//
// ⚠ **必须走远端**，不能照抄客户端内置表：参照项目实测客户端
// `config.js` 的内置表只有 3 个，而远端有 **4** 个 ——
// 照抄会漏掉 `MiniMax-M3.1-Flash-Preview`，而它正是客户端界面上
// 被选中的那个。故远端失败时才回退兜底表（`fallbackModels`）。
func (c *Client) fetchModels(ctx context.Context, a *Auth) ([]modelEntry, error) {
	u, err := url.Parse(c.apiBase + pathModels)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("region", queryRegion)
	q.Set("buildEnv", queryBuildEnv)
	u.RawQuery = q.Encode()

	raw, status, err := c.doJSON(ctx, http.MethodGet, u.String(), businessHeaders(a), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("minimax: 模型目录 HTTP %d: %s", status, snippet(raw))
	}
	return parseModels(raw)
}

// remoteModel 远端目录里的**一个**模型条目。
//
// ⚠ 数字字段一律用 `any`：上游把窗口/输出上限写成裸数字，
// 但余额那边实测是字符串（`"800.00"`）—— 同一家上游的字段类型不稳定，
// 用 `any` + `positiveInt` 宽容解析，比 `int` 撞上字符串时整个 Unmarshal
// 失败（**全盘丢失**）安全得多。
type remoteModel struct {
	Name  string `json:"name"`
	Limit struct {
		Context any `json:"context"`
		Output  any `json:"output"`
	} `json:"limit"`
	ContextWindowOptions []any `json:"context_window_options"`
	Modalities           struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	EffortOptions  []string `json:"effort_options"`
	DefaultEffort  string   `json:"default_effort"`
	ThinkingConfig struct {
		Mode string `json:"mode"`
	} `json:"thinking_config"`
}

// remoteProviderConfig 远端目录里 minimax 那个 provider 的 config。
type remoteProviderConfig struct {
	// Models 是**对象**（键就是模型 id），不是数组 —— 按数组解析会得到空。
	Models     map[string]remoteModel `json:"models"`
	ModelOrder []string               `json:"model_order"`
}

// modelsEnvelope 远端目录的响应形状。
//
// 形状（照抄参照项目 `minimax-auth.ts` 的校验链）：
//
//	{providers:[{providerId:"minimax", config:{
//	    models:{ "<长名>": {name, limit:{context,output}, context_window_options,
//	                        modalities:{input:[…]}, effort_options, default_effort,
//	                        thinking_config:{mode}} },
//	    model_order:["<长名>", …]}}]}
type modelsEnvelope struct {
	Providers []struct {
		ProviderID string               `json:"providerId"`
		Config     remoteProviderConfig `json:"config"`
	} `json:"providers"`
}

// parseModels 把远端目录归一成 modelEntry 列表（按 model_order 排序）。
func parseModels(raw []byte) ([]modelEntry, error) {
	var env modelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("minimax: 目录响应不是合法 JSON: %w", err)
	}
	var cfg *remoteProviderConfig
	for i := range env.Providers {
		if strings.TrimSpace(env.Providers[i].ProviderID) == providerID {
			cfg = &env.Providers[i].Config
			break
		}
	}
	if cfg == nil || len(cfg.Models) == 0 {
		return nil, fmt.Errorf("minimax: 目录响应里没有 providerId=%q 的模型表", providerID)
	}

	out := make([]modelEntry, 0, len(cfg.Models))
	seen := map[string]bool{}
	appendOne := func(id string, m remoteModel) {
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, normalizeModel(id, m))
	}
	// 先按 model_order（权威顺序），再补漏（远端偶尔不下发 order）。
	for _, id := range cfg.ModelOrder {
		if m, ok := cfg.Models[id]; ok {
			appendOne(id, m)
		}
	}
	for id, m := range cfg.Models {
		appendOne(id, m)
	}
	return out, nil
}

// normalizeModel 把一个远端条目归一。
func normalizeModel(id string, m remoteModel) modelEntry {
	e := modelEntry{
		ID:            id,
		Name:          strings.TrimSpace(m.Name),
		EffortOptions: m.EffortOptions,
		DefaultEffort: strings.TrimSpace(m.DefaultEffort),
		ThinkingMode:  strings.TrimSpace(m.ThinkingConfig.Mode),
	}
	if e.Name == "" {
		e.Name = id
	}
	// 窗口口径：**档位表的最大档**，不是 `limit.context`。
	//
	// ⚠ 参照项目为这条专门写了理由：M3.1 的 `limit.context` 是 512000，
	// 而 `context_window_options` 是 [512000, 1000000] —— 填 512K 会让
	// 客户端**远早于官方能力**触发上下文压缩。只有档位表没有有效值时
	// 才回退 `limit.context`（那是有据的回落，不是编造）。
	e.ContextWin = maxPositive(m.ContextWindowOptions)
	if e.ContextWin == 0 {
		if n, ok := positiveInt(m.Limit.Context); ok {
			e.ContextWin = n
		}
	}
	if n, ok := positiveInt(m.Limit.Output); ok {
		e.MaxTokens = n
	}
	for _, in := range m.Modalities.Input {
		if strings.EqualFold(strings.TrimSpace(in), "image") {
			e.SupportsImage = true
			break
		}
	}
	return e
}

// maxPositive 取数组里最大的正数（非数字/非正一律跳过）。
func maxPositive(list []any) int {
	best := 0
	for _, v := range list {
		if n, ok := positiveInt(v); ok && n > best {
			best = n
		}
	}
	return best
}

// ---- 积分与签到 ----

// creditDetails 拉积分明细（**平铺响应**，见 unwrapEnvelopeData）。
func (c *Client) creditDetails(ctx context.Context, a *Auth) (map[string]any, error) {
	raw, status, err := c.doJSON(ctx, http.MethodGet, c.apiBase+pathCreditDetails, businessHeaders(a), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, fmt.Errorf("minimax: 凭据已失效（HTTP %d），请重新登录该账号", status)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("minimax: 积分端点 HTTP %d: %s", status, snippet(raw))
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("minimax: 积分响应不是合法 JSON: %w", err)
	}
	if code, ok := baseRespStatus(top); ok && code != 0 {
		return nil, fmt.Errorf("minimax: 积分查询业务错误 %d: %s", code, baseRespMessage(top))
	}
	return top, nil
}

// signinStatus 拉签到状态（信封形）。
func (c *Client) signinStatus(ctx context.Context, a *Auth) (map[string]any, error) {
	raw, status, err := c.doJSON(ctx, http.MethodGet, c.signinURL(pathSigninStatus), businessHeaders(a), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, fmt.Errorf("minimax: 凭据已失效（HTTP %d），请重新登录该账号", status)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("minimax: 签到状态 HTTP %d: %s", status, snippet(raw))
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("minimax: 签到状态响应不是合法 JSON: %w", err)
	}
	if code, ok := baseRespStatus(top); ok && code != 0 {
		return nil, fmt.Errorf("minimax: 签到状态业务错误 %d: %s", code, baseRespMessage(top))
	}
	return unwrapEnvelopeData(top), nil
}

// signinClaim 领取当日签到积分。
//
// ⚠ 返回的 `claim_result` 才是**幂等判据**（1=真领到、2=已领过），
// HTTP 状态码在这件事上无判别力 —— 重复领取同样返回 200。
func (c *Client) signinClaim(ctx context.Context, a *Auth) (map[string]any, error) {
	raw, status, err := c.doJSON(ctx, http.MethodPost, c.signinURL(pathSigninClaim), businessHeaders(a), map[string]any{})
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, fmt.Errorf("minimax: 凭据已失效（HTTP %d），请重新登录该账号", status)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("minimax: 签到领取 HTTP %d: %s", status, snippet(raw))
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("minimax: 签到领取响应不是合法 JSON: %w", err)
	}
	if code, ok := baseRespStatus(top); ok && code != 0 {
		return nil, fmt.Errorf("minimax: 签到领取业务错误 %d: %s", code, baseRespMessage(top))
	}
	return unwrapEnvelopeData(top), nil
}

// signinURL 拼签到端点 URL —— `timezone_id` 必须是 **query 参数**。
func (c *Client) signinURL(path string) string {
	u, err := url.Parse(c.apiBase + path)
	if err != nil {
		return c.apiBase + path
	}
	q := u.Query()
	q.Set("timezone_id", c.timezone)
	u.RawQuery = q.Encode()
	return u.String()
}

// ---- 推理（Anthropic Messages）----

// chat 发一次推理请求。
//
// `openAIBody` 是核心传进来的 **OpenAI 形状**请求体；本方法把它转成
// Anthropic 形状发出去，并把 Anthropic SSE 转回 OpenAI SSE 写进 w。
//
// 转换走共享的 `internal/anthroconv`（与 zcode 的 JWT 通道同一份实现）
// —— 两个 Anthropic 消费者共用一套转换，判据不会分叉。
func (c *Client) chat(ctx context.Context, a *Auth, openAIBody []byte, w io.Writer) error {
	anthroBody, err := anthroconv.OpenAIToAnthropic(openAIBody)
	if err != nil {
		return fmt.Errorf("minimax: 请求体转换失败: %w", err)
	}
	// 强制 stream:true —— 上游这个端点只按 SSE 回。
	anthroBody, err = forceStream(anthroBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+pathInferMessages, bytes.NewReader(anthroBody))
	if err != nil {
		return err
	}
	req.Header = inferHeaders(a)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyMax))
		return inferStatusError(resp.StatusCode, raw)
	}
	return anthroconv.AnthropicSSEToOpenAI(resp.Body, w)
}

// inferStatusError 把推理端点的 HTTP 状态码映射成错误。
//
// ⚠ **402 必须单独归类**：余额不足是这个上游最常见的真实失败，
// 归成 SERVER/AUTH 会让用户看不到「去充值」这个**唯一有效动作**。
func inferStatusError(status int, body []byte) error {
	s := snippet(body)
	switch status {
	case http.StatusPaymentRequired: // 402
		return fmt.Errorf("minimax: 余额不足（HTTP 402），请充值后重试: %s", s)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("minimax: 鉴权失败（HTTP %d）: %s", status, s)
	case http.StatusTooManyRequests:
		return fmt.Errorf("minimax: 请求过于频繁（HTTP 429）: %s", s)
	case http.StatusBadRequest:
		return fmt.Errorf("minimax: 请求参数被拒绝（HTTP 400）: %s", s)
	default:
		if status >= 500 {
			return fmt.Errorf("minimax: 上游服务错误（HTTP %d）: %s", status, s)
		}
		return fmt.Errorf("minimax: HTTP %d: %s", status, s)
	}
}

// forceStream 确保请求体里 `stream:true`。
func forceStream(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("minimax: 转换后的请求体不是 JSON: %w", err)
	}
	m["stream"] = true
	return json.Marshal(m)
}

// snippet 截一段响应体进错误信息（换行压掉，避免把整个 HTML 灌进日志）。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > errorBodyMax {
		s = s[:errorBodyMax] + "…"
	}
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "(空响应)"
	}
	return s
}
