package raccoon

import "workbuddy2api/internal/gateway"

// accountcolumns.go —— raccoon 自报「账号池里我要哪些列」。
//
// # 为什么要裁剪（用户报的：有些列恒空，不该显示）
//
// 不实现本扩展点 = 用核心默认列集（11 列）。而 raccoon 撑不起 checkin：
//
//	checkin「今日签到」 → raccoon **没有**签到端点
//
// ⚠ 这里有个**容易搞错**的点：raccoon 确实有"桌面端登录奖励"
//（`POST …/login/points/grant`），但它**不是每日签到** ——
//
//	新人注册礼包  3000  注册时服务端自动发放        不涉及
//	桌面端登录奖励 3000  一次性（每号一次）          ✅ 已实现
//	每日积分发放   300   服务端按日自动发放，**无端点** ❌ 不可调用
//
// 「每日 300」是服务端按日自动发的（实测该账号 13:30 注册、13:31 就收到
// `daily_grant` 账单），**没有可调用的端点** —— 做成签到按钮必然失败。
//
// 而"登录奖励"是**幂等一次性**的（已领过返回 `granted:false`），
// 语义同「新手任务」，不是「每日可领」。所以：
//
//	Caps 不声明 CapCheckin（同一个判断的另一面）
//	本列集去掉 checkin（否则用户以为"今天还没签到"）
//
// 登录奖励的状态走 onboarding 端点（/raccoon/onboarding），不在这一列。
//
// # 保留的列
//
//	quota 积分余额（/points/v1/balance，已实现，各池分开列）
//
// 编译期断言。
var _ gateway.AccountColumnsExt = (*Provider)(nil)

// AccountColumns 返回 raccoon 要显示的列。
//
// 从默认列集出发去掉不适用的一列 —— 核心将来加列时自动跟随。
func (p *Provider) AccountColumns() []string {
	cols := gateway.DefaultAccountColumns()
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		// raccoon 没有每日签到（登录奖励是一次性的，不走这一列）。
		if c == gateway.AccountColCheckin {
			continue
		}
		out = append(out, c)
	}
	return out
}
