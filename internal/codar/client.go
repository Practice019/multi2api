package codar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// Client Codar 的 HTTP 客户端。
type Client struct {
	APIBase   string
	PayBase   string
	UserAgent string
	HTTP      *http.Client
}

// New 建 HTTP 客户端（与其它上游同名同义：这里是"建客户端"，不是"建 Provider"）。
func New() *Client {
	return &Client{
		APIBase:   DefaultAPIBase,
		PayBase:   DefaultPayBase,
		UserAgent: DefaultUserAgent,
		HTTP:      &http.Client{Timeout: 10 * time.Minute}, // 流式：给足
	}
}

// protocolOf 从请求体里读 model，判断它属于哪套协议。
//
// 依据 T-311 实测：目录里每个模型自带 protocol{type,path,authentication}
// （11 个 openai_chat_completions=bearer，4 个 claude 系 anthropic_messages=x-api-key）。
// 读不到 model 时按 OpenAI 处理（上游会回 400 missing model，原样带回给调用方）。
func protocolOf(body []byte) (path, model string) {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	if strings.HasPrefix(probe.Model, "claude-") {
		return "/v1/messages", probe.Model
	}
	return "/v1/chat/completions", probe.Model
}

// Chat 转发一次对话（字节级透传 body）。
func (c *Client) Chat(ctx context.Context, a *Auth, body []byte) (gateway.ChatStream, error) {
	base := strings.TrimRight(c.APIBase, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	path, _ := protocolOf(body)
	url := base + path

	// ★ 必须强制上游走流式（2026-11-08 真机踩出来的）：
	//
	// ChatStream.Body 的契约是"上游的**流**" —— 出口层一律按 SSE 解析它。
	// 若客户端要 stream:false 而我们照原样透传，上游会回**一整个 JSON**，
	// 出口层解析不到任何 data: 事件 ⇒ 502 {"code":"upstream_parse"}。
	// 所以统一让上游流式返回；客户端要非流式时由出口层聚合（与其它上游一致）。
	body = forceStream(body)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return gateway.ChatStream{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// ★ SSE 必须禁 gzip：压缩流会让上层按行解析 SSE 时拿到坏帧（T-311 实测 headers）。
	req.Header.Set("Accept-Encoding", "identity")
	ua := c.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	if path == "/v1/messages" {
		// Anthropic 系模型：目录标注 authentication=x-api-key
		req.Header.Set("x-api-key", a.Token)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}

	resp, err := c.http().Do(req)
	if err != nil {
		return gateway.ChatStream{}, err // 网络层失败才返回 error
	}
	// ⚠⚠ 非 2xx 时把**出站元信息**打进容器日志（2026-11-10 加）。
	//
	// # 为什么需要（一次没能定性的排障）
	//
	// 用户报「手动调用模型全是 200，但 DSH 调用就不行」。当时日志里已经有
	// `upstream_req`（body）与 `upstream_err`（上游原文），body 的各种组合
	// 逐项直连复测都成功 —— 于是差异**只可能在请求头**上，而核心拿不到
	// 上游私有的头（那是本包的事实，核心不该认识）。
	//
	// 所以这条日志只能在这里打：只有本包知道 endpoint / 鉴权头形态 / UA。
	//
	// ⚠ 不记 token（只记"用了哪种鉴权头"）：token 是凭证，日志会被导出与分享。
	// ⚠ 只在非 2xx 打，正常请求零噪音。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		authKind := "Bearer"
		if path == "/v1/messages" {
			authKind = "x-api-key"
		}
		log.Printf("codar: 上游 %d（uid=%s endpoint=%s auth=%s ua=%q bodyLen=%d）",
			resp.StatusCode, a.UID, path, authKind, ua, len(body))
	}
	return gateway.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
}

// FetchModels 拉模型目录（**免鉴权**）。失败返回 error，由 Provider.Models 回退静态目录。
func (c *Client) FetchModels(ctx context.Context, apiBase string) ([]gateway.ModelInfo, error) {
	base := strings.TrimRight(apiBase, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/codar/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codar: 模型目录返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return parseCatalog(raw)
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// forceStream 把请求体里的 stream 字段置为 true，**其余字段原样保留**。
//
// 为什么用 map[string]json.RawMessage 而不是解成结构体再拼回去：
// RawMessage 让每个字段的值**逐字节**保留（数字精度、嵌套结构、未知字段都不动），
// 只改我们明确要改的那一个键。解不动（非 JSON）就原样透传，让上游回它自己的错误。
func forceStream(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if v, ok := m["stream"]; ok && string(v) == "true" {
		return body
	}
	m["stream"] = json.RawMessage("true")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
