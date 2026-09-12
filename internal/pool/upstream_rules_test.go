// 评审 F6 的落地测试：核心 pool 里的**上游业务规则**必须可被上游替换。
//
// # 评审的原话（决定性反例）
//
// "两条真实的上游规则仍硬编码在 internal/pool 且被脚本评为 0：
//  1. nextDay4AM / CooldownUntilTomorrow4AM —— workbuddy 的签到恢复策略
//     （04:00 冷却、'等签到恢复'、09:00/21:00 签到时点）。
//     codearts 没有签到，账号冷却多久恢复纯属上游策略。
//  2. CoolHard 排除在 fallback 之外，理由是'余额耗尽 → 调了必 402'
//     —— workbuddy 的计费规则被写进了核心。"
//
// 这条指控成立：`nextDay4AM` 把"4 点"这个 **workbuddy 的签到时刻**写死在核心里。
// codearts 的上游没有签到，次日 4 点对它没有任何意义。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestCoolScheduleIsInjectable 冷却恢复时点必须由调用方（上游）决定。
//
// 设计：核心只提供 `Cooldown(uid, kind, duration, reason)` —— 时长由上游算好传入。
// 上游专属的"次日 4 点"逻辑属于 workbuddy，应搬去 internal/workbuddy。
//
// 本测试用**上游自己算时长**的方式，证明核心不需要内置那个策略也能工作。
func TestCoolScheduleIsInjectable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	t.Run("上游给出任意恢复时点", func(t *testing.T) {
		// 模拟 codearts：额度按小时重置，与"次日 4 点"无关
		p.Cooldown("u1", CoolHard, 90*time.Minute, "配额用尽，90 分钟后重置")

		st, _ := p.Status("u1")
		if !st.Cooling {
			t.Fatal("应处于冷却")
		}
		assertCoolingUntil(t, st, 90*time.Minute)
	})

	t.Run("上游给出完全不同的时点也能工作", func(t *testing.T) {
		p2 := New("")
		p2.Add(&auth.Auth{UID: "u1"})
		// workbuddy 想要的"次日 4 点"由上游算出时长后传入
		p2.Cooldown("u1", CoolHard, 6*time.Hour, "等签到恢复")

		st, _ := p2.Status("u1")
		assertCoolingUntil(t, st, 6*time.Hour)
	})
}

// assertCoolingUntil 断言剩余冷却时长接近预期（容忍秒级误差）。
func assertCoolingUntil(t *testing.T, st Status, want time.Duration) {
	t.Helper()
	got := time.Until(st.Until)
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 5*time.Second {
		t.Errorf("冷却剩余 %v，期望约 %v（差 %v）", got.Round(time.Second),
			want, diff.Round(time.Second))
	}
}

// TestCooldownUntilNextReset 通用冷却入口：重置时刻由调用方给。
//
// 解耦后的核心不再知道"次日 4 点" —— 04:00 是 workbuddy 的签到恢复时刻，
// codearts 没有签到。核心只接受一个目标时刻。
func TestCooldownUntilNextReset(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	target := time.Now().Add(45 * time.Minute)
	p.CooldownUntilNextReset("u1", target, "配额用尽（由上游判定）")

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("应处于冷却")
	}
	if st.CoolKind != CoolHard.String() {
		t.Errorf("应为 hard 冷却，得到 %q", st.CoolKind)
	}
	assertCoolingUntil(t, st, 45*time.Minute)
}

// TestCooldownUntilNextResetPastTime 目标时刻已过 → 不产生负时长。
//
// 上游算出的重置时刻可能因时钟漂移/传参错误而落在过去。
// 若不处理，until.Sub(now) 为负，冷却截止落到过去 ——
// 表现为"该号立刻可用"，但 coolKind 已被设成 CoolHard，
// 让兜底逻辑在语义矛盾的状态下做判断。显式夹到 0 更安全。
func TestCooldownUntilNextResetPastTime(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.CooldownUntilNextReset("u1", time.Now().Add(-time.Hour), "过期目标")

	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("目标时刻已过时不该处于冷却（until=%v）", st.Until)
	}
}

// TestHardCoolRecoveryPolicyLivesUpstream 确认核心**不再内置任何**签到恢复时点。
//
// # 这条测试是 Task 3c 的守卫
//
// 改造前 core 有 `nextDay4AM` 与它的薄封装 `CooldownUntilTomorrow4AM` ——
// 把 workbuddy 的"次日 04:00"写死在账号池里。评审（F6）指出这正是
// "上游规则硬编码在核心"的实例，而当时的审计脚本把它评成了 0。
//
// 策略现在住在 internal/workbuddy（NextCheckinReset / Provider.NextResetAt），
// 核心只接受一个**时刻**。本测试用"给一个与 04:00 毫无关系的时刻"来证明这一点：
// 核心必须老实按它冷却，而不是偷偷纠正成某个它自己认为对的时点。
func TestHardCoolRecoveryPolicyLivesUpstream(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 一个刻意与"次日 4 点"无关的恢复时点（模拟 codearts 的按小时配额）。
	until := time.Now().Add(17 * time.Minute).Truncate(time.Second)
	p.CooldownUntilNextReset("u1", until, "配额用尽")

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("应处于冷却")
	}
	if !st.Until.Equal(until) {
		t.Errorf("核心改写了调用方给的恢复时刻：得到 %v，期望 %v。"+
			"核心不该知道任何上游的恢复策略", st.Until, until)
	}
	if st.Until.Hour() == 4 && st.Until.Minute() == 0 {
		t.Error("核心把恢复时刻对齐到了 04:00 —— 上游策略又漏回核心了")
	}
}

// TestHardCoolKindIsGeneric 确认 CoolHard 的**语义是通用的**。
//
// CoolKind 本身是通用概念：
//
//	CoolHard  长时间冷却（原因由调用方给）
//	CoolSoft  短时间冷却（429 之类）
//
// 只要"时长从哪来"这件事由调用方决定，核心就不含上游策略。
// 真正要搬走的是 `nextDay4AM`（把 workbuddy 的 4 点写死）。
func TestHardCoolKindIsGeneric(t *testing.T) {
	// String() 给的应是通用语义名，而不是某个上游的业务名
	if got := CoolHard.String(); got == "" || got == "unknown" {
		t.Errorf("CoolHard.String() 应有可读名，得到 %q", got)
	}
	// 两个 kind 必须可区分
	if CoolHard == CoolSoft {
		t.Error("CoolHard 与 CoolSoft 必须可区分")
	}
	if CoolHard.String() == CoolSoft.String() {
		t.Error("两个 kind 的可读名不该相同")
	}
}
