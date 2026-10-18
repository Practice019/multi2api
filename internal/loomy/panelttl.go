package loomy

import (
	"sort"
	"time"
)

// 本文件：**分层 TTL** 与"过期集合"的判定。设计见 docs/cache-refresh-design.md 第 1、2 节。
//
// # 为什么单独一个文件
//
// 它是纯逻辑（输入：缓存内容 + 当前时刻；输出：谁过期了），**不打网络、不碰锁**，
// 所以能被单测完整覆盖 —— 而"该不该刷"这件事一旦算错，症状是
// "要么白刷上游、要么永远显示旧数据"，两种都不会崩、不会报错。
// ⚠ 把它从 `panelwarmup.go`（那个文件管"怎么刷"）里分出来，正是为了让"刷不刷"
// 与"怎么刷"能各自被单独验证。

// TTL 按**变化速度**分层（这是整个权衡的支点）。
//
// ⚠ 为什么必须分层，而不是一个统一的 TTL：
//
//	统一取最短的 3 分钟 ⇒ 邀请码一天被白查 480 次（它几乎不变）；
//	统一取最长的 6 小时 ⇒ 额度永远是旧的（它随每次对话变，还是**选号权重**的输入）。
//	⇒ 统一 TTL 必然要么浪费、要么过时，不存在能同时满足的单一值。
const (
	// TTLQuota 额度/余额：随每次对话消耗而变，且喂给**选号权重** ⇒ 最需要新鲜。
	TTLQuota = 3 * time.Minute
	// TTLTasks 任务状态：一天最多变 8 次（8 个任务各一次）⇒ 30 分钟内变化概率很低。
	TTLTasks = 30 * time.Minute
	// TTLInvite 邀请码：生成后几乎不变，绑定是用户手动动作 ⇒ 可以很宽。
	TTLInvite = 6 * time.Hour
)

// expiredRows 从一份行缓存里挑出**已经过了 TTL** 的 uid，按 uid 升序返回（结果稳定，便于断言）。
//
// ⚠⚠ 两条必须钉住的语义：
//
//  1. **零值不算过期**。`FetchedAt.IsZero()` 的含义是「**从来没读到过**」——
//     那批行的正事是"补数"（由 sweepPanels 的 `get` 未命中分支负责），
//     不是"刷新"。把它们混进过期集合，会让一批从未查过的号每一轮都被优先刷，
//     而真正过期的那些排在后面（饿死）。
//  2. **`!OK` 的行不参与**。失败的查询**不写缓存**（见 tasksViewFor 的契约），
//     所以理论上缓存里不会有 `!OK` 的行；万一有（老版本落盘的数据），
//     也不该按 TTL 去刷它 —— 它需要的是"重试"，不是"刷新"。
//
// force 为 true 时忽略 TTL，返回所有"曾经读到过"的行（用于"刷新额度"这类手动动作）。
func expiredRows[T any](rows map[string]T, now time.Time, ttl time.Duration, force bool,
	fetchedAt func(T) time.Time, ok func(T) bool) []string {
	out := make([]string, 0, len(rows))
	for uid, v := range rows {
		if !ok(v) {
			continue // 失败/占位行不按 TTL 处理（见上面第 2 条）
		}
		at := fetchedAt(v)
		if at.IsZero() {
			continue // 「从来没读到过」交给补数那条路，不进过期集合（见上面第 1 条）
		}
		if !force && now.Sub(at) < ttl {
			continue // 还在 TTL 内：这是"不能一直发请求"的落点
		}
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}

// expiredTasks 挑出过期（或 force 时全部）的 tasks 行。
func (p *Provider) expiredTasks(now time.Time, force bool) []string {
	c := p.tasksPanel()
	c.mu.RLock()
	rows := make(map[string]accountTasksView, len(c.m))
	for k, v := range c.m {
		rows[k] = v
	}
	c.mu.RUnlock()
	return expiredRows(rows, now, TTLTasks, force,
		func(v accountTasksView) time.Time { return v.FetchedAt },
		func(v accountTasksView) bool { return v.OK })
}

// expiredInvites 挑出过期（或 force 时全部）的 invite 行。
func (p *Provider) expiredInvites(now time.Time, force bool) []string {
	c := p.invitePanel()
	c.mu.RLock()
	rows := make(map[string]accountInviteView, len(c.m))
	for k, v := range c.m {
		rows[k] = v
	}
	c.mu.RUnlock()
	return expiredRows(rows, now, TTLInvite, force,
		func(v accountInviteView) time.Time { return v.FetchedAt },
		func(v accountInviteView) bool { return v.OK })
}
