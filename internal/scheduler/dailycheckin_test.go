package scheduler

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// fakeCheckinProvider 一个能控制签到结果的假上游（不碰网络）。
//
// ⚠ 它同时实现 gateway.Provider（4 个方法）与 DailyCheckinExt —— 这是共享 Job
// 发现上游的**唯一判据**（`gateway.ExtOf[gateway.DailyCheckinExt](p)`）。
type fakeCheckinProvider struct {
	id     string
	calls  int    // CheckinAll 被调了几次（断言幂等的唯一依据）
	status string // 每趟返回的账号状态：ok / already / fail
	errMsg string // Report.Error（整趟跑不动）
}

func (p *fakeCheckinProvider) ID() string               { return p.id }
func (p *fakeCheckinProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *fakeCheckinProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (p *fakeCheckinProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (p *fakeCheckinProvider) CheckinAll(context.Context) gateway.DailyCheckinReport {
	p.calls++
	st := p.status
	if st == "" {
		st = checkinlog.StatusOK
	}
	return gateway.DailyCheckinReport{
		Provider: p.id,
		Error:    p.errMsg,
		Results:  []gateway.DailyCheckinResult{{UID: p.id + "-u1", Status: st}},
	}
}

func fakeReg(t *testing.T, ps ...*fakeCheckinProvider) *gateway.Registry {
	t.Helper()
	reg := gateway.NewRegistry()
	for _, p := range ps {
		if err := reg.Register(p); err != nil {
			t.Fatalf("注册假上游 %s 失败: %v", p.id, err)
		}
	}
	return reg
}

// withClock 把包级时钟替换成可控的；返回推进函数。
func withClock(t *testing.T, start time.Time) func(time.Time) {
	t.Helper()
	old := dailyCheckinNow
	cur := start
	dailyCheckinNow = func() time.Time { return cur }
	t.Cleanup(func() { dailyCheckinNow = old })
	return func(next time.Time) { cur = next }
}

// TestDailyCheckinJobCoversAllUpstreams 一趟 Run 必须覆盖**所有**实现了签到能力的上游。
//
// # 为什么这条是核心
//
// 改造前 8 个上游里只有 2 个（trae / workbuddy）注册了签到 Job ⇒ 另外 6 个
// **静默失效**（用户实测：codar 两个号只能手动签，trigger=schedule 记录 0 条）。
// 共享 Job 的全部价值就在这条：**一处注册，遍历发现，一个都不漏**。
func TestDailyCheckinJobCoversAllUpstreams(t *testing.T) {
	a := &fakeCheckinProvider{id: "up-a"}
	b := &fakeCheckinProvider{id: "up-b"}
	c := &fakeCheckinProvider{id: "up-c"}
	j := NewDailyCheckinJob(fakeReg(t, a, b, c), nil, nil)
	withClock(t, time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	for _, p := range []*fakeCheckinProvider{a, b, c} {
		if p.calls != 1 {
			t.Errorf("上游 %s 被扫了 %d 次，want 1 —— 少扫一个就意味着它的自动签到又哑了", p.id, p.calls)
		}
	}
}

// TestDailyCheckinJobSkipsAlreadySweptUpstreams 同一天内，第二轮**一个都不该调**（L1 梯级）。
//
// # 为什么这是 L1「别做这件事」而不是 L2「少做几次」
//
// 各上游的 CheckinAll **内部不跳过已签账号**（会 doSign 每一个）⇒
// 若闸门只做到 L2（调了但内部跳过），同一账号一天仍会被 POST 24 次，
// 而 codar 面板写着「上游已有风控记录，请低频使用」⇒ 会打坏账号。
// ⇒ 闸门必须是"**连函数都不调**"，本测试就是它的机器证据。
func TestDailyCheckinJobSkipsAlreadySweptUpstreams(t *testing.T) {
	a := &fakeCheckinProvider{id: "up-a"}
	b := &fakeCheckinProvider{id: "up-b"}
	j := NewDailyCheckinJob(fakeReg(t, a, b), nil, nil)
	withClock(t, time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("第一轮 Run 报错: %v", err)
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("第二轮 Run 报错: %v", err)
	}
	for _, p := range []*fakeCheckinProvider{a, b} {
		if p.calls != 1 {
			t.Errorf("上游 %s 第二轮又被扫了（累计 %d 次，want 1）—— "+
				"闸门没做到 L1 ⇒ 一天会被 POST 24 次，上游有风控", p.id, p.calls)
		}
	}
	// 同一天内 Due 也应为 false（签完之后当天不再跑）
	if j.Due(time.Date(2026, 11, 9, 23, 0, 0, 0, time.Local)) {
		t.Error("同一天内 Due 仍为 true —— 签完之后当天不该再到期")
	}
}

// TestDailyCheckinJobRetriesFailedUpstream 失败的上游下一轮要重试，成功的不要。
//
// # 两个断言都是必需的
//
//	① A 返回 fail ⇒ 第二轮**只调 A**（不能把 B/C 再打一遍 ⇒ 那是浪费 + 风控）
//	② `already` **不算失败**（notes Q5④）：否则每天都会重试一次、
//	   日志上永远一条红，而实际上那天已经签过了。
func TestDailyCheckinJobRetriesFailedUpstream(t *testing.T) {
	bad := &fakeCheckinProvider{id: "up-bad", status: checkinlog.StatusFail}
	ok1 := &fakeCheckinProvider{id: "up-ok1"}
	already := &fakeCheckinProvider{id: "up-already", status: checkinlog.StatusAlready}
	j := NewDailyCheckinJob(fakeReg(t, bad, ok1, already), nil, nil)
	advance := withClock(t, time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("第一轮 Run 报错: %v", err)
	}
	// ★ T-435：第二轮必须**过了 retryFloor（30 分钟）**再跑 ——
	//   否则失败的上游会被每个调度 tick 重试（实测事故：约 40 秒一次、每天约 4300 次）。
	//   ⚠ 本条原本是「紧接着再跑一轮」，那恰好**编码了那个 bug**；
	//     时间下限本身由 TestDailyCheckinJobRespectsRetryFloor 专项守着。
	advance(time.Date(2026, 11, 9, 9, 31, 0, 0, time.Local))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("第二轮 Run 报错: %v", err)
	}
	if bad.calls != 2 {
		t.Errorf("失败的上游只被扫了 %d 次，want 2 —— 失败必须下一轮重试（否则那个号当天再也不签）", bad.calls)
	}
	if ok1.calls != 1 {
		t.Errorf("成功的上游被扫了 %d 次，want 1 —— 第二轮不该再打它", ok1.calls)
	}
	if already.calls != 1 {
		t.Errorf("already 的上游被扫了 %d 次，want 1 —— `already` 是正常状态，"+
			"不算失败（否则日志上每天一条假红）", already.calls)
	}
	// 第二轮之后 Due 仍为 true（还有失败的上游，且已过 retryFloor）
	if !j.Due(time.Date(2026, 11, 9, 10, 5, 0, 0, time.Local)) {
		t.Error("还有上游没签成功（且已过 retryFloor）⇒ Due 应为 true（要重试）")
	}
}

// TestDailyCheckinJobResetsAcrossDays 跨天必须重置 —— 这是"只有跨天才能暴露"的 bug。
//
// # 为什么值得单独一条
//
// `lastOKDay` 错了的后果是两种相反的灾难：
//
//	· 永远不重置 ⇒ 第二天再也不签到（用户每天丢额度，日志上看不出异常）
//	· 重置错了   ⇒ 一天签多次（上游有风控，会打坏账号）
//
// ⇒ 用注入时钟把"今天"推到明天，断言 Due 重新为 true、且第二轮真的再扫一遍。
func TestDailyCheckinJobResetsAcrossDays(t *testing.T) {
	a := &fakeCheckinProvider{id: "up-a"}
	j := NewDailyCheckinJob(fakeReg(t, a), nil, nil)
	advance := withClock(t, time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local))

	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("第一天 Run 报错: %v", err)
	}
	if a.calls != 1 {
		t.Fatalf("第一天扫了 %d 次，want 1", a.calls)
	}
	// 同一天内：不再到期
	if j.Due(time.Date(2026, 11, 9, 20, 0, 0, 0, time.Local)) {
		t.Error("同一天内 Due 应为 false")
	}
	// 推到第二天：必须重新到期，并真的再扫一遍
	advance(time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local))
	if !j.Due(time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)) {
		t.Error("跨天之后 Due 仍为 false —— 第二天再也不会签到（用户每天丢额度，且日志上看不出异常）")
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("第二天 Run 报错: %v", err)
	}
	if a.calls != 2 {
		t.Errorf("跨天之后只扫了 %d 次，want 2 —— 闸门没有重置", a.calls)
	}
}

// TestDailyCheckinJobLogLineIsMachineReadable 那行统计日志的措辞是**可机器读的契约**。
//
// # 为什么用源码级断言
//
// 启动后"8 个上游全覆盖"的**唯一**真机证据就是这行日志
// （Registry 在 main.go 里装配，单测造不出真实的那一份）。
// ⇒ 措辞一旦被改（比如把「可签到上游」改成「上游」），
//
//	验证脚本会静默失效、而没人发现 ⇒ 这里钉死两个关键词。
func TestDailyCheckinJobLogLineIsMachineReadable(t *testing.T) {
	b, err := os.ReadFile("dailycheckin.go")
	if err != nil {
		t.Fatalf("读 dailycheckin.go 失败：%v", err)
	}
	src := string(b)
	for _, want := range []string{"覆盖", "可签到上游", "跳过今天已扫过的"} {
		if !strings.Contains(src, want) {
			t.Errorf("统一签到的统计日志里缺少关键词 %q —— "+
				"真机验证靠这行日志读『覆盖了几个上游』，措辞一改验证就静默失效", want)
		}
	}
}
