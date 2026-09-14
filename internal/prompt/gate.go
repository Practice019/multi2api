// gate.go 内容拦截降级状态机。
//
// # 它解决什么问题
//
// passthrough 模式下，请求被上游内容策略拦截（HTTP 400 + 审核文案）时，
// 有两种可能：
//
//	① system 来源的**指纹误报** —— 客户端注入的模板句撞上了逐字匹配表
//	② 用户内容本身真的触发审核
//
// 两者在第一次拦截时**无法区分**（上游只给一句"blocked"）。
// 因此策略是：先按①处理 —— 换中性提示词重试一次；
// 第二次仍被拦，说明是②，如实返回给调用方。
//
// 如果每次都从头撞一遍 400 再重试，代价是每个受影响的请求都多一次
// 完整的失败往返（几百毫秒到数秒）。所以拦住一次之后**记住**，
// 在一段时间内直接使用中性提示词，不再先撞 400。
//
// # 为什么是"到次日 00:00 CST"而不是固定 TTL
//
// 上游的内容策略与风控是按**自然日**重置的（同一个上游的签到、
// 余额、成长额度也都按自然日走）。用固定 TTL 会出现两种情况：
//
//	TTL 太短 → 一天之内反复撞 400、反复重试
//	TTL 太长 → 上游策略已重置，我们却还在发降级提示词，白白丢掉人格
//
// 对齐到自然日边界后，两个问题同时消失：每天最多撞一次，
// 且上游一重置我们就在下一个请求恢复。
//
// # 为什么用固定 +08:00 而不是 now.Location()
//
// 容器/宿主机的时区是**不确定**的（Docker 默认 UTC、Windows 本地时区、
// 用户自定义 TZ）。用本地时区算"次日零点"会得出一个与上游无关的时刻，
// 而本状态机的语义是"跟随上游的自然日"—— 上游用 CST。
//
// 所以这里显式用 FixedZone("CST", +8h) 计算，与宿主 TZ 完全解耦。
// 这与 internal/upstream 里"每自然日 1 次派出按 CST 重置"的口径一致。
package prompt

import (
	"sync"
	"time"
)

// Gate 降级状态机：是否处于"使用中性提示词"的降级期。
//
// # 并发语义
//
// 被 HTTP handler（观察 400）与出站客户端（读状态决定用哪段提示词）
// 同时访问，所以内部用 mu 保护。
//
// # 零值可用
//
// 零值 Gate 的 Active() 返回 false（从未降级），Trigger() 正常工作。
// 因此调用方不需要判空，也不需要构造函数 —— 但为了可读性仍提供 NewGate。
type Gate struct {
	mu    sync.Mutex
	until time.Time
}

// NewGate 返回一个未进入降级期的 Gate。
func NewGate() *Gate { return &Gate{} }

// Active 报告当前是否处于降级期。
//
// nil 接收者返回 false：调用方（出站客户端）可能根本没被注入 Gate
// （例如单上游精简部署、测试直接构造 Client）。那种情况下"未降级"
// 是唯一安全的答案 —— 返回 true 会让它无故使用中性提示词。
func (g *Gate) Active() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Trigger 进入降级期，持续到次日 00:00 CST。
//
// # 已在降级期内时**不续期**
//
// 这里保持最早的触发点所对应的那个 00:00。
// 续期会让"连续被拦"不断把重置时刻往后推，形成永不恢复的降级 ——
// 而上游的策略是每天重置的，我们不该比它更悲观。
func (g *Gate) Trigger() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !time.Now().Before(g.until) {
		g.until = nextMidnightCST(time.Now())
	}
}

// Until 返回当前降级期的结束时刻（未降级返回零值）。
// 供管理台/日志展示"降级还有多久恢复"。
func (g *Gate) Until() time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.until
}

// Reset 立即结束降级期。仅用于测试与管理台手动恢复。
func (g *Gate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = time.Time{}
}

// cstZone 上游使用的时区（固定 +08:00，与宿主 TZ 解耦）。
var cstZone = time.FixedZone("CST", 8*60*60)

// nextMidnightCST 返回 now 之后最近的 CST 00:00（纯函数，方便单测）。
//
// 边界语义：
//
//	23:59:59 → 几秒后的次日 00:00
//	00:00:00 → **次日** 00:00（刚过零点，下一个零点是明天）
//
// 用 for 循环而不是 +24h：跨夏令时/闰秒的偏移在固定时区里虽然不会出现，
// 但循环写法对"midnight 恰好等于 now"的边界是自洽的（不依赖 ±1ns 的判断）。
func nextMidnightCST(now time.Time) time.Time {
	y, m, d := now.In(cstZone).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cstZone)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
