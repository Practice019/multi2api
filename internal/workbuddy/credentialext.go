package workbuddy

// credentialext.go —— workbuddy 回答「这份凭证有什么」。
//
// # 为什么需要（两个字段都取不到）
//
// 账号列表的 `has_token` 与 `token_expire_sec` 都先读核心投影
// `Pool.AuthByUID()`（`*auth.Auth`）。而按上游分子目录之后，
// 管理台两条入池路径交给池子的是**裸投影**：
//
//	auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname})
//
// 真凭证在池的不透明 secret 通道里 —— 对 workbuddy，secret 就是
// `*auth.Auth` 本身（见 credentialloader.go 的 LoadCredentialsWithSecrets）。
//
// 于是第二个实例（workbuddy-intl）的账号在界面上：
//
//	has_token=false、token_expire_sec 缺      ← 实测
//
// 而磁盘上的凭证里 accessToken / refreshToken / expiresAt **全都在**。
// 用户据此以为"登录没拿到 token"，实际上完全正常。
//
// # 与 gateway.CredentialExpiryExt / CredentialTokenExt 的关系
//
// 两个扩展点同构：核心不解释 secret，问**拥有它的上游**。
// workbuddy 的实现只是把 secret 断言回自己的 `*auth.Auth` 再读字段 ——
// 这个断言只出现在**本包内**，核心一行都没碰（判据 3）。

import (
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// 编译期断言：两个可选扩展点都要认。
var (
	_ gateway.CredentialTokenExt  = (*Provider)(nil)
	_ gateway.CredentialExpiryExt = (*Provider)(nil)
)

// wbAuthOf 从池的不透明 secret 里取回 workbuddy 自己的凭证。
//
// 返回 nil 的两种情形都当"没有"处理，与调用方语义一致：
//   - secret 为 nil（账号只以投影入池，例如其它上游的号）
//   - secret 不是 *auth.Auth（不该发生；防御性返回 nil 而不是 panic）
func wbAuthOf(cred gateway.Credential) *auth.Auth {
	if a, ok := cred.Secret.(*auth.Auth); ok {
		return a
	}
	return nil
}

// HasToken 报告这份凭证是否带有可用的 access token。
//
// 只读、不发网络请求 —— 它是账号列表渲染路径上的调用。
func (p *Provider) HasToken(cred gateway.Credential) bool {
	a := wbAuthOf(cred)
	return a != nil && a.AccessToken != ""
}

// TokenExpiry 报告这份凭证的过期时刻（Unix 秒）。
//
// ⚠ 契约要求：`ok && at > 0` 才填字段（见 gateway.CredentialExpiryExt）。
// 这里 ExpiresAt<=0 一律回 ok=false —— 让界面显示 `—`（未知），
// 而不是把 0 当成"1970 年就过期了"。
func (p *Provider) TokenExpiry(cred gateway.Credential) (at int64, ok bool) {
	a := wbAuthOf(cred)
	if a == nil || a.ExpiresAt <= 0 {
		return 0, false
	}
	return a.ExpiresAt, true
}
