// dailycheckin_coverage_test.go 跨上游签到覆盖度：**派生**自 allUpstreams()，不硬编码任何上游清单。
//
// # 为什么这条守卫住在这里（P1-b，2026-11-12 验证者实测）
//
// 同类断言原先只住在 `internal/gateway/dailycheckin_ext_test.go`，而它那里的 `checkinProviders()`
// 是一张**硬编码的 6 实例清单**。验证者的实测结论：把某个上游的签到接线注掉，
// `go test ./internal/gateway/ ./cmd/server/ -run "Checkin|Bulk"` **仍然全绿** ——
// 也就是说那条守卫保护的是一个**写死的名单**，不是"所有上游"。
//
// ⇒ 能构造出**全部**上游实例的地方只有 `allUpstreams()`（本包，见 account_columns_invariant_test.go），
//
//	所以覆盖度断言必须落在它上面；gateway 那份清单退化成"契约形状的夹具"（文件头已注明）。
//
// # 判据（全部派生，不点名任何上游）
//
//	① 声明了 CapCheckin ⇒ 必须实现 DailyCheckinExt
//	   （否则顶部「全部签到」照常回 202，那个上游的号一个都没动 —— trae/lobsterai/qoder 当年都这样）
//	② 实现了 DailyCheckinExt ⇒ 它报的动作里**至少有一条 AllURL 非空**
//	   （两个入口是独立的：顶部跨上游按钮走扩展点，卡片上那个按钮走 AllURL。
//	     实现了扩展点却给不出 AllURL ⇒ 用户没有「只签这一个上游」的入口）
//	③ 报了 Batch=true ⇒ AllURL 必须非空（dailyaction_test.go 里 BatchImpliesAllURL 的派生版）
//	④ **非空匹配**：本次真的扫到了 ≥1 个有签到能力的实例 ——
//	   否则这条守卫会退化成「永不失败」（本仓反复强调：空匹配的护栏比没有更危险）
//
// ⚠⚠ **不要**把判据写成「hasExt ⇔ Batch=true」——我第一版就是这么写的，跑出来立刻误报两个上游：
//
//	`Batch` 的语义不是「端点存在」，而是「**界面要不要给它一个全量入口**」，那可以是**刻意的用户决定**
//	（见 internal/workbuddy/dailyactions.go:52-65：保活 AllURL 如实报、Batch 故意为 false）。
//	客观可判的是 AllURL（端点真的挂着）；Batch 是产品决定，守卫不该替产品拍板。
package main

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// knownGaps 本守卫**已经抓到、但不在本轮改动边界内**的缺口。
//
// 它们**不被静默放过**：命中时会在 -v 输出里打一条 `已知缺口 …`（跑测试时看得见）。
//
// 纪律（本仓对"例外表"的既有要求）：每一行必须写清「缺什么 + 为什么现在不改 + 谁来改」；
// 且一旦缺口被修好，下面的"例外过期"检查会**反过来报红** —— 例外条目不允许长草。
var knownGaps = map[string]string{
	"allurl:workbuddy-intl": "实现了 DailyCheckinExt 却一条带 AllURL 的每日动作都没报" +
		"（顶部「全部签到」能签它，卡片上没有任何入口）；修它要动 internal/workbuddy，" +
		"不在 PA-2 本轮边界内 ⇒ 已上报，等派单",
}

func TestCheckinCoverageIsDerivedFromAllUpstreams(t *testing.T) {
	checkinCapable := 0
	seenGaps := map[string]bool{}
	for _, u := range allUpstreams(t) {
		capCheckin := u.p.Caps()&gateway.CapCheckin != 0
		if capCheckin {
			checkinCapable++
		}
		_, hasExt := gateway.ExtOf[gateway.DailyCheckinExt](u.p)
		// 它报出来的每日动作里，有没有"可点的全量入口"。
		//
		// ⚠ 这里**不按动作 ID 过滤**（不猜 checkin / welfare / daily 的命名）——
		//   本仓签到类动作的 ID 各家不同（checkin / trae-checkin / welfare / …），
		//   用子串推断"哪条才是签到"正是本仓反复吃亏的"用字符串猜语义"。
		//   代价：这一条只证明"它至少给了一个全量入口"（可能那个是保活的）——
		//   那是**漏报面**，比按命名猜要安全。
		hasAllURL := false
		batchWithoutAllURL := ""
		if dae, ok := gateway.ExtOf[gateway.DailyActionExt](u.p); ok {
			for _, a := range gateway.SanitizeDailyActions(dae.DailyActions()) {
				if a.AllURL != "" {
					hasAllURL = true
				}
				if a.Batch && a.AllURL == "" {
					batchWithoutAllURL = a.ID
				}
			}
		}
		// ① CapCheckin ⇒ 扩展点。
		if capCheckin && !hasExt {
			t.Errorf("%s(%s): 声明了 CapCheckin 却没实现 DailyCheckinExt —— "+
				"顶部「全部签到」会**跳过**它（不报错、不计失败，那个上游一个号都没动）", u.label, u.id)
		}
		// ② 扩展点 ⇒ 至少一个带 AllURL 的动作（少了它 = 卡片上没有「只签这个上游」的入口）。
		if hasExt && !hasAllURL {
			key := "allurl:" + u.label // label 才是唯一的（qoder/qodercn 共用 id）
			if why, known := knownGaps[key]; known {
				seenGaps[key] = true
				t.Logf("已知缺口（本轮边界外，见 knownGaps）%s(%s): %s", u.label, u.id, why)
			} else {
				t.Errorf("%s(%s): 实现了 DailyCheckinExt 却一条带 AllURL 的每日动作都没报 —— "+
					"顶部能签它，卡片上却没有那一个入口", u.label, u.id)
			}
		}
		// ③ Batch=true ⇒ AllURL 非空（假按钮守卫的派生版）。
		if batchWithoutAllURL != "" {
			t.Errorf("%s(%s): 动作 %q 报了 Batch=true 却没有 AllURL —— 那是个点下去不知道打哪里的按钮",
				u.label, u.id, batchWithoutAllURL)
		}
	}
	if checkinCapable == 0 {
		t.Fatal("一个声明 CapCheckin 的实例都没扫到 —— 这条守卫已经失效（空匹配），" +
			"先检查 allUpstreams() 是不是被改坏了")
	}
	// 例外过期检查：knownGaps 里的条目若已不再是缺口 ⇒ 报红，逼着删掉它。
	for key, why := range knownGaps {
		if !seenGaps[key] {
			t.Errorf("knownGaps 里的 %q 已经**不再是**缺口（%s）—— 请删掉这一行：例外条目不许长草", key, why)
		}
	}
	t.Logf("覆盖度：%d 个实例声明了 CapCheckin；已知缺口 %d 条（本轮边界外）", checkinCapable, len(seenGaps))
}
