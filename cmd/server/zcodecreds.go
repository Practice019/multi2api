// zcodecreds.go 把 ZCode 凭证并入核心账号池。
//
// 与 raccooncreds.go / clinecreds.go 同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/zcode"
)

// dedupeZCodeByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// # ⚠ 投影纪律（本仓踩过的坑，必须遵守）
//
// 核心账号只带 {UID, Nickname, FilePath}，**不要**把 ExpiresAt 抄进投影：
// 投影是**启动快照**，而活 secret 被续期后投影不会跟新 —— 喂一个会过期的
// 假 ExpiresAt 比 0 更糟（两个来源分叉，判"该不该续期"就会读错）。
// 过期的权威只走 `gateway.CredentialExpiryExt`（本包已实现 TokenExpiry）。
//
// 这条纪律在 refresh 那一轮是被实测代价换来的：出口层读池投影的
// ExpiresAt 判定续期，而投影里恒为 0 → 每个请求都续期。
func dedupeZCodeByUID(list []*zcode.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, za := range list {
		if za == nil {
			continue
		}
		uid := zcode.DisplayUID(za)
		if uid == "" {
			continue
		}
		secrets[uid] = za
		auths = append(auths, &auth.Auth{
			UID:      uid,
			Nickname: zcode.DisplayNameOf(za),
			FilePath: za.FilePath,
		})
	}
	return auths, secrets
}

// syncZCodeAccounts 把 ZCode 凭证目录里的账号并入核心账号池。
//
// 语义与其它上游完全对齐：
//   - 目录为空不是错误，但仍调一次 SyncToDirWithSecrets(nil, nil) 对账剔删
//     （否则删掉凭证文件后账号会留在池里变成幽灵号）
//   - 读目录目录失败只记日志、不动池子
func syncZCodeAccounts(p *pool.Pool, dir string) int {
	list, err := zcode.LoadDir(dir)
	if err != nil {
		log.Printf("zcode: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(zcode.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeZCodeByUID(list)
	p.SyncToDirWithSecrets(zcode.ProviderID, auths, secrets)
	return len(auths)
}
