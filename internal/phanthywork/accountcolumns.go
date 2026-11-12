// accountcolumns.go 账号池里本上游要显示的列（gateway.AccountColumnsExt）。
//
// # 本轮为什么要实现它（用户诉求是"按钮/列太少"）
//
// 默认列集是 9 列，对绝大多数上游够用。但本上游本轮补上了一个**默认列里没有的**
// 数字：每日可领的签到积分（会按周期重置的那部分，见 quota.go 的 dailyFreeQuota）。
// 不显式加这一列，那个数字在界面上**无处显示** —— 用户只能看到钱包余额，
// 看不到"今天还能领 300"。
//
// ⚠ 只加这一列（free_quota），不加 welfare：本上游**没有**福利中心这个概念，
// 加了就是一列恒为 `—` 的假列 —— 那与"不做假按钮"是同一条判据。
//
// # 为什么从 DefaultAccountColumns() 出发做插入，而不是硬编码一份列表
//
// 核心将来给默认列集加/删列时，本上游自动跟随（cline 的注释把这叫"同一模式"）。
// 硬编码一份就会在别人改默认集时悄悄漂移 —— 界面上表现为"这个上游少了一列"，
// 而没有任何报错。
package phanthywork

import "workbuddy2api/internal/gateway"

// 编译期断言。
var _ gateway.AccountColumnsExt = (*Provider)(nil)

// AccountColumns 返回本上游要显示的列。
//
// 在 `quota` 之后插入 `free_quota`（两列语义相邻：一个是不重置的钱包余额，
// 一个是每天重置的签到积分）。
func (p *Provider) AccountColumns() []string {
	cols := gateway.DefaultAccountColumns()
	out := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		out = append(out, c)
		if c == gateway.AccountColQuota {
			out = append(out, gateway.AccountColFreeQuota)
		}
	}
	return out
}
