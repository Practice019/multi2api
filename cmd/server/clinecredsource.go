// clinecredsource.go 按 uid 取 Cline 活凭证（余额端点用）。
//
// # 为什么不能用账号池的 AuthByUID
//
// `AuthByUID` 返回的是**核心账号投影**（只有 uid / nickname / filePath），
// 而余额查询要 `*cline.Auth`（含 access_token 与 **account_id**）——
// 那个东西在 `pool.SecretOf` 里（凭证并池时由 SyncToDirWithSecrets 装进去）。
//
// ⚠ 尤其要紧的是 account_id：余额端点必须用它（`usr-…`），
// 不能用 JWT 的 sub（`user_…`）—— 实测传 sub 返回
// `400 {"error":"Invalid request format"}`。投影里没有这个字段。
package main

import (
	"workbuddy2api/internal/cline"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// clineCredSource 造一个"按 uid 取活凭证"的访问器。
func clineCredSource(p *pool.Pool) func(string) (gateway.Credential, bool) {
	return func(uid string) (gateway.Credential, bool) {
		secret, ok := p.SecretOf(uid)
		if !ok {
			return gateway.Credential{}, false
		}
		a, ok := secret.(*cline.Auth)
		if !ok || a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider: cline.ProviderID,
			UID:      uid,
			Nickname: a.Nickname,
			Secret:   a,
		}, true
	}
}
