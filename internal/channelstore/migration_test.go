// migration_test.go U-09 迁移计划器的判据。
//
// # 三条验收（逐字对应计划 §5 U-09）
//
//	验收 1  新增可见／可调用模型**必须为零**   ⇒ 变宽一律阻塞
//	验收 2  旧受限空集**不能变为无约束**       ⇒ pinned 必须是「启用 + 空」
//	验收 3  等价权限表达不了就**阻塞该项**     ⇒ 旧"不限"必须人确认，不自动放宽
//
// # 还有一条本文件独有的：计划必须**自校验**
//
// VerifyPlan 把计划要写的白名单套回去重算。预测不准的计划比没有计划更危险 ——
// 人会照着它去按按钮。
package channelstore

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/groupstore"
)

// dryRunFixture2 同 dryRunFixture，但渠道同时提供 public 与 public2。
//
// 需要"组里多于一个模型"才能构造变宽场景。
func dryRunFixture2(t *testing.T) (*groupstore.Store, []*Channel, []string) {
	t.Helper()
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Upsert(&groupstore.Group{
		ID: "g-a", Name: "route", PolicyVersion: 2, ChannelIDs: []string{"ch-a"},
	}); err != nil {
		t.Fatal(err)
	}
	channels := []*Channel{{
		ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled,
		Models:   []string{"real", "real2"},
		ModelMap: map[string]string{"public": "real", "public2": "real2"},
	}}
	return gs, channels, []string{"public", "public2"}
}

// ── 验收 2：旧受限空集不能变为无约束 ──────────────────────────────────────

// TestPlanPinsRestrictedEmptyAsStillEmpty ★ 旧"启用 + 空"迁移后必须仍是"启用 + 空"。
//
// 这是验收 2。若计划把它写成"关闭白名单"，那个令牌就从不许用任何模型
// 变成**不限** —— 权限放大，而且没有任何运行期信号。
func TestPlanPinsRestrictedEmptyAsStillEmpty(t *testing.T) {
	gs, channels := dryRunFixture(t)

	inputs := []DryRunInput{{
		Name:              "empty",
		OldGroupsResolved: true,
		OldLimitEnabled:   true,
		OldLimits:         nil, // 旧：启用但空 ⇒ 一个都不能用
		// ⚠ 新解释**也**必须是空集，否则 dry-run 先说变宽、计划直接阻塞，
		// 验 2 就没机会被触发（第一版就是这么写错的）。
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"},
			ModelLimitsEnabled: true, ModelLimits: nil},
	}}
	plan := PlanMigration(gs, channels, testCatalog, inputs)

	item := plan.Items[0]
	if item.WhitelistDisabled {
		t.Fatal("★ 旧受限空集迁移后不能关闭白名单 —— 那等于从不许用变成不限（权限放大）")
	}
	if item.PinnedWhitelist == nil {
		t.Fatal("必须显式钉一个（空的）白名单，而不是「不改」")
	}
	if len(item.PinnedWhitelist) != 0 {
		t.Fatalf("钉住的白名单应为空集合，实际 %v", item.PinnedWhitelist)
	}

	// 自校验：应用计划后重算，这个令牌必须仍然一个模型都没有。
	ev := VerifyPlan(gs, channels, testCatalog, inputs, plan)
	if !ev.Verified {
		t.Fatalf("计划未通过自校验：widened=%v mismatches=%v", ev.WidenedAny, ev.Mismatches)
	}
	applied := inputs
	applied[0].Policy.ModelLimitsEnabled = true
	applied[0].Policy.ModelLimits = item.PinnedWhitelist
	after := ComputeDryRun(gs, channels, testCatalog, applied)
	if after.Items[0].New.Kind != PermRestrictedEmpty {
		t.Fatalf("★ 应用计划后应仍是 restricted_empty，实际 %q", after.Items[0].New.Kind)
	}
}

// ── 验收 1：变宽一律阻塞 ──────────────────────────────────────────────────

// TestPlanBlocksOnWidening 旧受限空集 → 新放出来 ⇒ 变宽 ⇒ 这一项阻塞。
func TestPlanBlocksOnWidening(t *testing.T) {
	gs, channels := dryRunFixture(t)

	inputs := []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		OldLimitEnabled:   true,
		OldLimits:         nil,                                                      // 旧：一个都不能用
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}}, // 新：未启用 ⇒ public
	}}
	plan := PlanMigration(gs, channels, testCatalog, inputs)

	item := plan.Items[0]
	if item.Blocked == "" {
		t.Fatal("★ 变宽必须阻塞（验收 1：新增可调用模型必须为零）")
	}
	// ⚠ 断言**计划自己的**措辞，不只是"非空"：
	//   dry-run 也会为变宽写一条阻塞原因，两者都非空 ⇒ 只断言"非空"的话，
	//   把计划这一条删掉判据照样绿（反向验证实测：确实没咬住）。
	//   U-09 验收 1 的责任在**计划**，所以这里必须认它的措辞。
	if !strings.Contains(item.Blocked, "无法表达等价权限") {
		t.Fatalf("★ 阻塞原因必须来自计划自身的验收 1 检查，实际 %q —— "+
			"只有 dry-run 的那条说明计划的检查没生效", item.Blocked)
	}
	if plan.Ready {
		t.Fatal("有阻塞时计划不该 Ready")
	}
	if plan.Blocked != 1 {
		t.Fatalf("Blocked 计数应为 1，实际 %d", plan.Blocked)
	}
}

// ── 验收 3：旧"不限"必须人确认，不自动放宽 ────────────────────────────────

// TestPlanRequiresConfirmationForUnrestricted 旧"不限"⇒ v2 表达不了 ⇒ 必须确认。
func TestPlanRequiresConfirmationForUnrestricted(t *testing.T) {
	gs, channels := dryRunFixture(t)

	inputs := []DryRunInput{{
		Name:              "wide-open",
		OldGroupsResolved: true,
		OldLimitEnabled:   false, // 旧：不限
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}}
	plan := PlanMigration(gs, channels, testCatalog, inputs)

	item := plan.Items[0]
	if item.Blocked != "" {
		t.Fatalf("旧不限不该「阻塞」（它是可迁移的，只是要人确认），实际 %q", item.Blocked)
	}
	if !item.RequiresConfirmation {
		t.Fatal("★ 旧不限令牌迁移到 v2 一定是有意收窄，必须人确认，不能静默发生")
	}
	if item.Note == "" {
		t.Fatal("必须有一句说明告诉人为什么需要确认")
	}
	if !plan.Ready {
		t.Fatal("无阻塞时计划应 Ready（Ready 不等于无需确认）")
	}
	if plan.Confirm != 1 {
		t.Fatalf("Confirm 计数应为 1，实际 %d", plan.Confirm)
	}
}

// TestPlanRequiresConfirmationForUnrestrictedWithoutGroupModels 组里一个模型都没有时，
// 只有「旧不限」那条专用逻辑能设确认位。
//
// ⚠ 这是反向验证逼出来的用例：组里**有**模型时，`Narrowed` 非空 ⇒ 后面那段通用
// 逻辑也会把 RequiresConfirmation 设回 true ⇒ 把专用逻辑删掉判据照样绿。
// 组为空时 Lost 为空，通用那条不触发，专用逻辑才是唯一来源。
func TestPlanRequiresConfirmationForUnrestrictedWithoutGroupModels(t *testing.T) {
	gs, channels := dryRunFixture(t)
	// 一个没有任何渠道的空组。
	if err := gs.Upsert(&groupstore.Group{ID: "g-empty", Name: "empty", PolicyVersion: 2}); err != nil {
		t.Fatal(err)
	}

	plan := PlanMigration(gs, channels, testCatalog, []DryRunInput{{
		Name:              "wide-open",
		OldGroupsResolved: true,
		OldLimitEnabled:   false, // 旧：不限
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-empty"}},
	}})

	item := plan.Items[0]
	if len(item.Lost) != 0 {
		t.Fatalf("前置条件：组为空时 Lost 应为空（否则测不到专用逻辑），实际 %v", item.Lost)
	}
	if !item.RequiresConfirmation {
		t.Fatal("★ 旧不限令牌即便碰上空组也必须要求确认 —— 它是「从什么都能用变成什么都用不了」")
	}
}

// ── 自校验：预测必须准 ────────────────────────────────────────────────────

// TestPlanVerifyDetectsWrongPrediction 计划预测的 Lost 与实际不符 ⇒ 自校验失败。
//
// ⚠ 这条判据防的是"计划看起来很合理、实际算错"。预测不准的计划比没有计划更危险：
// 人会照着它去按按钮。
func TestPlanVerifyDetectsWrongPrediction(t *testing.T) {
	gs, channels := dryRunFixture(t)

	inputs := []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		OldGroupsModels:   []string{"public"},
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"},
			ModelLimitsEnabled: true, ModelLimits: []string{"public"}},
	}}
	plan := PlanMigration(gs, channels, testCatalog, inputs)
	if !VerifyPlan(gs, channels, testCatalog, inputs, plan).Verified {
		t.Fatal("前置条件不成立：正常计划应通过自校验")
	}

	// 人为把预测改错（说它会丢掉一个它其实不会丢的模型）。
	plan.Items[0].Lost = []string{"phantom"}
	ev := VerifyPlan(gs, channels, testCatalog, inputs, plan)
	if ev.Verified {
		t.Fatal("★ 预测与重算不一致时自校验必须失败")
	}
	if len(ev.Mismatches) == 0 {
		t.Fatal("必须指出是哪一项对不上")
	}
}

// TestPlanVerifyDetectsWideningAfterApply 应用计划后仍变宽 ⇒ 自校验失败。
//
// ⚠ 要构造出"变宽"，组里必须有**多于一个**模型 —— 只有单个模型时，
// 关掉白名单也不会多放出任何东西，这条判据会误以为自校验没问题（第一版就这样）。
// 所以这里用 dryRunFixture2（ch-a 同时提供 public 与 public2）。
func TestPlanVerifyDetectsWideningAfterApply(t *testing.T) {
	gs, channels, catalog := dryRunFixture2(t)

	// 旧的只钉住其中一个模型。
	inputs := []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		OldGroupsModels:   []string{"public"},
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"},
			ModelLimitsEnabled: true, ModelLimits: []string{"public"}},
	}}
	plan := PlanMigration(gs, channels, catalog, inputs)
	if !VerifyPlan(gs, channels, catalog, inputs, plan).Verified {
		t.Fatal("前置条件不成立：正常计划应通过自校验")
	}

	// 把钉住的白名单改成"关闭" ⇒ 组里两个模型全放出来 ⇒ 变宽。
	plan.Items[0].PinnedWhitelist = nil
	plan.Items[0].WhitelistDisabled = true
	plan.Items[0].Lost = nil

	ev := VerifyPlan(gs, channels, catalog, inputs, plan)
	if ev.Verified {
		t.Fatal("★ 把白名单关掉会让权限变宽（多放出 public2），自校验必须失败")
	}
	if !ev.WidenedAny {
		t.Fatalf("应报告变宽，实际 %+v", ev)
	}
}

// ── 边界 ──────────────────────────────────────────────────────────────────

// TestPlanSkipsNonV2Tokens 不吃 v2 的令牌不出现在计划里（但计数里要有）。
func TestPlanSkipsNonV2Tokens(t *testing.T) {
	gs, channels := dryRunFixture(t)
	plan := PlanMigration(gs, channels, testCatalog, []DryRunInput{
		{Name: "legacy", OldGroupsResolved: true, Policy: RoutingPolicy{PolicyVersion: 1}},
	})
	if plan.Total != 1 {
		t.Fatalf("Total 应含所有令牌，实际 %d", plan.Total)
	}
	if plan.Items[0].Blocked != "" || plan.Items[0].PinnedWhitelist != nil {
		t.Fatal("不吃 v2 的令牌不该有迁移动作")
	}
	if !plan.Ready {
		t.Fatal("没有需要迁移的项时计划应 Ready")
	}
}

// TestPlanNeverCarriesSecret 计划里不许出现凭据字段。
func TestPlanNeverCarriesSecret(t *testing.T) {
	gs, channels := dryRunFixture(t)
	plan := PlanMigration(gs, channels, testCatalog, []DryRunInput{{
		Name: "app", Owner: "alice", OldGroupsResolved: true,
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})
	if _, ok := reflect.TypeOf(plan.Items[0]).FieldByName("KeyID"); ok {
		t.Fatal("MigrationPlanItem 不该有 KeyID 字段（它是 Bearer 值本身）")
	}
}

// TestPlanKeepsPinnedWhitelistSorted 钉住的白名单必须排序（写库前可比较、幂等）。
func TestPlanKeepsPinnedWhitelistSorted(t *testing.T) {
	gs, channels := dryRunFixture(t)
	plan := PlanMigration(gs, channels, testCatalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		OldLimitEnabled:   true,
		OldLimits:         []string{"zeta", "alpha", "mid"},
		Policy:            RoutingPolicy{PolicyVersion: 1}, // 不吃 v2 ⇒ 只走旧解释
	}})
	// policy_version=1 ⇒ Applicable=false ⇒ 无迁移动作；换一个吃 v2 的来验排序。
	plan = PlanMigration(gs, channels, testCatalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		OldLimitEnabled:   true,
		OldLimits:         []string{"zeta", "alpha", "mid"},
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})
	got := plan.Items[0].PinnedWhitelist
	want := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("钉住的白名单应排序，实际 %v", got)
	}
}
