// minimaxcreds.go 把 MiniMax 凭证并入核心账号池。
//
// 与 zcodecreds.go / raccooncreds.go 同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/minimax"
	"workbuddy2api/internal/pool"
)

// dedupeMiniMaxByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// # ⚠ 投影纪律（本仓踩过的坑，必须遵守）
//
// 核心账号只带 {UID, Nickname, FilePath}，**不要**把过期时刻抄进投影：
// 投影是**启动快照**，而活 secret 被续期后投影不会跟新 —— 喂一个会过期的
// 假时间比 0 更糟（两个来源分叉，判"该不该续期"就会读错）。
// 过期的权威只走 `gateway.CredentialExpiryExt`（本包已实现 TokenExpiry）。
//
// 按 uid 去重：同一个 access_token 落盘多次只会得到一条池记录
// （本上游的 uid 就是 token 的短哈希，见 minimax.Auth.UID）。
func dedupeMiniMaxByUID(list []*minimax.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	seen := make(map[string]bool, len(list))
	for _, ma := range list {
		if ma == nil {
			continue
		}
		uid := ma.UID()
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		secrets[uid] = ma
		auths = append(auths, &auth.Auth{
			UID:      uid,
			Nickname: ma.DisplayName(),
			FilePath: ma.FilePath,
		})
	}
	return auths, secrets
}

// syncMiniMaxAccounts 把 MiniMax 凭证目录里的账号并入核心账号池。
//
// 语义与其它上游完全对齐：
//   - 目录为空**不是错误**，但仍调一次 SyncToDirWithSecrets(nil, nil) 对账剔删
//     （否则删掉凭证文件后账号会留在池里变成幽灵号）
//   - 读目录失败只记日志、不动池子
func syncMiniMaxAccounts(p *pool.Pool, dir string) int {
	list, err := minimax.LoadDir(dir)
	if err != nil {
		log.Printf("minimax: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(minimax.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeMiniMaxByUID(list)
	p.SyncToDirWithSecrets(minimax.ProviderID, auths, secrets)
	return len(auths)
}
