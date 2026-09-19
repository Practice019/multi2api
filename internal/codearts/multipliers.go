package codearts

import (
	"context"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：codearts 实现模型倍率扩展点。
var _ gateway.ModelMultiplierExt = (*Provider)(nil)

// ModelMultipliers 返回 codearts 官方下发的模型倍率。
//
// 数据源是**官方接口下发的固定值**（knownModels.Multiplier = 模型配置块里的
// ratio_display，如 0.7 表示 0.7x）—— 不是本地估算、不随时间漂移。
// 只有 MultiplierKnown 的模型才进表（上游未下发的模型不冒充免费）。
//
// 无需凭证/网络：倍率是服务端随模型配置静态下发的同一份值。
func (p *Provider) ModelMultipliers(ctx context.Context, cred gateway.Credential) (map[string]float64, error) {
	out := map[string]float64{}
	for _, m := range AllModels() {
		if m.MultiplierKnown {
			out[m.ID] = m.Multiplier
		}
	}
	return out, nil
}
