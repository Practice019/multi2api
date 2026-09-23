// mimocreds.go 把 mimo 凭证并入核心账号池。
//
// 与 traecreds.go/loomycreds.go 同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做。
//
// # ⚠ 投影纪律（TRAE"账号突然过期"事故的根因之一）
//
// 核心账号只带 {UID, Nickname}，**不要**顺手把 ExpiresAt 抄进投影：
//
//   - auth.Auth.NeedsRefresh 对 ExpiresAt=0 恒真；
//   - 而"要不要刷"的判定（needsRefreshVia）读的就是这份投影；
//   - 投影是**启动快照**，活 secret 被后台/请求续期后投影不会跟新 ——
//     喂一个会过期的假 ExpiresAt 比 0 更糟（分叉）。
//
// 所以：过期权威只走 gateway.CredentialExpiryExt（问上游活 secret），
// 预检开关只走 RefreshSkew=(0,true)（mimo/extensions.go 事故档案）。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/mimo"
	"workbuddy2api/internal/pool"
)

// dedupeMimoByUID 按 uid 聚合（同 uid 多文件取 loggedInAt 较新者）。
func dedupeMimoByUID(list []*mimo.Auth) ([]*auth.Auth, map[string]any) {
	byUID := map[string]*mimo.Auth{}
	for _, a := range list {
		if a == nil || a.UID == "" {
			continue
		}
		if cur, ok := byUID[a.UID]; ok {
			if a.LoggedInAt <= cur.LoggedInAt {
				continue
			}
		}
		byUID[a.UID] = a
	}
	auths := make([]*auth.Auth, 0, len(byUID))
	secrets := make(map[string]any, len(byUID))
	for uid, a := range byUID {
		auths = append(auths, &auth.Auth{UID: uid, Nickname: a.Nickname, FilePath: a.FilePath})
		secrets[uid] = a
	}
	return auths, secrets
}

// syncMimoAccounts 并入账号池；返回实际生效账号数。
// 空目录也调一次 SyncToDirWithSecrets(nil,nil) 对账剔删（traecreds 同律）。
func syncMimoAccounts(p *pool.Pool, dir string) int {
	list, err := mimo.LoadDir(dir)
	if err != nil {
		log.Printf("mimo: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(mimo.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeMimoByUID(list)
	p.SyncToDirWithSecrets(mimo.ProviderID, auths, secrets)
	return len(auths)
}
