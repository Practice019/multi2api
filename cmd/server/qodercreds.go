// qodercreds.go 把 Qoder 凭证并入核心账号池（两个产品共用一个函数）。
//
// 与其它上游的 creds 文件同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做。
//
// # ⚠ 两个产品共用同一个目录
//
// qoder 与 qodercn 是同协议族，凭证放在同一目录（`auths/qoder/`），
// 靠凭证里的 product_id 区分。装配层按实例调 LoadDirFor 过滤。
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
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/qoder"
)

// dedupeQoderByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
func dedupeQoderByUID(list []*qoder.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, qa := range list {
		if qa == nil {
			continue
		}
		uid := qa.UIDValue()
		if uid == "" {
			continue
		}
		secrets[uid] = qa
		auths = append(auths, &auth.Auth{
			UID:      uid,
			Nickname: qa.Nickname,
			FilePath: qa.FilePath,
		})
	}
	return auths, secrets
}

// syncQoderAccounts 把某产品的 Qoder 凭证并入核心账号池。
//
// 语义与其它上游完全对齐：
//   - 目录为空不是错误，但仍调一次 SyncToDirWithSecrets(nil, nil) 对账剔删
//   - 读目录失败只记日志、不动池子
func syncQoderAccounts(p *pool.Pool, dir, productID string) int {
	list, err := qoder.LoadDirFor(dir, productID)
	if err != nil {
		log.Printf("qoder(%s): 读取凭证目录失败（账号池未并入）: %v", productID, err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(productID, nil, nil)
		return 0
	}
	auths, secrets := dedupeQoderByUID(list)
	p.SyncToDirWithSecrets(productID, auths, secrets)
	return len(auths)
}
