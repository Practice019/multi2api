// autodisable_test.go 阈值化自动禁用的表驱动测试。
//
// 覆盖四件事（对应验收口径）：
//
//	① 连败达阈值即禁用（不是冷却）—— 断言 Pick() 选不到它、Disabled 为真
//	② 成功即恢复（计数清零 + 解除本机制置的禁用）
//	③ 阈值可配（SetAutoDisable 的边界：非正保留默认、窗口 0 合法）
//	④ 并发安全（-race 下的多 goroutine 记账，且状态不自相矛盾）
//
// 另外钉住几条**容易回归的语义**（都是实现注释里写明的取舍）：
//   - 不复用 e.fails（熔断在阈值 3 时把 fails 归零，会导致永远到不了 5）
//   - 人工禁用不被"成功"静默推翻（owned 语义，对标 new-api 的 AutoDisabled 态）
//   - 被禁用后仍能被自动探测候选集看见（否则永不恢复）
//   - 账号从池里删掉后，残留状态被清理（重新导入不会带着旧进度上台）
package pool

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newAutoDisablePool 建一个无落盘的池，并注册注册表清理（见 autodisable.go 的架构妥协）。
func newAutoDisablePool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	t.Cleanup(func() { sweepAutoDisableBook(p) })
	return p
}

// autoReasonOf 读 entry.reason（状态文案的权威来源，与 Status.Reason 同源）。
func autoReasonOf(t *testing.T, p *Pool, uid string) string {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("账号 %s 不在池里", uid)
	}
	return st.Reason
}

// TestAutoDisableThresholdTable 表驱动：只有"连败达阈值"那一步才禁用。
//
// ⚠ 这一条同时是"不复用 e.fails"的回归测试：默认熔断阈值是 3，
// 若实现拿 e.fails 当判据，fails 会在第 3 次失败时被熔断器归零，
// 于是 fails=3/4/5 这三行都会得到 Disabled=false —— 测试立刻红。
func TestAutoDisableThresholdTable(t *testing.T) {
	cases := []struct {
		name      string
		threshold int
		failures  int
		want      bool
	}{
		{name: "默认阈值只差一次", threshold: 0, failures: 4, want: false},
		{name: "默认阈值刚好达到", threshold: 0, failures: 5, want: true},
		{name: "默认阈值超出", threshold: 0, failures: 6, want: true},
		{name: "阈值 1 首次失败即禁", threshold: 1, failures: 1, want: true},
		{name: "阈值 2 未达", threshold: 2, failures: 1, want: false},
		{name: "阈值 2 达到", threshold: 2, failures: 2, want: true},
		{name: "阈值 8 未达", threshold: 8, failures: 7, want: false},
		{name: "阈值 8 达到", threshold: 8, failures: 8, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newAutoDisablePool(t)
			p.Add(&auth.Auth{UID: "u1"})
			p.SetAutoDisable(c.threshold, DefaultAutoDisableWindow)

			// 把熔断阈值调到远大于本用例的次数，避免熔断把号先冷掉，
			// 让"禁用"与"熔断"两条线互不干扰（这正是两个机制解耦的验证）。
			p.SetBreaker(1000, time.Hour, time.Hour)

			var lastEvent bool
			for i := 0; i < c.failures; i++ {
				lastEvent = p.RecordAutoDisableFailure("u1", "upstream 500")
			}

			st, ok := p.AutoDisableStatus("u1")
			if !ok {
				t.Fatal("账号应存在于池里")
			}
			if st.Disabled != c.want {
				t.Errorf("Disabled=%v want %v（streak=%d threshold=%d）", st.Disabled, c.want, st.Streak, st.Threshold)
			}
			if st.Streak != c.failures {
				t.Errorf("streak=%d want %d", st.Streak, c.failures)
			}
			// "禁用事件"只在**状态跃迁**那一次为 true。
			wantEvent := c.want
			if lastEvent != wantEvent {
				t.Errorf("最后一次 RecordAutoDisableFailure 返回 %v want %v", lastEvent, wantEvent)
			}

			// 达阈值必须是**禁用**而非冷却：Pick 选不到它。
			if c.want {
				if got := p.Pick(); got != nil {
					t.Errorf("被自动禁用的账号不该被选中，实得 %+v", got)
				}
				if p.internalHealthy("u1") {
					t.Error("禁用后 internalHealthy 必须为 false")
				}
			} else {
				if got := p.Pick(); got == nil || got.UID != "u1" {
					t.Errorf("未达阈值应仍可选中，实得 %+v", got)
				}
			}
		})
	}
}

// TestAutoDisableSuccessClearsStreak 任意一次成功清零计数（这是"不误禁"的关键）。
func TestAutoDisableSuccessClearsStreak(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour) // 隔离熔断
	p.SetAutoDisable(5, DefaultAutoDisableWindow)

	for i := 0; i < 4; i++ {
		p.RecordAutoDisableFailure("u1", "boom")
	}
	if got := p.AutoDisableStreak("u1"); got != 4 {
		t.Fatalf("streak=%d want 4", got)
	}

	if recovered := p.RecordAutoDisableSuccess("u1"); recovered {
		t.Error("尚未禁用，成功不该报告 recovered")
	}
	if got := p.AutoDisableStreak("u1"); got != 0 {
		t.Fatalf("成功后 streak=%d want 0（否则零散失败会攒到阈值误禁健康号）", got)
	}

	// 再失败 4 次仍不该禁用（计数是从 0 开始的新一轮）。
	for i := 0; i < 4; i++ {
		p.RecordAutoDisableFailure("u1", "boom")
	}
	if st, _ := p.AutoDisableStatus("u1"); st.Disabled {
		t.Errorf("清零后应重新累计，不该在第 4 次就禁用: %+v", st)
	}
}

// TestAutoDisableSuccessRecoversOwnedDisable 由本机制置的禁用，成功一次即恢复。
func TestAutoDisableSuccessRecoversOwnedDisable(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(3, DefaultAutoDisableWindow)

	for i := 0; i < 3; i++ {
		if d := p.RecordAutoDisableFailure("u1", "boom"); d != (i == 2) {
			t.Fatalf("第 %d 次失败返回 disabled=%v", i+1, d)
		}
	}
	if !p.IsAutoDisabled("u1") {
		t.Fatal("应处于自动禁用")
	}
	if recovered := p.RecordAutoDisableSuccess("u1"); !recovered {
		t.Fatal("成功应解除本机制置的禁用")
	}
	if p.IsAutoDisabled("u1") {
		t.Error("恢复后 IsAutoDisabled 应为 false")
	}
	st, _ := p.AutoDisableStatus("u1")
	if st.Disabled || st.Streak != 0 {
		t.Errorf("恢复后状态应为干净: %+v", st)
	}
	if st.DisabledAt.IsZero() == false {
		t.Error("恢复后 DisabledAt 应清零（它是禁用时刻，不是历史记录）")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("恢复后应可被选中，实得 %+v", got)
	}
}

// TestAutoDisableDoesNotOverrideManualDisable 人工/凭证禁用不被自动成功推翻。
//
// 依据：new-api 的 ShouldEnableChannel 只对 ChannelStatusAutoDisabled 放行
// （service/channel.go:89），其余状态必须人工处理。
func TestAutoDisableDoesNotOverrideManualDisable(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(3, DefaultAutoDisableWindow)

	// 先由人工禁用（reason 是人工给的文案，不能被我们的覆盖）。
	p.Disable("u1", "凭证失效，需重新登录")

	// 后续失败仍然记账，但**不改**禁用原因、也不声称是自己禁的。
	for i := 0; i < 4; i++ {
		if d := p.RecordAutoDisableFailure("u1", "upstream 500"); d {
			t.Fatal("账号已被人禁，不该报告新的禁用事件")
		}
	}
	if got := autoReasonOf(t, p, "u1"); got != "凭证失效，需重新登录" {
		t.Errorf("人工禁用原因被覆盖了: %q", got)
	}
	if p.IsAutoDisabled("u1") {
		t.Error("被人禁的号不该被登记为自动禁用")
	}

	// 成功一次：计数清零，但**禁用不动**（否则一次探测成功就静默撤销了人的决定）。
	if recovered := p.RecordAutoDisableSuccess("u1"); recovered {
		t.Error("成功不该解除人工禁用")
	}
	st, _ := p.AutoDisableStatus("u1")
	if !st.Disabled {
		t.Error("人工禁用应保持")
	}
	if got := p.Pick(); got != nil {
		t.Errorf("人工禁用的账号仍被选中: %+v", got)
	}
}

// TestAutoDisableRecordsTimeAndReason 禁用时记录时间与原因（验收明确要求）。
func TestAutoDisableRecordsTimeAndReason(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(2, DefaultAutoDisableWindow)

	before := time.Now()
	p.RecordAutoDisableFailure("u1", "upstream 502 bad gateway")
	p.RecordAutoDisableFailure("u1", "upstream 502 bad gateway")
	after := time.Now()

	st, _ := p.AutoDisableStatus("u1")
	if st.DisabledAt.Before(before) || st.DisabledAt.After(after) {
		t.Errorf("DisabledAt=%v 不在 [%v, %v]", st.DisabledAt, before, after)
	}
	if st.LastFail.IsZero() {
		t.Error("LastFail 应被记录")
	}
	for _, want := range []string{"连续失败 2 次", "阈值 2", "upstream 502 bad gateway"} {
		if !strings.Contains(st.Reason, want) {
			t.Errorf("Reason 缺 %q: %q", want, st.Reason)
		}
	}
	// 同一份文案要进 entry.reason（管理台的"禁用原因"列读的是它）。
	if r := autoReasonOf(t, p, "u1"); r != st.Reason {
		t.Errorf("Status.Reason=%q 与 AutoDisableStatus.Reason=%q 不一致", r, st.Reason)
	}

	// 重复失败不应刷新"什么时候坏掉的"。
	firstAt := st.DisabledAt
	time.Sleep(2 * time.Millisecond)
	if d := p.RecordAutoDisableFailure("u1", "again"); d {
		t.Error("已禁用后再次失败不该报告新的禁用事件")
	}
	st2, _ := p.AutoDisableStatus("u1")
	if !st2.DisabledAt.Equal(firstAt) {
		t.Errorf("DisabledAt 被刷新了: %v -> %v（应保留首次禁用时刻）", firstAt, st2.DisabledAt)
	}
	if st2.Streak != 3 {
		t.Errorf("streak=%d want 3（禁用后仍应继续记账）", st2.Streak)
	}
}

// TestAutoDisableReasonTruncated 上游原因被截断（避免 state.json 被整页 HTML 撑爆）。
func TestAutoDisableReasonTruncated(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(1, DefaultAutoDisableWindow)

	huge := strings.Repeat("上游返回了一整页 HTML ", 500)
	p.RecordAutoDisableFailure("u1", huge)

	st, _ := p.AutoDisableStatus("u1")
	if len([]rune(st.Reason)) > autoDisableCauseMaxRunes+64 {
		t.Errorf("Reason 长度 %d 超过上限（截断失效）", len([]rune(st.Reason)))
	}
	if !strings.HasSuffix(st.Reason, "…") {
		t.Errorf("截断后应带省略号: %q", st.Reason[max(0, len(st.Reason)-8):])
	}
	// 多字节安全：必须仍是合法 UTF-8（不能被按字节切断）。
	if !utf8Valid(st.Reason) {
		t.Error("截断把多字节字符切坏了（应按 rune 截断）")
	}
}

// TestAutoDisableWindowTable 衰减窗口：窗口内累计、窗口外重新开始。
func TestAutoDisableWindowTable(t *testing.T) {
	cases := []struct {
		name       string
		window     time.Duration
		gap        time.Duration
		firstRun   int
		wantStreak int
		wantDis    bool
	}{
		{name: "窗口内继续累计", window: time.Hour, gap: 0, firstRun: 4, wantStreak: 5, wantDis: true},
		{name: "窗口外重新开始", window: time.Millisecond, gap: 5 * time.Millisecond, firstRun: 4, wantStreak: 1, wantDis: false},
		{name: "窗口 0 即不衰减", window: 0, gap: 5 * time.Millisecond, firstRun: 4, wantStreak: 5, wantDis: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newAutoDisablePool(t)
			p.Add(&auth.Auth{UID: "u1"})
			p.SetBreaker(1000, time.Hour, time.Hour)
			p.SetAutoDisable(5, c.window)

			for i := 0; i < c.firstRun; i++ {
				p.RecordAutoDisableFailure("u1", "boom")
			}
			if c.gap > 0 {
				time.Sleep(c.gap)
			}
			p.RecordAutoDisableFailure("u1", "boom")

			st, _ := p.AutoDisableStatus("u1")
			if st.Streak != c.wantStreak {
				t.Errorf("streak=%d want %d", st.Streak, c.wantStreak)
			}
			if st.Disabled != c.wantDis {
				t.Errorf("Disabled=%v want %v", st.Disabled, c.wantDis)
			}
		})
	}
}

// TestAutoDisableConfigTable SetAutoDisable 的边界语义（与 SetBreaker 同口径）。
func TestAutoDisableConfigTable(t *testing.T) {
	cases := []struct {
		name          string
		threshold     int
		window        time.Duration
		wantThreshold int
		wantWindow    time.Duration
	}{
		{name: "不配置走默认", threshold: 0, window: 0, wantThreshold: DefaultAutoDisableThreshold, wantWindow: 0},
		{name: "负阈值保留默认", threshold: -3, window: 0, wantThreshold: DefaultAutoDisableThreshold, wantWindow: 0},
		{name: "正整数生效", threshold: 9, window: 0, wantThreshold: 9, wantWindow: 0},
		{name: "窗口可设", threshold: 7, window: 10 * time.Minute, wantThreshold: 7, wantWindow: 10 * time.Minute},
		{name: "负窗口不改", threshold: 4, window: -time.Second, wantThreshold: 4, wantWindow: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newAutoDisablePool(t)
			p.SetAutoDisable(c.threshold, c.window)
			if got := p.AutoDisableThreshold(); got != c.wantThreshold {
				t.Errorf("threshold=%d want %d", got, c.wantThreshold)
			}
			if got := p.AutoDisableWindow(); got != c.wantWindow {
				t.Errorf("window=%v want %v", got, c.wantWindow)
			}
			// 状态里的 Threshold 必须反映生效值（前端按它画 "3/5"）。
			p.Add(&auth.Auth{UID: "u1"})
			st, _ := p.AutoDisableStatus("u1")
			if st.Threshold != c.wantThreshold {
				t.Errorf("状态里的 threshold=%d want %d", st.Threshold, c.wantThreshold)
			}
		})
	}
}

// TestAutoDisableConfigDoesNotRejudgeHistory 调阈值不回溯改写既有判定。
func TestAutoDisableConfigDoesNotRejudgeHistory(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(3, DefaultAutoDisableWindow)

	for i := 0; i < 3; i++ {
		p.RecordAutoDisableFailure("u1", "boom")
	}
	if !p.IsAutoDisabled("u1") {
		t.Fatal("应已被禁用")
	}

	// 调大阈值：已禁用的号**不会**被自动放出（禁用是一次决定，不是配置的派生值）。
	p.SetAutoDisable(100, DefaultAutoDisableWindow)
	if !p.IsAutoDisabled("u1") {
		t.Error("调大阈值不该自动解除既有禁用")
	}
	// 调小阈值：未达标的号**不会**被补禁。
	p2 := newAutoDisablePool(t)
	p2.Add(&auth.Auth{UID: "u2"})
	p2.SetBreaker(1000, time.Hour, time.Hour)
	p2.SetAutoDisable(50, DefaultAutoDisableWindow)
	p2.RecordAutoDisableFailure("u2", "boom")
	p2.SetAutoDisable(2, DefaultAutoDisableWindow)
	if st, _ := p2.AutoDisableStatus("u2"); st.Disabled {
		t.Errorf("调小阈值不该回溯禁用: %+v", st)
	}
}

// TestAutoDisableConcurrency 并发记账：不崩、不死锁、状态自洽（配 -race 跑）。
//
// ⚠ 断言只放在**确定性**的量上：并发成功/失败混跑时最终 streak 是不确定的，
// 但"禁用标记不会凭空出现"、"streak 不为负"、"成功之后不会再被判定为禁用"
// 这些不变量必须始终成立。
func TestAutoDisableConcurrency(t *testing.T) {
	p := newAutoDisablePool(t)
	const accounts = 6
	uids := make([]string, 0, accounts)
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("c%d", i)
		uids = append(uids, uid)
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(5, DefaultAutoDisableWindow)

	const workers = 12
	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				uid := uids[(w+i)%accounts]
				switch i % 3 {
				case 0:
					p.RecordAutoDisableFailure(uid, "boom")
				case 1:
					p.RecordAutoDisableSuccess(uid)
				default:
					// 与读取路径并发：查询必须只读、不得 panic/deadlock。
					_, _ = p.AutoDisableStatus(uid)
					_ = p.AutoDisabledCandidates()
					_ = p.pick(nil, "")
				}
			}
		}(w)
	}
	wg.Wait()

	for _, uid := range uids {
		st, ok := p.AutoDisableStatus(uid)
		if !ok {
			t.Fatalf("%s 应存在于池里", uid)
		}
		if st.Streak < 0 {
			t.Errorf("%s streak 为负: %d", uid, st.Streak)
		}
		// 禁用的账号必须真的 unhealthy；未禁用的必须 healthy。
		if st.Disabled && p.internalHealthy(uid) {
			t.Errorf("%s 标记禁用却 internalHealthy=true", uid)
		}
		if !st.Disabled && !p.internalHealthy(uid) {
			t.Errorf("%s 未标记禁用却 internalHealthy=false（状态撕裂）", uid)
		}
	}
}

// TestAutoDisableConcurrentFailuresReachThreshold 并发**纯失败**必须最终禁用
// （证明计数在锁内累加，没有丢更新）。
func TestAutoDisableConcurrentFailuresReachThreshold(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(10, DefaultAutoDisableWindow)

	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			p.RecordAutoDisableFailure("u1", "boom")
		}()
	}
	wg.Wait()

	st, _ := p.AutoDisableStatus("u1")
	if st.Streak != workers {
		t.Errorf("streak=%d want %d（丢更新说明计数不在锁内）", st.Streak, workers)
	}
	if !st.Disabled {
		t.Error("失败次数正好达到阈值，应被禁用")
	}
}

// TestAutoDisabledCandidates 被自动禁用的号必须出现在探测候选集里（恢复通道）。
func TestAutoDisabledCandidates(t *testing.T) {
	p := newAutoDisablePool(t)
	for _, uid := range []string{"b-auto", "a-auto", "c-manual", "d-fine"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(2, DefaultAutoDisableWindow)

	// a-auto / b-auto：自动禁用。
	for _, uid := range []string{"a-auto", "b-auto"} {
		p.RecordAutoDisableFailure(uid, "boom")
		p.RecordAutoDisableFailure(uid, "boom")
	}
	// c-manual：人工禁用（会失败几次但**不改**禁用来源）。
	p.Disable("c-manual", "人工禁用")
	p.RecordAutoDisableFailure("c-manual", "boom")
	p.RecordAutoDisableFailure("c-manual", "boom")
	// d-fine：健康。

	got := p.AutoDisabledCandidates()
	if len(got) != 2 {
		t.Fatalf("候选数=%d want 2（人工禁用不该进探测集）: %+v", len(got), got)
	}
	if got[0].UID != "a-auto" || got[1].UID != "b-auto" {
		t.Errorf("候选顺序应为 (provider, uid) 升序，实得 %+v", got)
	}
	for _, c := range got {
		if c.DisabledAt.IsZero() {
			t.Errorf("%s 的 DisabledAt 未记录", c.UID)
		}
		if !strings.Contains(c.Reason, "自动禁用") {
			t.Errorf("%s 的 Reason 不像自动禁用: %q", c.UID, c.Reason)
		}
		if c.Streak != 2 {
			t.Errorf("%s streak=%d want 2", c.UID, c.Streak)
		}
	}

	// 探测成功 → 恢复（这就是"被禁用的号唯一的复活通道"）。
	if !p.RecordAutoDisableSuccess("a-auto") {
		t.Fatal("探测成功应恢复 a-auto")
	}
	if got := p.AutoDisabledCandidates(); len(got) != 1 || got[0].UID != "b-auto" {
		t.Errorf("恢复后候选应为 [b-auto]，实得 %+v", got)
	}
}

// TestResetAutoDisable 人工恢复：清账 + 解除本机制置的禁用，且不会误放人工禁用。
func TestResetAutoDisable(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "auto"})
	p.Add(&auth.Auth{UID: "manual"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(3, DefaultAutoDisableWindow)

	for i := 0; i < 3; i++ {
		p.RecordAutoDisableFailure("auto", "boom")
	}
	p.Disable("manual", "人工禁用")

	if !p.ResetAutoDisable("auto") {
		t.Error("ResetAutoDisable 应报告解除了一个自动禁用")
	}
	st, _ := p.AutoDisableStatus("auto")
	if st.Disabled || st.Streak != 0 || st.Reason != "" {
		t.Errorf("人工恢复后状态应干净: %+v", st)
	}
	if p.internalHealthy("auto") == false {
		t.Error("恢复后应可用")
	}

	if p.ResetAutoDisable("manual") {
		t.Error("ResetAutoDisable 不该解除人工禁用")
	}
	if st, _ := p.AutoDisableStatus("manual"); !st.Disabled {
		t.Error("人工禁用应保持")
	}
	if p.ResetAutoDisable("nobody") {
		t.Error("不存在的账号应返回 false")
	}
}

// TestAutoDisableManualEnableResetsProgress 人工 Enable 之后进度重来（不自愈成秒禁）。
//
// 场景：禁用 → 操作员 Enable → 下一次失败。若不重置 streak，
// 它会在**下一次**失败时立刻又被禁（4/5+1），解禁看起来完全无效。
func TestAutoDisableManualEnableResetsProgress(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(3, DefaultAutoDisableWindow)

	for i := 0; i < 3; i++ {
		p.RecordAutoDisableFailure("u1", "boom")
	}
	if !p.Enable("u1") {
		t.Fatal("Enable 应成功")
	}
	// 下一次失败：进度应从头开始（streak=1），而不是立刻再禁。
	if d := p.RecordAutoDisableFailure("u1", "boom"); d {
		t.Fatal("解禁后的第一次失败不该立刻再次禁用")
	}
	st, _ := p.AutoDisableStatus("u1")
	if st.Streak != 1 || st.Disabled {
		t.Errorf("解禁后应重新计数: %+v", st)
	}
}

// TestAutoDisableUnknownUID 未知 uid / 已删除 uid 的边界。
func TestAutoDisableUnknownUID(t *testing.T) {
	p := newAutoDisablePool(t)

	if d := p.RecordAutoDisableFailure("ghost", "boom"); d {
		t.Error("未知账号不该报告禁用")
	}
	if r := p.RecordAutoDisableSuccess("ghost"); r {
		t.Error("未知账号不该报告恢复")
	}
	if _, ok := p.AutoDisableStatus("ghost"); ok {
		t.Error("未知账号不该返回 ok")
	}
	if p.AutoDisableStreak("ghost") != 0 {
		t.Error("未知账号 streak 应为 0")
	}
	if p.IsAutoDisabled("ghost") {
		t.Error("未知账号不该被判为自动禁用")
	}

	// 删号后再导入同名 uid：旧进度必须已清理。
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(2, DefaultAutoDisableWindow)
	p.RecordAutoDisableFailure("u1", "boom")
	if got := p.AutoDisableStreak("u1"); got != 1 {
		t.Fatalf("streak=%d want 1", got)
	}
	p.Remove("u1")
	p.Add(&auth.Auth{UID: "u1"})
	if got := p.AutoDisableStreak("u1"); got != 0 {
		t.Errorf("重新导入后 streak=%d want 0（残留状态未清理）", got)
	}
}

// TestAutoDisableSuccessOnCleanAccountCreatesNoState 没失败过的号不留空记录（注册表不膨胀）。
func TestAutoDisableSuccessOnCleanAccountCreatesNoState(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordAutoDisableSuccess("u1")

	p.mu.RLock()
	n := len(p.autoBookLocked().states)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("成功路径不该建状态，实得 %d 条", n)
	}
}

// TestTruncateRunes 截断工具的表驱动测试（rune 边界）。
func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{name: "空串", in: "", n: 5, want: ""},
		{name: "短于上限原样", in: "abc", n: 5, want: "abc"},
		{name: "恰好等长", in: "abcde", n: 5, want: "abcde"},
		{name: "ASCII 截断", in: "abcdefg", n: 3, want: "abc…"},
		{name: "中文按 rune 截断", in: "上游返回错误", n: 2, want: "上游…"},
		{name: "上限 0 返回空", in: "abc", n: 0, want: ""},
		{name: "负上限返回空", in: "abc", n: -1, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncateRunes(c.in, c.n); got != c.want {
				t.Errorf("truncateRunes(%q,%d)=%q want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

// TestAutoDisableSeparateFromBreaker 两条线互不污染：禁用后熔断计数照旧归零/冷却照旧到期。
//
// 这条钉住"禁用不是冷却"：自动禁用不会设置 until/breakerUntil，也不会被
// NoteSuccess 的熔断清零逻辑顺手解除（那个逻辑在 pool.go，不认识本机制）。
func TestAutoDisableSeparateFromBreaker(t *testing.T) {
	p := newAutoDisablePool(t)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1000, time.Hour, time.Hour)
	p.SetAutoDisable(2, DefaultAutoDisableWindow)

	p.RecordAutoDisableFailure("u1", "boom")
	p.RecordAutoDisableFailure("u1", "boom")
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("自动禁用不该产生冷却: %+v", st)
	}
	if st.Until.IsZero() == false || st.BreakerUntil.IsZero() == false {
		t.Errorf("不该设置冷却截止: until=%v breaker=%v", st.Until, st.BreakerUntil)
	}

	// NoteSuccess 只清熔断线，**不**认识自动禁用（由集成层同时调用本机制的 Record 方法）。
	p.NoteSuccess("u1")
	if !p.IsAutoDisabled("u1") {
		t.Error("NoteSuccess 不该解除自动禁用（它只负责熔断线）")
	}
}
