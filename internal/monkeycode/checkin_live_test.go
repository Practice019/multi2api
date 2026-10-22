// checkin_live_test.go 真机验证签到链路（查状态 → PoW → 领取）。
package monkeycode

import (
	"context"
	"testing"
	"time"
)

// TestLiveCheckinStatus 至少能查到状态（不真的签）。
func TestLiveCheckinStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	dir := liveAuthDir()
	list, err := LoadDir(dir)
	if err != nil || len(list) == 0 {
		t.Skipf("无真实凭证（%s），跳过", dir)
	}
	p := New(Config{AuthDir: dir})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, a := range list {
		signedIn, enabled, err := p.CheckinStatus(ctx, a)
		if err != nil {
			t.Errorf("[%s] 查签到状态失败: %v", a.RegionOrDefault(), err)
			continue
		}
		t.Logf("[%s] %s: 已签=%v 开放=%v", a.RegionOrDefault(), a.DisplayName(), signedIn, enabled)
	}
}

// TestLiveCheckin 真机签到一次（已签会早退，不会重复）。
func TestLiveCheckin(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	dir := liveAuthDir()
	list, err := LoadDir(dir)
	if err != nil || len(list) == 0 {
		t.Skipf("无真实凭证（%s），跳过", dir)
	}
	p := New(Config{AuthDir: dir})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	for _, a := range list {
		before, _, _ := p.CheckinStatus(ctx, a)
		n, action, err := p.ClaimCheckin(ctx, a)
		if err != nil {
			t.Errorf("[%s] %s 签到失败: %v", a.RegionOrDefault(), a.DisplayName(), err)
			continue
		}
		after, _, _ := p.CheckinStatus(ctx, a)
		t.Logf("[%s] %s: 签前=%v 签后=%v credits=%d action=%v",
			a.RegionOrDefault(), a.DisplayName(), before, after, n, action)
		if !before && !after {
			t.Errorf("[%s] 签到调用成功但状态仍为未签", a.RegionOrDefault())
		}
	}
}
