// models.go Verdent 的模型目录。
//
// # 两条来源，优先级分明
//
//	① 实时：GET https://llm-proxy.verdent.ai/config/model_list（Bearer 认证，需 token）
//	② 兜底：knownModels 静态表（register-machine 实测到的 28 条里的核心几条）
//
// # 为什么必须有兜底
//
// Models() 被调用的场合有三个：/v1/models 合并、面板展示、契约测试。
// 前两个都**带着真实凭证**，但契约测试拿的是伪造凭证 —— 那时网络调用必然失败。
// 如果只有 ①，契约会判"声明了 CapModels 却拿不到目录"，而实现其实是好的。
// （这是 monkeycode 那边踩过的同款：它的模型表就是静态的，理由相同。）
//
// # 为什么兜底表是"核心几条"而不是全部 28 条
//
// 全部 28 条里大半是变体（同一模型的 free/pro 档），抄全只会多一份要跟上游对齐的
// 常量表。真正的目录由 ① 实时给出，兜底只需要"够 /v1/models 不空、够契约通过"。
// ⚠ 上游改了模型名时，兜底表**不会**自动跟上 —— 这是刻意的取舍：
// 它的职责是"上游暂时不可达时别显示空目录"，不是"权威目录"。
package verdent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/gateway"
)

// knownModels 静态兜底目录（见文件头）。
//
// 来源：register-machine 的 verdent 账号档里 models 字段的实测值。
var knownModels = []gateway.ModelInfo{
	{ID: "deepseek-v4.1-flash-free", ContextWindow: 1000000},
	{ID: "deepseek-v4.1-flash", ContextWindow: 1000000},
	{ID: "deepseek-v4.1", ContextWindow: 1000000},
	{ID: "claude-sonnet-4.5", ContextWindow: 1000000},
	{ID: "claude-opus-4.5", ContextWindow: 1000000},
	{ID: "gpt-5.1", ContextWindow: 1000000},
	{ID: "gpt-5.1-codex", ContextWindow: 1000000},
	{ID: "gemini-3-pro", ContextWindow: 1000000},
	{ID: "glm-4.6", ContextWindow: 1000000},
	{ID: "kimi-k2", ContextWindow: 1000000},
}

// KnownModels 返回静态兜底目录的**副本**。
//
// 返回副本而不是包级切片：调用方（面板 / 测试）一个 append 就会把包级状态改坏。
func KnownModels() []gateway.ModelInfo {
	out := make([]gateway.ModelInfo, len(knownModels))
	copy(out, knownModels)
	return out
}

// modelListResp 上游 /config/model_list 的响应形状。
//
// ⚠ 只有 `data.model_config[].key` 是**实测确认**的路径；其余字段（context_window 之类）
// 上游给不给、叫什么名字都没有样本，所以用 any 兜住、按需探测，不要凭猜写死结构体。
type modelListResp struct {
	ErrCode int `json:"errCode"`
	Data    struct {
		ModelConfig []struct {
			Key           string `json:"key"`
			Name          string `json:"name"`
			ContextWindow int    `json:"context_window"`
			MaxTokens     int    `json:"max_tokens"`
		} `json:"model_config"`
	} `json:"data"`
}

// FetchModels 从上游拉模型目录。失败时返回 error（由调用方决定是否回落兜底）。
func (c *Client) FetchModels(ctx context.Context, token string) ([]gateway.ModelInfo, error) {
	status, raw, err := c.doJSON(ctx, token, modelListURL, "GET", nil)
	if err != nil {
		return nil, fmt.Errorf("verdent: 请求模型目录失败: %w", err)
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("verdent: 模型目录 HTTP %d：%s", status, truncate(string(raw), errSnippet))
	}
	var resp modelListResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("verdent: 解析模型目录失败: %w（片段：%s）", err, truncate(string(raw), errSnippet))
	}
	out := make([]gateway.ModelInfo, 0, len(resp.Data.ModelConfig))
	for _, m := range resp.Data.ModelConfig {
		key := strings.TrimSpace(m.Key)
		if key == "" {
			continue
		}
		out = append(out, gateway.ModelInfo{
			ID:              key,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxTokens,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("verdent: 模型目录为空（errCode=%d）", resp.ErrCode)
	}
	return out, nil
}

// truncate 截断到 n 字节，不切坏 UTF-8。
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
