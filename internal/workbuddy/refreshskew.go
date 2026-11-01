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
//	codearts  → STS 仅约 2h → 3m 窗口（见 codearts/refreshskew.go）
//
// 两个数字差 3 倍多。核心用一个数字覆盖两者，无论取哪个都会错一半。

import (
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// refreshSkewFallback workbuddy 的**兜底**续期窗口（读不到 token 时间时才用）。
//
// ⚠ 2026-11-01 用户要求统一：所有上游都按「生命周期过半就续期」
// （gateway.DefaultRefreshRatio = 0.5）。workbuddy 的 token 是 JWT、
// 实测寿命 **28~30 天**（解 demo 里 23 个凭证的 iat/exp 统计得出），
// 所以主路径读 iat/exp 算比例窗口；这个常量降级为**读不到时间时**的兜底。
//
// 保留它（而不是读不到就回 ok=false）的理由：ok=false 会让核心套用**它自己的**
// 同值兜底窗口，等于把"我不知道"伪装成一个决定。
const refreshSkewFallback = 10 * time.Minute

// 编译期断言（与 CredentialRefresher / CredentialLoader / AuthDirExt 并列）。
var _ gateway.RefreshSkewExt = (*Provider)(nil)

// RefreshSkew 报告 workbuddy 的提前续期窗口 —— **寿命过半**（统一判据）。
//
// 纯本地判断，不发网络请求（见 gateway.RefreshSkewExt 的实现约束）。
// 窗口**随凭证变化**（取决于 token 自己的 iat/exp），不再是常量策略。
// 类型校验留给 RefreshCredential / Chat —— 窗口是**上游的属性**，
// 与"这一份凭证能不能用"无关；读不出时间就回落到固定兜底值（不是 ok=false）。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	// 统一规则：寿命过半 ⇒ 该续期（与 cline / raccoon / qoder / lobsterai / minimax 同一判据）。
	//
	// 旧实现是固定 10 分钟 —— 对实测 28~30 天寿命的 token 等于"已用 99.99% 才续"，
	// 提前量小到几乎不存在（共享模块 refreshratio.go 的文件头就是拿它当反例写的）。
	// 换成比例后**续期频率不变**：跨过阈值刷一次，新 token 重新计时，
	// 每个周期仍是一次续期，只是从"最后一刻"提前到"过半"。
	if a, ok := cred.Secret.(*auth.Auth); ok && a != nil {
		if skew, ok2 := gateway.RefreshSkewFromToken(a.AccessToken); ok2 {
			return skew, true
		}
	}
	return refreshSkewFallback, true
}
