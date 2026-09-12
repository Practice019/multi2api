// reset.go workbuddy 的**额度恢复策略**：次日 04:00。
//
// # 为什么这条策略属于 workbuddy，不属于 pool
//
// 改造前 `internal/pool` 里写死了 `nextDay4AM`：账号因额度耗尽被硬冷却时，
// 一律冷到"次日 04:00"。而 04:00 这个时点**只对 CodeBuddy 有意义** ——
// 它是"签到恢复"的前置时刻（签到时点是 09:00/21:00，04:00 是账号额度
// 重置的自然日边界）。codearts 没有签到，04:00 对它毫无意义。
//
// 于是核心的账号池被迫理解一个上游的排程策略，这正是本任务要消除的耦合。
// 现在策略搬到这里，由 cmd/server 把它注入 server.Config.NextResetAt，
// pool 只通过 CooldownUntilNextReset(uid, until, reason) 收到一个**时刻**，
// 不知道那个时刻是怎么算出来的。
//
// 审计脚本 audit_semantic.js 用 `nextDay4AM|CooldownUntilTomorrow4AM` 作为
// "上游的签到恢复策略写在核心"的探针 —— 搬完之后 pool 的代码耦合归零。
package workbuddy

import "time"

// NextCheckinReset 返回 now 之后最近的一个 04:00（与 now 同一时区）。
//
// 语义细节（与搬迁前的 pool.nextDay4AM 逐字一致）：
//   - now 在当天 04:00 之前（凌晨 00:00~04:00）时返回**当天** 04:00 ——
//     此时签到尚未执行，该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
//   - 04:00 整及之后返回**次日** 04:00。
//   - time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），
//     天然覆盖跨日/跨月/跨年。
func NextCheckinReset(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// NextResetAt workbuddy 的额度恢复策略：次日 04:00（等 09:00/21:00 签到恢复）。
//
// 签名刻意不带参数：它要满足 server.Config.NextResetAt 的 `func() time.Time`，
// 由核心在需要冷却账号时调用一次。取"调用时刻"而不是固定时刻，
// 是因为一次硬冷却可能发生在任意时刻（额度被别的客户端消耗完）。
func (p *Provider) NextResetAt() time.Time {
	return NextCheckinReset(time.Now())
}
