// checkin_live_test.go 真机验证签到链路（查状态 → PoW → 领取）。
//
// ⚠ 判据（与 verify_live_test.go 的 credDeadErr **同一份**）：
// 凭证缺失 → Skip 整条；某个号的凭证已失效（401/403）→ **跳过这个号**，不算失败；
// 上游 5xx / 网络不通 / 解析失败 → 红；一个号都没测到 → Skip。
//
// # 为什么判据要落在"每个号"上，而不是先探一次活
//
// 2026-11-01 实测：同一个凭证，`FetchWallet` 通、而 `CheckinStatus` 回 401
// （不同端点的权限/会话要求不一样）。所以"先探一次活"会把这类号放进来，
// 然后在循环里报红 —— 那正是我们要修的东西。⇒ 用**这个测试真正要调的那个端点**
// 的结果来判是不是凭证问题，判据才准；也不必为探测多打一轮上游。
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

	tested, dead := 0, 0
	var lastErr error
	for _, a := range list {
		signedIn, enabled, err := p.CheckinStatus(ctx, a)
		if err != nil {
			if credDeadErr(err) {
				// 凭证已失效：这台机器上"这个号"不可用 —— 跳过它，不算失败。
				dead++
				lastErr = err
				t.Logf("[%s] %s: 凭证已失效（401/403），跳过这个号", a.RegionOrDefault(), a.DisplayName())
				continue
			}
			t.Errorf("[%s] 查签到状态失败: %v", a.RegionOrDefault(), err)
			continue
		}
		tested++
		t.Logf("[%s] %s: 已签=%v 开放=%v", a.RegionOrDefault(), a.DisplayName(), signedIn, enabled)
	}
	liveSkipIfNothingTested(t, tested, dead, lastErr)
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

	tested, dead := 0, 0
	var lastErr error
	for _, a := range list {
		// ① 先看状态：**用它的结果判凭证死活**（见文件头：别的端点可能"通"）。
		before, _, serr := p.CheckinStatus(ctx, a)
		if serr != nil {
			if credDeadErr(serr) {
				dead++
				lastErr = serr
				t.Logf("[%s] %s: 凭证已失效（401/403），跳过这个号", a.RegionOrDefault(), a.DisplayName())
				continue
			}
			t.Errorf("[%s] 查签到状态失败: %v", a.RegionOrDefault(), serr)
			continue
		}

		// ② 真签一次。
		n, action, err := p.ClaimCheckin(ctx, a)
		if err != nil {
			if credDeadErr(err) {
				dead++
				lastErr = err
				t.Logf("[%s] %s: 签到端点回报凭证失效，跳过这个号", a.RegionOrDefault(), a.DisplayName())
				continue
			}
			t.Errorf("[%s] %s 签到失败: %v", a.RegionOrDefault(), a.DisplayName(), err)
			continue
		}
		tested++
		after, _, _ := p.CheckinStatus(ctx, a)
		t.Logf("[%s] %s: 签前=%v 签后=%v credits=%d action=%v",
			a.RegionOrDefault(), a.DisplayName(), before, after, n, action)
		if !before && !after {
			t.Errorf("[%s] 签到调用成功但状态仍为未签", a.RegionOrDefault())
		}
	}
	liveSkipIfNothingTested(t, tested, dead, lastErr)
}
