// loomycreds.go 把 loomy 凭证并入核心账号池。
//
// # 为什么这个文件比 codeartscreds.go 短一个数量级
//
// 那边 400 行的复杂度全部来自 **CodeArts 的凭证会变**：
//
//	STS 约 2 小时过期 → 要后台续期
//	refresh_token 是**消费型**的 → 必须有"进程内唯一对象"的单一所有者 store，
//	                              否则一次续期换回来的新凭证写不回池子（503 的根因）
//	DPoP 私钥参与续期 → 续期不能只从一个字段重建
//
// 而 loomy 的凭证**不会变**：一个 session 字符串，无 TTL、无 refresh token
// （证据见 internal/loomy/credential.go 的包注释）。
//
// 于是这里只需要做一件事：把目录扫出来的凭证投影成"核心认识的形状"。
// **没有 store、没有锁、没有就地更新** —— 因为没有任何东西会去改它。
//
// 这不是偷懒：给一个不会变的凭证配一套单一所有者机制，只会让读代码的人
// 去找那个并不存在的"写回路径"。缺什么就不写什么，本身就是设计说明。
package main

import (
	"log"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/pool"
)

// dedupeLoomyByUID 把目录扫描的结果投影成 (核心账号, 不透明 secret)。
//
// 返回值与 pool.SyncToDirWithSecrets 的入参一一对应：
//
//	auths   []*auth.Auth      —— 核心只读 uid / nickname（主键与展示名）
//	secrets map[uid]any       —— 上游私有凭证，核心只保管不解释
//
// # 为什么 secrets 用 map 而不是切片
//
// 池子按 uid 装 secret（SyncToDirWithSecrets 内部 `secrets[a.UID]`），
// 用 map 让"投影数组与 secret 数组下标错位"这一整类 bug 不可能发生 ——
// 两个切片靠下标对齐时，任何一次过滤/排序都可能让它们错开，
// 而错开的后果是"账号 A 用了账号 B 的凭证"：请求会成功或失败得很随机。
//
// # 为什么不去重
//
// 去重已经在 loomy.LoadDir 里做了（pickWinners 按 UID 取 LoggedInAt 较新者）。
// 这里再做一次就是**第二份判据**——而两份判据迟早分叉（codearts 那边
// 专门把 codeartsWinners 抽出来就是为了避免这个）。所以本函数只做投影。
func dedupeLoomyByUID(list []*loomy.Auth) ([]*auth.Auth, map[string]any) {
	auths := make([]*auth.Auth, 0, len(list))
	secrets := make(map[string]any, len(list))
	for _, la := range list {
		if la == nil || la.UID == "" {
			// LoadDir 已经保证 UID 非空（ParseCredential 会派生），
			// 这里只是防御：把空 UID 放进池子会得到一个无法按 uid 寻址的账号。
			continue
		}
		// 上游凭证本身作为 secret 交给池子保管（不透明，池子不读它的字段）。
		secrets[la.UID] = la
		// 核心只需要 uid（主键）与 nickname（展示），其余一律留在 secret 里。
		auths = append(auths, &auth.Auth{UID: la.UID, Nickname: la.Nickname, FilePath: la.FilePath})
	}
	return auths, secrets
}

// syncLoomyAccounts 把 loomy 凭证目录里的账号并入核心账号池。
//
// 返回并入的账号数（供启动日志）。
//
// # 语义与 syncCodeartsAccounts 完全对齐
//
//   - 目录为空**不是错误**：用户可能刚起网关、还没放凭证。
//     这时仍然调一次 SyncToDirWithSecrets(nil, nil) ——
//     它的语义是"扫描结果的全集就是池中应有的全集"，
//     所以传 nil 会把**已删除**的 loomy 账号从池里剔掉。
//     不这么做的话，删掉凭证文件后账号会留在池子里变成一个永远失败的幽灵号。
//   - 读目录失败只记日志、不动池子（宁可保持现状，也不要因一次 IO 抖动清空账号）
func syncLoomyAccounts(p *pool.Pool, dir string) int {
	list, err := loomy.LoadDir(dir)
	if err != nil {
		log.Printf("loomy: 读取凭证目录失败（账号池未并入）: %v", err)
		return 0
	}
	if len(list) == 0 {
		p.SyncToDirWithSecrets(loomy.ProviderID, nil, nil)
		return 0
	}
	auths, secrets := dedupeLoomyByUID(list)
	p.SyncToDirWithSecrets(loomy.ProviderID, auths, secrets)
	return len(auths)
}
