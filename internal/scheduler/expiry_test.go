package scheduler

import (
	"testing"
	"time"
)

// daysUntil 的整天口径必须可预测 —— 界面直接显示它，算错就是给用户错信息。
//
// 口径（写在这里供后来者对照）：按**自然日**做差，不看时刻。
// 用户看到「9-30 23:59 到期」时，9-30 当天仍可用，所以 9-30 从 9-11 算是 19 天，
// 而不是"不到 19 个 24 小时所以 18 天"。
func TestDaysUntilNaturalDayBoundary(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}

	cases := []struct {
		name string
		end  string
		now  string
		want int
	}{
		{"同一天不同时刻 = 0", "2026-09-11T23:59:00+08:00", "2026-09-11T00:01:00+08:00", 0},
		{"同一天晚些 = 0（当天仍可用）", "2026-09-11T08:00:00+08:00", "2026-09-11T20:00:00+08:00", 0},
		{"隔一天 = 1", "2026-09-12T23:59:00+08:00", "2026-09-11T18:00:00+08:00", 1},
		{"实测样本：9-30 到期", "2026-09-30T23:59:00+08:00", "2026-09-11T18:00:00+08:00", 19},
		{"实测样本：10-10 到期", "2026-10-10T23:59:00+08:00", "2026-09-11T18:00:00+08:00", 29},
		{"实测样本：11-02 到期", "2026-11-02T23:59:00+08:00", "2026-09-11T18:00:00+08:00", 52},
		{"实测样本：11-13 到期", "2026-11-13T23:59:00+08:00", "2026-09-11T18:00:00+08:00", 63},
		{"已过期 1 天 = -1", "2026-09-10T23:59:00+08:00", "2026-09-11T01:00:00+08:00", -1},
		{"已过期 30 天 = -30", "2026-08-12T23:59:00+08:00", "2026-09-11T01:00:00+08:00", -30},
		{"跨月", "2026-10-01T00:00:00+08:00", "2026-09-30T23:59:00+08:00", 1},
		{"跨年", "2027-01-01T00:00:00+08:00", "2026-12-31T12:00:00+08:00", 1},
		{"闰年 2-29", "2028-03-01T00:00:00+08:00", "2028-02-28T12:00:00+08:00", 2},
	}

	for _, c := range cases {
		got := daysUntil(at(c.end), at(c.now))
		if got != c.want {
			t.Errorf("%s: daysUntil(%s, %s)=%d want %d", c.name, c.end, c.now, got, c.want)
		}
	}
}

// 时区不该改变"还剩几天"的直觉判断：同一时刻用不同时区表示，结果应一致。
//
// 用 UTC 归一是刻意的：上游给的是 +08:00，而服务器可能在任何时区。
func TestDaysUntilTimeZoneIndependent(t *testing.T) {
	end, _ := time.Parse(time.RFC3339, "2026-09-30T23:59:00+08:00")
	// 同一物理时刻的另一种写法（UTC）
	nowCST, _ := time.Parse(time.RFC3339, "2026-09-11T18:00:00+08:00")
	nowUTC := nowCST.UTC()

	a := daysUntil(end, nowCST)
	b := daysUntil(end, nowUTC)
	if a != b {
		t.Errorf("时区不该影响结果: CST=%d UTC=%d", a, b)
	}
}

// 阈值常量要有实际意义：实测四个带期限的任务剩余 18~63 天，
// 7 天阈值能把它们全部正确判为「不紧急」—— 否则一屏全是警示就没有警示作用。
func TestExpiringSoonThresholdIsSane(t *testing.T) {
	if expiringSoonDays <= 0 || expiringSoonDays > 30 {
		t.Errorf("阈值 %d 天不合理（应是个位数到 30 以内）", expiringSoonDays)
	}
	// 留个意图性断言：阈值必须小于实测最短剩余期限，避免误报
	const shortestObserved = 18
	if expiringSoonDays >= shortestObserved {
		t.Errorf("阈值 %d 不得 >= 实测最短剩余 %d，否则真实任务会被误标紧急",
			expiringSoonDays, shortestObserved)
	}
}
