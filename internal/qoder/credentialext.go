package qoder

import (
	"time"

	"workbuddy2api/internal/gateway"
)

// credentialext.go —— qoder 通过 `gateway.CredentialExpiryExt` 自报
// 「这份凭证什么时候过期」，供界面渲染 `token` 列（剩余有效期）。
//
// # 为什么需要它（用户报的：Token 那列是空的）
//
// 账号池里 qoder / qodercn 那行「Token」显示 `—`，因为本上游此前**没有**
// 实现本扩展点，核心拿不到过期时刻 → 按"未知"渲染。
//
// ⚠ 这**不能**靠账号池投影里的 `ExpiresAt` 解决（那条路是坏的）：
//
//	核心 auth.Parse 读 `auth.expiresAt`（秒）
//	qoder 凭证的过期信息在 access_token 的 JWT 里（`exp`）
//
// 投影里的 ExpiresAt 因此恒为 0。
//
// 同一个 0 还有第二重后果：`auth.Auth.NeedsRefresh` 对 `ExpiresAt <= 0`
// **恒返回 true**，于是"要不要续期"恒判为"要"。两个问题同源，修法不同：
//
//	RefreshSkewExt      → 修"要不要刷"
//	CredentialExpiryExt → 修"界面显示什么"（本文件）
//
// 编译期断言。
var _ gateway.CredentialExpiryExt = (*Provider)(nil)

// TokenExpiry 报告 qoder 凭证的过期时刻（**Unix 秒**）。
//
// qoder 的过期信息**只在 token 的 JWT 里**（`exp`）——
// 凭证结构上没有独立的过期字段（见 Auth 的定义）。
//
// ⚠ 不知道时返回 `(0, false)`，**不编造 0** ——
// `(0, true)` 会被渲染成"1970 年已过期"，比 `—` 更糟。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return 0, false
	}
	// 落盘字段优先（若将来补了独立过期字段，这里自动用上）。
	if ms := a.ExpiresAtMS(); ms > 0 {
		return time.UnixMilli(ms).Unix(), true
	}
	if t := gateway.ParseJWTTimes(a.AccessToken); t.ExpiresAt > 0 {
		return t.ExpiresAt, true
	}
	return 0, false
}
