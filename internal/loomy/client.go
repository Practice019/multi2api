// client.go Loomy 的上游 HTTP 客户端。
//
// # 这个上游的难点只有一处，但它足以让人卡住
//
// 端点本身是标准 OpenAI 兼容（POST /chat/completions），所以转发逻辑很薄。
// 真正的问题是**双轨鉴权**：同一个上游的两个端点认**不同的** Header。
// 见 applyModelsAuth / applyChatAuth —— 那两个函数刻意分开写。
//
// # 为什么不做 UA 伪造
//
// workbuddy 那边之所以精心伪造三段式 UA，是因为有实测证据表明上游会看它
// （见 internal/upstream/headers.go）。Loomy 这边**没有这种证据**：
// 手册第 6 节的复现步骤全程用 curl（UA 就是 curl 自己的），实测通过；
// 第 8 节的功能验证（function_calling / 流式 / 全模型遍历）也都在 curl 下完成。
//
// 所以本包**不**伪造客户端 UA。凭空造一个没有证据支持的指纹，只会引入一个
// 日后可能与上游对不上的特征 —— 那是净负债，不是兼容性。
package loomy

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

// DefaultBaseURL 上游基址（手册第 1 节速查表）。
//
// 注意它**已经带 `/api/v1`**：手册里的路径是
// `https://loomyad.xunfei.cn/api/v1/models` 与 `.../api/v1/chat/completions`。
// 因此本包不再额外拼 `/v1` —— 拼了会得到 `/api/v1/v1/models`。
const DefaultBaseURL = "https://loomyad.xunfei.cn/api/v1"

// 端点路径（相对 BaseURL）。
const (
	ModelsPath = "/models"
	ChatPath   = "/chat/completions"
)

// maxErrorBody 读取上游错误体时的上限。
//
// 与核心的判据对齐（见 gateway.ErrorClassifier：body 已被调用方限制过长度，
// 实现不必再截断）。这里是**我们自己**读上游响应的那一侧，必须自带上限：
// 一个畸形的巨大错误体不该把网关的内存吃掉。
const maxErrorBody = 1 << 20 // 1 MiB

// respHeaderTimeout 等响应头的上限。
//
// ⚠ 刻意**不设** http.Client.Timeout。
//
// Client.Timeout 是"整个请求"的时限，包含**读完 body** 的时间。
// 而本客户端最重要的用法是读一条 SSE 长流（一次对话可能持续几十秒到几分钟），
// 设了它就会把正常的长回答拦腰砍断 —— 表现为"回答到一半连接断了"，
// 且错因看起来像上游故障。真正的下限约束应该只作用在"上游多久不给响应头"，
// 那是 ResponseHeaderTimeout 的职责。
const respHeaderTimeout = 120 * time.Second

// Client Loomy 的上游客户端。
type Client struct {
	// BaseURL 基址。可注入（hermetic 契约测试指向 httptest 假上游）。
	BaseURL string
	// HTTP 底层客户端。nil 时用 defaultHTTP。
	HTTP *http.Client
}

func defaultHTTP() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: respHeaderTimeout,
			// 不设 DisableKeepAlives 等：默认连接池即可。上游是 HTTPS，
			// 每次请求重建 TLS 的代价远大于保持连接的风险。
		},
	}
}

// New 建一个指向生产上游的客户端。
//
// ⚠ 名字不能叫 NewProvider：本包的 NewProvider 是**契约工厂**
// （Provider，无依赖、可反复构造），两者是完全不同的东西。
// codearts 那边踩过同样的命名坑，见它的 provider.go 注释。
func New() *Client { return NewWithBase(DefaultBaseURL) }

// NewWithBase 建一个指向指定基址的客户端（hermetic 测试用）。
func NewWithBase(base string) *Client {
	if strings.TrimSpace(base) == "" {
		base = DefaultBaseURL
	}
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: defaultHTTP()}
}

// SetBaseURL 替换基址（启动期一次性注入）。
func (c *Client) SetBaseURL(base string) {
	if strings.TrimSpace(base) != "" {
		c.BaseURL = strings.TrimRight(base, "/")
	}
}

// httpClient 返回生效的底层客户端。
func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP()
}

// base 返回生效的基址。
func (c *Client) base() string {
	if c != nil && strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

// ── 双轨鉴权 ────────────────────────────────────────────────────────────
//
// ⚠⚠ 本文件最重要的一段。手册第 3 节把它列为"90% 的人卡在这"。
//
// 同一个上游的两个端点认**不同**的 Header：
//
//	GET  /models            →  token: <session>
//	POST /chat/completions  →  Authorization: Bearer <session>
//
// 用错的那一个返回 HTTP **200** + `{"code":"100002","desc":"缺少 token"}`。
//
// 注意那个 200：**"看状态码判断鉴权对不对"这条路是不通的**。
// 拿 Authorization 去拉目录会拿到一段没有模型列表的 JSON，于是失败以
// "目录为空/解析不出模型"的形态出现，而不是"鉴权头用错了"——
// 上游不会告诉你，你只能从"为什么目录是空的"倒推。
//
// # 为什么写成两个独立的小函数，而不是一个 applyAuth(h, kind)
//
// 合成一个带 kind 参数的函数之后，两处调用点长得一模一样：
//
//	applyAuth(h, authModels)
//	applyAuth(h, authChat)
//
// 评审时看不出哪个端点用了哪个 Header，改错一个（把两个 kind 写反）
// 在 diff 里几乎不可见。拆成两个各自命名的函数之后，"谁用了哪个"
// 在调用点就是字面事实。
//
// 有测试钉死这对判据（TestDualTrackAuth），改错任一方向都会红。

// applyModelsAuth 给**模型目录**请求装鉴权头。
//
// 目标形态：`token: <session>`
func applyModelsAuth(h http.Header, a *Auth) {
	h.Set("token", a.Session)
}

// applyChatAuth 给**对话**请求装鉴权头。
//
// 目标形态：`Authorization: Bearer <session>`
func applyChatAuth(h http.Header, a *Auth) {
	h.Set("Authorization", "Bearer "+a.Session)
}

// ── 模型目录 ────────────────────────────────────────────────────────────

// modelsWire 上游 `/models` 的响应形态。
//
// 手册只说"返回 12 个模型的 JSON"，没有给出确切的字段名，而 OpenAI 兼容
// 实现里 models 有两种常见形态：
//
//	{"data":[{"id":"...","context_window":N,"max_output_tokens":M}, ...]}
//	{"models":[{"id":"..."}, ...]}
//
// 两种都认，是**刻意的宽容**：本函数的产出只用于"更新静态快照"
// （见 Provider.Models），认不出来时回落到静态表即可，不会让功能失效。
// 反过来，如果只认一种而猜错了，实时目录就永远是空的 ——
// 一个静默失效的路径比一个宽松的解析器糟糕得多。
type modelsWire struct {
	Data   []modelEntry `json:"data"`
	Models []modelEntry `json:"models"`
}

type modelEntry struct {
	ID string `json:"id"`
	// 上下文与输出上限的字段名在各家实现里不统一，把见过的几种都收进来。
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
	MaxTokens       int `json:"max_tokens"`
	ContextLength   int `json:"context_length"`
	MaxOutputLength int `json:"max_output_length"`
}

// ModelList 拉取上游模型目录。
//
// 返回的 Model 只保证 ID 有效；上下文/上限拿不到时为 0（表示未知，
// 调用方应当回落到静态快照里的值，见 Provider.Models）。
func (c *Client) ModelList(ctx context.Context, a *Auth) ([]Model, error) {
	if a == nil || strings.TrimSpace(a.Session) == "" {
		return nil, fmt.Errorf("loomy: 凭证缺少 session，无法拉取模型目录")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+ModelsPath, nil)
	if err != nil {
		return nil, fmt.Errorf("loomy: 构造模型目录请求失败: %w", err)
	}
	applyModelsAuth(req.Header, a) // ⚠ 目录走 token:（不是 Bearer）
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy: 拉取模型目录失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return nil, fmt.Errorf("loomy: 读取模型目录响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("loomy: 模型目录返回 HTTP %d: %s",
			resp.StatusCode, summarize(raw))
	}

	var w modelsWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("loomy: 模型目录不是合法 JSON: %w（原文 %s）", err, summarize(raw))
	}
	entries := w.Data
	if len(entries) == 0 {
		entries = w.Models
	}
	out := make([]Model, 0, len(entries))
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		out = append(out, Model{
			ID:              id,
			ContextWindow:   firstPositive(e.ContextWindow, e.ContextLength),
			MaxOutputTokens: firstPositive(e.MaxOutputTokens, e.MaxTokens, e.MaxOutputLength),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("loomy: 模型目录为空（原文 %s）", summarize(raw))
	}
	return out, nil
}

// ── 对话 ────────────────────────────────────────────────────────────────

// ChatStream 发一次对话并返回**未解析的原始流**。
//
// 返回值语义与另两个上游对齐（这是 gateway.Provider.Chat 的契约）：
//
//	status 非 2xx 时**不返回 error**，而是把状态码与上游错误体一起交回，
//	由调用方统一判断"业务错误"（该换号还是该冷却）与"传输错误"。
//	只有网络层失败（连不上、超时、构造请求失败）才返回 err。
//
// 调用方负责 Close 返回的 ReadCloser（非 2xx 时返回的是一个包着错误体的
// NopCloser，同样可关）。
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	if a == nil || strings.TrimSpace(a.Session) == "" {
		return nil, 0, nil, fmt.Errorf("loomy: 凭证缺少 session，无法发起对话")
	}
	out := prepareBody(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+ChatPath, bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("loomy: 构造对话请求失败: %w", err)
	}
	applyChatAuth(req.Header, a) // ⚠ 对话走 Authorization: Bearer（不是 token:）
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("loomy: 对话请求失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 业务错误：读完错误体、关掉连接、把正文交回调用方。
		// 不在这里分类 —— 分类是 errorclassifier.go 的职责（纯函数，可单测）。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		return io.NopCloser(bytes.NewReader(raw)), resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ── 出站请求体改写 ──────────────────────────────────────────────────────

// prepareBody 在出站前改写请求体。
//
// 只做两件事，都是"上游会因此失败"或"显然是白费"的：
//
//  1. **强制 stream=true**。出口层要把上游的流按客户端要求重新编码
//     （流式或非流式），所以 Provider 这一层统一拿流 —— 与另两个上游一致。
//  2. **裁剪超限的 max_tokens / max_completion_tokens**。每个 Loomy 模型有
//     自己的输出上限（最大的是 MiniMax-M3 的 512000），客户端抄来的配置
//     很容易填一个通用大值（比如 1e6）。超出上限会被上游拒绝，而那是一次
//     完全白跑的往返。
//
// # 为什么用 UseNumber
//
// 不用它的话，`max_tokens: 1048576` 会在 map[string]any 里变成 float64，
// 重新编码时写成 `1.048576e+06` —— **同一个语义，不同的字面量**，
// 而 JSON 数字的科学计数法形态不是所有上游解析器都接受。
// json.Number 保留原始字面量，往返无损。
//
// # 解析失败时原样返回
//
// 与 internal/prompt 的 Rewrite 同一条理由：本函数的职责是"优化"，不是"校验"。
// 在这里造一个错误，等于把一个上游能诊断的问题变成一个我们自己编的问题。
func prepareBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return body
	}

	obj["stream"] = true

	if id, ok := obj["model"].(string); ok {
		if limit := MaxOutputFor(id); limit > 0 {
			clampTokenField(obj, "max_tokens", limit)
			clampTokenField(obj, "max_completion_tokens", limit)
		}
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// clampTokenField 把 obj[key] 夹到 limit 以内（只在它确实超限时才动）。
//
// 只在超限时改写：未超限时保留原始值（含它的字面量形态），
// 避免"每次请求都重写一遍数字"这种无意义的差异。
func clampTokenField(obj map[string]any, key string, limit int) {
	n, ok := obj[key].(json.Number)
	if !ok {
		return
	}
	v, err := n.Int64()
	if err != nil || v <= int64(limit) {
		return
	}
	obj[key] = json.Number(fmt.Sprintf("%d", limit))
}

// modelOf 从请求体里取模型名（取不到返回空串）。
func modelOf(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj struct {
		Model string `json:"model"`
	}
	if err := dec.Decode(&obj); err != nil {
		return ""
	}
	return obj.Model
}

// firstPositive 返回第一个 > 0 的整数（用于在多种候选字段名里挑一个有效值）。
func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// summarize 把一段可能很大的响应体压成一行可读摘要，供错误信息使用。
//
// 按**字符**边界截断而不是字节：上游错误体是中文，
// 按字节切会把一个汉字劈成两半，产出 \ufffd 乱码
// （本仓库在 handler.go 的 contentBlockMsg 上踩过这个坑）。
func summarize(raw []byte) string {
	const max = 300
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "(空)"
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
