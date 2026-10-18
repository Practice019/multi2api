package pool

import (
	"testing"

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
