package codearts

// refreshskew.go —— codearts 通过 `gateway.RefreshSkewExt` 自报
// 「我的凭证多早算该刷」。
//
// # 为什么必须由 codearts 回答，而不是核心的 RefreshSkew 配置
//
// 核心的 `cfg.RefreshSkew` 默认 **10 分钟**，而 codearts 的 STS 凭证
// 寿命只有约 **2 小时** —— 10m 窗口意味着"还剩十二分之一寿命就去续期"。
//
// 对 codearts 这不只是"太早"，而是**有代价的**：
// refresh_token 是**一次性消费**的（用一次即作废，见 jobs.go 包注释与
// client.RefreshToken 的 refreshMu）。核心的 10m 规则会在 token 还剩 8m 时
// 就触发续期分支，而 `Provider.RefreshCredential` 内部**没有**自己的
// skew 检查（它只做断言+转发），于是真的去消费那个 refresh_token。
// 为省一次 401 往返烧掉一个凭证 —— 而出站路径的刷新**不落盘**
// （见 credentialrefresher.go 的落盘语义），即使成功也只是内存里换了新的。
//
// 所以窗口是 codearts 的事实，必须由它自己报。
//
// # 为什么等于请求路径的那个 refreshSkew 常量
//
// `provider.go` 的 `refreshSkew`（3m）与 `jobs.go` 的 `defaultRefreshSkew`
// 已经是同一个值（后者就是前者的别名）。本文件把同一个事实**再暴露给核心**，
// 不引入第三个数字：请求路径、后台续期、核心出站三处判定共用一份窗口，
// 任何一个改了另两个自动跟随。
//
// # 为什么不直接返回常量（看起来更简单）
//
// 返回常量的话，`ok=true` 就是恒真的，本扩展点退化成"永远有答案"。
// 保留 `ok` 的语义（见 gateway.RefreshSkewExt）是为了让**没有**上报窗口的
// 上游与"上报了 0"的上游可区分 —— 前者回落核心兜底，后者是明确声明。
// codearts 属于前者还是后者要显式表达：这里恒上报，所以 ok 恒 true。

import (
	"time"

	"workbuddy2api/internal/gateway"
)

// stsLifetime 华为云 securityToken 的寿命（实测约 2 小时）。
//
// ⚠ 它只用于把"不透明 token 的起点"反推出来（见 RefreshSkew），
//
//	从而让本上游也能用统一的"寿命过半"判据。它不是判据本身 ——
//	凭证真的能不能用，永远由真实请求决定（401 → 续期）。
const stsLifetime = 2 * time.Hour

// 编译期断言（与 gateway.Provider / CredentialRefresher / JobExt 并列）。
var _ gateway.RefreshSkewExt = (*Provider)(nil)

// RefreshSkew 报告 codearts 的提前续期窗口。
//
// 纯本地判断：不读网络、不改状态，只在出站循环里被问一次"要不要刷"。
// `cred` 被断言成 `*Auth` 校验凭证类型（与 Chat / RefreshCredential 同构：
// 核心把组装好的整份凭证交回来，上游自己认识它）。
//
// ⚠ 凭证类型不对时仍然上报窗口（ok=true）：窗口是**上游的属性**，
// 与"这一份凭证能不能用"无关。类型不对会在 Chat / RefreshCredential
// 里得到明确错误，不该在这里变成一个"不知道窗口"而让核心套用 10m。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	a, _ := cred.Secret.(*Auth) // 类型不对时 a=nil，refreshSkewOf 会回落固定窗口
	return refreshSkewOf(a)
}

// refreshSkewOf 本上游统一的续期窗口 —— **请求路径与后台任务共用这一份**。
//
// # 为什么必须共用（2026-11-01 用户要求统一后续期时补的）
//
// 统一之前，请求路径（RefreshSkew）与后台任务（runRefresh）是两套判据：
// 任务那边"无条件全量续"（jobs.go 里的历史注释），与"寿命过半才续"直接冲突。
// 两处各写一份的表现是：改了请求路径、后台照样每 30 分钟把每个号续一遍，
// 而且**不会报错**。所以窗口计算收成一个函数，两条路径都只能通过它表达。
func refreshSkewOf(a *Auth) (time.Duration, bool) {
	if a != nil && a.ExpiresAt > 0 {
		// 统一规则：寿命过半 ⇒ 该续期（用户 2026-11-01 要求所有上游同一套）。
		//
		// # 为什么本上游也能算比例（虽然 token 是不透明串、没有 iat）
		//
		// 比例需要两个点：开始与结束。华为云 securityToken **只有结束时刻**，
		// 但它的寿命是一个**已知常量**（约 2 小时，见 provider.go / oauth.go 的实测），
		// 所以起点可以反推：issued = expires − stsLifetime。
		// 这不是猜：寿命写在下面对常量的注释里，改了寿命这里自动跟随。
		//
		// ⚠ 代价要写清楚（这是本上游唯一与别家不同的地方）：
		//   refresh_token 是**一次性消费**的。窗口从 3 分钟放到寿命的一半（约 1 小时），
		//   意味着同样 2 小时的凭证周期里续期次数**翻倍**（原来一个周期刷一次，
		//   现在从 t=1h 起就满足条件）。这是"统一 50%"的代价，用户已明确要求统一；
		//   要退回去只需把下面的 stsLifetime 换回 refreshSkew 常量。
		issued := a.ExpiresAt - int64(stsLifetime.Seconds())
		if skew, ok2 := gateway.RefreshSkewAtRatio(gateway.JWTTimes{
			IssuedAt:  issued,
			ExpiresAt: a.ExpiresAt,
		}, gateway.DefaultRefreshRatio); ok2 {
			return skew, true
		}
	}
	// 读不到到期时刻（手写/异常凭证）⇒ 保持原来的固定窗口。
	return refreshSkew, true
}
