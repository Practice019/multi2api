package cline

import "workbuddy2api/internal/gateway"

// accountcolumns.go —— cline 自报「账号池里我要哪些列」。
//
// # 为什么要裁剪（用户报的：有些列恒空，不该显示）
//
// 不实现本扩展点 = 用核心默认列集（11 列，workbuddy 的形态）。
// 而 cline 撑不起其中一列：
//
//	checkin「今日签到」 → cline **没有**签到端点
//
// Caps() 里不声明 CapCheckin 就是同一件事的另一面 ——
// 整个 sidecar 做字符串扫描，checkin / daily / campaign 均无 Cline
// 业务端点命中（campaign 的命中是 PostHog 的 UTM 参数与 feature-flag 属性，
// daily 是 YAML cron 别名）。**声明了就等于给前端画一个点了必然失败的按钮**，
// 显示一个恒为 `—` 的列同理：用户会以为"今天还没签到"。
//
// # 保留的列
//
//	provider / nickname / uid  账号标识
//	quota                      余额（/api/v1/users/{id}/balance，已实现）
//	status                     正常/冷却/禁用/熔断
//	token                      剩余有效期（cline token 只活 1 小时，这列有用）
//	success / ops              成功次数 / 操作
//
// 编译期断言（与 CredentialRefresher / RefreshSkewExt 等并列）。
var _ gateway.AccountColumnsExt = (*Provider)(nil)

// AccountColumns 返回 cline 要显示的列。
//
// 从默认列集出发**去掉**不适用的那列，而不是硬编码一份新列表 ——
// 于是核心将来给默认列集加列时，cline 自动跟随（不用改这里）。
// 这与 workbuddy 的 AccountColumns 同一模式。
func (p *Provider) AccountColumns() []string {
	cols := gateway.DefaultAccountColumns()
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		// cline 没有签到端点：这一列恒为 `—`，显示它等于误导用户。
		if c == gateway.AccountColCheckin {
			continue
		}
		out = append(out, c)
	}
	return out
}
