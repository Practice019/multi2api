// zcodecredsource.go 按 uid 取 ZCode 活凭证（额度与计量端点用）。
//
// # 为什么不能用账号池的 AuthByUID
//
// `AuthByUID` 返回的是**核心账号投影**（只有 uid / nickname / filePath），
// 而额度查询要 `*zcode.Auth`（含 api_key / jwt / origin）——
// 那个东西在 `pool.SecretOf` 里（凭证并池时由 SyncToDirWithSecrets 装进去）。
//
// 与 raccoonCredSource 同一模式（本仓每个需要"用活令牌打上游"的上游都有这样一份）。
package main

import (
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/zcode"
)

// zcodeCredSource 造一个"按 uid 取活凭证"的访问器。
func zcodeCredSource(p *pool.Pool) func(string) (gateway.Credential, bool) {
	return func(uid string) (gateway.Credential, bool) {
		secret, ok := p.SecretOf(uid)
		if !ok {
			return gateway.Credential{}, false
		}
		a, ok := secret.(*zcode.Auth)
		if !ok || a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider: zcode.ProviderID,
			UID:      uid,
			Nickname: a.Nickname,
			Secret:   a,
		}, true
	}
}

// zcodeUIDEnumerator 列出账号池里属于本上游的全部 uid。
//
// # 为什么需要它（实测发现的空列表缺陷）
//
// `zcodeCredSource` 只能**按名查**，枚举不出全部。而诊断端点
// （`/admin/zcode/diagnose`、`/admin/zcode/quota`）要列出所有账号 ——
// 没有这个枚举器时它们返回 `{"accounts":[]}`：
// **HTTP 200 + 空数组**，看起来"没有账号"，而实际账号好好的。
// 一个静默的空列表比报错更难查，所以这个函数是必需的而不是锦上添花。
//
// # ⚠ 必须用 ListFor 而不是 AvailableUIDsFor
//
// `AvailableUIDsFor` 会**过滤掉不健康的账号** —— 那正是诊断端点最需要
// 看到的那些（冷却中的、被禁用的）。用它会让"账号为什么用不了"
// 这个问题在最该有答案的地方显示成"没有账号"。
//
// `ListFor` 返回**全部**账号的状态，这正是诊断的语义。
func zcodeUIDEnumerator(p *pool.Pool) func() []string {
	return func() []string {
		rows := p.ListFor(zcode.ProviderID)
		uids := make([]string, 0, len(rows))
		for _, r := range rows {
			if r.UID != "" {
				uids = append(uids, r.UID)
			}
		}
		return uids
	}
}
