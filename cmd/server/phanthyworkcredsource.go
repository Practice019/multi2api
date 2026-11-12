// phanthyworkcredsource.go 按 uid 取 PhanthyWork 活凭证（后台续期 / 管理端点用）。
//
// # 为什么不能用账号池的 AuthByUID
//
// `AuthByUID` 返回的是**核心账号投影**（只有 uid / nickname / filePath），而本上游要的是
// `*phanthywork.Auth` —— 它装着 accessToken / refreshToken、**三把 ed25519 私钥**
// （identity / device / inference）与 180 秒推理令牌的进程内缓存。那个东西在
// `pool.SecretOf` 里（凭证并池时由 SyncToDirWithSecrets 装进去）。
//
// 与 minimaxCredSource / codarCredSource / raccoonCredSource 同一模式。
//
// # 本上游独有的两条"为什么必须是活对象"
//
//	① **刷新必须落在池里那个 `*Auth` 上**：本包用 `refreshGate`（credential.go 的
//	   并发闸）保证同账号 refresh **串行**，而它的副作用只能作用在"池里持有的那个对象"上。
//	   刷磁盘副本的后果有两层：界面（读池）仍显示旧状态，而且会**绕过串行闸**——
//	   本上游的 refreshToken 是**轮换**的（报告 §3 坑⑤），并发消费同一把 = 登录莫名失效
//	   （不是"慢一点"，是"旧 token 已被换掉"）。
//	② **access_token 只活 1800 秒**（其余上游常见小时级），所以"凭证要不要刷"这个判断
//	   必须拿池里那份**当前**的 `ExpiresAt` 去看 —— 投影是启动快照，几分钟后就失去意义
//	   （这也是并池投影刻意不抄 ExpiresAt 的原因，见 phanthyworkcreds.go）。
package main

import (
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/phanthywork"
	"workbuddy2api/internal/pool"
)

// phanthyworkCredSource 造一个"按 uid 取活凭证"的访问器。
//
// ⚠ 传给上游的 `Secret` 必须是**池里那个指针**（`p.SecretOf` 返回的就是它）——
// 理由见上面的 ①。
func phanthyworkCredSource(p *pool.Pool) func(string) (gateway.Credential, bool) {
	return func(uid string) (gateway.Credential, bool) {
		secret, ok := p.SecretOf(uid)
		if !ok {
			return gateway.Credential{}, false
		}
		a, ok := secret.(*phanthywork.Auth)
		if !ok || a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider: phanthywork.ProviderID,
			UID:      uid,
			// ⚠ 走方法 `Nickname()` 而不是字段 `NicknameValue`：本包对展示名有一个
			// "服务端脱敏手机号 → uid" 的收敛（credential.go 的 normalize），
			// 直接读字段会绕过它，界面上就会出现一行空白账号。
			Nickname: a.Nickname(),
			Secret:   a,
		}, true
	}
}

// phanthyworkUIDEnumerator 列出账号池里属于本上游的全部 uid。
//
// # 为什么需要它（zcode / minimax 都实测过的空列表缺陷）
//
// `phanthyworkCredSource` 只能**按名查**，枚举不出全部。后台续期任务要挨个账号
// 判断"该不该刷"，没有这个枚举器时它只能看到"没有账号"——
// 表现是 token 静静过期而任务日志一句都没有（本仓三次同类报障的形态）。
//
// # ⚠ 必须用 ListFor 而不是 AvailableUIDsFor
//
// `AvailableUIDsFor` 会**过滤掉不健康的账号**，而被禁 / 冷却中的账号恰恰是
// "续期能救回来"的那些（本上游的 access_token 只活 30 分钟，一个号冷却超过半小时
// 就会真的过期）。用过滤版会让需要续期的号正好不在名单里。
func phanthyworkUIDEnumerator(p *pool.Pool) func() []string {
	return func() []string {
		list := p.ListFor(phanthywork.ProviderID)
		out := make([]string, 0, len(list))
		for _, st := range list {
			out = append(out, st.UID)
		}
		return out
	}
}

// phanthyworkDisabledChecker 装配「这个号是否已被禁用」的判据。
//
// 用途：签到这类**批量动作**在打上游之前先问它 —— 已被禁用的号打上游只会白喂风控
// （设计梯级 L1「别做这件事」）。不接线时包内恒为 false，行为仍正确，只是多打几次请求。
//
// # ⚠ 判据必须走 ListFor 而不是 AvailableUIDsFor
//
// `AvailableUIDsFor` 会**把禁用号过滤掉**，于是回调查不到那个 uid、落到「未找到 ⇒ false」，
// 判据永远是 false（「名单里没有它」≠「它没被禁用」）。
//
// 代价说明：每次回调扫一遍本上游的账号列表（几十条量级），而调用点在**批量动作**里、
// 不在请求热路径上 —— 换来的是不改 pool 的公开面（加一个 IsDisabled 会把「禁用」这个
// 语义从池子泄漏进每个上游的装配代码）。
func phanthyworkDisabledChecker(p *pool.Pool) func(string) bool {
	return func(uid string) bool {
		if uid == "" {
			return false
		}
		for _, st := range p.ListFor(phanthywork.ProviderID) {
			if st.UID == uid {
				return st.Disabled
			}
		}
		return false
	}
}
