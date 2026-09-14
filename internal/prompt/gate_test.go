// gate_test.go 降级状态机的判据锁定。
//
// # 为什么值得单独测
//
// 这个状态机只有一个布尔量的对外表现（Active），但它的**时间语义**决定
// 用户体验的上下界：
//
//	重置太勤 → 一天之内反复撞 400、反复重试（每次多一次失败往返）
//	重置太迟 → 上游策略已重置我们还在发中性提示词，人格白白丢掉
//
// 因此重点测的是 nextMidnightCST 的边界，而不是 Active() 本身。
package prompt

import (
	"testing"
	"time"
)

// TestGateZeroValueInactive 零值可用：从未触发 → 不活跃。
//
// 出站客户端可能拿到一个从未被装配层设置过的 Gate（零值指针非 nil
// 但内部 until 为零时间），此时必须报"未降级"。
func TestGateZeroValueInactive(t *testing.T) {
	var g Gate
	if g.Active() {
		t.Error("零值 Gate 不该处于降级期")
	}
	if !g.Until().IsZero() {
		t.Errorf("零值 Gate 的 Until 应为零时间，得到 %v", g.Until())
	}
}

// TestGateNilReceiverIsSafe 方法在 nil 接收者上不得 panic。
//
// # 为什么这是硬要求
//
// core 的 server.Config.PromptGate 是**可选**的（nil 表示本部署未接提示词体系），
// 出站客户端读的就是这个可能为 nil 的值。若 Active() 在 nil 上 panic，
// 一个"没配提示词"的部署会在第一次请求时崩掉。
//
// 语义上 nil = "永远不会降级" → Active() 返回 false，是唯一安全的答案。
func TestGateNilReceiverIsSafe(t *testing.T) {
	var g *Gate
	if g.Active() {
		t.Error("nil Gate 应报未降级")
	}
	g.Trigger() // 不得 panic
	g.Reset()   // 不得 panic
	if !g.Until().IsZero() {
		t.Error("nil Gate 的 Until 应为零时间")
	}
}

// TestGateTriggerThenActive 触发后进入降级期，并落在 CST 次日零点。
func TestGateTriggerThenActive(t *testing.T) {
	g := NewGate()
	if g.Active() {
		t.Fatal("新 Gate 不该已降级")
	}
	g.Trigger()
	if !g.Active() {
		t.Fatal("Trigger 之后应处于降级期")
	}
	until := g.Until()
	if until.IsZero() {
		t.Fatal("Until 不该为零")
	}
	if !until.After(time.Now()) {
		t.Errorf("Until=%v 应在未来", until)
	}
	// 必须落在 CST 的 00:00 整。
	cst := until.In(cstZone)
	if cst.Hour() != 0 || cst.Minute() != 0 || cst.Second() != 0 || cst.Nanosecond() != 0 {
		t.Errorf("Until 应为 CST 零点整，得到 %v（CST %v）", until, cst.Format(time.RFC3339Nano))
	}
	// 且不超过 24 小时（否则说明多推进了一天）。
	if d := time.Until(until); d > 24*time.Hour {
		t.Errorf("距离重置 %v，超过 24 小时 —— 跨日推进多了", d)
	}
}

// TestGateTriggerDoesNotExtend 降级期内重复 Trigger **不续期**。
//
// # 为什么这是硬判据
//
// 续期会让"连续被拦"不断把重置时刻往后推，形成永不恢复的降级 ——
// 上游的策略是每天重置的，我们不该比它更悲观。
//
// 反向判别力：把 Trigger 里的 `if !time.Now().Before(g.until)` 去掉，
// 本用例必然失败。
func TestGateTriggerDoesNotExtend(t *testing.T) {
	g := NewGate()
	g.Trigger()
	first := g.Until()

	// 模拟降级期内又撞了几次拦截。
	for i := 0; i < 5; i++ {
		g.Trigger()
	}
	if got := g.Until(); !got.Equal(first) {
		t.Errorf("降级期内 Trigger 不得续期：第一次 %v，之后 %v", first, got)
	}
}

// TestGateReset 手动重置立即结束降级期。
func TestGateReset(t *testing.T) {
	g := NewGate()
	g.Trigger()
	if !g.Active() {
		t.Fatal("Trigger 后应活跃")
	}
	g.Reset()
	if g.Active() {
		t.Error("Reset 后不该再活跃")
	}
	if !g.Until().IsZero() {
		t.Errorf("Reset 后 Until 应为零时间，得到 %v", g.Until())
	}
	// 重置之后可以再次触发（不是一次性状态机）。
	g.Trigger()
	if !g.Active() {
		t.Error("Reset 后应能再次 Trigger")
	}
}

// TestNextMidnightCSTBoundaries 边界语义逐条钉住。
//
// 用的是**固定 +08:00**，与宿主时区无关 —— 这本身就是被测的性质之一：
// 同样的 CST 墙上时间，无论宿主 TZ 是什么，结果都必须一样。
func TestNextMidnightCSTBoundaries(t *testing.T) {
	// 注意：期望值用 CST 视角构造，断言也用 CST 视角比较，
	// 这样本用例在 UTC / 任意 TZ 的 CI 上结论一致。
	cases := []struct {
		name     string
		nowCST   time.Time
		wantNext string // 期望的 CST 日期
	}{
		{"23:59:59 → 次日 00:00", time.Date(2026, 9, 14, 23, 59, 59, 0, cstZone), "2026-09-15"},
		{"12:00 → 次日 00:00", time.Date(2026, 9, 14, 12, 0, 0, 0, cstZone), "2026-09-15"},
		{"00:00:00 → **次日** 00:00", time.Date(2026, 9, 14, 0, 0, 0, 0, cstZone), "2026-09-15"},
		{"00:00:01 → 次日 00:00", time.Date(2026, 9, 14, 0, 0, 1, 0, cstZone), "2026-09-15"},
		// 跨月 / 跨年：time.Date 自动进位，这里把它变成可执行的期望。
		{"月末 9/30 23:00 → 10/1", time.Date(2026, 9, 30, 23, 0, 0, 0, cstZone), "2026-10-01"},
		{"年末 12/31 23:00 → 次年 1/1", time.Date(2026, 12, 31, 23, 0, 0, 0, cstZone), "2027-01-01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextMidnightCST(c.nowCST).In(cstZone)
			if wantD := c.wantNext; got.Format("2006-01-02") != wantD {
				t.Errorf("nextMidnightCST(%v) = %v，期望 %s 的 00:00",
					c.nowCST.In(cstZone).Format(time.RFC3339), got.Format(time.RFC3339), wantD)
			}
			if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
				t.Errorf("结果不是零点整：%v", got.Format(time.RFC3339))
			}
			if !got.After(c.nowCST) {
				t.Errorf("结果必须**严格晚于** now（否则冷却长度为 0）：now=%v got=%v",
					c.nowCST, got)
			}
		})
	}
}

// TestNextMidnightCSTIndependentOfHostTZ 结果与宿主时区无关。
//
// # 为什么要单独钉这一条
//
// 状态机的语义是"跟随上游的自然日"（上游用 CST）。
// 用 now.Location() 或 time.Local 计算会得出一个与上游无关的时刻 ——
// 在 UTC 容器里会整整偏 8 小时：上游已经重置了我们还没恢复，
// 或者反过来在错误的时间点重置。
//
// 本用例把同一个瞬时用不同 Location 构造，断言结果**逐纳秒相同**。
func TestNextMidnightCSTIndependentOfHostTZ(t *testing.T) {
	utc := time.Date(2026, 9, 14, 15, 30, 0, 0, time.UTC) // = CST 23:30
	base := nextMidnightCST(utc)

	for _, loc := range []*time.Location{
		time.UTC,
		time.FixedZone("UTC-8", -8*60*60),
		time.FixedZone("UTC+3", 3*60*60),
		cstZone,
	} {
		same := utc.In(loc)
		if got := nextMidnightCST(same); !got.Equal(base) {
			t.Errorf("同一瞬时在 %v 下算出不同结果：%v vs %v", loc, got, base)
		}
	}
}
