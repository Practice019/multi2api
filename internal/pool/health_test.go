// health_test.go 主动健康检查 / 刷新失败计数 / 熔断窗口衰减（A2 移植）的测试。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func newHealthPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.SetBreaker(3, 30*time.Minute, 6*time.Hour)
	return p
}

// available 用 Status 判断账号当前可用（未禁用、未冷却、未熔断）。
func available(st Status) bool {
	return !st.Disabled && !st.Cooling && st.BreakerUntil.IsZero()
}

// TestRecoverableCandidates 只列冷却/熔断中、未禁用、错误不新鲜的账号。
func TestRecoverableCandidates(t *testing.T) {
	p := newHealthPool(t)
	now := time.Now()

	// 冷却中的账号（未到恢复时刻）。
	p.AddFor("workbuddy", &auth.Auth{UID: "cool-1"}, nil)
	p.CooldownUntilNextReset("cool-1", now.Add(2*time.Hour), "额度耗尽")

	// 熔断中的账号。
	p.AddFor("workbuddy", &auth.Auth{UID: "breaker-1"}, nil)
	p.NoteError("breaker-1")
	p.NoteError("breaker-1")
	p.NoteError("breaker-1") // 达到阈值 3 → 熔断

	// 健康账号（不该出现在候选里）。
	p.AddFor("workbuddy", &auth.Auth{UID: "ok-1"}, nil)

	// 禁用账号（不该出现在候选里）。
	p.AddFor("workbuddy", &auth.Auth{UID: "disabled-1"}, nil)
	p.Disable("disabled-1", "session 死亡")

	// 冷却已到点的账号（自然恢复，不该探测）。
	p.AddFor("workbuddy", &auth.Auth{UID: "expired-1"}, nil)
	p.CooldownUntilNextReset("expired-1", now.Add(-time.Minute), "已到点")

	cands := p.RecoverableCandidates(0)
	got := map[string]bool{}
	for _, c := range cands {
		got[c.UID] = true
	}
	if !got["cool-1"] || !got["breaker-1"] {
		t.Errorf("冷却/熔断账号应在候选里: %v", got)
	}
	if got["ok-1"] || got["disabled-1"] || got["expired-1"] {
		t.Errorf("健康/禁用/已到点账号不该在候选里: %v", got)
	}
}

// TestRecoverableCandidatesMinAge 刚报错的账号被 minAge 挡住。
func TestRecoverableCandidatesMinAge(t *testing.T) {
	p := newHealthPool(t)
	p.AddFor("workbuddy", &auth.Auth{UID: "fresh-1"}, nil)
	p.NoteError("fresh-1") // 刚报错（lastErr = 现在）
	p.CooldownUntilNextReset("fresh-1", time.Now().Add(time.Hour), "冷却")
	// 刚报错 → minAge 挡掉。
	if cands := p.RecoverableCandidates(2 * time.Minute); len(cands) != 0 {
		t.Errorf("刚报错的账号不该被探测: %v", cands)
	}
	// minAge=0 → 允许。
	if cands := p.RecoverableCandidates(0); len(cands) != 1 {
		t.Errorf("minAge=0 时应列出 1 个: %v", cands)
	}
}

// TestNoteRefreshFailure 连续 3 次刷新失败 → 禁用；中间成功清零。
func TestNoteRefreshFailure(t *testing.T) {
	p := newHealthPool(t)
	p.AddFor("codearts", &auth.Auth{UID: "r-1"}, nil)

	if p.NoteRefreshFailure("r-1") {
		t.Fatal("第一次失败不该禁用")
	}
	if p.NoteRefreshFailure("r-1") {
		t.Fatal("第二次失败不该禁用")
	}
	p.NoteSuccess("r-1") // 一次成功清零
	if p.NoteRefreshFailure("r-1") {
		t.Fatal("成功清零后第一次失败不该禁用")
	}
	if p.NoteRefreshFailure("r-1") {
		t.Fatal("清零后第二次失败不该禁用")
	}
	if !p.NoteRefreshFailure("r-1") {
		t.Fatal("连续第三次失败应禁用")
	}
	if st, ok := p.Status("r-1"); !ok || !st.Disabled {
		t.Fatalf("账号应被禁用: %+v", st)
	}
}

// TestEnableResetsDisableProgressCounters 人工"启用"必须把**禁用进度**计数器归零。
//
// # 为什么这条判据必须存在（2026-11-14 现场踩到）
//
// autodisable 的判据是"连续 N 次"（refreshFails / sessionDeadStreak >= 3）。
// 计数走到 3 ⇒ 账号被禁用。此时人工点"启用"，若只清 disabled 而不清计数，
// 那么**下一次**续期失败立刻再次命中 `>= 3` ⇒ 账号瞬间又被禁用。
//
// 真实现场（就是这条判据要防的形态）：
//
//	18:50:46  管理员启用账号（disabled=false，但 refreshFails 仍为 3）
//	18:56:18  一次续期失败 ⇒ 立刻回到禁用
//	18:56:32  再启用、再失败 ⇒ 又禁用
//
// 用户看到的是"我点了启用，它过一会儿自己又变回禁用了"，据此**无法判断**
// 到底是凭据真的废了、还是这个开关坏了 —— 而这个区分决定了下一步该
// "重新登录"还是该"报 bug"。
func TestEnableResetsDisableProgressCounters(t *testing.T) {
	p := newHealthPool(t)
	p.AddFor("phanthywork", &auth.Auth{UID: "e-1"}, nil)

	// ① 把续期失败推到阈值 ⇒ 禁用
	for i := 1; i <= 3; i++ {
		got := p.NoteRefreshFailure("e-1")
		if i < 3 && got {
			t.Fatalf("第 %d 次续期失败不该禁用", i)
		}
		if i == 3 && !got {
			t.Fatal("连续第 3 次续期失败应禁用")
		}
	}
	if st, ok := p.Status("e-1"); !ok || !st.Disabled {
		t.Fatalf("前置条件不成立（账号应已被禁用）: %+v", st)
	}

	// ② 人工启用
	if !p.Enable("e-1") {
		t.Fatal("Enable 应返回 true")
	}
	if st, _ := p.Status("e-1"); st.Disabled {
		t.Fatal("启用后不应再是禁用态")
	}

	// ③ ★ 核心：启用后的**第一次**失败不能立刻重新禁用
	if p.NoteRefreshFailure("e-1") {
		t.Fatal("启用后第一次续期失败就再次禁用 ⇒ refreshFails 没归零，" +
			"「启用」等于白按：用户点完过一会儿它自己又变回禁用，且无法据此判断根因")
	}

	// ④ 会话失效是同一个坑的另一半：它也参与"连续 N 次即禁用"
	for i := 1; i <= 3; i++ {
		p.NoteSessionDead("e-1")
	}
	if st, ok := p.Status("e-1"); !ok || !st.Disabled {
		t.Fatalf("会话失效连续 3 次应禁用: %+v", st)
	}
	p.Enable("e-1")
	if p.NoteSessionDead("e-1") {
		t.Fatal("启用后第一次会话失效就再次禁用 ⇒ sessionDeadStreak 没归零")
	}
}

// TestBreakerWindowDecay 错误计数按时间窗衰减：窗口外的失败先把计数清零。
func TestBreakerWindowDecay(t *testing.T) {
	p := newHealthPool(t)
	p.AddFor("workbuddy", &auth.Auth{UID: "w-1"}, nil)

	p.NoteError("w-1") // fails=1, lastErr=now
	p.NoteError("w-1") // fails=2
	// 把 lastErr 拨回窗口外（61s 前）→ 下次失败先清零再记。
	p.mu.Lock()
	e := p.byUID["w-1"]
	e.lastErr = time.Now().Add(-breakerFailureWindow - time.Second)
	p.mu.Unlock()

	p.NoteError("w-1") // 窗口外：fails 先清零再 +1 = 1，不触发熔断
	if st, ok := p.Status("w-1"); !ok || !st.BreakerUntil.IsZero() {
		t.Fatalf("窗口外失败不该触发熔断: %+v", st)
	}
}

// TestClearCooldownByHealthCheck 健康检查路径：ClearCooldown 后账号重新可用。
func TestClearCooldownByHealthCheck(t *testing.T) {
	p := newHealthPool(t)
	p.AddFor("trae", &auth.Auth{UID: "t-1"}, nil)
	p.CooldownUntilNextReset("t-1", time.Now().Add(3*time.Hour), "权益不足")

	if !p.ClearCooldown("t-1") {
		t.Fatal("ClearCooldown 应返回 true（幂等清除）")
	}
	if st, ok := p.Status("t-1"); !ok || !available(st) {
		t.Fatalf("清除后应恢复可用: %+v", st)
	}
	// 已健康的账号再清一次也是无害的幂等（现有 API 语义）。
	_ = p.ClearCooldown("t-1")

	// 禁用账号：ClearCooldown 只清冷却，**不翻案禁用**。
	p.AddFor("workbuddy", &auth.Auth{UID: "d-1"}, nil)
	p.Disable("d-1", "session 死亡")
	_ = p.ClearCooldown("d-1")
	if st, ok := p.Status("d-1"); !ok || !st.Disabled {
		t.Fatal("ClearCooldown 不应翻案禁用账号")
	}
}
