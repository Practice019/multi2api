package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 全部软冷却（429 限流）时，选号必须**立刻返回 nil**，不能兜底。
//
// # 用户实测报障（2026-10-24）
//
//	「这个渠道没有可用的账号了 但是还是会尝试访问」
//	「不就是没有健康的账号么 那就不要尝试选渠道的时候就别选不就好了」
//
// 数据：workbuddy 上游 6 个号全部 soft_rate 冷却，请求仍花 **1.3-1.6 秒**
// 才回 503；而成功请求的同等阶段只要 5-18ms。差两个数量级。
//
// 根因：pickEarliestExpiryLocked 的兜底判据只跳过长冷却（CoolHard），
// 软冷却的号会被选中 ⇒ 请求真的发出去 ⇒ 上游再回一次 429 ⇒
// 白等一个完整往返（约 270-300ms），试满预算就是 1.3-1.6 秒。
//
// ⚠ 429 是上游**当场告诉我们**的事实，没有猜测空间 —— 与熔断不同
// （熔断是连续失败推断出来的，可能已经恢复，值得试一次）。
func TestAllSoftCooledReturnsNilImmediately(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.Cooldown("a", CoolSoft, time.Hour, "429")
	p.Cooldown("b", CoolSoft, time.Hour, "429")

	if got := p.Pick(); got != nil {
		t.Fatalf("★ 全部软冷却时仍选出了 %q —— 会白打一次必然失败的请求（用户报的 1.5 秒）", got.UID)
	}
}

// 软冷却 + 健康号混在一起时，只能选健康号（不能兜底到冷却的那个）。
func TestSoftCooledSkippedWhenHealthyExists(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cooled"})
	p.Add(&auth.Auth{UID: "ok"})
	p.Cooldown("cooled", CoolSoft, time.Hour, "429")

	for i := 0; i < 10; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("有健康号时应能选出")
		}
		if got.UID != "ok" {
			t.Fatalf("★ 选到了软冷却的号 %q（应只选 ok）", got.UID)
		}
	}
}

// 熔断仍然兜底 —— 熔断是推断出来的，可能已恢复。
//
// 这条与上面两条互补：改动不能把熔断的兜底一起砍掉。
func TestBreakerStillFallsBack(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "tripped"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("tripped") // 触发熔断

	if got := p.Pick(); got == nil || got.UID != "tripped" {
		t.Fatalf("★ 熔断号没被兜底选中（got=%v）—— 熔断可能已恢复，值得试一次", got)
	}
}

// 长冷却（额度耗尽）也不兜底 —— 与软冷却同一口径。
func TestHardCooledReturnsNil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Cooldown("a", CoolHard, time.Hour, "quota")

	if got := p.Pick(); got != nil {
		t.Fatalf("★ 长冷却时仍选出了 %q", got.UID)
	}
}
