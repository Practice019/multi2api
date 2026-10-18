package pool

import (
	"strconv"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ⚠ 本文件钉住「选号时排除**明确知道**已无额度的账号」（2026-10-18）。
//
// # 为什么（用户原话）
//
//	"我感觉他在尝试无额度的渠道或账号，这些都是可以避免的，
//	 我希望可以让轮询变得和使用单个账号一样就行了"
//
// ⚠ 而代码里**确实如此**：`pool.go` 的 `weightOf` 注释自己写着
//   "额度**不再参与选号**…只保留成功率 + 闲置两个因子" ⇒ ⚠ 额度为 0 的号照旧被随机选中
//   ⇒ 打上去必然失败/空等（⚠ 那也正是"首字 18 秒"那种慢号的来源）。
//
// # 判据必须是"明确知道"（⚠ 别把"未知"当成"零"）
//
//	quota.go 的 `EffectiveFor(model)` 对 `!HasData` 与 `per_model` 缺键**都返回 0**，
//	而那两个语义是"**还没探测过**"而不是"没钱" ⇒ ⚠ 直接用它判会把新号永久排掉。
//	所以只排这两类：
//	  · credits 且 Remaining <= 0
//	  · per_model 且该模型键**存在**且 <= 0
//	⚠ unlimited 与 未知 一律不排。

func TestPickSkipsExhaustedCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "dead"})
	p.Add(&auth.Auth{UID: "alive"})
	p.SetCredits("dead", 0) // ⚠ 明确知道 0
	p.SetCredits("alive", 100)

	for i := 0; i < 60; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatalf("第 %d 次选号返回 nil（有 alive 可用）", i)
		}
		if got.UID != "alive" {
			t.Fatalf("第 %d 次选到了已耗尽的账号 %s —— 那正是用户抱怨的『尝试无额度的号』", i, got.UID)
		}
	}
}

func TestPickKeepsUnknownCredits(t *testing.T) {
	// ⚠ 从未 SetCredits 的号 = **未知额度**，不许当成"零"排掉
	//   （否则：新导入的账号永远选不到，而界面上它明明在池里 —— 又是一个难查的静默问题）
	p := New("")
	p.Add(&auth.Auth{UID: "fresh"})
	for i := 0; i < 20; i++ {
		if p.Pick() == nil {
			t.Fatalf("第 %d 次：未知额度的号不该被排除", i)
		}
	}
}

func TestPickFallsBackWhenAllExhausted(t *testing.T) {
	// ⚠ 全都 0 额度 ⇒ **不能返回 nil**（那会把"没额度"变成一个新的 503）
	//   ⇒ 退回旧行为：照常参与选号（打上去才知道，但至少不误报"池子空了"）
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 0)
	p.SetCredits("b", 0)
	if got := p.Pick(); got == nil {
		t.Fatal("全部账号额度为 0 时应退回旧行为（仍可选），而不是返回 nil 制造 503")
	}
}

// ⚠ 上游级熔断（2026-10-18 加）：账号级熔断的**聚合效应**是
// "某个上游整体坏了，也要等它的号被逐个熔断完才会表现为选不出" ——
// 在 44 个号的规模下，这意味着每个请求都要白试若干个号。
//
// 上游级熔断把"这个上游现在整体不通"变成**即时可知**的事实。
// ⚠ 退避算法与账号级**同构**（照抄 recordBreakerFailureLocked），阈值/时长也复用同一组参数 ——
//   避免出现"账号 3 次熔断、上游 5 次才熔断"这种第二个真相。
func TestProviderBreakerTripsAfterThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"}) // ⚠ 不给 provider ⇒ 归默认上游
	if _, ok := p.ProviderTripped(""); ok {
		t.Fatal("刚开始不该有上游级熔断")
	}
	// ⚠ 阈值默认 3（defaultBreakerThreshold）：喂 3 次失败即触发
	p.NoteError("u1")
	p.NoteError("u1")
	if _, ok := p.ProviderTripped(""); ok {
		t.Fatal("2 次失败还不该触发（阈值 3）—— 否则抖动就会打掉整个上游")
	}
	p.NoteError("u1")
	until, ok := p.ProviderTripped("")
	if !ok {
		// ⚠ 诊断输出：不再猜，把中间状态打出来（同包可直接读私有字段）
		p.mu.RLock()
		keys := make([]string, 0, len(p.provState))
		for k, st := range p.provState {
			keys = append(keys, "key="+k+" fails="+string(rune('0'+st.fails))+" tripped="+strconv.FormatBool(!st.breakerUntil.IsZero()))
		}
		e := p.byUID["u1"]
		provOf := "<nil>"
		if e != nil {
			provOf = p.providerOf(e)
		}
		p.mu.RUnlock()
		t.Fatalf("3 次失败后应触发上游级熔断。诊断: defaultProvider=%q providerOf(u1)=%q 阈值=%d 窗口=%v provState=%v",
			p.defaultProvider, provOf, p.breakerThreshold, breakerFailureWindow, keys)
	}
	if until.Before(time.Now()) {
		t.Errorf("熔断截止时刻 %v 应在未来", until)
	}
}

// TestProviderBreakerClearedByOneSuccess ⚠ 一次成功就该清掉上游的失败线。
//
// ⚠ 不这么做的话：跨天的无关抖动会一路攒到阈值，把**健康的上游**误判成熔断
//   —— 账号级 NoteSuccess 的注释里为同一件事写过理由（"一次成功证明…"）。
func TestProviderBreakerClearedByOneSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1")
	p.NoteError("u1")
	p.NoteSuccess("u1") // ⚠ 应把上游级计数清零
	p.NoteError("u1")   // 这是清零后的第 1 次
	if _, ok := p.ProviderTripped(""); ok {
		t.Fatal("成功清零后不该立刻熔断（否则抖动累积会误伤健康上游）")
	}
}

// TestProviderBreakerIsPerProvider ⚠ 一个上游熔断**不能**影响另一个上游。
//
// ⚠ 这条是"多账号正常使用"的守门：若两家的计数混在一起，
//   loomy 坏掉会把 workbuddy 一起停掉 —— 而那恰恰是**唯一健康的那家**（实测它失败率 0.13%）。
func TestProviderBreakerIsPerProvider(t *testing.T) {
	p := New("")
	// ⚠ provider 是**池子侧的标签**，不在 auth.Auth 上（它没有 Provider 字段）——
	//   由 upsertLockedFor 在首次插入时写入。同包测试可直接用它。
	p.upsertLockedFor("loomy", &auth.Auth{UID: "loomy1"})
	p.upsertLockedFor("workbuddy", &auth.Auth{UID: "wb1"})
	for i := 0; i < 3; i++ {
		p.NoteError("loomy1")
	}
	if _, ok := p.ProviderTripped("loomy"); !ok {
		t.Fatal("loomy 喂满阈值后应熔断")
	}
	if _, ok := p.ProviderTripped("workbuddy"); ok {
		t.Fatal("workbuddy 不该被 loomy 的失败牵连 —— 那是唯一健康的上游")
	}
}
