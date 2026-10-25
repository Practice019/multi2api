// verdentcreds.go 把 Verdent 凭证并入核心账号池。
//
// 与 monkeycodecreds.go / zcodecreds.go 同构：上游包不依赖 internal/pool，
// "投影成核心账号 + 不透明 secret"由装配层做。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/verdent"
)

// dedupeVerdentByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// # ⚠ 投影纪律（本仓踩过的坑，必须遵守）
//
// 核心账号只带 {UID, Nickname, FilePath, ExpiresAt}。
//
// ⚠ **ExpiresAt 这一条与 monkeycode 那份不同**：那边刻意**不填**（它的 session
// 是 30 天硬过期且上游不声明时刻，抄进来只会是个假值）。而 Verdent 的 JWT
// **自带 exp**，是权威且稳定的 —— 不填反而让界面永远显示"不提供"，
// 而"这个号还有几天到期"恰恰是运维最需要看的一列。
//
// 判据：**上游声明了到期时刻就透传，没声明才留零值**。不是"一律不填"。
func dedupeVerdentByUID(list []*verdent.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	seen := make(map[string]bool, len(list))
	for _, va := range list {
		if va == nil {
			continue
		}
		uid := va.UID()
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		secrets[uid] = va
		// ⚠ 零值必须留 0，不能直接 .Unix()：time.Time{} 的 Unix() 是 -62135596800
		//（公元 1 年），填进去会让界面显示"已过期 2000 年"。
		var expSec int64
		if t := va.TokenExpiredAt(); !t.IsZero() {
			expSec = t.Unix()
		}
		auths = append(auths, &auth.Auth{
			UID:       uid,
			Nickname:  va.DisplayName(),
			FilePath:  va.FilePath,
			ExpiresAt: expSec, // auth.Auth 的 ExpiresAt 是 **Unix 秒**
		})
	}
	return auths, secrets
}

// syncVerdentAccounts 把 Verdent 凭证目录里的账号并入核心账号池。
//
// 语义与其它上游完全对齐：
//   - 目录为空**不是错误**，但仍调一次 SyncToDirWithSecrets(nil, nil) 对账剔删
//     （否则删掉凭证文件后账号会留在池里变成幽灵号）
//   - 读目录失败只记日志、不动池子
func syncVerdentAccounts(p *pool.Pool, dir string) int {
	list, err := verdent.LoadDir(dir)
	if err != nil {
		log.Printf("verdent: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(verdent.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeVerdentByUID(list)
	p.SyncToDirWithSecrets(verdent.ProviderID, auths, secrets)
	return len(auths)
}
