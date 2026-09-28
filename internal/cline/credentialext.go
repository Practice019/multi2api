package cline

import (
	"time"

	"workbuddy2api/internal/gateway"
)

// credentialext.go —— cline 通过 `gateway.CredentialExpiryExt` 自报
// 「这份凭证什么时候过期」，供界面渲染 `token` 列（剩余有效期）。
//
// # 为什么需要它（用户报的：Token 那列是空的）
//
// 账号池里 cline 那行「Token」显示 `—`。原因是 cline 此前**没有**实现
// 本扩展点，核心因此拿不到过期时刻 → 按"未知"渲染 `—`。
//
// ⚠ 这**不能**靠账号池投影里的 `ExpiresAt` 解决（那条路是坏的）：
//
//	核心 auth.Parse 读的是 `auth.expiresAt`（**秒**）
//	cline 落盘写的是 `auth.expire_time`（**毫秒**）
//
// 字段名不同 → 投影里的 ExpiresAt 恒为 0 → 核心按"未知"处理。
//
// 而这个 0 还有第二重后果：`auth.Auth.NeedsRefresh` 对 `ExpiresAt <= 0`
// **恒返回 true**，于是"要不要续期"恒判为"要"（见 refreshskew.go 的注释）。
// 两个问题同源，但**修法不同**：
//
//	RefreshSkewExt     → 修"要不要刷"（判据取自 token 自己的 iat/exp）
//	CredentialExpiryExt → 修"界面显示什么"（本文件）
//
// # 为什么读 token 而不是读那个字段
//
// 权威来源是**活凭证**（`cred.Secret` 里那份 `*Auth` 的 access_token）：
//
//	token 是 JWT，`exp` 就是真正的过期时刻（实测与 auth.expire_time 一致）
//	不依赖落盘字段名 → 不碰 auth.Parse 的兼容性（那是别家的事）
//
// 编译期断言。
var _ gateway.CredentialExpiryExt = (*Provider)(nil)

// TokenExpiry 报告 cline 凭证的过期时刻（**Unix 秒**）。
//
// 返回值语义（见 gateway.CredentialExpiryExt）：
//
//	ok=true,  at>0  → 到期时刻
//	ok=false        → 不知道，前端显示 `—`（**不要**编造 0）
//
// ⚠ 绝不能在不知道时返回 `(0, true)` —— 那会被渲染成"1970 年已过期"，
// 比显示 `—` 更糟（本项目实测过这个形态，见 gateway 的注释）。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return 0, false
	}
	// ① 落盘字段（毫秒）优先 —— 它在续期时会被更新，是"服务端最后一次告诉我们"的值。
	if a.ExpireTime > 0 {
		return time.UnixMilli(a.ExpireTime).Unix(), true
	}
	// ② 回落 token 自己的 `exp`（也覆盖"手工导入、没有 expire_time"的凭证）。
	if t := gateway.ParseJWTTimes(a.AccessToken); t.ExpiresAt > 0 {
		return t.ExpiresAt, true
	}
	return 0, false
}
