// accountview.go codearts 通过两个扩展点自报
// 「账号池里我显示哪些列」与「我的凭证什么时候过期」。
//
// # 为什么 codearts 要自报列（而不是沿用核心默认的 11 列）
//
// 核心默认那 11 列是**按 workbuddy 的事实**排的，其中至少两列对 codearts 是错的：
//
//	「今日签到」   codearts 没有签到端点（Caps 里都不声明 CapCheckin）。
//	              它的每日动作叫「领取福利」（见 dailyactions.go）。
//	              沿用默认列 → 这一列永远是 `—`，用户会以为"功能坏了"。
//
//	「Token」      codearts 的凭证不是 Bearer accessToken，而是
//	              AK/SK/SecurityToken 三元组 + 一次性 refresh_token。
//	              核心那份投影里 `AccessToken` 恒为空 → `has_token=false`
//	              → 这一列也是 `—`。而它**确实有过期时间**（STS 有效期），
//	              只是走 CredentialExpiryExt 才读得到（见下）。
//
// 所以 codearts 报自己那套：去掉不适用的，补上真的有的。
//
// # 为什么「Token 到期」是**新增**的列 id，而不是复用「Token」
//
// 两者语义不同，混用会让前端只能画一种形态：
//
//	token        剩余有效期 → 一个色彩分档的胶囊（workbuddy 用它）
//	token_expiry 到期时刻   → 绝对值 + 相对时间，且**短效凭证**需要 title
//	                          说明"这是 2 小时一轮的 STS"
//
// 而且 workbuddy 的「Token」列**逐字不能变**（用户明确要求复用现在的表头）。
// 复用它做 codearts 的列，就必须让那一列的渲染同时照顾两种凭证形态 ——
// 那正是"两个渲染点互相污染"的形态。
package codearts

import "workbuddy2api/internal/gateway"

// 编译期断言：codearts 实现这两个扩展点。
//
// 与 provider.go 里那两条（Provider / AdminExt）并列。不实现会在**编译期**
// 失败，而不是等到运行时"列神秘地没出现"（那类缺陷最难查：页面不报错，
// 只是少一列）。
var (
	_ gateway.AccountColumnsExt   = (*Provider)(nil)
	_ gateway.CredentialExpiryExt = (*Provider)(nil)
)

// AccountColumns 报 codearts 在账号池里要显示的列（有序）。
//
// # 为什么是这 7 列
//
//	昵称 / UID     身份
//	额度           它有 quota-probe 能力位，是真实可读的
//	状态           核心状态（正常/冷却/禁用/熔断），与上游无关
//	Token 到期     **本轮补上的** —— 见本文件顶部注释
//	福利           **本轮补上的** —— 今天领过没有 / 还能领几项
//	操作           行内动作（领取福利 / 禁用 / 移除…）
//
// 刻意**不含**：
//
//	「上游」       分组后同组同名，冗余
//	「今日签到」   它没有这个能力位
//	成功/熔断/在途 通用计数器。用户要求 codearts 那份"重写"、保持清爽；
//	              这三个数在 8 列里会占掉近一半宽度却几乎不变。
//	              （要恢复只需往下面这个数组里加三个 id，前端零改动。）
//
// 返回值是切片字面量，每次调用都是新的（调用方可能排序，不该共享底层数组）。
func (p *Provider) AccountColumns() []string {
	return []string{
		gateway.AccountColNickname,
		gateway.AccountColUID,
		gateway.AccountColQuota,
		gateway.AccountColStatus,
		gateway.AccountColTokenExpiry,
		gateway.AccountColWelfare,
		gateway.AccountColOps,
	}
}

// TokenExpiry 报这份 codearts 凭证的 STS 过期时刻（Unix 秒）。
//
// # 为什么读的是 Secret 而不是别的
//
// 核心把池子里那份**不透明的** secret 原样交回来，本包断言回 `*Auth`
// （与 Chat / RefreshCredential / RefreshSkew 同构）—— 它**就是**续期时
// 被原地更新的那个对象，所以这里读到的是**活的**值，不是启动时的快照。
//
// # 三种返回
//
//	凭证类型不对 / 凭据为空  → (0, false)  未知
//	ExpiresAt <= 0           → (0, false)  上游没给（手写凭证可能没有）
//	ExpiresAt > 0            → (ExpiresAt, true)
//
// ⚠ 不在这里发网络请求、也不触发续期：它是账号列表渲染路径上的调用，
// 每个账号一次。续期有它自己的后台任务与请求路径两条通道。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return 0, false
	}
	if a.ExpiresAt <= 0 {
		// 没有可读的过期时间 —— **必须**与"过期时刻是 0"区分开，
		// 否则前端会把它渲染成"1970 年就过期了"（比不显示更糟）。
		return 0, false
	}
	return a.ExpiresAt, true
}
