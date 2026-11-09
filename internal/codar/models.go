package codar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/gateway"
)

// catalogResponse 上游 /v1/codar/models 的响应（免鉴权）。
type catalogResponse struct {
	SchemaVersion int `json:"schema_version"`
	Data          []struct {
		ID            string `json:"id"`
		DisplayName   string `json:"display_name"`
		ContextWindow int    `json:"context_window"`
		Protocol      struct {
			Type           string `json:"type"`
			Path           string `json:"path"`
			Authentication string `json:"authentication"`
		} `json:"protocol"`
	} `json:"data"`
}

// parseCatalog 解析目录响应 ⇒ gateway.ModelInfo 列表（ID 直接沿用上游 id）。
func parseCatalog(raw []byte) ([]gateway.ModelInfo, error) {
	var cr catalogResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, err
	}
	out := make([]gateway.ModelInfo, 0, len(cr.Data))
	seen := map[string]bool{}
	for _, m := range cr.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, gateway.ModelInfo{ID: m.ID, ContextWindow: m.ContextWindow})
	}
	return out, nil
}

// fallbackModels T-311 实测的 15 个模型 id（远端目录不可用时的兜底）。
//
// 为什么要有兜底：契约测试与"目录接口抖动"都不该让 /v1/models 变空
// （本仓教训：Models() 返回空会被判成"声明了 CapModels 却没实现"）。
func fallbackModels() []gateway.ModelInfo {
	ids := []string{
		"auto",
		"glm-5.3-flash", "glm-5.2", "glm-5.3",
		"gpt-6.1-sol", "gpt-6-sol", "gpt-5.6-sol", "gpt-6-astra",
		"deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-pro",
		"claude-opus-5-5", "claude-opus-5-5-kiro",
		"claude-sonnet-5-5", "claude-sonnet-5-5-kiro",
	}
	out := make([]gateway.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, gateway.ModelInfo{ID: id})
	}
	return out
}

// fetchCatalogRaw 拉目录原始 JSON（免鉴权）。共用给 parseCatalog 与 FetchPricing。
func (c *Client) fetchCatalogRaw(ctx context.Context, apiBase string) ([]byte, error) {
	base := strings.TrimRight(apiBase, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/codar/models", nil)
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codar: 模型目录返回 HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}
