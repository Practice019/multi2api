// raccooncreds.go 把 Raccoon 凭证并入核心账号池。
//
// 与 clinecreds.go 同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/raccoon"
)

// dedupeRaccoonByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// # ⚠ 投影纪律
//
// 核心账号只带 {UID, Nickname}，**不要**顺手把 ExpiresAt 抄进投影：
// 投影是启动快照，活 secret 被续期后投影不会跟新 —— 喂一个会过期的假
// ExpiresAt 比 0 更糟（分叉）。过期权威只走 gateway.CredentialExpiryExt。
//
// # Nickname 用 DisplayUID 而不是裸 Nickname
//
// Raccoon 的 nickname 是服务端**自动生成**的默认名（实测形如 RaccoonAva），
// 微信扫码不回传昵称 —— 多账号时很可能重名，只显示它分不清谁是谁。
// DisplayUID 会拼上手机号后 4 位或 user_id。
func dedupeRaccoonByUID(list []*raccoon.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, ra := range list {
		if ra == nil {
			continue
		}
		uid := ra.UID()
		if uid == "" {
			continue
		}
		secrets[uid] = ra
		auths = append(auths, &auth.Auth{
			UID:      uid,
			Nickname: ra.DisplayUID(),
			FilePath: ra.FilePath,
		})
	}
	return auths, secrets
}

// syncRaccoonAccounts 把 Raccoon 凭证目录里的账号并入核心账号池。
//
// 语义与其它上游完全对齐：
//   - 目录为空不是错误，但仍调一次 SyncToDirWithSecrets(nil, nil) 对账剔删
//     （否则删掉凭证文件后账号会留在池里变成幽灵号）
//   - 读目录失败只记日志、不动池子
func syncRaccoonAccounts(p *pool.Pool, dir string) int {
	list, err := raccoon.LoadDir(dir)
	if err != nil {
		log.Printf("raccoon: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(raccoon.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeRaccoonByUID(list)
	p.SyncToDirWithSecrets(raccoon.ProviderID, auths, secrets)
	return len(auths)
}
