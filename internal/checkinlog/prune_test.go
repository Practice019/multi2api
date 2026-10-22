// prune_test.go 钉住「高频类型不许挤掉签到记录」。
//
// # 用户报的 bug
//
// 「签到的字段不会一直显示有的时候会消失」——
// 原实现的保留策略是**无差别丢掉最早的记录**（out[len-limit:]）。
// 而各类记录的产出速率差两个数量级，实测 5000 条只覆盖 9.5 小时：
//
//	credits   4013 条
//	checkin     83 条
//
// 于是签到那格的数据存在率取决于**别的类型写了多少**：
// 早上签过的号，下午 credits 一多就被挤出窗口 → 界面变空。
//
// 这类 bug 很难靠手测发现（要等到当天的记录真的被挤掉），
// 所以用构造的极端分布钉住。
package checkinlog

import (
	"fmt"
	"testing"
	"time"
)

// TestPruneKeepsCheckinUnderNoise 高频噪音不能淘汰签到记录。
func TestPruneKeepsCheckinUnderNoise(t *testing.T) {
	l := &Log{path: "", keepDays: 30, limit: 8000}
	now := time.Now()

	// 先塞一条「今天早上签的」。
	l.records = append(l.records, Record{At: now.Add(-8 * time.Hour), UID: "u1", Kind: KindCheckin, Status: StatusOK})

	// 再塞大量 credits 噪音 —— 真实分布里它占 80%。
	for i := 0; i < 20000; i++ {
		l.records = append(l.records, Record{
			At:   now.Add(-time.Duration(i) * time.Second),
			UID:  fmt.Sprintf("u%d", i%50),
			Kind: KindCredits,
		})
	}

	kept := l.pruneLocked(l.records)

	// 判据：那条签到**必须还在**。
	found := false
	for _, r := range kept {
		if r.Kind == KindCheckin && r.UID == "u1" {
			found = true
		}
	}
	if !found {
		t.Fatal("★ 签到记录被 credits 噪音挤掉了 —— " +
			"界面上「今日签到」列就是这样变空的（用户报的「有时有有时消失」）")
	}

	// 总量必须仍受总上限约束（不能因为分桶就无限增长）。
	if len(kept) > l.limit {
		t.Errorf("裁剪后 %d 条，超过上限 %d", len(kept), l.limit)
	}
}

// TestPruneKeepsTimeOrder 裁剪后必须仍按时间升序。
//
// # 为什么这条必须单独测
//
// 分桶会打乱顺序，而我靠末尾的 sort.Slice 恢复。若忘了那一步，
// Since（从头扫）与 LastByUID（从尾往前找）都会返回**错的**记录 ——
// 而且不会报错，只是「今天签到了吗」答错。
func TestPruneKeepsTimeOrder(t *testing.T) {
	l := &Log{path: "", keepDays: 30, limit: 100}
	now := time.Now()
	for i := 0; i < 500; i++ {
		kinds := []string{KindCheckin, KindCredits, KindWelfare}
		l.records = append(l.records, Record{
			At:   now.Add(-time.Duration(i) * time.Minute),
			UID:  "u1",
			Kind: kinds[i%3],
		})
	}
	kept := l.pruneLocked(l.records)
	for i := 1; i < len(kept); i++ {
		if kept[i].At.Before(kept[i-1].At) {
			t.Fatalf("裁剪破坏了时序：第 %d 条 (%v) 早于第 %d 条 (%v) —— " +
				"Since/LastByUID 都会答错且不报错",
				i, kept[i].At, i-1, kept[i-1].At)
		}
	}
}

// TestPruneRespectsKeepDays 超过保留天数的必须删掉（与按条数裁剪无关）。
func TestPruneRespectsKeepDays(t *testing.T) {
	l := &Log{path: "", keepDays: 7, limit: 8000}
	now := time.Now()
	l.records = []Record{
		{At: now.Add(-10 * 24 * time.Hour), UID: "old", Kind: KindCheckin},
		{At: now.Add(-1 * time.Hour), UID: "new", Kind: KindCheckin},
	}
	kept := l.pruneLocked(l.records)
	if len(kept) != 1 || kept[0].UID != "new" {
		t.Errorf("keepDays 未生效: %+v", kept)
	}
}
