// phanthyworkcreds.go 把 PhanthyWork 凭证并入核心账号池。
//
// 与 clinecreds.go / codarcreds.go 同构：上游包不依赖 internal/pool，
// "目录扫描 → 核心账号投影 + 不透明 secret"由装配层做 —— 它同时握有 pool 与 phanthywork。
//
// ⚠ 本上游的 secret 是 `*phanthywork.Auth`，里面装着 accessToken/refreshToken、
// 三把 ed25519 私钥（identity/device/inference）与 180 秒推理令牌的内存缓存。
// 池子只搬运、不解释它（gateway.Credential.Secret 的契约）。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/phanthywork"
	"workbuddy2api/internal/pool"
)

// dedupePhanthyworkByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// 去重（同一 UID 保留"更好"的一份）与"uid 为空则跳过"都在 phanthywork.LoadDir
// 里做完了 —— 这里**不重复那套判据**（两份判据迟早分叉）。下面那次 uid 判空只是兜底：
// 万一 LoadDir 的过滤条件将来变了，池子里会立刻多出无法按 uid 寻址的幽灵号，
// 而那种号只在"某个号永远选不中"时才被人发现。
//
// # ⚠ 投影纪律：只给 {UID, Nickname, FilePath}
//
// **不要**顺手把到期时刻抄进投影（本仓 TRAE"账号突然过期"事故的根因之一）：
//
//   - auth.Auth.NeedsRefresh 对 ExpiresAt=0 恒真，而"要不要刷"读的就是这份投影；
//   - 投影是**启动快照**，活 secret 续期后投影不会跟新 ——
//     喂一个会过期的假值比 0 更糟。
//
// 本上游尤其容易踩：它的 access_token 只有 **1800 秒**（其余上游常见小时级），
// 一个"抄来的"到期值在启动几分钟后就变成误导。过期权威只走
// phanthywork.Provider 的 TokenExpiry（gateway.CredentialExpiryExt）。
func dedupePhanthyworkByUID(list []*phanthywork.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, pa := range list {
		if pa == nil {
			continue
		}
		uid := pa.UID()
		if uid == "" {
			continue // 没有 uid 就无法按 uid 寻址，放进池子只会产生幽灵号
		}
		secrets[uid] = pa
		// ⚠ 只投影 {UID, Nickname, FilePath}（理由见上）。
		// Nickname 走方法而不是字段：本包的展示名有一个"服务端脱敏手机号 → 空"的收敛，
		// 直接读字段会绕过它，界面上就会出现一行空白账号。
		auths = append(auths, &auth.Auth{
			UID:      uid,
			Nickname: pa.Nickname(),
			FilePath: pa.FilePath,
		})
	}
	return auths, secrets
}

// syncPhanthyworkAccounts 把 PhanthyWork 凭证目录里的账号并入核心账号池。
//
// # 语义与其它上游完全对齐
//
//   - 目录为空**不是错误**：用户可能刚起网关、还没放凭证。
//     这时仍然调一次 SyncToDirWithSecrets(nil, nil) ——
//     它的语义是"扫描结果的全集就是池中应有的全集"，
//     所以传 nil 会把**已删除**的 phanthywork 账号从池里剔掉。
//     不这么做的话，删掉凭证文件后账号会留在池子里变成一个永远失败的幽灵号。
//   - 读目录失败只记日志、不动池子（宁可保持现状，也不要因一次 IO 抖动清空账号）。
//
// ⚠ 本函数**不发任何网络请求**：设备注册（唯一那个写操作）与推理令牌换取都在
// 上游包里按需发生（每次启动都发一次注册是会触发上游设备风控的，见包内 registrationCooldown）。
func syncPhanthyworkAccounts(p *pool.Pool, dir string) int {
	list, err := phanthywork.LoadDir(dir)
	if err != nil {
		log.Printf("phanthywork: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(phanthywork.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupePhanthyworkByUID(list)
	p.SyncToDirWithSecrets(phanthywork.ProviderID, auths, secrets)
	return len(auths)
}
