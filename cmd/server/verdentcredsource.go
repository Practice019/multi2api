// verdentcredsource.go 按 uid 取 Verdent 活凭证（额度探测用）。
//
// # 为什么不能用账号池的 AuthByUID
//
// AuthByUID 返回的是**核心账号投影**（只有 uid / nickname / filePath / expiresAt），
// 而本上游要 *verdent.Auth（含 token）—— 它在 pool.SecretOf 里
// （凭证并池时由 SyncToDirWithSecrets 装进去）。
//
// 与 monkeycodeCredSource / minimaxCredSource / zcodeCredSource 同一模式。
package main

import (
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/verdent"
)

// verdentCredSource 造一个"按 uid 取活凭证"的访问器。
func verdentCredSource(p *pool.Pool) func(string) (gateway.Credential, bool) {
	return func(uid string) (gateway.Credential, bool) {
		secret, ok := p.SecretOf(uid)
		if !ok {
			return gateway.Credential{}, false
		}
		a, ok := secret.(*verdent.Auth)
		if !ok || a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{
			Provider:  verdent.ProviderID,
			UID:       uid,
			Nickname:  a.DisplayName(),
			ExpiresAt: a.TokenExpiredAt(),
			Secret:    a,
		}, true
	}
}

// verdentUIDEnumerator 列出账号池里属于本上游的全部 uid。
//
// ⚠ 必须用 ListFor 而不是 AvailableUIDsFor：后者会**滤掉不健康的账号**，
// 而那正是诊断端点最需要看到的（冷却中的、被禁用的）。
// 用它会让"账号为什么用不了"在最该有答案的地方显示成"没有账号"。
func verdentUIDEnumerator(p *pool.Pool) func() []string {
	return func() []string {
		list := p.ListFor(verdent.ProviderID)
		out := make([]string, 0, len(list))
		for _, st := range list {
			out = append(out, st.UID)
		}
		return out
	}
}
