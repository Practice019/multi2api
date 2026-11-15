// migration.go 存量令牌的**迁移计划器**（U-09）。
//
// # 它产出什么，不产出什么
//
// 产出一份**计划**：逐令牌写明「迁移后 v2 的字段该写成什么」，以及
// 「这么写之后这个令牌会失去哪些模型」。
//
// **不执行**任何写入。计划与执行分开是刻意的 —— 计划可以被审阅、被 diff、
// 被贴进工单；执行是一步不可逆的写库，它必须由人按过按钮。
//
// # 三条验收（逐字对应计划 §5 U-09）
//
//	验收 1  「新增可见／可调用模型必须为零」
//	         ⇒ 计划里任何一项出现 widened 就直接阻塞，不给自己解释的余地。
//	验收 2  「旧受限空集不能变为无约束」
//	         ⇒ 旧解释是 restricted_empty 的令牌，迁移后**必须**仍是
//	            「启用白名单 + 空列表」，绝不能落到"未启用 = 不限"。
//	验收 3  「无法表达等价权限则阻塞该项，不自动放宽或静默丢失」
//	         ⇒ v2 表达不了"全部模型"（旧的不限），所以旧不限令牌一定**需要人确认**，
//	            而不是悄悄给它一个组就算了。
//
// # 计划必须**自校验**
//
// `VerifyPlan` 会把计划里要写的白名单套回去，用同一个比较器重算一遍，然后断言：
//
//	· 没有任何一项变宽（验 1 的机器化形式）
//	· 每一对「计划预测的 Lost」与「实际重算出来的 narrowed」**逐字一致**
//	  （预测不准的计划比没有计划更危险：人会照它去按按钮）
package channelstore

import (
	"sort"

	"workbuddy2api/internal/groupstore"
)

// MigrationPlanItem 一个令牌的迁移计划项。
//
// ⚠ 与 DryRunItem 一样：**没有** ID 字段。它是 Bearer 值本身，
// 计划要能贴进工单。
type MigrationPlanItem struct {
	Name  string `json:"name"`
	Owner string `json:"owner,omitempty"`

	// PinnedWhitelist 迁移后 v2 白名单**该写成的集合**。
	// nil + WhitelistDisabled=false = 不改白名单字段。
	PinnedWhitelist []string `json:"pinned_whitelist,omitempty"`
	// WhitelistDisabled true = 迁移后**关闭**白名单（旧解释是"不限"，v2 表达不了）。
	WhitelistDisabled bool `json:"whitelist_disabled,omitempty"`

	// Kept / Lost 迁移后仍可用 / 会失去的模型。Lost 非空 ⇒ 需要人确认。
	Kept []string `json:"kept,omitempty"`
	Lost []string `json:"lost,omitempty"`

	// RequiresConfirmation 需要人按下确认（失去模型，或旧解释是"不限"）。
	RequiresConfirmation bool `json:"requires_confirmation,omitempty"`
	// Note 给人看的一句说明（为什么需要确认 / 为什么被阻塞）。
	Note string `json:"note,omitempty"`

	// Blocked 非空 = 这一项**无法迁移**（不自动放宽、不静默丢失）。
	Blocked string `json:"blocked,omitempty"`
}

// MigrationPlan 整份计划。
type MigrationPlan struct {
	Items   []MigrationPlanItem `json:"items"`
	Total   int                 `json:"total"`
	Blocked int                 `json:"blocked"`
	Confirm int                 `json:"confirm"`
	// Ready 可以进入执行（无阻塞）。⚠ 它**不**代表"无需人确认"。
	Ready bool `json:"ready"`
}

// PlanEvidence 计划的自校验证据。
type PlanEvidence struct {
	// WidenedAny 重算后仍有任何一项变宽 ⇒ **计划不可用**。
	WidenedAny bool `json:"widened_any"`
	// Mismatches 计划预测的 Lost 与实际重算的 narrowed 不一致的项。
	Mismatches []string `json:"mismatches,omitempty"`
	// Verified 两项都通过。
	Verified bool `json:"verified"`
}

// PlanMigration 从 dry-run 结果推出迁移计划。
//
// 参数就是 ComputeDryRun 的那一套 —— 本函数内部先算 dry-run，再据它推计划。
func PlanMigration(gs *groupstore.Store, channels []*Channel, catalog []string, inputs []DryRunInput) MigrationPlan {
	report := ComputeDryRun(gs, channels, catalog, inputs)
	plan := MigrationPlan{Items: make([]MigrationPlanItem, 0, len(report.Items)), Total: len(report.Items)}

	for i, item := range report.Items {
		out := MigrationPlanItem{Name: item.Name, Owner: item.Owner}
		var input DryRunInput
		if i < len(inputs) {
			input = inputs[i]
		}

		if !item.Applicable {
			out.Note = "不吃 v2（policy_version != 2 或没有组），不参与迁移"
			plan.Items = append(plan.Items, out)
			continue
		}

		// 验收 1：变宽一律阻塞。没有"稍后处理"这一档。
		if len(item.Widened) > 0 {
			out.Blocked = "无法表达等价权限：新解释会多放出 " +
				joinComma(item.Widened) + "（验收要求新增可调用模型为零）"
		}
		// dry-run 自己报了阻塞（组解析不了 / 目录歧义）⇒ 原样带过来。
		if out.Blocked == "" && item.Blocked != "" {
			out.Blocked = item.Blocked
		}

		// 按旧解释的三种形态给出"该写什么"。
		switch item.Old.Kind {
		case PermModels:
			// 钉住原有效集合：白名单 = 旧集合。组候选之外的会被交集削掉 ——
			// 那些就是 Lost，必须让人看见。
			out.PinnedWhitelist = append([]string{}, item.Old.Models...)
			out.Kept = intersect(item.Old.Models, input.OldGroupsModels, len(input.OldGroupsModels) > 0)

		case PermRestrictedEmpty:
			// 验收 2：旧"启用 + 空"必须保持"启用 + 空"。
			// ⚠ 这里如果写成"关闭白名单"，就变成了"不限" —— 权限放大。
			out.PinnedWhitelist = []string{}
			out.Note = "旧解释是「启用白名单但列表空」= 一个都不能用；迁移后必须保持同样形态"

		case PermUnrestricted:
			// 验收 3：v2 无法表达"全部模型" ⇒ 必须人确认，不自动放宽。
			out.WhitelistDisabled = true
			out.RequiresConfirmation = true
			out.Note = "旧解释是「不限」（任何模型可用）；v2 只能给出组范围 —— " +
				"这是**有意的收窄**，需要人确认，不能静默发生"
		}

		if item.NarrowedAll {
			out.Lost = append([]string{}, item.Old.Models...)
			if item.Old.Kind == PermUnrestricted {
				out.Lost = nil // 不限没有名字可列，用 RequiresConfirmation 表达
			}
		} else {
			out.Lost = append([]string{}, item.Narrowed...)
		}
		if len(out.Lost) > 0 && out.Blocked == "" {
			out.RequiresConfirmation = true
			if out.Note == "" {
				out.Note = "迁移后会失去 " + joinComma(out.Lost) + " —— 需要人确认"
			}
		}

		sort.Strings(out.PinnedWhitelist)
		sort.Strings(out.Kept)
		sort.Strings(out.Lost)

		if out.Blocked != "" {
			plan.Blocked++
		}
		if out.RequiresConfirmation {
			plan.Confirm++
		}
		plan.Items = append(plan.Items, out)
	}
	plan.Ready = plan.Blocked == 0
	return plan
}

// VerifyPlan 把计划里要写的白名单套回去重算，断言计划是**准确**的。
//
// 两件事：
//
//	① 重算后**没有任何一项变宽** —— 验收 1 的机器化形式
//	② 每一对「计划预测的 Lost」与「实际 narrowed」逐字一致
//	   —— 预测不准的计划比没有计划更危险：人会照它去按按钮
func VerifyPlan(gs *groupstore.Store, channels []*Channel, catalog []string, inputs []DryRunInput, plan MigrationPlan) PlanEvidence {
	var ev PlanEvidence
	applied := make([]DryRunInput, len(inputs))
	copy(applied, inputs)
	for i := range applied {
		if i >= len(plan.Items) {
			break
		}
		item := plan.Items[i]
		if item.Blocked != "" || item.PinnedWhitelist == nil && !item.WhitelistDisabled {
			continue
		}
		if item.WhitelistDisabled {
			applied[i].Policy.ModelLimitsEnabled = false
			applied[i].Policy.ModelLimits = nil
			continue
		}
		applied[i].Policy.ModelLimitsEnabled = true
		applied[i].Policy.ModelLimits = append([]string{}, item.PinnedWhitelist...)
	}

	report := ComputeDryRun(gs, channels, catalog, applied)
	for i, item := range report.Items {
		if len(item.Widened) > 0 {
			ev.WidenedAny = true
		}
		if i >= len(plan.Items) {
			continue
		}
		want := plan.Items[i].Lost
		got := item.Narrowed
		if !sameStringSet(want, got) && !(len(want) == 0 && plan.Items[i].WhitelistDisabled) {
			ev.Mismatches = append(ev.Mismatches,
				plan.Items[i].Name+": 计划预测 Lost="+joinComma(want)+"，实际 narrowed="+joinComma(got))
		}
	}
	ev.Verified = !ev.WidenedAny && len(ev.Mismatches) == 0
	return ev
}

// intersect 取 a 与 b 的交集；bUnlimited 时直接返回 a 的副本。
//
// ⚠ 不让调用方传 nil 表示"不限"：那种写法在调用点总有人把空切片读成不限，
// 而"空集 = 一个都没有"与"不限"是**相反**的语义（F-09 那个坑）。
func intersect(a, b []string, bUnlimited bool) []string {
	if bUnlimited {
		return append([]string{}, a...)
	}
	in := make(map[string]bool, len(b))
	for _, m := range b {
		in[m] = true
	}
	out := make([]string, 0, len(a))
	for _, m := range a {
		if in[m] {
			out = append(out, m)
		}
	}
	return out
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string{}, a...)
	y := append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
