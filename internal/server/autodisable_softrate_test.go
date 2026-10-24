package server

import (
	"strings"
	"testing"
)

// 429 限流（ErrKindSoftRate）**不得**触发自动禁用。
//
// # 用户实测报障（2026-10-24）
//
// workbuddy 上游 6 个号里 3 个被禁用，reason 全是「429 rate limit」，
// 而它们的 credits 分别是 1309 / 280 / 1591 —— **有额度**。
//
// 根因：applyErrorPolicy 的自动禁用 switch 把 SoftRate 计了进去，
// 连续 5 次 429 就永久摘除一个号。
//
// # 为什么这是错的
//
// 429 的语义是「你打太快了，等一会儿」，不是「这个号坏了」：
//
//	· 同一函数下面**已经**对 SoftRate 做了软冷却（CoolSoft + 指数退避），
//	  那才是正确动作 —— 冷却到窗口结束就能再用。
//	· 再叠加禁用是**双重惩罚**：冷却本该"暂时"，禁用是"长期摘除"。
//	  一个限流把产能永久打掉，而界面上只显示"已禁用"，看不出是限流引起的。
//
// ⇒ 保留 HardCredit / SessionDead / NotFound / Server 四条。
func TestSoftRateDoesNotAutoDisable(t *testing.T) {
	src := sliceFuncBody(t, readHandlerSrc(t), "func (h *Handler) applyErrorPolicy(")
	if src == "" {
		t.Fatal("找不到 applyErrorPolicy")
	}
	// 只看自动禁用那一段（RecordAutoDisableFailure 之前 1200 字节内）
	idx := strings.Index(src, "RecordAutoDisableFailure")
	if idx < 0 {
		t.Fatal("找不到 RecordAutoDisableFailure")
	}
	start := idx - 1200
	if start < 0 {
		start = 0
	}
	seg := src[start:idx]

	// 这一段里不能出现 ErrKindSoftRate（出现即说明它又被计进禁用线）。
	if strings.Contains(seg, "ErrKindSoftRate") {
		// 但要排除注释里的提及 —— 去掉注释行再判。
		var code []string
		for _, line := range strings.Split(seg, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			code = append(code, line)
		}
		if strings.Contains(strings.Join(code, "\n"), "ErrKindSoftRate") {
			t.Error("★ SoftRate 又被计进自动禁用线了 —— 一个 429 限流会永久摘除有额度的号")
		}
	}
}

// 429 仍然要做软冷却（不能因为不禁止就什么都不做）。
func TestSoftRateStillCools(t *testing.T) {
	src := sliceFuncBody(t, readHandlerSrc(t), "func (h *Handler) applyErrorPolicy(")
	if !strings.Contains(src, "ErrKindSoftRate") {
		t.Fatal("找不到 SoftRate 的处理")
	}
	// 找到 SoftRate 的 case 块，里面必须有 Cooldown(..., CoolSoft, ...)
	i := strings.Index(src, "case gateway.ErrKindSoftRate:")
	if i < 0 {
		t.Fatal("找不到 SoftRate 的 case")
	}
	seg := src[i:]
	if len(seg) > 300 {
		seg = seg[:300]
	}
	if !strings.Contains(seg, "CoolSoft") {
		t.Error("★ SoftRate 没有软冷却 —— 429 之后号会被立刻再选中，造成雪崩")
	}
}
