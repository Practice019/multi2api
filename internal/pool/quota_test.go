package pool

import (
	"encoding/json"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// Task 2：把 pool 的**额度语义**从上游专属的"积分"抽象出来。
//
// # 为什么必须改
//
// 现状 `Status.Credits int64` 与 `SetCredits(uid, int64)` 隐含假设
// "额度就是一个整数"。但第二个上游 codearts 的额度是**按模型**的
// （`ProbeQuota(model) map[string]QuotaState`），一个 int64 装不下。
//
// # 审计判据
//
// `node D:\tmp\audit_precise.js` 中 internal/pool 的上游概念数必须降到 **0**。
// 现状 18 处（全是 Credits）。
//
// # 设计
//
//	QuotaView{
//	    Kind      string             // "credits" | "per_model" | "unlimited" | "unknown"
//	    Remaining int64              // 单值额度（workbuddy 用）
//	    ByModel   map[string]int64   // 按模型额度（codearts 用）
//	    HasData   bool               // 是否探测过（区分"0 额度"与"没查过"）
//	}
//
// 关键约束：**pool 不解释额度语义**。
// "这个账号能不能用"由上游回答（`Usable bool`），pool 只做选号权重。

// TestQuotaViewZeroValueIsUnknown 零值必须表示"未知"，不能表示"0 额度"。
//
// 这个区分很重要：新账号还没探测过额度时，不能因为"额度 0"就永远排在最后。
func TestQuotaViewZeroValueIsUnknown(t *testing.T) {
	var q QuotaView
	if q.HasData {
		t.Error("零值应表示未探测（HasData=false）")
	}
	if q.Kind != "" {
		t.Errorf("零值 Kind 应为空，得到 %q", q.Kind)
	}
	// Effective() 在未探测时返回 0
	if got := q.Effective(); got != 0 {
		t.Errorf("未探测时 Effective()=%d want 0", got)
	}
}

// TestQuotaViewSingleValue 单值额度（workbuddy 的积分）。
func TestQuotaViewSingleValue(t *testing.T) {
	q := QuotaView{Kind: QuotaKindCredits, Remaining: 4200, HasData: true}
	if got := q.Effective(); got != 4200 {
		t.Errorf("Effective()=%d want 4200", got)
	}
}

// TestQuotaViewPerModel 按模型额度（codearts）。
//
// Effective() 取**最大值**而不是求和：它表达"这个账号最多还能跑多少"，
// 用于展示是合理的。**但选号必须用 EffectiveFor(模型)** ——
// 取最大值会让"对请求的模型零额度、但别的模型额度很高"的账号被优先选中。
func TestQuotaViewPerModel(t *testing.T) {
	q := QuotaView{
		Kind:    QuotaKindPerModel,
		ByModel: map[string]int64{"gpt-5.5": 100, "claude": 300, "gemini": 50},
		HasData: true,
	}
	if got := q.Effective(); got != 300 {
		t.Errorf("Effective()=%d want 300（取最大值，不求和）", got)
	}
	// 选号用的口径：按请求的模型取值
	if got := q.EffectiveFor("gpt-5.5"); got != 100 {
		t.Errorf("EffectiveFor(gpt-5.5)=%d want 100", got)
	}
	if got := q.EffectiveFor("不存在的模型"); got != 0 {
		t.Errorf("EffectiveFor(未列出的模型)=%d want 0", got)
	}
}

// TestQuotaViewUnlimited 无限额度的账号应当排最前。
func TestQuotaViewUnlimited(t *testing.T) {
	q := QuotaView{Kind: QuotaKindUnlimited, HasData: true}
	if got := q.Effective(); got <= 0 {
		t.Errorf("无上限账号的 Effective() 应为正数（排最前），得到 %d", got)
	}
}

// TestQuotaViewJSONRoundTrip JSON 契约：前端与落盘都依赖它。
func TestQuotaViewJSONRoundTrip(t *testing.T) {
	orig := QuotaView{
		Kind:      QuotaKindPerModel,
		Remaining: 42,
		ByModel:   map[string]int64{"a": 1, "b": 2},
		HasData:   true,
	}
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back QuotaView
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Kind != orig.Kind || back.Remaining != orig.Remaining || !back.HasData {
		t.Errorf("往返后不一致: %+v vs %+v", back, orig)
	}
	if back.ByModel["b"] != 2 {
		t.Errorf("ByModel 未正确往返: %v", back.ByModel)
	}
}

// TestSetQuotaAndStatus 新的写入口 + Status 暴露。
func TestSetQuotaAndStatus(t *testing.T) {
	p := poolWithU1(t)
	p.SetQuota("u1", QuotaView{Kind: QuotaKindCredits, Remaining: 777, HasData: true})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号不存在")
	}
	if st.Quota.Remaining != 777 || !st.Quota.HasData {
		t.Errorf("Status.Quota=%+v want 777/HasData", st.Quota)
	}
}

// TestSetQuotaPerModel 按模型额度也能存进 Status。
func TestSetQuotaPerModel(t *testing.T) {
	p := poolWithU1(t)
	p.SetQuota("u1", QuotaView{
		Kind:    QuotaKindPerModel,
		ByModel: map[string]int64{"m1": 5, "m2": 9},
		HasData: true,
	})
	st, _ := p.Status("u1")
	if st.Quota.Kind != QuotaKindPerModel || st.Quota.Effective() != 9 {
		t.Errorf("Status.Quota=%+v want per_model/9", st.Quota)
	}
}

// TestUsableDrivesReenable 「能不能用」由调用方（上游）判断，pool 不解释额度语义。
//
// 对应旧 API 的 ReenableIfCredits(uid, remain)：它把"余额 > 0 就解冻"
// 这条上游专属规则写进了 pool。改为 ReenableIfUsable(uid, usable, quota)。
func TestUsableDrivesReenable(t *testing.T) {
	p := poolWithU1(t)
	// 让它进入冷却
	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	before, _ := p.Status("u1")
	if !before.Cooling {
		t.Fatal("前置：应处于冷却")
	}

	// usable=false → 不解冻
	p.ReenableIfUsable("u1", false, QuotaView{Kind: QuotaKindCredits, Remaining: 0, HasData: true})
	mid, _ := p.Status("u1")
	if !mid.Cooling {
		t.Error("usable=false 时不该解冻")
	}

	// usable=true → 解冻
	p.ReenableIfUsable("u1", true, QuotaView{Kind: QuotaKindCredits, Remaining: 500, HasData: true})
	after, _ := p.Status("u1")
	if after.Cooling {
		t.Error("usable=true 时应解冻")
	}
	if after.Quota.Remaining != 500 {
		t.Errorf("解冻时应同时更新额度，得到 %+v", after.Quota)
	}
}

// TestSelectionWeightUsesEffective 选号权重用 Effective()，
// 两种额度形态都能参与三因子加权。
//
// 注意 withNoPickGap：默认的防并发撞号窗口会把分布强行均匀化
// （两个账号各 50%），从而掩盖权重差异 —— 既有测试同样这样处理。
func TestSelectionWeightUsesEffective(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "poor"})
	// rich 用单值、poor 用按模型 —— 两种形态都要能进权重
	p.SetQuota("rich", QuotaView{Kind: QuotaKindCredits, Remaining: 100000, HasData: true})
	p.SetQuota("poor", QuotaView{Kind: QuotaKindPerModel, ByModel: map[string]int64{"m": 1}, HasData: true})

	got := map[string]int{}
	for i := 0; i < 3000; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatal("Pick 返回 nil")
		}
		got[a.UID]++
	}
	if got["rich"] <= got["poor"] {
		t.Errorf("高额度账号应更常被选中: rich=%d poor=%d", got["rich"], got["poor"])
	}
}

// ---------------------------------------------------------------- 兼容层

// TestLegacySetCreditsStillWorks 旧的 SetCredits / ReenableIfCredits 保留为薄封装。
//
// 为什么保留：**55 处测试调用点**依赖它们（46 处 pool_test + 6 处 handler_test + …）。
// 全部改名会让这次提交淹没在噪音里，且增加回归风险。
// 保留薄封装 = 零调用点改动，同时新代码用新 API。
//
// 这也是"向后兼容优先"的一贯做法：先让新的能用，再逐步迁移。
func TestLegacySetCreditsStillWorks(t *testing.T) {
	p := poolWithU1(t)
	p.SetCredits("u1", 1234) // 旧 API
	st, _ := p.Status("u1")
	if st.Quota.Remaining != 1234 {
		t.Errorf("旧 SetCredits 应等价于 SetQuota(credits)，得到 %+v", st.Quota)
	}
	if st.Quota.Kind != QuotaKindCredits {
		t.Errorf("旧 API 应产生 credits 形态，得到 %q", st.Quota.Kind)
	}
}

// TestLegacyReenableIfCreditsStillWorks 旧解冻 API 的语义保持不变：
// remain > 0 且账号非禁用时才解冻。
func TestLegacyReenableIfCreditsStillWorks(t *testing.T) {
	p := poolWithU1(t)
	p.Cooldown("u1", CoolSoft, time.Hour, "test")

	p.ReenableIfCredits("u1", 0) // 余额 0 → 不解冻
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Error("remain=0 时不该解冻（旧语义）")
	}
	p.ReenableIfCredits("u1", 500) // 余额 > 0 → 解冻
	if st, _ := p.Status("u1"); st.Cooling {
		t.Error("remain>0 时应解冻（旧语义）")
	}
}

// TestLegacyCreditsFieldStillExposed Status.Credits 保留（前端与统计在用）。
//
// 但它是**派生字段**：值来自 Quota.Remaining，不再是独立真相来源。
func TestLegacyCreditsFieldStillExposed(t *testing.T) {
	p := poolWithU1(t)
	p.SetQuota("u1", QuotaView{Kind: QuotaKindCredits, Remaining: 888, HasData: true})
	st, _ := p.Status("u1")
	if st.Credits != 888 {
		t.Errorf("Status.Credits 应为 Quota.Remaining 的派生值，得到 %d", st.Credits)
	}
	// 非 credits 形态时，Credits 给 Effective()，便于前端继续显示一个数
	p.SetQuota("u1", QuotaView{Kind: QuotaKindPerModel, ByModel: map[string]int64{"m": 77}, HasData: true})
	st2, _ := p.Status("u1")
	if st2.Credits != 77 {
		t.Errorf("per_model 形态下 Status.Credits 应给 Effective()，得到 %d", st2.Credits)
	}
}

// poolWithU1 建一个只含 u1 的池（本文件用）。
func poolWithU1(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	return p
}
