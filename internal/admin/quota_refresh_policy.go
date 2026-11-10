package admin

import "log"

// quota_refresh_policy.go 手动额度刷新的**收尾处置**：凭据失效⇒摘号 / 刷新成功⇒恢复。
//
// # 为什么处置不放在 refreshQuotas 里
//
// `refreshQuotas` 被三条路径共用（手动单上游 / 手动全部 / 后台自动分层刷新），
// 而后台那条是**定时**跑的（分层刷新）。把"失败即摘号"接在它上面，一次上游故障
// 会在 5×3 ≈ 15 分钟内把整池号刷成禁用 —— 那正是本仓反复禁止的"用重试策略误杀好号"。
//
// ⇒ 结论（`AuthFailedUIDs` / `FreshUIDs`）由 refreshQuotas 回传，
//   处置只在**用户明确点了按钮**的那条路径上执行。
//
// # 判据
//
//	上游报告"这次失败是**账号层失效**" ⇒ **一次即摘**（RecordAutoDisableFailureNow）
//	真正拿到数据（HasData）        ⇒ 记一次自动禁用成功（**只解禁本机制禁的号**）
//	其余（Unknown / Failed / 跳过） ⇒ 什么都不做
//
// ⚠ 三条都刻意"保守"，理由各不相同：
//
//  ① 用 RecordAutoDisableFailureNow 而不是 Pool.Disable：**仍然走自动禁用线**
//     （owned=true）⇒ 之后刷新成功能被自动解除；而直接 Pool.Disable 会变成人工禁用，
//     永久回不来。
//     之所以敢跳过阈值：判据是**上游原文**（网络抖动永远产生不出那两句），
//     而不是"状态码/超时"那种模糊信号。判据模糊就必须用阈值，判据明确就该立刻生效。
//  ② 只有上游**明确说**是凭据失效才算（gateway.QuotaAuthFailureExt）。
//     判据留在上游包里（它看得见响应原文），核心不猜。
//  ③ Unknown **不算成功**：没有证据证明凭据可用，拿它解禁会把"查不到"当成"好了"。
//     而 RecordAutoDisableSuccess 自身带 `ownsDisableLocked` —— **人工禁用不会被它清掉**。
func (h *Handler) applyQuotaRefreshPolicy(res quotaRefresher) (disabled, recovered int) {
	if h.cfg.Pool == nil {
		return 0, 0
	}
	// 同一轮里既报"凭据失效"又拿到数据的号（loomy 的本地缓存回落可能造成）
	// 只按**失效**处理：两个相反结论同时成立时，摘号那一侧更该被听见。
	authFailed := make(map[string]bool, len(res.AuthFailedUIDs))
	for _, uid := range res.AuthFailedUIDs {
		authFailed[uid] = true
		// ⚠ 用 **Now** 而不是计数版：判据是**上游原文**（"登录已失效" / "账号已被停用"），
		//   网络抖动永远产生不出它 ⇒ 不需要"连 N 次"这道防线，而用户点一次按钮就该看到效果。
		//   仍然走自动禁用线（owned=true）⇒ 之后刷新成功仍能自动解除。
		if h.cfg.Pool.RecordAutoDisableFailureNow(uid, "额度刷新：账号层失效（登录已失效 / 账号已被停用）") {
			log.Printf("admin: 账号 %s 额度刷新连续失败（凭据失效），已自动禁用", uid)
			disabled++
		}
	}
	for _, uid := range res.FreshUIDs {
		if authFailed[uid] {
			continue
		}
		if h.cfg.Pool.RecordAutoDisableSuccess(uid) {
			log.Printf("admin: 账号 %s 刷新额度成功，已解除自动禁用", uid)
			recovered++
		}
	}
	return disabled, recovered
}
