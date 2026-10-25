// client.go Verdent 的 HTTP 出站：请求头拼法、加密、流式对话、模型/额度拉取。
//
// # 协议要点（全部来自 register-machine 的实测抓包，逐条都有代价）
//
//	端点   POST https://llm-proxy.verdent.ai/llm/stream
//	认证   Authorization: Bearer <jwt>  **且**  Cookie: token=<jwt>（双写，缺一可能被拒）
//	头部   verdent-proxy-beta: hybrid-stream@20250919   —— **缺了报 10004**
//	       agent_type: ts_agent / x-device-id / x-team-id / X-Device-Type / X-OS-Type / X-Version-Code
//	加密   system 与 messages **各自** AES-256-GCM 后 base64（见 crypto.go）
//
// # 三条"不报错但行为错"的坑（都写在代码里，别删）
//
//	① system **必须包含字符串 "Verdent"**，否则服务端回一个**伪装成 429 的校验失败**
//	   —— 看到 429 会以为是限流，然后去换号，而每个号都会一样地失败。
//	② messages 的 content **必须是数组块**（[{"type":"text","text":...}]），不能是字符串。
//	   传字符串**不报错**（HTTP 200），只是模型完全不看你的话（实测：问"1+1"回了首英文诗）。
//	③ id 必须有前缀（session_ / conv_ / model_agent_），react_type 是 "Main Agent"（带空格）。
package verdent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/wire"
)

// ProviderID 上游标识（注册表与模型前缀都用它）。
const ProviderID = "verdent"

// 端点。
const (
	streamPath = "https://llm-proxy.verdent.ai/llm/stream"
	// modelListURL 模型目录（GET，Bearer 认证）。
	modelListURL = "https://llm-proxy.verdent.ai/config/model_list"
	// usageURL 额度窗口（GET，Bearer 认证）。同时也是**最便宜的探活端点**。
	usageURL = "https://api.verdent.ai/verdent/usage_windows?team_id=0"
)

// 协议常量。
const (
	// proxyBetaHeader / proxyBetaValue 版本握手头。缺了报 10004。
	proxyBetaHeader = "verdent-proxy-beta"
	proxyBetaValue  = "hybrid-stream@20250919"
	// defaultDeviceID 客户端内嵌的固定设备 id（实测它不参与校验，只是要有个值）。
	defaultDeviceID = "8fc0ff8f-e984-4efc-8e36-9bcc7f7c6682"
	// modelCatalogVersion 客户端上报的目录版本号（实测不影响鉴权，但照抄更稳）。
	modelCatalogVersion = "model-catalog-1792857216432"
	// agentName / reactType 客户端标识。reactType **带空格**，写成 "main" 会被拒。
	agentName = "VerdentDeck"
	reactType = "Main Agent"
	// DefaultModel 上游默认模型（free 档）。
	DefaultModel = "deepseek-v4.1-flash-free"
	// defaultSystem 兜底 system。⚠ 含 "Verdent" 是硬要求，见文件头坑①。
	defaultSystem = "You are Verdent, a helpful AI assistant."
	// DefaultTeamID 团队 id。"0" = 个人空间（无团队）。
	DefaultTeamID = "0"

	// clientVersion 客户端版本号（头部 X-Version-Code）。
	clientVersion = "2.17.0"

	// bodyLimit 读上游响应体的硬上限（1MiB）。与其余上游同口径，防超大同页把我们 OOM。
	bodyLimit = 1 << 20
	// errSnippet 错误信息里回带的上游响应片段长度。
	errSnippet = 300
)

// Client Verdent 的出站客户端。
//
// 零值可用（用包级默认 http.Client）。测试注入 httptest 的 client。
type Client struct {
	// HTTP 自定义出站客户端。nil = 用带超时的默认客户端。
	HTTP *http.Client
	// StreamTimeout 流式对话的整体超时。0 = 默认 5 分钟。
	StreamTimeout time.Duration
}

// NewClient 构造一个出站客户端。
func NewClient() *Client { return &Client{} }

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

// defaultHTTP 包级默认客户端。
//
// ⚠ 超时给的是"整体"上限，不是每字节读超时：流式对话里两次 delta 之间
// 可能隔几十秒（模型在思考），按读超时掐会误杀长回答。
var defaultHTTP = &http.Client{Timeout: 5 * time.Minute}

// headersOf 统一请求头（Bearer + Cookie 双写）。
//
// 为什么双写：实测缺任一都可能被拒，而上游的拒绝不带"你缺了 Cookie"这种提示。
func headersOf(token string, extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Cookie", "token="+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	h.Set("X-Device-Type", "pc")
	h.Set("X-OS-Type", "windows")
	h.Set("X-Version-Code", clientVersion)
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// randUUID 生成一个 uuid v4（形状与 crypto.randomUUID 一致：小写带连字符）。
func randUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 到不了（crypto/rand 在现代系统上不失败）。真失败时也要给个可用的值，
		// 而不是让整个请求崩掉 —— id 的作用只是"别重复"，不参与校验。
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// doJSON 发一个普通 JSON 请求（模型目录 / 额度用）。
func (c *Client) doJSON(ctx context.Context, token, url, method string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header = headersOf(token, nil)
	if method == http.MethodGet {
		req.Header.Del("Content-Type")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// parseSystemAndMessages 从 OpenAI 请求体里取出 system 与消息，转成上游要的形状。
//
//	OpenAI                        Verdent
//	{"role":"system",   "content":"…"}  → 顶层 system（字符串，须含 "Verdent"）
//	{"role":"user",     "content":"…"}  → messages[].content = [{"type":"text","text":"…"}]  ← **数组块**
//
// ⚠ content 传字符串**不报错**（HTTP 200），只是模型完全不看你的话 —— 见文件头坑②。
// 所以这里一律转成数组块；多模态里的图片等块原样透传（上游认同一套形状）。
func parseSystemAndMessages(body []byte) (system string, messages []any, model string, err error) {
	model = wire.ModelOf(body)
	if model == "" {
		model = DefaultModel
	}
	var src struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return "", nil, model, fmt.Errorf("解析 OpenAI 请求体失败: %w", err)
	}
	var sysParts []string
	for _, m := range src.Messages {
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if t := textOf(m["content"]); t != "" {
				sysParts = append(sysParts, t)
			}
		default:
			messages = append(messages, map[string]any{
				"role":    role,
				"content": blocksOf(m["content"]), // ← 数组块，不是字符串
			})
		}
	}
	system = strings.Join(sysParts, "\n\n")
	return system, messages, model, nil
}

// textOf 把 content 压成一段纯文本（content 可能是字符串，也可能是块数组）。
func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, part := range t {
			if m, ok := part.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// blocksOf 把 content 归一成上游要的块数组。
//
// 输入是字符串 → [{"type":"text","text":<字符串>}]；
// 输入已经是数组 → 原样透传（多模态的图片块上游认同一套形状）。
func blocksOf(v any) []any {
	if s, ok := v.(string); ok {
		return []any{map[string]any{"type": "text", "text": s}}
	}
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{map[string]any{"type": "text", "text": ""}}
}

// ensureVerdentSystem 保证 system 含 "Verdent"（否则上游回伪装成 429 的校验失败）。
func ensureVerdentSystem(s string) string {
	if strings.Contains(strings.ToLower(s), "verdent") {
		return s
	}
	if strings.TrimSpace(s) == "" {
		return defaultSystem
	}
	return "You are Verdent. " + s
}
