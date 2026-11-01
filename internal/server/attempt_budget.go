// attempt_budget.go 换号重试的**时间预算**：别让"上游整体超时"被逐号重试放大。
//
// # 为什么需要它（用户实测数据）
//
//	用户直连讯飞，同样 35 个 token 的请求：
//	  第 1 次 408 响应超时  34.2s
//	  第 2 次 408 响应超时  46.2s
//	  第 3 次 200 成功      38.9s   ← 成功也要 39 秒
//
// 也就是说**单次尝试就接近 40 秒**。而池里若有 104 个账号，按"换号重试"的老行为，
// 最坏情况总等待 = 104 × 40s ≈ 70 分钟；即使只试 MaxRotate 次（默认 3）也有 2 分钟。
// 客户端早断了，日志里却只留一句含糊的 503。
//
// # 与 retry_decision.go 的分工（**不要混用**）
//
//	retry_decision.go  按**次数**预算（AttemptsLeft / MaxRotate）—— 回答"还能再试几次"
//	本文件            按**时间**预算（墙钟）—— 回答"再试还有意义吗"
//
// 两者是乘法关系：次数够但时间已经花掉 100 秒时，再试一次只会让客户端多等 40 秒。
// 所以判据取"哪个先耗尽"。
//
// # 预算取值理由
//
// 默认 90 秒：覆盖"一次 40 秒的慢尝试 + 一次可能成功的重试"（40+40=80 < 90），
// 又不会让客户端等过久（多数 OpenAI 兼容客户端的默认超时在 60~120 秒之间）。
// 刻意**不**取 maxAttempts×单次超时：那是"最坏情况可接受"，不是"用户愿意等"。
package server

import "time"

// DefaultAttemptBudget 一次请求在**换号重试**上最多愿意花的时间（见文件头的取值理由）。
//
// ⚠ 它约束的只是重试循环内部的等待，不包含：
//   - 排队等号的时间（那是池子容量问题，不是上游慢）
//   - 客户端已经断开之后的残余尝试（ctx 取消会更快退出）
const DefaultAttemptBudget = 90 * time.Second

// attemptBudget 时间预算记账器。零值不可用，用 newAttemptBudget 构造。
//
// 线程模型：只在**单个请求的池循环**里用，不跨 goroutine，因此无锁。
type attemptBudget struct {
	limit time.Duration
	start time.Time
}

// newAttemptBudget 记下起点。limit<=0 时用 DefaultAttemptBudget。
func newAttemptBudget(limit time.Duration) *attemptBudget {
	if limit <= 0 {
		limit = DefaultAttemptBudget
	}
	return &attemptBudget{limit: limit, start: time.Now()}
}

// Exhausted 报告"预算是否已经用尽"。
//
// ⚠ 判据是"已花时间 >= 预算"，且**在发起下一次尝试之前**判断 —— 这样才能避免
// "最后一次尝试又花了 40 秒"的情况：预算 90 秒、已经花了 85 秒时，不该再开一次
// 可能 40 秒的尝试。
func (b *attemptBudget) Exhausted() bool {
	if b == nil {
		return false // nil = 未装配 = 不限制（池循环里可能没配预算）
	}
	return b.Spent() >= b.limit
}

// Spent 已花时间（观测用：写进请求日志）。
func (b *attemptBudget) Spent() time.Duration {
	if b == nil {
		return 0
	}
	return time.Since(b.start)
}

// Limit 预算上限（观测用）。
func (b *attemptBudget) Limit() time.Duration {
	if b == nil {
		return 0
	}
	return b.limit
}

// ShouldTryAgain 综合"次数 + 时间"两个预算，回答"还该不该再开一轮"。
//
// 抽成一个函数是为了**只有一个判据**：散在两处的判断迟早会分叉，
// 而分叉的表现正是"某个分支忘了看时间预算"（本文件存在的原因）。
//
// # 为什么还要看 lastAttempt（2026-11-02 修）
//
// 第一版只判 Spent() >= limit，真机实测跑出 **144 秒**（预算 90 秒）：
// 前两次各约 48 秒，第三次启动时恰好 96 秒 —— 但它判断的是"已经花了多少"，
// 而不是"下一次还要花多少"，于是放过了第三次，又白等 48 秒。
//
// 教训：**只判已花时间、不判下一次成本，预算就只是装饰**。
// 现在把上次尝试耗时当**预估成本**：Spent() + lastAttempt 会超预算就不开。
// 首次尝试 lastAttempt=0 ⇒ 预估 0 ⇒ 永远放行（第一次必须允许）。
func (b *attemptBudget) ShouldTryAgain(attemptsLeft int, lastAttempt time.Duration) bool {
	if attemptsLeft <= 0 {
		return false
	}
	if b == nil {
		return true
	}
	return b.Spent()+lastAttempt < b.limit
}
