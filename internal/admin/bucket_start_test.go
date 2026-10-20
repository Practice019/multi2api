// bucket_start_test.go 时间桶边界的正确性（2026-10-20）。
//
// # 为什么单独钉这个
//
// 从 new-api 的图表测试里看到一条我们可能同样会中的问题：
//
//	test('orders model buckets across New Year while aggregating repeated labels')
//
// 它验的是"跨年时桶的排序与重复标签聚合"。这类错误的可怕之处在于**平时看不出来**：
// 一年里有 364 天都正常，只有跨年/跨月/闰年那天聚错，而那天往往正是用户回头
// 看年度报表的时候。
//
// 我们的实现用 `bucketStart`（series.go）把时刻对齐到桶起点。它有两种桶：
// 小时桶（24h/7d 档）与天桶（30d/365d 档）。天桶**必须按本地自然日对齐** ——
// 用 UTC 零时截断会让桶起点落在本地 08:00，于是"10-17 那一格"实际是
// 10-17 08:00 ~ 10-18 08:00（这个坑 series.go 里已有注释记着）。
package admin

import (
	"testing"
	"time"
)

func TestBucketStartHourBoundary(t *testing.T) {
	loc := time.Local
	cases := []struct {
		name   string
		in     time.Time
		bucket time.Duration
		want   time.Time
	}{
		{"普通小时", time.Date(2026, 10, 20, 15, 37, 12, 0, loc), time.Hour,
			time.Date(2026, 10, 20, 15, 0, 0, 0, loc)},
		{"跨零点的小时桶（23:59 仍属当天 23 点）", time.Date(2026, 12, 31, 23, 59, 59, 0, loc), time.Hour,
			time.Date(2026, 12, 31, 23, 0, 0, 0, loc)},
		{"新年第 1 分钟", time.Date(2027, 1, 1, 0, 1, 30, 0, loc), time.Hour,
			time.Date(2027, 1, 1, 0, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bucketStart(c.in, c.bucket); !got.Equal(c.want) {
				t.Fatalf("bucketStart(%v) = %v，期望 %v", c.in, got, c.want)
			}
		})
	}
}

func TestBucketStartDayBoundary(t *testing.T) {
	loc := time.Local
	day := 24 * time.Hour
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		// ⚠ 关键：天桶起点必须是**本地 00:00**，不是 UTC 零时（那是本地 08:00）
		{"普通日", time.Date(2026, 10, 20, 15, 37, 0, 0, loc),
			time.Date(2026, 10, 20, 0, 0, 0, 0, loc)},
		{"本地零点整", time.Date(2026, 10, 20, 0, 0, 0, 0, loc),
			time.Date(2026, 10, 20, 0, 0, 0, 0, loc)},
		{"跨年当天 23:59", time.Date(2026, 12, 31, 23, 59, 59, 0, loc),
			time.Date(2026, 12, 31, 0, 0, 0, 0, loc)},
		{"新年第 1 天 00:01", time.Date(2027, 1, 1, 0, 1, 0, 0, loc),
			time.Date(2027, 1, 1, 0, 0, 0, 0, loc)},
		{"闰日 2-29", time.Date(2028, 2, 29, 13, 0, 0, 0, loc),
			time.Date(2028, 2, 29, 0, 0, 0, 0, loc)},
		{"闰日次日 3-01", time.Date(2028, 3, 1, 13, 0, 0, 0, loc),
			time.Date(2028, 3, 1, 0, 0, 0, 0, loc)},
		{"月末 1-31", time.Date(2026, 1, 31, 22, 0, 0, 0, loc),
			time.Date(2026, 1, 31, 0, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bucketStart(c.in, day)
			if !got.Equal(c.want) {
				t.Fatalf("bucketStart(%v, 24h) = %v，期望 %v", c.in, got, c.want)
			}
			// 附加判据：桶起点的小时必须是 0（本地自然日对齐的直接证据）
			if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
				t.Fatalf("天桶起点应为本日 00:00:00，实际 %v", got)
			}
		})
	}
}

// TestBucketStartIsIdempotent 对齐是幂等的：再对齐一次不应改变结果。
//
// 为什么值得钉：series 里会先对齐起点、再按偏移逐桶推进并**再次**对齐。
// 如果 bucketStart 不幂等（比如对已是起点的时刻再截断会漂移），
// 画出来的柱子会一根比一根偏，而总数看着"差不多"——最难发现的那类错。
func TestBucketStartIsIdempotent(t *testing.T) {
	loc := time.Local
	for _, bucket := range []time.Duration{time.Hour, 24 * time.Hour} {
		in := time.Date(2026, 10, 20, 15, 37, 12, 0, loc)
		once := bucketStart(in, bucket)
		twice := bucketStart(once, bucket)
		if !once.Equal(twice) {
			t.Fatalf("bucket=%v 不幂等：once=%v twice=%v", bucket, once, twice)
		}
	}
}

// TestBucketStartAcrossYearAggregatesSameLabel 跨年时"同一本地日"必须归到同一桶。
//
// 这条模拟 new-api 那个 'aggregating repeated labels' 测试的意图：
// 12-31 23:00 与 12-31 01:00 必须落到**同一个**天桶（同一天），
// 而它们不能因为"跨了 UTC 的某条线"被拆成两个桶。
func TestBucketStartAcrossYearAggregatesSameLabel(t *testing.T) {
	loc := time.Local
	day := 24 * time.Hour
	a := time.Date(2026, 12, 31, 1, 0, 0, 0, loc)
	b := time.Date(2026, 12, 31, 23, 59, 0, 0, loc)
	ba, bb := bucketStart(a, day), bucketStart(b, day)
	if !ba.Equal(bb) {
		t.Fatalf("同一天的 01:00 与 23:59 应归同一桶，实际 %v vs %v", ba, bb)
	}
	// 而次日的必须不同桶
	c := time.Date(2027, 1, 1, 1, 0, 0, 0, loc)
	if bucketStart(c, day).Equal(ba) {
		t.Fatalf("次年 1-1 不应与 12-31 同桶")
	}
}
