// accountcolumns.go 本上游自报「账号池里要哪些列」。
//
// # 为什么需要它
//
// 不实现就用**核心默认列集**，而默认列集是 workbuddy 的形态，与本上游事实不符：
//
//	token / token_expiry  本上游的 JWT **自带 exp**，这两列有真值 → **要留**
//	                       （与 monkeycode 相反：它的 session 没有可展示的到期时刻）
//	checkin               本上游**没有签到动作** → 显示它只会恒为 `—`
//	welfare / growth / travel / tasks / invite  都没有 → 同理去掉
//
// # 保留哪些列
//
//	provider / nickname / uid  账号标识
//	quota                      剩余比例（见 quota.go）
//	free_quota                 同上（Verdent 只有一个账本，两列同值）
//	status                     正常/冷却/禁用/熔断
//	token / token_expiry       JWT 到期（**本上游真的有**）
//	success / ops              成功次数 / 操作
//
// ⚠ 「额度」与「免费额度」两列同值 —— 因为 Verdent 的额度**本来就是**
// 周期性的（5 小时 / 7 天窗口），没有"永久积分"与"每日赠送"之分。
// 这不是重复录入，是"这个上游只有一个账本"的如实表达。
package verdent

import "workbuddy2api/internal/gateway"

// 编译期断言。
var (
	_ gateway.AccountColumnsExt   = (*Provider)(nil)
	_ gateway.CredentialExpiryExt = (*Provider)(nil)
	_ gateway.CredentialTokenExt  = (*Provider)(nil)
)

// HasToken 这份凭证带不带可用的 access token（CredentialTokenExt）。
//
// # 为什么必须实现（不实现时 TOKEN 列恒 `—`）
//
// 账号列表里的 `has_token` 原来只读核心投影 `Pool.AuthByUID()` ——
// 而按上游分派的正确做法是**问拥有这份凭证的上游**（见 admin.credentialHasToken
// 的注释：workbuddy-intl 就因此显示过 `has_token=false`，尽管 secret 里明明有 token）。
//
// 本上游的判据很直接：token 非空且 JWT 能解出 user_id（= Usable()）。
//
// ⚠ 实现必须**只读、不发网络请求** —— 它是账号列表渲染路径上的调用，
// 每个账号一次。发请求会让打开面板变成一个 N 次网络往返的动作。
func (p *Provider) HasToken(cred gateway.Credential) bool {
	a, err := authOf(cred)
	return err == nil && a != nil && a.Usable()
}

// AccountColumns 返回本上游要显示的列。
//
// 从默认列集出发**去掉**不适用的，而不是硬编码一份新列表 ——
// 于是核心将来给默认列集加列时本上游自动跟随（照 monkeycode 的写法）。
func (p *Provider) AccountColumns() []string {
	cols := gateway.DefaultAccountColumns()
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		switch c {
		case gateway.AccountColCheckin,
			gateway.AccountColWelfare,
			gateway.AccountColBreaker,
			gateway.AccountColInFlight:
			// 本上游没有签到，也没有福利 —— 留着只会是恒 `—` 的空列。
			// 熔断/在途两列默认列集里本来就没有，写在这里是为了"如果将来加进来"。
			continue
		}
		out = append(out, c)
	}
	return out
}

// TokenExpiry 报出本账号的 JWT 到期时刻（AccountColumnsExt 的可选一部分）。
//
// # 为什么必须实现（不实现时那一列恒 `—`）
//
// 「Token」列的值**不是**来自池里的 ExpiresAt 字段，而是来自本方法 ——
// core 只认上游自己报的（见 admin.accountViews 的注释）。
// 上游最清楚自己那把凭证的到期语义，核心猜不了。
//
// 返回 (unix 秒, ok)：ok=false 表示"不知道"，界面显示 `—` 而不是一个假时间。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	// 先看核心投影里带的那个（装配层已经填过），再回落自己解 JWT。
	if cred.ExpiresAt.Unix() > 0 {
		return cred.ExpiresAt.Unix(), true
	}
	a, err := authOf(cred)
	if err != nil {
		return 0, false
	}
	if t := a.TokenExpiredAt(); !t.IsZero() {
		return t.Unix(), true
	}
	return 0, false
}
