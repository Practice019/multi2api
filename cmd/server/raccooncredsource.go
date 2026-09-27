// raccooncredsource.go 按 uid 取 Raccoon 活凭证（余额与登录奖励端点用）。
//
// # 为什么不能用账号池的 AuthByUID
//
// `AuthByUID` 返回的是**核心账号投影**（只有 uid / nickname / filePath），
// 而余额与领取要 `*raccoon.Auth`（含 access_token 与 uid）——
// 那个东西在 `pool.SecretOf` 里（凭证并池时由 SyncToDirWithSecrets 装进去）。
package main

import (
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/raccoon"
)

// raccoonCredSource 造一个"按 uid 取活凭证"的访问器。
func raccoonCredSource(p *pool.Pool) func(string) (gateway.Credential, bool) {
	return func(uid string) (gateway.Credential, bool) {
		secret, ok := p.SecretOf(uid)
		if !ok {
			return gateway.Credential{}, false
		}
		a, ok := secret.(*raccoon.Auth)
		if !ok || a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider: raccoon.ProviderID,
			UID:      uid,
			Nickname: a.Nickname,
			Secret:   a,
		}, true
	}
}
