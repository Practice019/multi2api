package admin

import (
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// 本文件：额度刷新**排队次序**的守门用例（`orderQuotaRefreshTargets`）。
//
// ⚠ 为什么它值得单独一个文件：我第一版把排序**内联**在 `RefreshAllQuotasNow` 里，
// 然后做变异测试时发现 —— 把 `After` 改成 `Before`（即"每轮先刷从没用过的号"）
// **没有任何测试会红**。而那个错误的症状是：真正该刷的号被挤到一轮的后面，
// 上游偶发失败时它们就一直是旧值 —— **不报错、不崩，只是数据慢慢变旧**。

// TestOrderQuotaRefreshTargetsRecentlyUsedFirst 最近用过的排最前面。
func TestOrderQuotaRefreshTargetsRecentlyUsedFirst(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	list := []pool.Status{
		{UID: "old", LastUsed: t0.Add(-2 * time.Hour)},
		{UID: "new", LastUsed: t0},
		{UID: "mid", LastUsed: t0.Add(-30 * time.Minute)},
	}
	orderQuotaRefreshTargets(list)

	got := []string{list[0].UID, list[1].UID, list[2].UID}
	want := []string{"new", "mid", "old"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("次序 = %v，want %v —— "+
				"最近被用过的必须排前面：它的额度**一定**变了（一次调用无论成功失败都消耗额度）",
				got, want)
		}
	}
}

// TestOrderQuotaRefreshTargetsNeverUsedLast 「从没被选中过」的排在最后。
//
// ⚠ 这是整个排序依赖的唯一性质：零值（`time.Time{}`）是最小值，
//   `After()` 比较天然把它排到最后。⚠ 若有人把判据换成"字段非零优先"之类，
//   零值会跑到**最前面**，于是每轮都先刷一批从没用过的号。
func TestOrderQuotaRefreshTargetsNeverUsedLast(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	list := []pool.Status{
		{UID: "never-1"},
		{UID: "used", LastUsed: t0},
		{UID: "never-2"},
	}
	orderQuotaRefreshTargets(list)

	if list[0].UID != "used" {
		t.Errorf("第 1 个应当是 used，实际 %s —— 零值必须排最后", list[0].UID)
	}
	if list[1].UID != "never-1" || list[2].UID != "never-2" {
		t.Errorf("两个零值之间应保持原有次序（稳定排序），实际 %s, %s",
			list[1].UID, list[2].UID)
	}
}

// TestOrderQuotaRefreshTargetsStableForTies 时间相同（例如都是零值）时保持原次序。
//
// ⚠ 用 `SliceStable` 而不是 `Slice` 的理由就在这条：`List()` 本身按 uid 有序，
//   稳定排序能让结果**可预测**（同一份输入永远同一个输出），
//   而 `Slice` 在相等元素上的顺序是未定义的 —— 那会让"这一轮先刷谁"变成随机的，
//   排障时无法复现。
func TestOrderQuotaRefreshTargetsStableForTies(t *testing.T) {
	list := []pool.Status{{UID: "a"}, {UID: "b"}, {UID: "c"}, {UID: "d"}}
	orderQuotaRefreshTargets(list)
	for i, want := range []string{"a", "b", "c", "d"} {
		if list[i].UID != want {
			t.Fatalf("全部并列时次序应当原样保持，实际 %v —— "+
				"这里用 Slice 的话顺序是未定义的，会让'这一轮先刷谁'不可复现",
				[]string{list[0].UID, list[1].UID, list[2].UID, list[3].UID})
		}
	}
}

// TestOrderQuotaRefreshTargetsEmpty 空集合不 panic。
func TestOrderQuotaRefreshTargetsEmpty(t *testing.T) {
	orderQuotaRefreshTargets(nil)
	orderQuotaRefreshTargets([]pool.Status{})
}
