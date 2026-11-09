package dailycheckin

import (
	"context"
	"errors"
	"testing"
)

// fakeUp 一个不碰网络的假上游。方法集严格照 Upstream 接口（Accounts/Status/Claim）。
type fakeUp struct {
	accounts    []Account
	statusErr   error
	claimErr    error
	statusCalls int
	claimCalls  int
}

func (f *fakeUp) Accounts(context.Context) ([]Account, error) { return f.accounts, nil }
func (f *fakeUp) Status(context.Context, Account) (bool, bool, error) {
	f.statusCalls++
	return false, true, f.statusErr
}
func (f *fakeUp) Claim(context.Context, Account) (int64, bool, error) {
	f.claimCalls++
	return 10, false, f.claimErr
}

// TestIsCredentialError 判定表：**误杀防线**（T-431）。
//
// # 为什么这是全任务最重要的一条
//
// 误判一个账号为"凭据失效"= 把好号踢下线（用户额度损失 + 人工恢复）。
// 所以这张表要钉住两个方向：
//
//	· 明确的凭据措辞 ⇒ true（否则漏判 ⇒ 号一直失败、用户看不见原因）
//	· 明确的网络措辞 + **模糊措辞** ⇒ false（否则误杀 ⇒ 好号被静默踢下线）
//
// 样本全部来自**部署实测的真实报错文本**（data/checkin-log.json）。
func TestIsCredentialError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		// ── 必须是 true：明确的凭据失效 ──
		{"lobsterai 实测", `领取 HTTP 401: {"code":51102,"message":"请先登录"}`, true},
		{"monkeycode 实测", "查签到状态返回 401：Unauthorized", true},
		{"裸 401", "HTTP 401", true},
		{"小写 unauthorized", "unauthorized", true},
		{"未登录", "未登录，请重新登录", true},
		{"登录已失效", "登录已失效", true},
		{"需要重新登录", "该账号需要重新登录", true},
		{"invalid token", "invalid token", true},
		{"token expired", "token expired", true},
		{"refresh_token", "refresh_token 已失效", true},
		{"华为 APIG.0602", "APIG.0602: token expired", true},
		{"session expired", "session expired", true},

		// ── 必须是 false：网络/超时/上游维护（误杀防线）──
		{"timeout", "context deadline exceeded (timeout)", false},
		{"dial tcp", `dial tcp 1.2.3.4:443: connect: connection refused`, false},
		{"connection reset", "connection reset by peer", false},
		{"EOF", "unexpected EOF", false},
		{"no such host", "dial tcp: lookup api.x.com: no such host", false},
		{"i/o timeout", "i/o timeout", false},
		{"中文 超时", "请求超时", false},
		{"中文 连接失败", "连接失败", false},

		// ── 必须是 false：**模糊措辞**（分不清就不要动）──
		{"qoder 实测（模糊）", "签到状态查询失败（网络 / 凭据 / 响应形状异常）", false},
		{"空错误", "", false},
	}
	for _, c := range cases {
		err := errors.New(c.msg)
		if got := CredentialError(err); got != c.want {
			t.Errorf("%s：CredentialError(%q) = %v，want %v —— "+
				"要么会漏判（号一直失败）、要么会误杀（好号被踢下线）", c.name, c.msg, got, c.want)
		}
	}
	// nil 必须安全（很多路径直接传上游返回的 err）
	if CredentialError(nil) {
		t.Error("CredentialError(nil) = true —— nil 是「没有错误」，不能判成凭据失效")
	}
}

// TestRunAllSkipsDisabledAccounts 已禁用账号**一个上游请求都不发**（L1 梯级，T-431）。
//
// # 判据用 Status 调用数，不是 CheckinOne 调用数
//
// 因为"跳过"要跳得**彻底**：禁用的号连查状态都不该查
// （查状态也是一次上游请求，白喂风控）。
func TestRunAllSkipsDisabledAccounts(t *testing.T) {
	up := &fakeUp{accounts: []Account{
		{UID: "u1"},
		{UID: "u2", Disabled: true}, // ← 已禁用
		{UID: "u3"},
	}}
	d := New(up, Descriptor{ID: "fake-checkin", Label: "签到"}, Options{})
	if err := d.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll 报错：%v", err)
	}
	if up.statusCalls != 2 {
		t.Errorf("Status 被调了 %d 次，want 2 —— 禁用的账号不该被查状态"+
			"（它连一次上游请求都不该发，L1 梯级）", up.statusCalls)
	}
	if up.claimCalls != 2 {
		t.Errorf("Claim 被调了 %d 次，want 2", up.claimCalls)
	}
}

// TestCheckinOneCallsOnCredentialFail 回调**只在**凭据类失败时触发（T-431）。
func TestCheckinOneCallsOnCredentialFail(t *testing.T) {
	// ① 凭据类 ⇒ 触发
	up401 := &fakeUp{statusErr: errors.New(`领取 HTTP 401: {"code":51102,"message":"请先登录"}`)}
	var gotUID string
	var gotErr error
	d401 := New(up401, Descriptor{ID: "fake-checkin", Label: "签到"}, Options{
		OnCredentialFail: func(uid string, err error) { gotUID, gotErr = uid, err },
	})
	d401.CheckinOne(context.Background(), Account{UID: "u-bad"}, "sched")
	if gotUID != "u-bad" || gotErr == nil {
		t.Error("401「请先登录」没有触发 OnCredentialFail —— 那个号永远要手动重登，" +
			"而系统不知道（用户报障的就是这种）")
	}

	// ② 网络类 ⇒ **不触发**（否则误杀好号）
	upTO := &fakeUp{statusErr: errors.New("context deadline exceeded (timeout)")}
	called := false
	dTO := New(upTO, Descriptor{ID: "fake-checkin", Label: "签到"}, Options{
		OnCredentialFail: func(string, error) { called = true },
	})
	dTO.CheckinOne(context.Background(), Account{UID: "u-net"}, "sched")
	if called {
		t.Error("网络超时触发了 OnCredentialFail —— 网络抖动会把**好号**禁用（误杀）")
	}

	// ③ 模糊措辞 ⇒ **不触发**
	upAmbig := &fakeUp{statusErr: errors.New("签到状态查询失败（网络 / 凭据 / 响应形状异常）")}
	called2 := false
	dAmbig := New(upAmbig, Descriptor{ID: "fake-checkin", Label: "签到"}, Options{
		OnCredentialFail: func(string, error) { called2 = true },
	})
	dAmbig.CheckinOne(context.Background(), Account{UID: "u-ambig"}, "sched")
	if called2 {
		t.Error("模糊措辞触发了 OnCredentialFail —— 分不清就不该动（qoder 实测就是这种）")
	}

	// ④ 没接回调时不能 panic（契约：零值 Options 也要能跑）
	New(&fakeUp{statusErr: errors.New("HTTP 401")}, Descriptor{ID: "x", Label: "签到"}, Options{}).
		CheckinOne(context.Background(), Account{UID: "u-nil-cb"}, "sched")
}
