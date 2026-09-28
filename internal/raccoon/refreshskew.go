package raccoon

import (
	"time"

	"workbuddy2api/internal/gateway"
)

// refreshskew.go —— raccoon 通过 `gateway.RefreshSkewExt` 自报「多早算该刷」。
//
// # 为什么用寿命比例，而不是固定时长（用户提出的统一策略）
//
// 固定时长窗口的问题是：**它相对于凭证寿命的意义随寿命而变**。
// 同一个"提前 10 分钟"，对 1 小时寿命的 token 是"已用 83% 才续"
//（几乎没有重试余量），对 55 天寿命的 token 则是"已用 99.99% 才续"。
//
// 按寿命比例就与寿命无关：
//
//	已用 ≥ 50% 就续期  → 无论寿命多长，都在"用掉一半"时换新
//
// raccoon 的 access_token 是 JWT（`iat` + `exp` 都在）。
//
// # ⚠ 修的是一个真实缺陷：此前"每个请求都续期"
//
// 本上游此前**没有**实现本扩展点，于是核心走兜底分支
//
//	acct.NeedsRefresh(10 * time.Minute)
//
// 而账号池投影给核心的 `auth.Auth.ExpiresAt` 是 **0** ——
// 核心的 `auth.Parse` 读 `auth.expiresAt`（秒），而本上游落盘写的
// 过期字段名与它不同。`NeedsRefresh` 对 `ExpiresAt <= 0` **恒返回 true**，
// 于是每个 chat 请求都先做一次续期往返。
//
// ⚠ 这个坑项目里有记录（cmd/server/mimocreds.go 的文件头，
// 标注为 TRAE「账号突然过期」事故的根因之一）：
//
//	过期权威只走 gateway.CredentialExpiryExt（问上游活 secret），
//	预检开关只走 RefreshSkew
//
// 现在按比例报窗口，判据取自 **token 自己的 iat/exp**（活凭证），
// 与那份 ExpiresAt=0 的启动快照投影无关。
//
// 编译期断言（与 CredentialRefresher / CredentialLoader / AuthDirExt 并列）。
var _ gateway.RefreshSkewExt = (*Provider)(nil)

// RefreshSkew 报告 raccoon 的提前续期窗口（已用寿命 ≥ 50% 即触发）。
//
// 纯本地判断：只解本地 token 的时间戳，不发网络、不改状态 ——
// 它在出站循环的**每次**请求上被调用（每个候选账号一次）。
//
// token 解不出 iat/exp 时返回 `(0, false)`，核心据此回落通用窗口。
// **不返回 (0,true)** —— 那是"永远该刷"，会退化成修复前那个形态。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil {
		// 按比例算必须先拿到 token；类型不对时只能回落通用窗口
		//（类型不对会在 Chat / RefreshCredential 得到明确错误）。
		return 0, false
	}
	return gateway.RefreshSkewFromToken(a.AccessToken)
}
