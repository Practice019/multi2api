// lobsteraicreds.go 把 LobsterAI 凭证并入核心账号池。
//
// 与其它上游的 creds 文件同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做。
//
// # ⚠ 投影纪律
//
// 核心账号只带 {UID, Nickname}，**不要**把 ExpiresAt 抄进投影：
// 投影是启动快照，活 secret 被续期后投影不会跟新 —— 喂一个会过期的假
// ExpiresAt 比 0 更糟（分叉）。过期权威只走 gateway.CredentialExpiryExt。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/lobsterai"
	"workbuddy2api/internal/pool"
)

// dedupeLobsteraiByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
func dedupeLobsteraiByUID(list []*lobsterai.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, la := range list {
		if la == nil {
			continue
		}
		uid := la.UIDValue()
		if uid == "" {
			continue
		}
		secrets[uid] = la
		auths = append(auths, &auth.Auth{
			UID:      uid,
			Nickname: la.Nickname,
			FilePath: la.FilePath,
		})
	}
	return auths, secrets
}

// syncLobsteraiAccounts 把 LobsterAI 凭证目录里的账号并入核心账号池。
//
// 语义与其它上游完全对齐：
//   - 目录为空不是错误，但仍调一次 SyncToDirWithSecrets(nil, nil) 对账剔删
//     （否则删掉凭证文件后账号会留在池里变成幽灵号）
//   - 读目录失败只记日志、不动池子
func syncLobsteraiAccounts(p *pool.Pool, dir string) int {
	list, err := lobsterai.LoadDir(dir)
	if err != nil {
		log.Printf("lobsterai: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(lobsterai.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeLobsteraiByUID(list)
	p.SyncToDirWithSecrets(lobsterai.ProviderID, auths, secrets)
	return len(auths)
}
