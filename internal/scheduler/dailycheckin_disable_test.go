package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// fakeRecorder 记录禁用/恢复调用（不碰任何真实账号池）。
type fakeRecorder struct {
	reasons  map[string]string // uid → 最近一次禁用理由
	recovery map[string]int    // uid → 恢复调用次数
}

func (f *fakeRecorder) RecordAutoDisableFailure(uid, reason string) bool {
	if f.reasons == nil {
		f.reasons = map[string]string{}
	}
	f.reasons[uid] = reason
	return true // 与真实 pool 一样返回"这次真的摘了"
}
func (f *fakeRecorder) RecordAutoDisableSuccess(uid string) bool {
	if f.recovery == nil {
		f.recovery = map[string]int{}
	}
	f.recovery[uid]++
	return true
}

// fakeMulti 能返回**多条**结果的假上游（判据要逐条看，所以需要多结果）。
type fakeMulti struct {
	id      string
	results []gateway.DailyCheckinResult
	calls   int
}

func (f *fakeMulti) ID() string               { return f.id }
func (f *fakeMulti) Caps() gateway.Capability { return gateway.CapChat }
func (f *fakeMulti) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (f *fakeMulti) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (f *fakeMulti) CheckinAll(context.Context) gateway.DailyCheckinReport {
	f.calls++
	return gateway.DailyCheckinReport{Provider: f.id, Results: f.results}
}

func runOnce(t *testing.T, rec DisableRecorder, ps ...*fakeMulti) {
	t.Helper()
	reg := gateway.NewRegistry()
	for _, p := range ps {
		if err := reg.Register(p); err != nil {
			t.Fatalf("注册假上游 %s 失败: %v", p.id, err)
		}
	}
	j := NewDailyCheckinJob(reg, nil, rec)
	withClock(t, time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local))
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
}

// TestDailyCheckinJobDisablesOnCredentialFail 凭据类失败 ⇒ 上报禁用（T-433）。
func TestDailyCheckinJobDisablesOnCredentialFail(t *testing.T) {
	rec := &fakeRecorder{}
	p := &fakeMulti{id: "up-bad", results: []gateway.DailyCheckinResult{{
		UID: "u-401",
		// ← 部署实测原文（lobsterai）
		Status: checkinlog.StatusFail,
		Detail: `领取 HTTP 401: {"code":51102,"message":"请先登录"}`,
	}}}
	runOnce(t, rec, p)
	if _, ok := rec.reasons["u-401"]; !ok {
		t.Error("凭据类失败（401 请先登录）没有上报禁用 —— 那个号会一直失败，" +
			"而系统不知道它要重登（用户报障的就是这种）")
	}
	if !strings.Contains(rec.reasons["u-401"], "签到凭据失效") {
		t.Errorf("禁用理由没有带上语义前缀（实际 %q）—— 管理台上看不出为什么被摘", rec.reasons["u-401"])
	}
}

// TestDailyCheckinJobDoesNotDisableOnNetworkError **防误杀**：网络类失败不许摘号（T-433）。
//
// # 为什么这条是核心
//
// 误判 = 把一个**好号**踢下线（用户额度损失 + 人工恢复）。
// 网络抖动/上游维护会让账号连续失败好几轮 —— 那是"它自己会好"的情况。
func TestDailyCheckminJobDoesNotDisableOnNetworkError(t *testing.T) {
	rec := &fakeRecorder{}
	p := &fakeMulti{id: "up-net", results: []gateway.DailyCheckinResult{
		{UID: "u-timeout", Status: checkinlog.StatusFail, Detail: "context deadline exceeded (timeout)"},
		{UID: "u-dial", Status: checkinlog.StatusFail, Detail: "dial tcp 1.2.3.4:443: connect: connection refused"},
		{UID: "u-eof", Status: checkinlog.StatusFail, Detail: "unexpected EOF"},
	}}
	runOnce(t, rec, p)
	if len(rec.reasons) != 0 {
		t.Errorf("网络类失败被上报禁用了 %v —— **误杀**：网络抖动会把好号摘掉，用户额度损失", rec.reasons)
	}
}

// TestDailyCheckinJobIgnoresAmbiguousError 模糊措辞不许动（T-433）。
//
// ⚠ qoder 实测原文就是这种：它自己都说可能是网络 ⇒ 我们不能替它断言是凭据。
func TestDailyCheckinJobIgnoresAmbiguousError(t *testing.T) {
	rec := &fakeRecorder{}
	p := &fakeMulti{id: "up-ambig", results: []gateway.DailyCheckinResult{{
		UID:    "u-ambig",
		Status: checkinlog.StatusFail,
		Detail: "签到状态查询失败（网络 / 凭据 / 响应形状异常）", // ← qoder 实测
	}}}
	runOnce(t, rec, p)
	if len(rec.reasons) != 0 {
		t.Errorf("模糊措辞被上报禁用了 %v —— 分不清就不该动（防误杀）", rec.reasons)
	}
}

// TestDailyCheckinJobRecoversOnSuccess 成功/已签 ⇒ 解除自动禁用（T-433 的"探活恢复"）。
//
// # 为什么这一条就是"探活"
//
// 我们本来就要为这个号打一次 CheckinAll —— 它返回 ok 就证明凭据活着。
// 再单独发 Status() 探活是白打上游（codar 面板写着「上游已有风控记录，请低频使用」）。
func TestDailyCheckinJobRecoversOnSuccess(t *testing.T) {
	rec := &fakeRecorder{}
	p := &fakeMulti{id: "up-ok", results: []gateway.DailyCheckinResult{
		{UID: "u-ok", Status: checkinlog.StatusOK},
		{UID: "u-already", Status: checkinlog.StatusAlready},
		// skip（上游未开放签到）⇒ 不动：它不是账号的问题
		{UID: "u-skip", Status: checkinlog.StatusSkip, Detail: "上游未开放签到"},
	}}
	runOnce(t, rec, p)
	for _, uid := range []string{"u-ok", "u-already"} {
		if rec.recovery[uid] == 0 {
			t.Errorf("%s 签到成功/已签，但没有解除自动禁用 —— "+
				"用户修好凭据后系统不知道（只能人工恢复）", uid)
		}
	}
	if rec.recovery["u-skip"] != 0 {
		t.Error("skip（上游未开放签到）不该触发解除 —— 那不是「账号可用」的证据")
	}
	// nil recorder 不能 panic（契约：装配层可以不传）
	runOnce(t, nil, &fakeMulti{id: "up-nil", results: []gateway.DailyCheckinResult{
		{UID: "u-x", Status: checkinlog.StatusFail, Detail: "HTTP 401"},
	}})
}
