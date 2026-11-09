package main

// codarcreds.go —— Codar（科搭）凭证并池。
//
// 上游包不依赖 internal/pool，所以"目录扫描 → 核心账号投影 + 不透明 secret"这一步
// 必须在装配层做（与其它上游完全同构）。

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/codar"
	"workbuddy2api/internal/pool"
)

// dedupeCodarByUID 把目录扫描结果投影成 (核心账号, 不透明 secret)。
//
// ⚠ 只投影 {UID, Nickname, FilePath}：**别顺手抄 ExpiresAt**。
// 投影是启动快照，活 secret 续期后投影不会跟新，喂一个会过期的假值比 0 更糟
// （过期权威只走 codar.Provider 的 CredentialExpiryExt）。
func dedupeCodarByUID(list []*codar.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, ca := range list {
		if ca == nil || ca.UID == "" {
			continue // 没有 uid 的号进池子只会变成幽灵号
		}
		secrets[ca.UID] = ca
		auths = append(auths, &auth.Auth{UID: ca.UID, Nickname: ca.Nickname, FilePath: ca.FilePath})
	}
	return auths, secrets
}

// syncCodarAccounts 扫描凭证目录并并池。
//
//   - 目录为空不是错误，但仍调一次 SyncToDirWithSecrets(nil, nil) 做**对账剔删**
//     （否则从目录删掉的号会永远留在池子里）。
//   - 去重在 codar.LoadDir 里已按 UID 做完（同一 UID 保留到期最晚的一份）。
func syncCodarAccounts(p *pool.Pool, dir string) int {
	list, err := codar.LoadDir(dir)
	if err != nil {
		log.Printf("codar: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(codar.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeCodarByUID(list)
	p.SyncToDirWithSecrets(codar.ProviderID, auths, secrets)
	return len(auths)
}
