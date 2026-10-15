package main

import "workbuddy2api/internal/pool"

// quota_consumed.go 上游"我刚消耗了额度"的上报落点（核心侧）。
//
// # 为什么由核心实现，而不是让上游直接写池子
//
// 额度的真相在账号池里（`pool.SetQuota`），而**上游不允许 import 核心**
// （`arch_test.go` 守着这条接缝）。所以走"上报"：
//
//	上游 算出"这个账号少了几个**显示单位**"（它自己知道刻度：qoder 是小数
//	     credits，所以要内部累计、跨整数才上报）
//	核心 决定怎么用它 —— 这里就是那个决定点
//
// ⚠ 落点只有一行（`ApplyQuotaConsumed`）是**刻意的**：读-改-写必须在池子
// 的**同一把锁**里完成。若在这里写成 `Get + 减 + SetQuota`，两次调用之间可能
// 插进一次 RefreshQuota 写进新余额 ⇒ 我们基于过期值做减法 ⇒ 多扣。
type quotaConsumedSink struct{ p *pool.Pool }

// OnQuotaConsumed 实现 gateway.QuotaConsumedSink。
//
// ⚠ 它在**转发路径**上被调用（响应体关闭时），所以：
//   - 不阻塞、不报错、可并发（池子内部加锁）
//   - 任何失败都静默 —— 额度跟踪是附线，绝不能影响对话收尾
//
// ⚠ 不用 provider 参数做查表：账号池按 **uid** 索引，而两个产品的 uid 不同
// （qoder / qodercn 在同一凭证目录里靠 product_id 区分，uid 也各自独立），
// 所以 uid 已经足够定位。参数保留是为了日志/将来可能的按上游策略差异。
func (s quotaConsumedSink) OnQuotaConsumed(provider, uid string, wholeUnits int64) {
	if s.p == nil {
		return
	}
	s.p.ApplyQuotaConsumed(uid, wholeUnits)
}
