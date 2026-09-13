// reset_policy.go —— 第七个扩展点：上游自报「额度耗尽的号什么时候能再用」。
//
// # 为什么必须有这个扩展点（P2 设计缺口）
//
// 出口层（internal/server）在 `applyErrorPolicy` 的 ErrHardCredit 分支里做一件事：
//
//	CooldownUntilNextReset(uid, <下次重置时刻>, "额度不足")
//
// 而"下次重置时刻"原先来自 `server.Config.NextResetAt` —— 一个
//
//	func() time.Time        // ← 无参！
//
// 的无参回调。签名本身就**排除了按上游分派的可能**：它拿不到任何上游上下文，
// 于是永远只能返回**同一个**上游的答案。装配层（cmd/server/main.go）注入的是
//
//	NextResetAt: wb.NextResetAt     // ← workbuddy 的"次日 04:00 等签到恢复"
//
// 后果：**任何上游**的账号在 ErrHardCredit 时都被冷却到 workbuddy 的次日 04:00。
// 04:00 是 workbuddy"签到恢复"的前置时刻（签到时点 09:00/21:00），
// 对 codearts 这种**没有签到恢复机制**的上游，它是一个语义完全不同的时刻：
//
//	明明已经恢复却还冷到次日凌晨 → 白白闲置近 24h
//	按 04:00 解冻而实际未恢复   → 又撞一次硬错误
//
// 讽刺的是 handler.go 的注释**早已写着正确的设计**：
//
//	"现在改为向 Provider 要时刻：出口层只问'这个号什么时候能再用'，
//	 具体策略由上游定义"
//
// 但 `func() time.Time` 这个类型不允许它做那件事。本文件把那个类型换成
// **按上游 ID 分派**的形状，与 Chat / Credential / RefreshCredential /
// RefreshSkew 完全同构（见 provider_router.go 的 ProviderRouter）。
//
// # 与 gateway.RefreshSkewExt 的关系（两个"时刻/窗口"扩展点必须成对看）
//
//	RefreshSkewExt  → 「我的凭证多早算该刷」      （凭证寿命决定）
//	ResetPolicyExt  → 「我的额度耗尽的号何时能再用」（上游排程决定）
//
// 拆成两个而不是合并：一个上游完全可能上报续期窗口却**没有**额度恢复排程
// （纯 API Key 的上游）。把两者塞进一个接口会逼所有人写空实现。
//
// # 为什么参数是 Credential 而不是 uid
//
// 与 CredentialRefresher / RefreshSkewExt 同理：重置时刻可能取决于凭证本身
// （例如不同套餐的配额窗口不同，套餐信息在凭证里），而核心**不得读**
// Credential.Secret —— 那是上游的私有格式。上游拿到整份凭证自己断言类型。
//
// # 为什么需要 Credential（而不是干脆用 providerID 就够）
//
// 只有 ID 也能回答静态策略（workbuddy 的"次日 04:00"就是静态的）。
// 但本仓既有的两个同类扩展点全部收 Credential，保持同一形状的理由是：
// 事后加参数是不兼容变更，而**现在就多给一份凭证的成本是零**。
// 上游不需要时可以像 codearts 的 RefreshSkew 那样显式 `_ = cred`。
package gateway

import "time"

// ResetPolicyExt 上游自报「我额度耗尽的账号什么时候能再用」。
//
// 与 ErrorClassifier / RefreshSkewExt 同一类接缝：核心只问，不做判断。
type ResetPolicyExt interface {
	// ResetAt 返回该凭证对应账号的下次可用时刻。
	//
	// # 返回值语义
	//
	//	ok == true   这个上游**有**自己的恢复排程，调用方必须尊重这个时刻
	//	ok == false  这个上游**没有**这个信息 → 调用方回落到自己的通用保守值
	//
	// ⚠ 与 gateway.RefreshSkewExt 的形状刻意一致（T, ok）：
	// "没有这个信息"与"给了某个值"必须可区分，否则没有排程的上游会被
	// 强加上一个不是它的事实 —— 那正是本扩展点要修的那类错误。
	//
	// # 实现约束
	//
	//   - 必须是**纯本地**判断：不发网络请求、不读/写共享状态。
	//     它在出站循环的非 2xx 分支上被调用（每个出错的号一次）。
	//   - **不得 panic**（与 Provider 的契约一致）。
	//   - 返回的时刻应当在**将来**：早于 now 的时刻会被
	//     pool.CooldownUntilNextReset 归一成"不产生负时长"，
	//     但那会让这次硬冷却退化成一次无冷却，是静默的行为退化。
	//
	// # 上游应当怎么回答
	//
	//	workbuddy → 次日 04:00（等 09:00/21:00 签到恢复）
	//	codearts  → ok=false（没有签到恢复机制；配额按窗口滚动，不归它管）
	//	            核心回落 now+1h —— 那才是"通用保守值"存在的意义
	//
	// ⚠ **没有实现本扩展点的上游不算错**：装配层返回 ok=false，
	// 核心用兜底值。这是明确的保守兜底，不是"核心假装知道上游的排程"。
	ResetAt(cred Credential) (until time.Time, ok bool)
}
