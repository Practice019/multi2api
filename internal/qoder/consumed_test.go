package qoder

import "testing"

// consumed_test.go 消耗累计器的守卫。
//
// # 为什么需要"累计 + 跨整数才上报"（这是 qoder 特有的复杂度）
//
// 实测 `usage.credits` = `0.006352698571428571`/次，而池子里的显示值是
// **整数 credits**（用户明确要求"显示原始值不换算"）。于是：
//
//	直接写 → 写进去是 0（截断）
//	每次减 → 减 0，永远扣不动
//
// 正确做法是内部累计小数、跨过整数才上报。**而这里最容易出的错是"余量丢了"**
//（每次把不足 1 credit 的部分抹掉）—— 那会长期少扣，表现为"额度看起来比实际多"，
// 而"以为还有额度"正是用户最不想要的形态。

func TestConsumedAccReportsWholeUnitsOnly(t *testing.T) {
	a := newConsumedAcc()
	const uid = "u1"

	// 0.6 credit：不足 1 ⇒ 不上报
	if got := a.add(uid, 600_000); got != 0 {
		t.Errorf("0.6 credit 不该上报，实际 %d", got)
	}
	// 再 0.6 ⇒ 累计 1.2 ⇒ 上报 1（并保留 0.2）
	if got := a.add(uid, 600_000); got != 1 {
		t.Errorf("累计 1.2 credit 应上报 1，实际 %d", got)
	}
	// 再 0.8 ⇒ 余量 0.2 + 0.8 = 1.0 ⇒ 上报 1
	if got := a.add(uid, 800_000); got != 1 {
		t.Errorf("余量 0.2 加 0.8 应上报 1，实际 %d", got)
	}
	// 余量应当正好清空：再 0.5 ⇒ 不上报
	if got := a.add(uid, 500_000); got != 0 {
		t.Errorf("0.5 credit 不该上报，实际 %d", got)
	}
}

// ★ **余量必须保留** —— 这是本文件最要紧的一条。
//
// 抹掉余量 = 每次都把不足 1 credit 的部分丢掉 = 长期少扣 =
// "额度看起来比实际多"。而上游的消耗恰恰**几乎每次都小于 1**。
func TestConsumedAccKeepsRemainder(t *testing.T) {
	a := newConsumedAcc()
	const uid = "u1"

	// 模拟真实的 qoder 消耗：每次 0.00635 credit。
	//
	// ⚠ 次数要**算准**：157 × 6352 = 997,264 微 < 1e6 ⇒ 还差一点，不该上报；
	// 158 次才跨过 1 credit。我第一版写的 157 就是算错了（代码是对的，
	// 断言错了）—— 所以下面两个边界都断言，而不是只挑一个。
	const per = 6352 // 微 credits（实测 0.006352698571428571 截到微）
	var total int64
	for i := 0; i < 157; i++ {
		total += a.add(uid, per)
	}
	if total != 0 {
		t.Errorf("157 次 × 0.006352 = 0.997 credit，还不足 1，不该上报（实际 %d）", total)
	}
	// 第 158 次跨过 1 credit ⇒ 必须上报 1（**余量保留**，所以是 1 而不是 0）。
	if got := a.add(uid, per); got != 1 {
		t.Errorf("★ 第 158 次累计跨过 1 credit ⇒ 必须上报 1，实际 %d ——\n"+
			"\n"+
			"若这里是 0，说明余量被抹掉了（每次都丢掉不足 1 的部分）：\n"+
			"上游的消耗**几乎每次都小于 1 credit** ⇒ 抹掉就等于永远不扣 ⇒\n"+
			"额度看起来比实际多，用户以为还有免费额度而继续用。", got)
	}

	// 反向：不能多报（单次远不足 1 时必须报 0）。
	b := newConsumedAcc()
	if got := b.add(uid, per); got != 0 {
		t.Errorf("单次 0.00635 credit 远不足 1，不该上报，实际 %d", got)
	}
}

// reset 清空（成功查过余额之后必须调）。
func TestConsumedAccReset(t *testing.T) {
	a := newConsumedAcc()
	const uid = "u1"
	a.add(uid, 900_000) // 攒 0.9
	a.reset(uid)
	// 清空之后再攒 0.9 仍不该上报（若没清空，1.8 就会上报 1）
	if got := a.add(uid, 900_000); got != 0 {
		t.Errorf("reset 之后应当从零开始（实际上报 %d）——\n"+
			"不清的话，那次查询（值已包含这些消耗）之后我们还会再上报一遍 ⇒ 多扣", got)
	}
	// reset 不存在的 uid 不该 panic
	a.reset("不存在")
	a.reset("")
}

// 非正数/空 uid：静默忽略（不 panic、不污染累计）。
func TestConsumedAccIgnoresBadInput(t *testing.T) {
	a := newConsumedAcc()
	if got := a.add("", 1_000_000); got != 0 {
		t.Errorf("空 uid 不该上报，实际 %d", got)
	}
	if got := a.add("u1", 0); got != 0 {
		t.Errorf("0 消耗不该上报，实际 %d", got)
	}
	if got := a.add("u1", -5); got != 0 {
		t.Errorf("负数消耗不该上报，实际 %d", got)
	}
}

// ⚠ spec 必须是 UsageConsumed —— 声明错的代价是编数据。
//
// （`gateway.UsageTapSpec.Validate` 会拦 Unknown；这条钉住"我们声明的是对的那个"。）
func TestQuotaTapSpecIsConsumed(t *testing.T) {
	s := quotaTapSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("spec 非法: %v", err)
	}
	if s.Key != "credits" {
		t.Errorf("键名应取实测到的 credits，实际 %q", s.Key)
	}
	if s.Scale != creditsScale {
		t.Errorf("Scale 应为 %d（把小数换算成整数），实际 %d", creditsScale, s.Scale)
	}
	if s.Mode.String() != "consumed" {
		t.Errorf("★ Mode 应为 consumed，实际 %q ——\n"+
			"`usage.credits` 是**消耗掉的额度**（与余额同单位），不是 cline 那种\n"+
			"「成本/费用」。声明错的代价是编数据（把成本当额度扣）。", s.Mode)
	}
}
