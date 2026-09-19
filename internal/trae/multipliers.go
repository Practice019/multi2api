package trae

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：trae 实现模型倍率扩展点。
var _ gateway.ModelMultiplierExt = (*Provider)(nil)

// nameMultiplierRe 匹配模型展示名末尾的官方倍率后缀（全角/半角都认）。
var nameMultiplierRe = regexp.MustCompile(`[（(]x([0-9]+(?:\.[0-9]+)?)[）)]\s*$`)

// parseNameMultiplier 从官方展示名提取倍率；没有后缀或非法返回 (0, false)。
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

// ModelMultipliers 从**官方 get_detail_param 接口**的模型展示名解析倍率。
//
// 数据源：官方模型目录（FetchModels，get_detail_param）的 display_name ——
// 与 loomy 同款约定（倍率下放在展示名后缀，如 "xx（x1.0）"）。
// 若上游 display_name 不带该后缀（实测待凭证有效时确认），返回空表，
// 前端显示 x无 —— 不伪造本地估算值。
//
// 拉不到实时目录（凭证失效/网络错）时返回空表，**不估算**。
func (p *Provider) ModelMultipliers(ctx context.Context, cred gateway.Credential) (map[string]float64, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	live, lerr := p.client.FetchModels(ctx, a)
	if lerr != nil || len(live) == 0 {
		// 实时目录拿不到：没有官方数据源可解析，返回空表（调用方跳过）。
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
