// dialect.go MiMo 协议方言层：reasoning_content 的回注（出站）与聚合回写（入站）。
//
// # 协议硬点（评审报告 §2.1-1）
//
// thinking 模式下，带 tool_calls 的 assistant 历史消息**必须把 reasoning_content
// 原样回传**，否则 HTTP 400：
//
//	{"error":{"message":"Param Incorrect","param":"The reasoning_content in the
//	 thinking mode must be passed back to the API.","code":"400"}}
//
// 而我们的下游（普通 OpenAI 客户端）经常不回传（协议里它就不是必填字段）。
// 所以网关自己做记忆：**入站**把每轮流里的 reasoning_content 与 tool_calls 配对
// 缓存，**出站**把下游没带的 reasoning_content 回注进去。
//
// # 算法出处与本地化改动
//
// 主体照抄 Mintneko/mimo-proxy 的双策略（回注命中即用；未命中降级=剥掉该历史
// 消息的 tool_calls，把摘要并进 content），两处按本仓纪律改：
//   - 缓存键加**会话维度**（报告 R7：跨下游串会话会互相投毒）：
//     key = sha256(会话锚 ‖ tool_call.id)；
//   - 不做"降级即静默"——降级次数计数 + 日志（可观测性）。
//
// 会话锚=body 里第一条 user 消息内容的哈希前 16 位（同 mimo-code-proxy 会话键
// 思路）；同一会话的多轮请求里它稳定，跨会话几乎不碰撞。
package mimo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// 缓存容量与 TTL（Mintneko 实测参数：LRU 2000 / 7200s）。
const (
	reasoningCacheCap = 2000
	reasoningCacheTTL = 2 * time.Hour
)

// reasoningCache 每进程一份（Provider 级）。
//
// 不持久化：重启后第一轮带工具历史的会话会走降级路径——可接受
// （降级只丢"这一条历史消息的工具形态"，对话继续），换来的是零磁盘依赖。
type reasoningCache struct {
	mu    sync.Mutex
	items map[string]*reasoningEntry
	order []string // 简单 LRU：追加式 + 超容从头淘汰
}

type reasoningEntry struct {
	text string
	at   time.Time
}

func newReasoningCache() *reasoningCache {
	return &reasoningCache{items: make(map[string]*reasoningEntry)}
}

func (c *reasoningCache) key(sessionAnchor, toolCallID string) string {
	if sessionAnchor == "" || toolCallID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionAnchor + "\x00" + toolCallID))
	return hex.EncodeToString(sum[:])[:32]
}

// Put 记录一条 reasoning（按 tool_call.id 索引）。
func (c *reasoningCache) Put(anchor string, callIDs []string, reasoning string) {
	if c == nil || reasoning == "" {
		return
	}
	for _, id := range callIDs {
		k := c.key(anchor, id)
		if k == "" {
			continue
		}
		c.mu.Lock()
		if _, exists := c.items[k]; !exists {
			c.order = append(c.order, k)
			for len(c.order) > reasoningCacheCap {
				oldest := c.order[0]
				c.order = c.order[1:]
				delete(c.items, oldest)
			}
		}
		c.items[k] = &reasoningEntry{text: reasoning, at: time.Now()}
		c.mu.Unlock()
	}
}

// Get 取一条 reasoning（TTL 内有效）。
func (c *reasoningCache) Get(anchor, callID string) (string, bool) {
	if c == nil {
		return "", false
	}
	k := c.key(anchor, callID)
	if k == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[k]
	if !ok || time.Since(e.at) > reasoningCacheTTL {
		return "", false
	}
	return e.text, true
}

// sessionAnchorOf 从请求体取会话锚（第一条 user 消息哈希）。
func sessionAnchorOf(body []byte) string {
	var probe struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"` // string 或多模态数组
		} `json:"messages"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	for _, m := range probe.Messages {
		if m.Role != "user" {
			continue
		}
		s := flattenContent(m.Content)
		if s != "" {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:])[:16]
		}
	}
	return ""
}

func flattenContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, part := range t {
			if pm, ok := part.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// backfillOutbound 出站回注：给缺失 reasoning_content 的
// assistant(tool_calls) 历史消息补缓存值。
//
// 返回 (新 body, 补齐数, 缺失数)。缺失>0 时调用方可决定是否先降级重发
// （dialect 的 400 重试路径用）。body 解析失败 = 原样返回（不吞请求）。
func backfillOutbound(body []byte, anchor string, cache *reasoningCache) ([]byte, int, int) {
	if anchor == "" {
		return body, 0, 0
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, 0, 0
	}
	msgs, ok := doc["messages"].([]any)
	if !ok {
		return body, 0, 0
	}
	filled, missing := 0, 0
	changed := false
	for _, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		if m["role"] != "assistant" {
			continue
		}
		calls, ok := m["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
			continue // 下游自己带了，尊重它
		}
		// 收集这一条消息的 tool_call ids，逐个查缓存（同一条消息的多调用
		// 共享一份 reasoning：命中任一即用）。
		var ids []string
		for _, cv := range calls {
			if cm, ok := cv.(map[string]any); ok {
				if id, ok := cm["id"].(string); ok && id != "" {
					ids = append(ids, id)
				}
			}
		}
		hit := ""
		for _, id := range ids {
			if txt, ok := cache.Get(anchor, id); ok {
				hit = txt
				break
			}
		}
		if hit != "" {
			m["reasoning_content"] = hit
			filled++
			changed = true
		} else {
			missing++
		}
	}
	if !changed {
		return body, filled, missing
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body, filled, missing
	}
	return out, filled, missing
}

// downgradeToolHistory 降级出站：**本轮新请求之外**的历史里，把无 reasoning
// 的 assistant(tool_calls) 消息的 tool_calls 剥掉、content 缺失时补一句占位。
//
// 语义取舍（Mintneko 同款、报告 R7 的"冷启动未命中即降级=丢工具链"）：
// 只在**上游已用 400 明确拒绝**后使用——宁可丢历史里的工具形态（模型少一层
// 上下文），也不让整个请求 400 死掉。
func downgradeToolHistory(body []byte) ([]byte, int) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, 0
	}
	msgs, ok := doc["messages"].([]any)
	if !ok {
		return body, 0
	}
	stripped := 0
	for _, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		calls, ok := m["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		if rc, ok2 := m["reasoning_content"].(string); ok2 && rc != "" {
			continue // 有 reasoning 的保留原形态（它是合法的）
		}
		delete(m, "tool_calls")
		if c, ok2 := m["content"].(string); !ok2 || c == "" {
			m["content"] = "(called tools)"
		}
		stripped++
	}
	if stripped == 0 {
		return body, 0
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body, 0
	}
	return out, stripped
}

// isReasoningParamError 判定 400 是否是"reasoning_content 未回传"方言错误。
func isReasoningParamError(status int, body string) bool {
	if status != 400 {
		return false
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "reasoning_content") &&
		(strings.Contains(lower, "param incorrect") || strings.Contains(lower, "must be passed back"))
}

// 流内聚合（供 client 的旁路 reader 调用）--------------------------------

// streamAggregator 解析**出站方向的响应流**（上游→我们的 OpenAI SSE），
// 旁路收集 reasoning_content 与 tool_calls id 的配对。**不改变字节流**。
//
// 为什么要 tool_calls 增量拼接后的 id：SSE 里 id 只在第一个 delta 片段出现，
// 参数分很多片。我们只需要 id + 该 choice 的完整 reasoning。
type streamAggregator struct {
	reasoning strings.Builder
	callIDs   []string
}

func (s *streamAggregator) feedLine(line string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return // 不是我们的方言帧（如 MiMo 私有 {"type":"error"}）→ 忽略
	}
	for _, ch := range chunk.Choices {
		if ch.Delta.ReasoningContent != "" {
			s.reasoning.WriteString(ch.Delta.ReasoningContent)
		}
		for _, tc := range ch.Delta.ToolCalls {
			if tc.ID != "" {
				s.callIDs = append(s.callIDs, tc.ID)
			}
		}
	}
}

func (s *streamAggregator) finish(anchor string, cache *reasoningCache) {
	if s.reasoning.Len() == 0 || len(s.callIDs) == 0 || anchor == "" {
		return
	}
	cache.Put(anchor, s.callIDs, s.reasoning.String())
}
