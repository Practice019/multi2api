// clinecreds.go 把 Cline 凭证并入核心账号池。
//
// 与 traecreds.go / loomycreds.go 同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做 —— 它同时握有 pool 与 cline。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/cline"
	"workbuddy2api/internal/pool"
)

// dedupeClineByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// 去重按 uid（clipload 已在 LoadDir 里按可用性判据裁决，这里只做投影）——
// 再做一次就是**第二份判据**，而两份判据迟早分叉。
//
// # ⚠ 投影纪律（TRAE"账号突然过期"事故的根因之一）
//
// 核心账号只带 {UID, Nickname}，**不要**顺手把 ExpiresAt 抄进投影：
//
//   - auth.Auth.NeedsRefresh 对 ExpiresAt=0 恒真；
//   - 而"要不要刷"的判定读的就是这份投影；
//   - 投影是**启动快照**，活 secret 被后台/请求续期后投影不会跟新 ——
//     喂一个会过期的假 ExpiresAt 比 0 更糟（分叉）。
//
// 所以：过期权威只走 gateway.CredentialExpiryExt（问上游活 secret）。
func dedupeClineByUID(list []*cline.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, ca := range list {
		if ca == nil {
			continue
		}
		uid := ca.UID()
		if uid == "" {
			continue // 没有 uid 就无法按 uid 寻址，放进池子只会产生幽灵号
		}
		secrets[uid] = ca
		auths = append(auths, &auth.Auth{
			UID:      uid,
			Nickname: ca.Nickname,
			FilePath: ca.FilePath,
		})
	}
	return auths, secrets
}

// syncClineAccounts 把 Cline 凭证目录里的账号并入核心账号池。
//
// # 语义与其它上游完全对齐
//
//   - 目录为空**不是错误**：用户可能刚起网关、还没放凭证。
//     这时仍然调一次 SyncToDirWithSecrets(nil, nil) ——
//     它的语义是"扫描结果的全集就是池中应有的全集"，
//     所以传 nil 会把**已删除**的 cline 账号从池里剔掉。
//     不这么做的话，删掉凭证文件后账号会留在池子里变成一个永远失败的幽灵号。
//   - 读目录失败只记日志、不动池子（宁可保持现状，也不要因一次 IO 抖动清空账号）
func syncClineAccounts(p *pool.Pool, dir string) int {
	list, err := cline.LoadDir(dir)
	if err != nil {
		log.Printf("cline: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(cline.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeClineByUID(list)
	p.SyncToDirWithSecrets(cline.ProviderID, auths, secrets)
	return len(auths)
}
