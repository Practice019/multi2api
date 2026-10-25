// revive_probe_test.go 解封探测的换算判据。
//
// 为什么单独一条：这个旋钮对外用**小时**、config 里用**秒**，中间那一步换算
// 一旦取整方向错，表现是"页面显示关闭、后台其实在跑"（或反过来）——
// 那类缺陷不会自我暴露，只能靠断言抓。
package main

import (
	"testing"

	"workbuddy2api/internal/admin"
)

// TestReviveProbeHoursOf 秒 → 小时的换算边界。
func TestReviveProbeHoursOf(t *testing.T) {
	for _, c := range []struct {
		name string
		secs int
		want int
	}{
		{"0 必须原样透出为 0（关闭态不能被取整成 1）", 0, 0},
		{"负数也当关闭", -3600, 0},
		{"1 秒 = 1 小时（向上取整，不能显示成 0）", 1, 1},
		{"899 秒 = 1 小时（⚠ 四舍五入会得到 0 = 「关闭」，那就把开着说成关着）", 899, 1},
		{"恰好 1 小时", 3600, 1},
		{"1 小时零 1 秒 = 2 小时", 3601, 2},
		{"默认值 86400 秒 = 24 小时", 86400, 24},
		{"12 小时", 43200, 12},
	} {
		got := reviveProbeHoursOf(c.secs)
		if got != c.want {
			t.Errorf("%s: reviveProbeHoursOf(%d) = %d，want %d", c.name, c.secs, got, c.want)
		}
		// 不变量：**正数**秒值绝不能被显示成 0（0 在本页语义是"关闭"）。
		// ⚠ 只对 secs > 0 成立：负数/零本来就是"关闭"，显示 0 是对的。
		if c.secs > 0 && got == 0 {
			t.Errorf("%d 秒（开着）被显示成 0 = 关闭态 —— 用户会以为功能没开", c.secs)
		}
	}
}

// TestReviveProbeRoundTrip 设置页的"小时 → config 秒 → 读回小时"必须回到原值。
//
// ⚠ 这条守的是接线而不是算术：页面能填、保存被静默丢弃（parse/persist 清单里
// 漏了一行）时，applyToMemory 之后 currentSettings 会读回**旧值**，
// 本用例红在"写进去没生效"这一步。
func TestReviveProbeRoundTrip(t *testing.T) {
	cfg := &Config{}
	s := settingsStoreForTest(t, cfg)
	s.applyToMemory(admin.Settings{ReviveProbeHours: 6})
	if got := cfg.Pool.ReviveProbeIntervalSeconds; got != 6*3600 {
		t.Errorf("applyToMemory 后 cfg.Pool.ReviveProbeIntervalSeconds = %d，want %d", got, 6*3600)
	}
	if got := s.currentSettings().ReviveProbeHours; got != 6 {
		t.Errorf("读回的小时 = %d，want 6（说明 persist/parse 漏了这个键）", got)
	}
}
