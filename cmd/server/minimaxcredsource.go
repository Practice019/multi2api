// minimaxcredsource.go 按 uid 取 MiniMax 活凭证（额度/签到/推理用）。
//
// # 为什么不能用账号池的 AuthByUID
//
// `AuthByUID` 返回的是**核心账号投影**（只有 uid / nickname / filePath），
// 而本上游要 `*minimax.Auth`（含 access_token）—— 那个东西在
// `pool.SecretOf` 里（凭证并池时由 SyncToDirWithSecrets 装进去）。
//
// 与 raccoonCredSource / zcodeCredSource 同一模式。
package main

import (
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/minimax"
	"workbuddy2api/internal/pool"
)

// minimaxCredSource 造一个"按 uid 取活凭证"的访问器。
func minimaxCredSource(p *pool.Pool) func(string) (gateway.Credential, bool) {
	return func(uid string) (gateway.Credential, bool) {
		secret, ok := p.SecretOf(uid)
		if !ok {
			return gateway.Credential{}, false
		}
		a, ok := secret.(*minimax.Auth)
		if !ok || a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider: minimax.ProviderID,
			UID:      uid,
			Nickname: a.DisplayName(),
			Secret:   a,
		}, true
	}
}

// minimaxUIDEnumerator 列出账号池里属于本上游的全部 uid。
//
// # 为什么需要它（zcode 上实测过的空列表缺陷）
//
// `minimaxCredSource` 只能**按名查**，枚举不出全部。而管理端点
// （`/admin/minimax/quota`、`/admin/minimax/diagnose`）要列出所有账号 ——
// 没有这个枚举器时它们返回 `{"accounts":[]}`：
// **HTTP 200 + 空数组**，看起来"没有账号"，而实际账号好好的。
//
// # ⚠ 必须用 ListFor 而不是 AvailableUIDsFor
//
// `AvailableUIDsFor` 会**过滤掉不健康的账号** —— 那正是诊断端点最需要
// 看到的那些（冷却中的、被禁用的）。用它会让"账号为什么用不了"
// 在最该有答案的地方显示成"没有账号"。
func minimaxUIDEnumerator(p *pool.Pool) func() []string {
	return func() []string {
		list := p.ListFor(minimax.ProviderID)
		out := make([]string, 0, len(list))
		for _, st := range list {
			out = append(out, st.UID)
		}
		return out
	}
}
