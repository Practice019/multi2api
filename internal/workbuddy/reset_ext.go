package workbuddy

// reset_ext.go —— workbuddy 通过 `gateway.ResetPolicyExt` 自报
// 「我额度耗尽的号什么时候能再用」。
//
// # 为什么 workbuddy 必须显式实现它（而不是只留一个无参回调）
//
// 出口层（internal/server）在 ErrHardCredit 上要一个**时刻**。改造前它靠的是一个
//
//	func() time.Time        // ← 无参
//
// 的回调，装配层注入 workbuddy 的"次日 04:00"。那个签名没有参数，
// 于是**任何**上游的号都拿到同一个答案 —— codearts 的号被冷到 workbuddy 的
// 次日 04:00，而 codearts 没有签到恢复机制，04:00 不是它的任何事实。
//
// 现在的接缝是 `gateway.ResetPolicyExt`：按上游 ID 分派，workbuddy 回答自己的
// 那一刻，其它上游回答自己的（或 ok=false 表示"我没有这个信息"）。
//
// # 为什么不直接复用无参的 NextResetAt
//
// 因为本包**必须**显式实现这个扩展点，否则装配层的 `ExtOf[ResetPolicyExt]`
// 落空 → ok=false → workbuddy 的号会回落到核心的通用兜底 now+1h。
// 那就把「次日 04:00 等签到恢复」这条**真实存在的策略**丢掉了 ——
// 是行为回归（白撞一整天的硬错误），不是"更保守"。
//
// 与 errorclassifier.go 同一个道理：只让别的上游实现、自己不实现，
// 等于让核心的通用兜底**变成** workbuddy 的策略，只是藏在 core 的代码里。
//
// # 行为逐字不变
//
// 本方法返回的时刻与 `NextResetAt()` **完全同一个**（都走 NextCheckinReset）：
//
//	ResetAt(cred) == NextResetAt() == NextCheckinReset(time.Now())
//
// 这是硬要求（P2 修复不得改变 workbuddy 的既有行为），有测试钉住。

import (
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言（与 ErrorClassifier / CredentialRefresher / RefreshSkewExt 并列）。
var _ gateway.ResetPolicyExt = (*Provider)(nil)

// ResetAt 报告 workbuddy 的额度恢复时刻：**次日 04:00**。
//
// # 为什么是 04:00（这条策略的事实依据）
//
// 04:00 是 workbuddy"签到恢复"的**前置时刻**：签到时点是 09:00 / 21:00，
// 04:00 是账号额度重置的自然日边界。所以额度耗尽的号冷到下一个 04:00，
// 是最早起作用的那一刻。语义细节见 NextCheckinReset 的注释
// （凌晨窗内返回**当天** 04:00，否则次日 04:00）。
//
// # 为什么 cred 被忽略
//
// 04:00 是 workbuddy 的**排程事实**，与某一份凭证无关 —— 不同账号的恢复
// 时刻完全相同。忽略而不是断言类型：凭证类型不对会在 Chat / RefreshCredential
// 里拿到明确错误，不该在这里变成一个"不知道恢复时刻"而让核心套用 now+1h。
// （与 codearts 的 RefreshSkew 忽略 cred 同一取舍。）
//
// # 为什么 ok 恒为 true
//
// 本上游**确实**有恢复排程。返回 false 会让核心用通用兜底，
// 那是把一个真实的策略丢掉。
func (p *Provider) ResetAt(_ gateway.Credential) (time.Time, bool) {
	return NextCheckinReset(time.Now()), true
}
