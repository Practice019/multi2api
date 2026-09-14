// accountview.go Loomy 自报「账号池里我显示哪些列」与「我的凭证会不会过期」。
//
// # 为什么 loomy 要自报列（而不是沿用核心默认的 11 列）
//
// 核心默认那 11 列是**按 workbuddy 的事实**排的，其中至少三列对 loomy 是错的
// （用户实测报的正是这三格都是空的）：
//
//	「Token」     loomy 的凭证是一个 session 字符串，不是 Bearer accessToken。
//	              `accessToken` 恒为空 → `has_token=false` → 这一列永远是 `—`。
//
//	「今日签到」   loomy **没有签到端点**。日额度由服务端按自然日自动重置，
//	              没有任何"领取"动作（证据见 clientstore.go 的说明与手册第 4.3 节：
//	              09-12 → 09-14 整天没开客户端，再打开仍是满额 5000）。
//	              沿用默认列 → 这一列永远是 `—`，用户会以为"功能坏了"。
//
//	「Token 到期」 见下面 NeverExpires 的说明 —— 这一列**保留**，
//	              但它的答案不是 `—` 而是「永久」。
//
// 所以 loomy 报自己那套：去掉两列不适用的，换上它真的有的。
//
// # 首列与其他上游一致
//
// 与 codearts 同一处理（见 internal/codearts/accountview.go 的注释）：
// 第一列是 `provider`，与走默认列集的 workbuddy **逐列对齐**。
// 用户对 codearts 提的要求原文是「和其他上游的表格统一一下第一个列」——
// 那是**跨上游**的要求，所以这里也照做，而不是只改被点名的那一个。
package loomy

import (
	"workbuddy2api/internal/gateway"
)

// 编译期断言：loomy 实现这三个扩展点。
//
// 与 provider.go 里那几条并列。少了它们不会编译失败（可选扩展点的缺席
// 在 Go 里是静默的），但**功能会静默降级**：列集回落成默认 11 列、
// 「Token 到期」永远显示 `—`。所以钉在编译期。
var (
	_ gateway.AccountColumnsExt     = (*Provider)(nil)
	_ gateway.CredentialLifetimeExt = (*Provider)(nil)
)

// AccountColumns 报 loomy 在账号池里要显示的列（有序）。
//
// # 为什么是这 8 列
//
//	上游 / 昵称 / UID   身份
//	额度               积分网关查到的可用总额度（见 quota_ext.go）
//	状态               核心状态（正常/冷却/禁用/熔断），与上游无关
//	Token 到期         走 CredentialLifetimeExt → 渲染成「永久」
//	成功               通用计数器
//	操作               行内动作（额度 / 禁用 / 移除…）
//
// 刻意**不含**：
//
//	「Token」      它的 accessToken 恒为空（凭证是 session），那一列永远是 `—`；
//	              用 token_expiry 代替（与 codearts 同一条判据）。
//	「今日签到」   它没有签到能力位（Caps 里不声明 CapCheckin）。
//	「福利」       它没有福利领取。
//	「熔断」「在途」 **用户明确要求删掉**（workbuddy 的默认列集也删了，见
//	              gateway.DefaultAccountColumns 的注释）—— 排障计数器
//	              几乎不变却占两列宽，账号池要"简单统一"。
//
// 返回值是切片字面量，每次调用都是新的（调用方可能排序，不该共享底层数组）。
func (p *Provider) AccountColumns() []string {
	return []string{
		gateway.AccountColProvider,
		gateway.AccountColNickname,
		gateway.AccountColUID,
		gateway.AccountColQuota,
		gateway.AccountColStatus,
		gateway.AccountColTokenExpiry,
		gateway.AccountColSuccess,
		gateway.AccountColOps,
	}
}

// NeverExpires 报告这份凭证是否**设计上就没有过期时间**（gateway.CredentialLifetimeExt）。
//
// # 为什么这是 loomy 的正确答案，而不是"补一个到期时刻"
//
// 用户报的是「token 到期时间 都没有做」。实测的结论是：**它压根没有到期时间**，
// 所以"补一列/补一个读数"是不可能的 —— 能做的是把这个事实**如实显示出来**。
//
// 证据（手册第 2.2/2.3 节 + 本机 leveldb 实测）：
//
//	· `loomy-auth-session` 的 JSON 里**没有** refreshToken / expiresIn /
//	  tokenType 任何一个字段（实测原样：phone/maskedPhone/session/userid/loggedInAt）
//	· Local Storage 里该键只有 1 条历史，客户端重启 4 次（09-11/12/13/14）
//	  session 值**始终未变**，从未轮换
//	· 源码里没有 /auth/refresh、checkSession、sessionExpired 之类的判断
//	· 服务端 /models 响应无 expires/ttl 字段、响应头无 Expires
//	· 无效 session 的报错是「登录已失效，请重新登录」——**区分"无效"与"过期"**，
//	  说明服务端只做有效性查询，不做过期
//
// 也就是说：session 是服务端**持久化**的登录态，不绑时间。失效只可能发生在
// 主动登出、改密、服务端清库或被风控 —— 这三件事都不是"到点自动失效"。
//
// # 为什么用了一个参数却几乎不看它
//
// 唯一做不到的事是"看着一个非 loomy 凭证说它永久"：凭证类型不对时返回 false
// （拿不准就当未知）。这与 gateway.CredentialLifetimeExt 的硬要求一致：
// **不确定时必须返回 false** —— 把不确定报成「永久」是更强的错断言。
//
//	本包**刻意不实现** `CredentialExpiryExt`：它没有可报的到期时刻，
//
// 实现一个恒返回 (0,false) 的版本只会多一个"看起来在做检查"的空壳。
// 两个扩展点是独立发现的（`ExtOf` 各判一次），所以只实现后者完全成立 ——
// 核心的顺序本来就是"先问时刻，拿不到才问是不是永久"。
func (p *Provider) NeverExpires(cred gateway.Credential) bool {
	_, err := authOf(cred)
	return err == nil
}
