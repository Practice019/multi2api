// dailycheckin_disable.go 凭据失效 ⇒ 禁用 / 成功 ⇒ 恢复（T-433）。
//
// # 方案演进（为什么不在各上游做）
//
// 最初的方案是"给每个上游注入两个钩子"（仿 uidEnumerator / pool.SetQuota）。
// 实测推翻它：
//
//	· 10 个上游里**只有 lobsterai** 走 dailycheckin.Driver，另外 7 个**手写 CheckinAll**
//	  ⇒ 要有 7 条手写路径各写一遍过滤与上报；
//	· 且每个上游都拿不到 Pool（上游包不得依赖 pool，arch_test 守着）
//	  ⇒ 还要 10 处注入点。
//
// 而 `gateway.DailyCheckinResult` 已经带齐了判据所需的**全部**信息：
//
//	UID（定位账号）/ Status（ok|already|fail|skip）/ Detail（错误原文）
//
// ⇒ **在 Job 层后置处理**：统一签到拿到 report 之后自己判、自己禁用。
//
//	**10 个上游一行都不用改**，也不需要任何注入点。
//
// # 探活恢复为什么是"免费"的
//
// 我们本来就要为这个号打一次 CheckinAll —— 它返回 ok 就证明凭据活着。
// 再单独发一次 Status() 探活是白打上游（codar 面板写着「上游已有风控记录，请低频使用」）。
// ⇒ 一次请求同时完成"签到"与"探活"两件事，**不需要单独的探活路径、不需要加频**。
//
// # 代价（如实记）
//
// 禁用后每小时仍会为它发一次 CheckinAll（`CheckinAll(ctx)` 不接受"跳过哪些号"）。
// 成本 = 被禁用账号数 × 24 次/天，与现在已在发生的失败重试量相同 ⇒ 可接受。
// 若要省掉它，就得给 gateway 加"列账号 + 跳过"的接口 —— 那是另一个大改动（见 design-notes）。
package scheduler

import (
	"errors"
	"log"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// DisableRecorder 账号禁用/恢复能力（由装配层注入）。
//
// # 为什么用接口而不是直接 import pool
//
// `scheduler` 现在依赖 `gateway` + `checkinlog`。"禁用账号"的能力住在 `pool` 里，
// 而直接 import pool 会把调度器与账号池的**具体机制**（阈值、窗口、持久化细节）
// 绑在一起 —— 那些语义不属于调度。
// ⇒ 只依赖这两个方法的最小面（这也是 T-427 抽共享的同一条思路）。
//
// ⚠ 与 `pool` 的真实方法签名**一致**：`RecordAutoDisableFailure` 返回"这次是否真的摘了"
//
//	—— 它内部带**阈值与窗口**（连续 N 次才禁用）⇒ 一次抖动不会摘号。
//	这正是我们要的防误杀行为 ⇒ **不要**在这里自己数次数、自己定阈值（那会漂移）。
type DisableRecorder interface {
	RecordAutoDisableFailure(uid, reason string) bool
	RecordAutoDisableSuccess(uid string) bool
}

// applyDisablePolicy 按签到结果禁用/恢复账号。
//
// 判据（顺序即策略）：
//
//	ok / already  ⇒ 该号此刻是**可用**的 ⇒ 若它曾被自动禁用，解除
//	                （这就是"探活恢复"，零额外请求 —— 见文件头）
//	fail          ⇒ **只有凭据类**才摘号；网络/超时/模糊措辞**什么都不做**（等下一轮重试）
//	skip          ⇒ 不动（上游没开放签到，不是账号的问题）
//
// ⚠ `fail` 的分支是**误杀防线**：判定完全交给 `dailycheckin.CredentialError`
//
//	（保守白名单，网络类优先排除，分不清就不动）—— 这里**不许**再加任何宽判据。
func applyDisablePolicy(rec DisableRecorder, results []gateway.DailyCheckinResult) {
	if rec == nil {
		return
	}
	for _, r := range results {
		if r.UID == "" {
			continue
		}
		switch r.Status {
		case checkinlog.StatusOK, checkinlog.StatusAlready:
			if rec.RecordAutoDisableSuccess(r.UID) {
				log.Printf("统一签到: 账号 %s 签到成功，已解除自动禁用", r.UID)
			}
		case checkinlog.StatusFail:
			if !dailycheckin.CredentialError(errors.New(r.Detail)) {
				continue // 网络抖动 / 超时 / 模糊措辞 ⇒ 不摘号（防误杀）
			}
			if rec.RecordAutoDisableFailure(r.UID, "签到凭据失效: "+r.Detail) {
				log.Printf("统一签到: 账号 %s 因签到凭据连续失效已自动禁用（%s）",
					r.UID, truncateForLog(r.Detail, 80))
			}
		}
	}
}

// truncateForLog 截断错误原文用于日志（上游正文可能很长，别把日志刷爆）。
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
