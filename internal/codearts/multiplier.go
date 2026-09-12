package codearts

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 模型倍率的**动态数据源**。
//
// 静态表（knownModels 里的 Multiplier）是实测快照，会随上游调价过期。
// 权威来源是 AgentCenter 的 agent detail 接口：
//
//	GET {engineBase}/v1/agent-center/agents/detail?agent_id=<id>
//	Headers: Agent-Type: AgentCenter, X-Language, area
//
// 响应 gpts.models[].credit[0].ratio_display 即倍率（如 "0.7x"）。
//
// 注意路径是 **/v1/agent-center/...**，不是 product.json 里
// gptsUrl 拼出来的 /PromptCenterService/v1/... ——后者实测 404，
// 商业版实际走的是短前缀（与内核日志里的调用一致）。
const (
	agentDetailPath = "/v1/agent-center/agents/detail"
	// defaultAgentID 是商业版的 CodeAgent（覆盖 4 个有倍率的模型）。
	defaultAgentID = "a8bcb36232554267a5142361cc25a393"
)

// multiplierCache 缓存从接口拉到的倍率。
type multiplierCache struct {
	mu      sync.RWMutex
	values  map[string]float64 // model_id -> 倍率
	fetched time.Time
	lastErr error
}

var multCache = &multiplierCache{}

// multiplierTTL 缓存有效期。
//
// 上游调价是低频事件，1 小时足够新鲜；同时避免每个请求都打接口。
const multiplierTTL = time.Hour

// agentDetailResp 是 detail 接口的相关片段。
type agentDetailResp struct {
	Gpts struct {
		Models []struct {
			ModelAlias string `json:"model_alias"`
			ModelName  string `json:"model_name"`
			Credit     []struct {
				RatioDisplay string `json:"ratio_display"`
			} `json:"credit"`
		} `json:"models"`
	} `json:"gpts"`
}

// FetchMultipliers 从上游拉取模型倍率（带 1h 缓存）。
//
// 失败时返回上一次成功的值（若有），不让上游抖动影响管理台展示。
func (c *Client) FetchMultipliers(a *Auth) (map[string]float64, error) {
	multCache.mu.RLock()
	if multCache.values != nil && time.Since(multCache.fetched) < multiplierTTL {
		v := multCache.values
		multCache.mu.RUnlock()
		return v, nil
	}
	stale := multCache.values
	multCache.mu.RUnlock()

	url := c.engineBase() + agentDetailPath + "?agent_id=" + defaultAgentID
	hdr := map[string]string{
		"Agent-Type": "AgentCenter",
		"X-Language": "zh-cn",
		"area":       "green",
	}
	body, status, err := SignedGet(c, a, url, hdr)
	if err != nil || status >= 400 {
		multCache.mu.Lock()
		multCache.lastErr = fmt.Errorf("拉取倍率失败: status=%d err=%v", status, err)
		multCache.mu.Unlock()
		if stale != nil {
			return stale, nil // 降级用旧值
		}
		return nil, multCache.lastErr
	}

	var resp agentDetailResp
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		if stale != nil {
			return stale, nil
		}
		return nil, fmt.Errorf("解析倍率响应: %w", err)
	}

	out := make(map[string]float64, len(resp.Gpts.Models))
	for _, m := range resp.Gpts.Models {
		id := m.ModelAlias
		if id == "" {
			id = m.ModelName
		}
		if id == "" || len(m.Credit) == 0 {
			continue
		}
		if v, ok := parseRatioDisplay(m.Credit[0].RatioDisplay); ok {
			out[id] = v
		}
	}

	multCache.mu.Lock()
	multCache.values = out
	multCache.fetched = time.Now()
	multCache.lastErr = nil
	multCache.mu.Unlock()
	return out, nil
}

// parseRatioDisplay 把 "0.7x" 解析成 0.7。
//
// 上游格式固定为 "<数字>x"；解析失败返回 ok=false，
// 调用方据此回落到静态表，而不是把解析失败当成 0（那会被读成免费）。
func parseRatioDisplay(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	s = strings.TrimSuffix(strings.ToLower(s), "x")
	var v float64
	if _, err := fmt.Sscanf(s, "%f", &v); err != nil {
		return 0, false
	}
	return v, true
}

// ResetMultiplierCache 清空缓存（供 /admin/models/refresh 调用）。
func ResetMultiplierCache() {
	multCache.mu.Lock()
	multCache.values = nil
	multCache.fetched = time.Time{}
	multCache.mu.Unlock()
}
