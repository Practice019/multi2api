// traecreds.go 把 trae 凭证并入核心账号池。
//
// 与 loomycreds.go 同构：上游包不得依赖 internal/pool，所以"把凭证投影成
// 核心账号 + 不透明 secret"这件事由装配层做 —— 它同时握有 pool 与 trae。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/trae"
)

// dedupeTraeByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// 去重按 UID：同一 uid 的凭证文件以**目录遍历顺序靠后**者为准
// （trae.LoadDir 已按文件名排序，后写的文件排后面 —— 落盘总是整体覆盖，
// 所以同 uid 多文件属于用户手工复制导致的重复，取较新者合理）。
func dedupeTraeByUID(list []*trae.Auth) ([]*auth.Auth, map[string]any) {
	byUID := make(map[string]*trae.Auth, len(list))
	for _, a := range list {
		if a == nil || a.UID == "" || a.AccessToken == "" {
			continue
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

// syncTraeAccounts 把 trae 凭证目录里的账号并入核心账号池。
func syncTraeAccounts(p *pool.Pool, dir string) int {
	list, err := trae.LoadDir(dir)
	if err != nil {
		log.Printf("trae: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		// 目录空/全不可用：显式并入空集 —— 让对账把**已删除**的 trae
		// 账号从池里剔掉（与 loomy 同一条判据）。
		p.SyncToDirWithSecrets(trae.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeTraeByUID(list)
	p.SyncToDirWithSecrets(trae.ProviderID, auths, secrets)
	return len(auths)
}
