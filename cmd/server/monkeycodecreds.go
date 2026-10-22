// monkeycodecreds.go 把 MonkeyCode 凭证并入核心账号池。
//
// 与 zcodecreds.go / minimaxcreds.go 同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/monkeycode"
	"workbuddy2api/internal/pool"
)

// dedupeMonkeyCodeByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// # ⚠ 投影纪律（本仓踩过的坑，必须遵守）
//
// 核心账号只带 {UID, Nickname, FilePath}，**不要**把过期时刻抄进投影：
// 投影是**启动快照**，而活 secret 被更新后投影不会跟新 —— 喂一个会过期的
// 假时间比 0 更糟（两个来源分叉，判"该不该续期"就会读错）。
//
// # uid 的去重依据
//
// 用 Auth.UID()：优先上游 user.id（稳定），其次 session 短哈希。
// **不能**用 api_key —— 它可无限重签，重签一次就多一条幽灵账号。
func dedupeMonkeyCodeByUID(list []*monkeycode.Auth) ([]*auth.Auth, map[string]any) {
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

// syncMonkeyCodeAccounts 把 MonkeyCode 凭证目录里的账号并入核心账号池。
//
// 语义与其它上游完全对齐：
//   - 目录为空**不是错误**，但仍调一次 SyncToDirWithSecrets(nil, nil) 对账剔删
//     （否则删掉凭证文件后账号会留在池里变成幽灵号）
//   - 读目录失败只记日志、不动池子
func syncMonkeyCodeAccounts(p *pool.Pool, dir string) int {
	list, err := monkeycode.LoadDir(dir)
	if err != nil {
		log.Printf("monkeycode: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(monkeycode.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeMonkeyCodeByUID(list)
	p.SyncToDirWithSecrets(monkeycode.ProviderID, auths, secrets)
	return len(auths)
}
