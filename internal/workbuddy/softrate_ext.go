package workbuddy

// softrate_ext.go —— workbuddy 通过 `gateway.SoftRateExt` 自报
// 「429 里哪个是模型级限流、它说什么时候重置」。
//
// # 为什么这个事实必须留在 workbuddy 包里
//
// 两个判据都是 **workbuddy 的私有知识**：
//
//	业务码 6004                —— 别的上游可能用同一个数字表达完全不同的东西
//	「将在 YYYY-MM-DD HH:MM:SS 重置」文案 —— 中文文案 + 固定 UTC+8，是它的表述习惯
//
// 把它们放进 core 的通用分类器，就等于让每个上游都被这套词汇解释 ——
// 与 gateway 包注释里的判据 1 直接冲突。
//
// # 它与 ErrorClassifier 的关系
//
// `ErrorClassifier` 先回答"这是软限流"（workbuddy 的实现把 429 判成
// gateway.ErrKindSoftRate）；本扩展点再回答"这个软限流有没有精确时刻"。
// 两步是**先后**关系而不是二选一：分类决定走哪条策略路径，
// 本接口决定那条路径要不要收窄。
//
// 所以它**只在 kind == ErrKindSoftRate 时被调用** —— 对 400/402 调它
// 没有意义（那些错误体里不会带重置时刻，而即便带了也不该收窄冷却）。

import (
	"time"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// 编译期断言：Provider 实现了软限流扩展点。
var _ gateway.SoftRateExt = (*Provider)(nil)

// SoftRateReset 解析 429 body 里的「将在 … 重置」时刻（gateway.SoftRateExt）。
//
// # 为什么内部仍然调 upstream.ParseSoftRateReset（而不把判据抄一遍）
//
// 本包是 workbuddy 的适配层，`internal/upstream` 就是**本上游的 HTTP 客户端
// 实现所在** —— 那个解析器（含 6004 码判据、中文文案正则、UTC+8 固定时区）
// 是 workbuddy 的既有、已测的事实。调用它不引入任何新耦合。
//
// 与 ErrorClassifier 的适配同一形状：判据留在原地，本包只做**发现**。
func (p *Provider) SoftRateReset(status int, body string) (time.Time, bool) {
	// 只对限流状态码解析：别的状态码即便体里碰巧有"将在 … 重置"
	// 也不是限流语义（例如某些提示文案）。这一条与
	// ParseSoftRateReset 内部的 6004 判据是**两道独立闸门**，都必要。
	if status != 429 {
		return time.Time{}, false
	}
	return upstream.ParseSoftRateReset(body)
}
