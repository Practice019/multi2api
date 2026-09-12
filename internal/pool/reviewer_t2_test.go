package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// 复现评审 F1（CRITICAL）：stateOverviewLocked 从不写 Quota。
//
// 指控：SetQuota(per_model{...}) + Flush() 之后，state.json 里是
//   "quota": {},  "credits": 300
// 重载后 Kind 变回 "credits" —— **按模型额度在每次落盘时被销毁**。
//
// 这正是这个提交本该防止的"退化成 int64"。
func TestReviewerF1_PerModelQuotaSurvivesFlush(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetQuota("u1", QuotaView{
		Kind:    QuotaKindPerModel,
		ByModel: map[string]int64{"gpt-5.5": 300, "claude": 500},
		HasData: true,
	})
	p.Flush()

	// 先看落盘内容
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	accts, _ := doc["accounts"].(map[string]any)
	u1, _ := accts["u1"].(map[string]any)
	t.Logf("落盘内容 u1: %v", u1)

	qv, hasQuotaField := u1["quota"]
	if !hasQuotaField {
		t.Errorf("落盘缺少 quota 字段 —— 按模型额度会丢失")
	} else {
		qm, _ := qv.(map[string]any)
		if qm["kind"] != QuotaKindPerModel {
			t.Errorf("落盘的 quota.kind=%v，期望 %q —— 额度形态未被持久化",
				qm["kind"], QuotaKindPerModel)
		}
		if _, ok := qm["by_model"]; !ok {
			t.Errorf("落盘的 quota 缺 by_model —— 按模型额度未被持久化")
		}
	}

	// 再看重载结果
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("账号未恢复")
	}
	t.Logf("重载后: Quota=%+v Effective=%d", st.Quota, st.Quota.Effective())
	if st.Quota.Kind != QuotaKindPerModel {
		t.Errorf("重载后 Kind=%q，期望 %q —— 按模型额度退化成了单值",
			st.Quota.Kind, QuotaKindPerModel)
	}
	if got := st.Quota.ByModel["claude"]; got != 500 {
		t.Errorf("重载后 claude 额度=%d，期望 500 —— 按模型额度丢失", got)
	}
}

// 评审 F2（HIGH）：SetCredits 曾把 per_model 降级成 credits。
//
// 修复后的**正确行为**：保留额度形态，只更新派生标量。
// 本测试断言修复后的行为（先前它断言的是 bug，已翻转）。
func TestReviewerF2_SetCreditsMustNotDowngradeKind(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 先设成按模型额度
	p.SetQuota("u1", QuotaView{
		Kind:    QuotaKindPerModel,
		ByModel: map[string]int64{"gpt-5.5": 900, "claude": 300},
		HasData: true,
	})
	st, _ := p.Status("u1")
	if st.Quota.Effective() != 900 {
		t.Fatalf("前置：Effective 应为 900，得到 %d", st.Quota.Effective())
	}

	// 走旧的 SetCredits（scheduler 的「刷新积分」、旅行领奖后同步都这么调）
	p.SetCredits("u1", 42)

	st2, _ := p.Status("u1")
	if st2.Quota.Kind != QuotaKindPerModel {
		t.Errorf("SetCredits 把额度形态降级了：Kind=%q，期望仍为 %q\n"+
			"  旧调用点想表达的是「刷新余额数字」，不该有「把额度模型改成单值」的副作用",
			st2.Quota.Kind, QuotaKindPerModel)
	}
	if st2.Quota.ByModel["claude"] != 300 {
		t.Errorf("按模型额度被冲掉了：%v", st2.Quota.ByModel)
	}
	if st2.Quota.Effective() != 900 {
		t.Errorf("Effective 应仍为 900，得到 %d", st2.Quota.Effective())
	}
}

// 评审 F2 的第二半：unlimited 也不该被降级。
func TestReviewerF2_UnlimitedNotDowngraded(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetQuota("u1", QuotaView{Kind: QuotaKindUnlimited, HasData: true})

	p.SetCredits("u1", 10)

	st, _ := p.Status("u1")
	if st.Quota.Kind != QuotaKindUnlimited {
		t.Errorf("unlimited 被降级成 %q", st.Quota.Kind)
	}
}

// 评审 F4：per_model + 空 ByModel 曾会变成"可信的 0"。
//
// 修复后的**正确行为**：内容为空时回落到 Credits，
// 不让一个自称 per_model 但表为空的状态把账号静默降权。
func TestReviewerF4_EmptyPerModelFallsBackToCredits(t *testing.T) {
	restored := restoreQuota(QuotaView{
		Kind:    QuotaKindPerModel,
		HasData: true,
		// ByModel 为 nil（"自称有数据，实际没有"）
	}, 5000)

	if restored.Kind != QuotaKindCredits {
		t.Errorf("空内容的 per_model 应回落到 credits，得到 %q", restored.Kind)
	}
	if restored.Effective() != 5000 {
		t.Errorf("应保留 credits=5000，得到 %d —— 账号被静默降权了",
			restored.Effective())
	}
}

// 有内容的 per_model 必须原样保留（别把修复做成"一律回落"）。
func TestReviewerF4_NonEmptyPerModelIsPreserved(t *testing.T) {
	restored := restoreQuota(QuotaView{
		Kind:    QuotaKindPerModel,
		ByModel: map[string]int64{"m": 7},
		HasData: true,
	}, 5000)

	if restored.Kind != QuotaKindPerModel {
		t.Errorf("有内容的 per_model 不该被回落，得到 %q", restored.Kind)
	}
	if restored.ByModel["m"] != 7 {
		t.Errorf("按模型额度丢失: %v", restored.ByModel)
	}
}

// 评审 F3（HIGH）：Effective() 取最大值对**选号是反的**。
//
//	X = {gpt-5.5: 1000, other: 0}      → Effective = 1000
//	Y = {gpt-5.5: 0,    other: 999999} → Effective = 999999
//
// 请求 gpt-5.5 时 Y 对该模型**零额度**，却排在 X 前面 → 选到跑不了的账号。
// 修复：按模型取值用 EffectiveFor。
func TestReviewerF3_EffectiveForIsModelAware(t *testing.T) {
	x := QuotaView{Kind: QuotaKindPerModel, ByModel: map[string]int64{"gpt-5.5": 1000}, HasData: true}
	y := QuotaView{Kind: QuotaKindPerModel, ByModel: map[string]int64{"other": 999999}, HasData: true}

	// 旧口径（Effective）—— 反的
	if !(y.Effective() > x.Effective()) {
		t.Fatal("前置：Effective() 下 Y 确实大于 X（这正是缺陷）")
	}

	// 新口径（EffectiveFor）—— 对请求的模型而言 X 可用、Y 不可用
	if got := x.EffectiveFor("gpt-5.5"); got != 1000 {
		t.Errorf("X 对 gpt-5.5 应有 1000，得到 %d", got)
	}
	if got := y.EffectiveFor("gpt-5.5"); got != 0 {
		t.Errorf("Y 对 gpt-5.5 应无额度（0），得到 %d —— 仍会选到跑不了的账号", got)
	}
	if !(x.EffectiveFor("gpt-5.5") > y.EffectiveFor("gpt-5.5")) {
		t.Error("按模型取值后 X 应排在 Y 前面")
	}
}

// EffectiveFor 对不分模型的形态应与 Effective 一致。
func TestReviewerF3_EffectiveForNonPerModel(t *testing.T) {
	c := QuotaView{Kind: QuotaKindCredits, Remaining: 42, HasData: true}
	if c.EffectiveFor("任意模型") != 42 {
		t.Error("credits 形态下 EffectiveFor 应等于 Remaining（不分模型）")
	}
	u := QuotaView{Kind: QuotaKindUnlimited, HasData: true}
	if u.EffectiveFor("任意模型") != u.Effective() {
		t.Error("unlimited 形态下 EffectiveFor 应等于 Effective")
	}
	unknown := QuotaView{}
	if unknown.EffectiveFor("m") != 0 {
		t.Error("未探测应为 0")
	}
}

// 评审 F4：HasData 的区分必须**真的被用到**，而不是装饰。
func TestReviewerF4_UnknownQuotaIsSelectable(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "unknown"})
	p.Add(&auth.Auth{UID: "known-zero"})
	// 一个从没探测过额度；一个明确知道是 0
	// unknown 不调用 SetQuota；known-zero 明确设 0
	p.SetQuota("known-zero", QuotaView{Kind: QuotaKindCredits, Remaining: 0, HasData: true})

	// 两者 Effective 都是 0，所以都能参与选号（不会因为"未探测"被排除）
	for i := 0; i < 200; i++ {
		if p.Pick() == nil {
			t.Fatal("Pick 返回 nil —— 未探测额度的账号不该被排除")
		}
	}
	st, _ := p.Status("unknown")
	if !st.Quota.IsUnknown() {
		t.Error("未 SetQuota 的账号应报告 IsUnknown")
	}
	st2, _ := p.Status("known-zero")
	if st2.Quota.IsUnknown() {
		t.Error("明确设为 0 的账号不该报告 IsUnknown")
	}
}

// 复现评审 F7（新增 nit）：Status.Quota 把内部 map 直接递出去。
func TestReviewerF7_QuotaMapMustBeCopied(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetQuota("u1", QuotaView{
		Kind:    QuotaKindPerModel,
		ByModel: map[string]int64{"m": 1},
		HasData: true,
	})

	st, _ := p.Status("u1")
	if st.Quota.ByModel != nil {
		st.Quota.ByModel["m"] = 424242 // 外部改动
	}

	st2, _ := p.Status("u1")
	if st2.Quota.ByModel["m"] == 424242 {
		t.Errorf("外部通过 Status 返回值改到了内部状态（%d）—— 必须深拷贝 ByModel",
			st2.Quota.ByModel["m"])
	}
}
