// accountcolumns.go 本上游自报「账号池里要哪些列」。
//
// # 用户报的问题
//
// UI 里 monkeycode 这一组缺「Token」列，而且「今日签到」是空的。
// 根因是**没实现本扩展点** —— 不实现就用核心默认列集，
// 而默认列集是 workbuddy 的形态，与本上游事实不符：
//
//	token    本上游用 session cookie + oma/omas 三把凭据，没有 bearer 过期时间
//	         显示它只会是 `—`，用户会以为"凭证坏了"
//	checkin  **有**签到（已接共享驱动），所以这一列**要留**
//
// # 保留哪些列
//
//	provider / nickname / uid  账号标识
//	quota                      积分 + 每日 token（QuotaExt 已实现）
//	status                     正常/冷却/禁用
//	checkin                    今日签到（共享驱动 + checkinlog）
//	success / ops              成功次数 / 操作
//
// 去掉 token 与 token_expiry：本上游没有可展示的 bearer 到期时刻。
// （HasToken 仍实现，但那是给别处判"能不能用"，不是给这一列填值。）
package monkeycode

import "workbuddy2api/internal/gateway"

// 编译期断言：与 CredentialTokenExt / DailyActionExt 等并列。
var _ gateway.AccountColumnsExt = (*Provider)(nil)

// AccountColumns 返回本上游要显示的列。
//
// 从默认列集出发**去掉**不适用的，而不是硬编码一份新列表 ——
// 于是核心将来给默认列集加列时本上游自动跟随。
func (p *Provider) AccountColumns() []string {
	cols := gateway.DefaultAccountColumns()
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		switch c {
		case gateway.AccountColToken, gateway.AccountColTokenExpiry:
			// 没有可展示的 bearer 到期时刻：显示它只会恒为 `—`。
			continue
		}
		out = append(out, c)
	}
	return out
}
