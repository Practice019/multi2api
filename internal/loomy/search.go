package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ── 联网搜索 ────────────────────────────────────────────────────────────

// Search 调上游的腾讯搜索端点。
//
// # 请求形状（实测，2026-10-03）
//
//	POST {base}/search/tencent
//	{"query":"…","Mode":0}
//
// ⚠ **`Mode` 必须是 int（0/1/2），不是字符串。** 实测传 `"natural"`
// 会得到 400：
//
//	{"error":{"code":"10011","message":"请求体解析失败: json: cannot unmarshal
//	 string into Go struct field TencentSearchRequest.mode of type int64"}}
//
// Loomy 客户端内部其实认字符串（其 `normalizeMode` 有 natural/vr/mixed
// → 0/1/2 的映射），但**那个映射发生在客户端侧、不在线上协议里**。
// 照抄客户端的入参形态会写出一个恒定 400 的调用 —— 这正是"读代码
// 而不打上游"会踩的坑。
//
// 响应（实测）：
//
//	{"Pages":[{…}],"Query":"…","RequestId":"…","Version":"…","points_consumed":N}
//
// # 鉴权：只发 Authorization（与生图端点不同，这是实测差异）
//
// 生图端点实测**两个头都认**（所以那里两个都发）；搜索端点实测
// **只用 Authorization 就通**。这里按**最小必要**发 ——
// 多发一个头一旦被上游将来用作别的语义，就是一个没人会预料到的行为变化。
func (c *Client) Search(ctx context.Context, a *Auth, query string, count int) ([]byte, int, error) {
	if a == nil || strings.TrimSpace(a.Session) == "" {
		return nil, 0, fmt.Errorf("loomy: 凭证缺少 session，无法搜索")
	}
	body := map[string]any{
		"query": query,
		"Mode":  searchModeNatural,
	}
	// count 可选。上游只接受 10/20/30/40/50（客户端侧校验的集合）。
	// 传 0 表示"用上游默认"—— 不塞这个字段比塞一个非法值好。
	if count > 0 && searchCountAllowed(count) {
		body["Cnt"] = count
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("loomy: 构造搜索请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+SearchPath, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, fmt.Errorf("loomy: 构造搜索请求失败: %w", err)
	}
	applyChatAuth(req.Header, a)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("loomy: 搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchRespBody))
	if err != nil {
		return nil, 0, fmt.Errorf("loomy: 读取搜索响应失败: %w", err)
	}
	return out, resp.StatusCode, nil
}

// searchModeNatural 搜索模式 0 = natural。
//
// 实测的三个取值（来自 Loomy 客户端的 normalizeMode）：
//
//	0  natural  自然语言检索
//	1  vr       结果带更多结构化字段
//	2  mixed    混合
//
// 本实现只用 0：它是客户端默认值，返回的 Pages 已含 content 等字段，
// 足够喂给模型。需要别的模式时再加。
const searchModeNatural = 0

// searchCountAllowed 报告 count 是否是上游允许的取值。
//
// 与客户端侧同一个集合（COUNT_OPTIONS）。不在集合里就**不发**这个字段
// 而不是发一个会被拒的值 —— 后者会让模型的一次合法调用变成 400。
func searchCountAllowed(n int) bool {
	switch n {
	case 10, 20, 30, 40, 50:
		return true
	}
	return false
}
