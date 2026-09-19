package loomy

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：loomy 实现模型倍率扩展点。
var _ gateway.ModelMultiplierExt = (*Provider)(nil)

// nameMultiplierRe 匹配模型展示名末尾的官方倍率后缀：
//
//	"DeepSeek V4 Flash 0731（x3.0）" → 3.0
//	"GLM 5.3 Flash(x0.8)"           → 0.8
//	"Qwen 3.8 Max (x12.0)"          → 12.0
//
// 全角（（xN））与半角（(xN)）都要认 —— 实测上游两种都发。
var nameMultiplierRe = regexp.MustCompile(`[（(]x([0-9]+(?:\.[0-9]+)?)[）)]\s*$`)

// parseNameMultiplier 从官方展示名提取倍率；没有后缀或非法返回 (0, false)。
// 倍率是**上游在展示名里下发的官方值**（loomy 的 /models 没有独立价格字段）。
func parseNameMultiplier(name string) (float64, bool) {
	m := nameMultiplierRe.FindStringSubmatch(strings.TrimSpace(name))
	if len(m) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// ModelMultipliers 从**官方 /models 接口**的模型展示名解析倍率。
//
// 数据源：`GET /models` 的每个模型的 `name` 字段（如
// "DeepSeek V4 Flash 0731（x3.0）"）—— 官方把成本倍率下放在展示名里。
// 拉不到实时目录（凭证失效/网络错）时返回空表，**不估算**：
// 没有官方数据就不显示倍率（前端 x无），而不是伪造一个数。
func (p *Provider) ModelMultipliers(ctx context.Context, cred gateway.Credential) (map[string]float64, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	live, lerr := p.client.ModelList(ctx, a)
	if lerr != nil || len(live) == 0 {
		// 实时目录拿不到：没有官方数据源可解析，返回空表（调用方跳过）。
		if lerr != nil {
			return nil, nil
		}
		return nil, nil
	}
	out := map[string]float64{}
	for _, m := range live {
		if v, ok := parseNameMultiplier(m.Name); ok {
			out[m.ID] = v
		}
	}
	return out, nil
}
