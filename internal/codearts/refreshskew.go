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
	_ = cred
	return refreshSkew, true
}
