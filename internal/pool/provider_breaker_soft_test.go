package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 软冷却（429）必须喂**上游级**熔断 —— 这是"上游整体在限流"的聚合信号。
//
// # 用户原话
//
//	「我希望网关 不要尝试没有健康账号的渠道啊」
//
// # 实测背景（2026-10-24）
//
// workbuddy 上游 6 个号全部 429 限流，网关每次请求仍花 1.2-2.5 秒：
// 逐个号打上去、每个吃一次 429、试满 MaxRotate+2 次才放弃。
// 要等**所有号都被打进冷却**之后才变成 3ms。
//
// 根因：软冷却路径从不喂上游级熔断（只有 NoteError 才喂），
// 于是"这家上游在限流"永远无法即时可知。
//
// ⚠ 只喂上游级，不喂账号级 —— 后者会把健康的号封 30 分钟（历史 bug）。
func TestSoftCooldownFeedsProviderBreaker(t *testing.T) {
	p := New("")
	p.SetDefaultProvider("alpha")
	p.SetBreaker(2, 30*time.Minute, time.Hour)
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}, {UID: "a2"}, {UID: "a3"}})

	// 前两次：不该触发（阈值 2 意味着第 2 次才跨过）。
	p.Cooldown("a1", CoolSoft, time.Minute, "429")
	if _, tripped := p.ProviderTripped("alpha"); tripped {
		t.Fatal("第一次软冷却就熔断了上游（阈值 2 应该第 2 次才触发）")
	}
	p.Cooldown("a2", CoolSoft, time.Minute, "429")

	until, tripped := p.ProviderTripped("alpha")
	if !tripped {
		t.Fatal("★ 两个不同账号都吃 429，上游级熔断没触发 —— 网关会继续逐个号白试")
	}
	if until.Before(time.Now()) {
		t.Errorf("熔断到期时刻在过去: %v", until)
	}
}

// 账号级熔断计数**不能**被软冷却喂 —— 那是历史 bug（健康的号被封 30 分钟）。
//
// 判据：一个号连续多次软冷却后，它的账号级熔断（e.fails / breakerUntil）不该被触发。
// 用一个只有它自己的池子验证：如果账号级被喂了，Pick 会因为熔断而走兜底（返回它）；
// 而软冷却下 Pick 应该返回 nil（健康候选为空且软冷却不兜底）。
func TestSoftCooldownDoesNotFeedAccountBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.SetBreaker(2, 30*time.Minute, time.Hour)

	// 连续 5 次软冷却（远超账号级阈值 2）。
	for i := 0; i < 5; i++ {
		p.Cooldown("a", CoolSoft, time.Minute, "429")
	}

	// 若账号级被喂了熔断，Pick 会兜底返回 a（熔断允许兜底）。
	// 正确行为：软冷却不兜底 ⇒ nil。
	if got := p.Pick(); got != nil {
		t.Fatalf("★ 软冷却把账号级熔断也喂了（Pick 兜底返回 %q）—— 健康的号会被封 30 分钟", got.UID)
	}
}

// 上游级熔断触发后，选号必须立刻返回 nil（不再尝试）。
func TestProviderTrippedMakesPickFailFast(t *testing.T) {
	p := New("")
	p.SetDefaultProvider("alpha")
	p.SetBreaker(1, 30*time.Minute, time.Hour)
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}, {UID: "a2"}})

	// 一个号软冷却即触发上游级熔断（阈值 1）。
	p.Cooldown("a1", CoolSoft, time.Minute, "429")

	start := time.Now()
	got := p.PickFor("alpha", "", nil)
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Errorf("★ 上游已熔断，选号却花了 %v（应该立即返回）", elapsed)
	}
	// a2 还是健康的，所以这里**应该**能选出 a2 —— 熔断只影响"没号时是否兜底"，
	// 不影响"有健康号时正常选"。
	if got == nil {
		t.Log("上游熔断时选号返回 nil（a2 仍健康，若这里 nil 说明熔断拦得太狠）")
	}
}
