// traecredsource.go 「取池里那份活 trae 凭证」的访问器。
//
// # 为什么必须有它（2026-10 实测确认的缺陷）
//
// TRAE 的后台续期原先走 `localAccounts()` = `LoadDir(authDir)`，而 `LoadDir`
// 用 `Parse(raw)` **每次新建对象**；池子持有的是启动时 `syncTraeAccounts`
// 放进去的**另一个** `*Auth`。于是续期成功但只更新了磁盘副本：
//
//	实测（internal/trae/zz_... 那条守卫的输出）
//	  上游交换次数=1
//	  池里那份 AccessToken = "old-access"   ← 没变
//	  磁盘那份 AccessToken = "new-access"   ← 变了
//
// 而 `TokenExpiry`（界面「Token」列的数据源）读的是**池里那份** ⇒
// 界面一直显示「已过期」，而磁盘上的 token 其实是新的。
//
// # 为什么这对 TRAE 尤其致命（而不是"偶尔显示不准"）
//
// TRAE 的 `RefreshSkew` 返回 **(0, true)** —— 显式声明"请求路径不预检续期"
// （理由见 extensions.go：refreshToken 是消费型的，预检会让每个请求都轮换
// 一次）。所以**后台是唯一的续期路径**：一旦它刷错对象，那个账号的界面状态
// 就永久停在过期上，直到进程重启（重启才重新 LoadDir 读到新 token）。
//
// # 为什么现在补上一个"访问器"而不是让 trae 包自己去读池
//
// 这是本仓的架构约定：`internal/gateway` 是唯一接缝，上游包**不能** import
// 核心（池）。所以由装配层（cmd/server）注入一个只读访问器，上游包通过它
// 拿到池里那个对象 —— 与 raccoon / cline / qoder / zcode / minimax 完全同款。
//
// ⚠ 这也解释了为什么 TRAE 是唯一漏掉这条接线的上游：它比 raccoon / minimax
// 先写（那时还没有这个约定），而后台续期是后来才补的（见 extensions.go 的
// JobRefresh）—— 补续期时只接了"失败通知"（OnRefreshFailure），漏了"活对象"。
package main

import (
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/trae"
)

// traeCredSource 返回"按 uid 取池里那份 *trae.Auth"的访问器。
//
// 与 raccoonCredSource 逐行同构（那份的注释同样适用）：
// 池子暴露的只是**核心投影**（uid/nickname/filePath），而本包要的是
// 含 accessToken 的那个 `*trae.Auth` —— 它在 `pool.SecretOf` 里。
func traeCredSource(p *pool.Pool) func(string) (gateway.Credential, bool) {
	return func(uid string) (gateway.Credential, bool) {
		secret, ok := p.SecretOf(uid)
		if !ok {
			return gateway.Credential{}, false
		}
		a, ok := secret.(*trae.Auth)
		if !ok || a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider: trae.ProviderID,
			UID:      uid,
			Nickname: a.Nickname,
			Secret:   a,
		}, true
	}
}
