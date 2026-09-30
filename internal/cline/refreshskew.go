package cline

import (
	"time"

	"workbuddy2api/internal/gateway"
)

// refreshskew.go —— cline 通过 `gateway.RefreshSkewExt` 自报「多早算该刷」。
//
// # 为什么必须是比例而不是固定时长（用户提出的统一策略）
//
// 实测 cline 的 access_token **寿命只有 1 小时**（JWT 的 `iat`→`exp`），
// 而核心的通用兜底窗口是 10 分钟 —— 那意味着"已用 83% 才续期"。
// 对一个 1 小时寿命的 token，留给续期失败重试的余量只剩 10 分钟。
//
// 按寿命比例就与寿命无关了：
//
//	已用 ≥ 50% 就续期  →  剩 30 分钟时触发（对 1 小时 token）
//
// 这正是参照 `refresh.ts` 的 `REFRESH_LEAD_MS = 3_600_000`（1 小时提前量）
// 想表达的意思 —— 只不过它写成了固定时长，而 cline 的寿命恰好也是 1 小时。
// 用比例表达就不会跟着寿命漂移。
//
// # ⚠ 修的是一个真实缺陷：此前"每个请求都续期"
//
// cline 此前**没有**实现本扩展点。于是核心走兜底分支
//
//	acct.NeedsRefresh(10 * time.Minute)
//
// 而账号池投影给核心的 `auth.Auth.ExpiresAt` 是 **0** ——
// 因为核心的 `auth.Parse` 读 `auth.expiresAt`（秒），
// 而 cline 的落盘文件写的是 `auth.expire_time`（毫秒），字段名不同。
//
// `NeedsRefresh` 对 `ExpiresAt <= 0` **恒返回 true**，于是：
//
//	每个 chat 请求 → 先调一次 RefreshCredential → 一次网络往返 → 拿到新 token
//
// 也就是"能续，但白白多一次往返，且每次都在换 token"。
//
// ⚠ 这个坑项目里有记录（handoff.md 的「TRAE 账号突然过期」事故档案，
// 标注为 TRAE「账号突然过期」事故的根因之一）：
//
//	过期权威只走 gateway.CredentialExpiryExt（问上游活 secret），
//	预检开关只走 RefreshSkew=(0,true)
//
// 现在改为按比例报窗口，两个问题一次解决：
//   - `RefreshSkew` 返回真实窗口 → 核心不再恒判"该刷"
//   - 判据取自 **token 自己的 iat/exp**（而不是那份 ExpiresAt=0 的投影）
//
// # 为什么不再需要 ExpiresAt 投影
//
// 判据完全来自活凭证（`cred.Secret` 里那份 `*Auth` 的 access_token），
// 与账号池投影无关 —— 投影是启动快照、不会跟新，喂它过期时间反而会分叉。
//
// 编译期断言（与 CredentialRefresher / CredentialLoader / AuthDirExt 并列）。
var _ gateway.RefreshSkewExt = (*Provider)(nil)

// RefreshSkew 报告 cline 的提前续期窗口（已用寿命 ≥ 50% 即触发）。
//
// 纯本地判断：只解本地 token 的时间戳，不发网络、不改状态 ——
// 它在出站循环的**每次**请求上被调用（每个候选账号一次）。
//
// token 不是 JWT（解不出 iat/exp）时返回 `(0, false)`：
// 核心据此回落通用窗口。**不返回 (0,true)** —— 那是"永远该刷"，
// 会退化成修复前那个"每请求都续期"的形态。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil {
		// 凭证类型不对：窗口是**上游的属性**，不该在这里变成"不知道"。
		// 但按比例算必须有 token 才能算，所以只能回落通用窗口
		// （类型不对会在 Chat / RefreshCredential 得到明确错误）。
		return 0, false
	}
	return gateway.RefreshSkewFromToken(a.AccessToken)
}
