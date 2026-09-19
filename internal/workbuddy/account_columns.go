package workbuddy

import (
	"workbuddy2api/internal/gateway"
)

// 编译期断言：workbuddy 自报账号池列集。
var _ gateway.AccountColumnsExt = (*Provider)(nil)

// AccountColumns 自报账号池列集（有序，决定表头顺序）。
//
// 默认列集含「今日签到」（gateway.AccountColCheckin）。海外版
// （DisableGrowthTravel）没有签到玩法（product.json DisableCheckin=true），
// 那一列只会永远显示「—/今日已签」，纯属噪声 —— 这里去掉。
//
// 国内版实例返回默认列集原样（与改造前逐列一致，用户要求保持）。
func (p *Provider) AccountColumns() []string {
	cols := gateway.DefaultAccountColumns()
	if p == nil || !p.cfg.DisableGrowthTravel {
		return cols
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if c == gateway.AccountColCheckin {
			continue
		}
		out = append(out, c)
	}
	return out
}
