// reset_test.go 额度恢复策略（04:00）的契约。
//
// # 这组断言为什么现在住在这里
//
// 04:00 是 **workbuddy 的签到恢复策略**，不是核心的通用规则。
// 改造前它由 internal/pool 的 nextDay4AM 承担，测试也在 pool_test.go
// （TestCooldownUntilTomorrow4AM / ...Persists）。Task 3c 把策略迁进本包，
// 于是对"04:00"的断言也一并搬来 —— 留在核心的那份改为断言**通用契约**
// （冷却到调用方给的那个时刻），与上游策略无关。
package workbuddy

import (
	"testing"
	"time"
)

// TestNextResetIsNextDay4AM 钉住 04:00 这个时点的三个边界。
//
// 边界为什么是这三条：
//   - 05:00（04:00 之后）→ 次日 04:00：当日恢复窗口已过，只能等明天；
//   - 02:00（04:00 之前）→ **当天** 04:00：当天的签到还没执行，等它就好；
//     返回次日会白冷约一整天（这是改造前注释里明确写下的坑）；
//   - 04:00 整 → 次日 04:00：边界点不含"当天"，与 `now.Hour() < 4` 的判据一致。
func TestNextResetIsNextDay4AM(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "05:00 之后 → 次日 04:00",
			now:  time.Date(2026, 9, 11, 5, 0, 0, 0, loc),
			want: time.Date(2026, 9, 12, 4, 0, 0, 0, loc),
		},
		{
			name: "02:00（凌晨窗内）→ 当天 04:00",
			now:  time.Date(2026, 9, 11, 2, 0, 0, 0, loc),
			want: time.Date(2026, 9, 11, 4, 0, 0, 0, loc),
		},
		{
			name: "04:00 整 → 次日 04:00（边界不含当天）",
			now:  time.Date(2026, 9, 11, 4, 0, 0, 0, loc),
			want: time.Date(2026, 9, 12, 4, 0, 0, 0, loc),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NextCheckinReset(c.now)
			if !got.Equal(c.want) {
				t.Errorf("NextCheckinReset(%s)=%s，期望 %s",
					c.now.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
			}
		})
	}
}

// TestNextResetCrossesMonthAndYear 跨月/跨年不靠手写进位，靠 time.Date 的日溢出。
//
// 这条不是边界值游戏：月末与年末是"手写 +24h 再取整点"最容易出错的地方
// （9 月 30 日 +1 天在朴素实现里会变成 9 月 31 日）。
func TestNextResetCrossesMonthAndYear(t *testing.T) {
	loc := time.FixedZone("CST", 8*60*60)
	monthEnd := NextCheckinReset(time.Date(2026, 9, 30, 22, 0, 0, 0, loc))
	if want := time.Date(2026, 10, 1, 4, 0, 0, 0, loc); !monthEnd.Equal(want) {
		t.Errorf("月末跨月: got %s want %s", monthEnd, want)
	}

	yearEnd := NextCheckinReset(time.Date(2026, 12, 31, 22, 0, 0, 0, loc))
	if want := time.Date(2027, 1, 1, 4, 0, 0, 0, loc); !yearEnd.Equal(want) {
		t.Errorf("年末跨年: got %s want %s", yearEnd, want)
	}
}

// TestNextResetAtMatchesPolicy Provider.NextResetAt 必须走同一条策略。
//
// 为什么单独测：它是注入给 server.Config.NextResetAt 的那个函数，
// 一旦它和 NextCheckinReset 分家（比如有人后来在这里加了偏移量），
// 核心的冷却时长就会与策略脱节，而单测只覆盖 NextCheckinReset 时看不出来。
func TestNextResetAtMatchesPolicy(t *testing.T) {
	p := &Provider{}
	got := p.NextResetAt()
	// 结果必须是一个 04:00 整点，且在"现在"之后（含当天凌晨窗）。
	if got.Hour() != 4 || got.Minute() != 0 || got.Second() != 0 {
		t.Errorf("NextResetAt()=%s，期望 04:00 整点", got.Format(time.RFC3339))
	}
	now := time.Now()
	if got.Before(now) {
		t.Errorf("NextResetAt()=%s 早于现在 %s（会算出负冷却）",
			got.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	// 距今不超过 24 小时：它是"最近的一个 04:00"，不是随便某个 04:00。
	if d := got.Sub(now); d > 24*time.Hour {
		t.Errorf("NextResetAt() 距现在 %v，超过 24 小时 —— 不是最近的那个 04:00", d)
	}
}
