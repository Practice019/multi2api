package codearts

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// 福利领取的**两步协议**（claim + 按需 confirm）—— hermetic 覆盖。
//
// # 为什么这个文件必须存在（补的是一处**真缺陷**的回归网）
//
// 本包的福利领取原先只做 claim 一步，没有 confirm。参照项目
// （dsh-codearts-auth 的 src/codearts-credits.ts，已实测跑通）明确写着：
//
//	5. 响应 `id !== null` 时补 `POST /v1/ops/confirm`（**漏掉会让积分停在待确认**）
//
// 即：领取动作"成功"了，但积分停在待确认态、没真正入账 —— 而界面与日志
// 都显示成功。这是最难排查的一类不一致。现在补齐了，但**必须有测试钉住**：
//
// 本包原有的福利测试全部是 `*Live`（要真实凭证 → 无凭证环境下全 skip），
// 也就是说这条新逻辑在没有凭证的环境里**一行都没被跑过**。
// 本文件用 httptest 假上游覆盖它，不需要任何凭证。
//
// # 断言的是"发了哪些请求"，不是"返回了什么"
//
// 与 mockupstream_test.go 同一教训：只断言返回值会漏掉"少发了一个请求"
// 这类缺陷 —— 而那正是本次要防的（confirm 漏发时 Claimed 仍为 true）。

// welfareMockUpstream 造一个假 snap 上游。
//
// claimID 是 claim 响应里 `data.id` 的**原始 JSON 片段**：
// 传 `null` / `0` / `123` 可以逐一验证「哪些形态会触发 confirm」这条判据。
func welfareMockUpstream(t *testing.T, claimID string, confirmStatus int) (*Client, *welfareCalls, func()) {
	t.Helper()
	calls := &welfareCalls{}

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/ops/delivery"):
			atomic.AddInt32(&calls.delivery, 1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/v1/ops/claim"):
			atomic.AddInt32(&calls.claim, 1)
			// 回 "code":0 表示业务成功；data.id 形态由调用方指定。
			resp := `{"code":0,"message":"ok"`
			if claimID != "" {
				resp += `,"data":{"id":` + claimID + `}`
			}
			resp += `}`
			_, _ = w.Write([]byte(resp))
		case strings.HasSuffix(r.URL.Path, "/v1/ops/confirm"):
			atomic.AddInt32(&calls.confirm, 1)
			if confirmStatus >= 400 {
				w.WriteHeader(confirmStatus)
				_, _ = w.Write([]byte(`{"code":500,"message":"boom"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	c, done := newMockClient(t, handler, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return c, calls, done
}

type welfareCalls struct {
	delivery int32
	claim    int32
	confirm  int32
}

func testWelfareAuth() *Auth {
	return &Auth{AccessKey: "ak", SecretKey: "sk", SecurityToken: "st", ClientID: "vscode-codebot", UID: "u1"}
}

// TestClaimWelfareConfirmsWhenIDPresent `data.id` 有值 → 必须补 confirm。
//
// 这是本次修复的**主断言**：去掉 confirmWelfare 调用后本用例必须变红。
func TestClaimWelfareConfirmsWhenIDPresent(t *testing.T) {
	c, calls, done := welfareMockUpstream(t, `123`, 200)
	defer done()

	res, err := c.ClaimWelfare(testWelfareAuth(), 1)
	if err != nil {
		t.Fatalf("ClaimWelfare 报错: %v", err)
	}
	if !res.Claimed {
		t.Fatalf("claim 业务成功后应 Claimed=true，实际 message=%q", res.Message)
	}
	if got := atomic.LoadInt32(&calls.confirm); got != 1 {
		t.Fatalf("data.id=123 时必须补 1 次 confirm，实际发了 %d 次。\n"+
			"  ⚠ 漏发 confirm 的后果：积分停在待确认态（领了但没入账），\n"+
			"  而 Claimed 仍为 true —— 界面与日志都看不出问题。", got)
	}
}

// TestClaimWelfareSkipsConfirmWhenIDNull `id: null` → **不**补 confirm。
//
// 反向断言。没有它，把 confirm 改成"无条件发"也能通过上一条测试，
// 那会对着不存在的 benefit 多发请求（虽然幂等，但语义是错的）。
func TestClaimWelfareSkipsConfirmWhenIDNull(t *testing.T) {
	c, calls, done := welfareMockUpstream(t, `null`, 200)
	defer done()

	if _, err := c.ClaimWelfare(testWelfareAuth(), 1); err != nil {
		t.Fatalf("ClaimWelfare 报错: %v", err)
	}
	if got := atomic.LoadInt32(&calls.confirm); got != 0 {
		t.Errorf("data.id=null（服务端明确表示无需确认）时不该发 confirm，实际 %d 次", got)
	}
}

// TestClaimWelfareSkipsConfirmWhenIDAbsent 字段**缺席** → 不补 confirm。
//
// 与 null 分开测：JSON 里 `"id"` 缺席与 `"id":null` 在 Go 里都解成 nil，
// 但判据必须覆盖两种输入形态，否则实现若写成检查 map 键存在性就会漏。
func TestClaimWelfareSkipsConfirmWhenIDAbsent(t *testing.T) {
	c, calls, done := welfareMockUpstream(t, ``, 200)
	defer done()

	if _, err := c.ClaimWelfare(testWelfareAuth(), 1); err != nil {
		t.Fatalf("ClaimWelfare 报错: %v", err)
	}
	if got := atomic.LoadInt32(&calls.confirm); got != 0 {
		t.Errorf("data 段缺席时不该发 confirm，实际 %d 次", got)
	}
}

// TestClaimWelfareConfirmsOnIDZero `id: 0` → 补 confirm（刻意不把 0 当"无"）。
//
// 这是一条**有意的偏保守**判据，钉住它的理由：
// ID 用 0 表示"无"是常见约定，但服务端若真返回 0 且需要确认，
// 漏掉的代价是静默丢积分；多发一次 confirm 的代价是幂等请求。
// 两害相权取其轻，所以 0 也补。本用例把这个取舍固定下来，
// 防止后人"顺手优化"成 `if id > 0`。
func TestClaimWelfareConfirmsOnIDZero(t *testing.T) {
	c, calls, done := welfareMockUpstream(t, `0`, 200)
	defer done()

	if _, err := c.ClaimWelfare(testWelfareAuth(), 1); err != nil {
		t.Fatalf("ClaimWelfare 报错: %v", err)
	}
	if got := atomic.LoadInt32(&calls.confirm); got != 1 {
		t.Errorf("data.id=0 时也应补 confirm（宁可多发幂等请求，不可漏确认丢积分），实际 %d 次", got)
	}
}

// TestClaimWelfareConfirmFailureDoesNotFailClaim confirm 失败**不**把整体判失败。
//
// 参照项目的判据（照搬）：confirm 失败时不报 failed —— 积分已进入待确认态，
// 报失败会让用户以为没领到而重复点击。正确行为是如实 Claimed=true，
// 把确认异常留在 Message 里可见。
//
// 断言 Message **必须**提到确认异常：否则"确认失败"变成另一个静默失败
// （用户以为一切都好，但积分其实卡住了）。
func TestClaimWelfareConfirmFailureDoesNotFailClaim(t *testing.T) {
	c, calls, done := welfareMockUpstream(t, `123`, 500)
	defer done()

	res, err := c.ClaimWelfare(testWelfareAuth(), 1)
	if err != nil {
		t.Fatalf("confirm 失败不该让 ClaimWelfare 返回 error（claim 本身已成功）: %v", err)
	}
	if !res.Claimed {
		t.Error("claim 已成功，Claimed 必须为 true —— 报 false 会让用户重复点击")
	}
	if got := atomic.LoadInt32(&calls.confirm); got != 1 {
		t.Fatalf("confirm 应被尝试 1 次，实际 %d 次", got)
	}
	if !strings.Contains(res.Message, "确认") {
		t.Errorf("确认失败必须体现在 Message 里（否则是静默失败），实际 message=%q", res.Message)
	}
}

// TestClaimWelfareFailureSkipsConfirm claim 业务失败 → 不发 confirm。
//
// claim 返回非 0 业务码时（例如"今日已领"），不该再补 confirm ——
// 那会对着一个不存在的 benefit 发请求。
func TestClaimWelfareFailureSkipsConfirm(t *testing.T) {
	calls := &welfareCalls{}
	c, done := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/ops/claim") {
			atomic.AddInt32(&calls.claim, 1)
			// 业务失败 + 带 data.id：判据必须是"先看 code"，不能被 id 带跑。
			_, _ = w.Write([]byte(`{"code":400,"message":"今日已领取","data":{"id":123}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1/ops/confirm") {
			atomic.AddInt32(&calls.confirm, 1)
			_, _ = w.Write([]byte(`{"code":0}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	defer done()

	res, err := c.ClaimWelfare(testWelfareAuth(), 1)
	if err != nil {
		t.Fatalf("ClaimWelfare 报错: %v", err)
	}
	if res.Claimed {
		t.Error("业务码非 0 时 Claimed 必须为 false")
	}
	if got := atomic.LoadInt32(&calls.confirm); got != 0 {
		t.Errorf("claim 业务失败时不该补 confirm，实际 %d 次", got)
	}
	if res.Message != "今日已领取" {
		t.Errorf("业务失败原文应原样透出，实际 %q", res.Message)
	}
}

// TestNeedsConfirmJudgement 直接钉住 needsConfirm 的判据表。
//
// 它是纯函数（无网络、无状态），所以可以逐形态穷举 ——
// 上游若换 ID 形态，这张表是最先该改的地方。
func TestNeedsConfirmJudgement(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
		why  string
	}{
		{``, false, "字段缺席"},
		{`null`, false, "显式 null（服务端表示无需确认）"},
		{`  null  `, false, "带空白的 null 也应识别"},
		{`0`, true, "0 也补（偏保守：宁可多发幂等请求）"},
		{`123`, true, "普通 ID"},
		{`"abc"`, true, "字符串 ID"},
		{`{"a":1}`, true, "对象 ID"},
	}
	for _, c := range cases {
		if got := needsConfirm(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("needsConfirm(%q) = %v, want %v（%s）", c.raw, got, c.want, c.why)
		}
	}
}
