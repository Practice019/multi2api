package workbuddy

// refreshskew.go —— workbuddy 通过 `gateway.RefreshSkewExt` 自报
// 「我的凭证多早算该刷」。
//
// # 为什么 workbuddy 也要实现它
//
// 只让 codearts 实现本扩展点、workbuddy 不实现，功能上也能跑
// （不实现会回落到核心的 10m 兜底，而 10m 正是 workbuddy 一直在用的值）。
// 但那样**核心的兜底值就成了 workbuddy 的策略**，只是藏在注释里 ——
// 与 `NextResetAt`（"次日 04:00"是 workbuddy 的签到策略，因此移出核心）
// 同一个理由：一个上游的实现细节不该以"默认值"的形式留在核心。
//
// 显式上报之后，核心的 `cfg.RefreshSkew` 只剩两个用途：
//
//	单上游模式（cfg.Provider == nil，没有上游可问）
//	没有实现本扩展点的上游（明确的保守兜底）
//
// 两者都不再携带 workbuddy 的知识。
//
// # 为什么是 10m
//
// 与 `cmd/server` 传给 server.Config 的 RefreshSkew 同值：workbuddy 的
// access token 寿命以小时计，10 分钟窗口留足了续期往返与重试的余量。
// 这个数字**属于 workbuddy**（它的 token 寿命决定的），所以它住在这里。
//
// # 与 codearts 的对照（本扩展点存在的理由）
//
//	workbuddy → token 以小时计 → 10m 窗口
//	codearts  → STS 仅约 30m → 3m 窗口（见 codearts/refreshskew.go）
//
// 两个数字差 3 倍多。核心用一个数字覆盖两者，无论取哪个都会错一半。

import (
	"time"

	"workbuddy2api/internal/gateway"
)

// refreshSkew workbuddy 的提前续期窗口。
//
// 与 token 寿命（小时级）相比是一个显著小的值，符合
// gateway.RefreshSkewExt 的约束（窗口必须显著小于凭证寿命）。
const refreshSkew = 10 * time.Minute

// 编译期断言（与 CredentialRefresher / CredentialLoader / AuthDirExt 并列）。
var _ gateway.RefreshSkewExt = (*Provider)(nil)

// RefreshSkew 报告 workbuddy 的提前续期窗口。
//
// 纯本地判断，不发网络请求（见 gateway.RefreshSkewExt 的实现约束）。
// `cred` 不参与判定：workbuddy 的窗口是常量策略，不随凭证变化。
// 类型校验留给 RefreshCredential / Chat —— 窗口是**上游的属性**，
// 与"这一份凭证能不能用"无关。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	_ = cred
	return refreshSkew, true
}
