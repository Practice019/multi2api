// upstream5xx_test.go —— "上游 5xx（服务端故障）"与"池子真空"必须给出不同语义。
//
// # 为什么补这条（用户实测报障）
//
// 讯飞 Loomy 网关抖动时短暂返 502（`<center>xunfei</center>` 那页），而默认
// MaxRotate=3：一次用户请求试完 3 个号就回
//
//	503 no_healthy_account: all accounts unavailable (cooling/disabled)
//
// —— 池里 11 个号其实全健康（实测：同刻直连上游 200）。用户看到的是
// "账号都不可用"，真相是"上游抖了几秒"。两处修正：
//
//  1. server 类错误额外给 serverRetryExtra 次换号机会（跨过短抖动）；
//  2. 出口按 lastKind 分流：server → 502 upstream_error（可重试语义），
//     池子真空/全冷却 → 503 no_healthy_account（重试无用，去控制台处理）。
package server

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// scriptedProvider 按调用序返回不同状态（模拟"抖几秒后恢复"的真实上游）。
type scriptedProvider struct {
	id string
	// steps 第 n 次调用（0-based）的状态与体；超出长度则沿用最后一项。
	steps []struct {
		status int
		body   string
	}
	calls atomic.Int32
}

var _ gateway.Provider = (*scriptedProvider)(nil)

func (p *scriptedProvider) ID() string               { return p.id }
func (p *scriptedProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *scriptedProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return []gateway.ModelInfo{{ID: p.id + "-m"}}, nil
}

func (p *scriptedProvider) Chat(_ context.Context, _ gateway.Credential, _ []byte) (gateway.ChatStream, error) {
	n := int(p.calls.Add(1)) - 1
	if len(p.steps) == 0 {
		return gateway.ChatStream{Status: 200, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	}
	if n >= len(p.steps) {
		n = len(p.steps) - 1
	}
	st := p.steps[n]
	return gateway.ChatStream{Status: st.status, Body: io.NopCloser(strings.NewReader(st.body))}, nil
}

// handlerWithAccounts 造一个"某上游 N 个健康号 + 指定 Provider"的 handler。
func handlerWithAccounts(t *testing.T, n int, pv gateway.Provider) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetDefaultProvider("codearts")

	auths := make([]*auth.Auth, 0, n)
	secrets := map[string]any{}
	for i := 0; i < n; i++ {
		uid := string(rune('a'+i)) + "-uid"
		auths = append(auths, &auth.Auth{UID: uid, Nickname: uid})
		secrets[uid] = &fakeCodeartsSecret{AK: "AK-" + uid}
		p.SetCredits(uid, 100000)
	}
	p.SyncToDirWithSecrets("codearts", auths, secrets)
	p.SetMaxInFlight(50) // 在途上限是全局的：给宽松值，避免残留租约把候选清空

	h := NewHandler(Config{
		Pool:              p,
		Provider:          &scriptedRouter{pv: pv, uid: "codearts"},
		DefaultProvider:   "codearts",
		OwnedBy:           "codearts",
		DefaultClassifier: defaultClassifierOf(),
	})
	return h, p
}

// scriptedRouter 本文件自带的最小 ProviderRouter：只实现出口层真正会走到的
// 那几个方法，Chat 转给 scriptedProvider。刻意不复用 multiRouter —— 那是
// 另一份测试的桩，为它加"可替换 Chat"的字段会污染那边的语义。
type scriptedRouter struct {
	pv  gateway.Provider
	uid string
}

func (r *scriptedRouter) Has(id string) bool { return id == "codearts" }
func (r *scriptedRouter) Default() string    { return "codearts" }
func (r *scriptedRouter) Models(context.Context, string) ([]gateway.ModelInfo, bool) {
	return []gateway.ModelInfo{{ID: "m"}}, true
}
func (r *scriptedRouter) ModelMultipliers(context.Context, string) (map[string]float64, bool) {
	return nil, false
}
func (r *scriptedRouter) Chat(ctx context.Context, _ string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	cs, err := r.pv.Chat(ctx, cred, body)
	return cs, true, err
}
func (r *scriptedRouter) Credential(id, uid string) (gateway.Credential, bool) {
	return gateway.Credential{Provider: id, UID: uid, Secret: &fakeCodeartsSecret{AK: "AK-" + uid}}, true
}
func (r *scriptedRouter) RefreshCredential(context.Context, string, gateway.Credential) (bool, error) {
	return false, nil
}
func (r *scriptedRouter) Classify(_ string, status int, body string) (gateway.ErrorKind, bool) {
	return defaultClassifierOf()(status, body), true
}
func (r *scriptedRouter) RefreshSkew(string, gateway.Credential) (time.Duration, bool) {
	return 0, true
}
func (r *scriptedRouter) ResetAt(string, gateway.Credential) (time.Time, bool) {
	return time.Time{}, false
}
func (r *scriptedRouter) SoftRateReset(string, int, string) (time.Time, bool) {
	return time.Time{}, false
}

// ⚠ 假上游的 502 体照抄用户实测原文（讯飞那页 HTML）。
const xunfei502 = "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n" +
	"<body>\r\n<center><h1>502 Bad Gateway</h1></center>\r\n<hr><center>xunfei</center>\r\n</body>\r\n</html>"

// TestUpstream5xxReturns502Not503 拉满 MaxRotate + serverRetryExtra 次仍 5xx →
// 必须回 **502 upstream_error**（上游故障，可重试），而不是 503 no_healthy_account。
func TestUpstream5xxReturns502Not503(t *testing.T) {
	pv := &scriptedProvider{id: "codearts", steps: []struct {
		status int
		body   string
	}{{502, xunfei502}}}
	h, _ := handlerWithAccounts(t, 6, pv)

	code, body := chatOnce(t, h, "codearts/glm-5.3-flash")

	if code != 502 {
		t.Fatalf("code=%d want 502（上游 5xx 不是账号的错，不能被报成 no_healthy_account）body=%s", code, body)
	}
	if !strings.Contains(body, "upstream_error") {
		t.Errorf("错误码应为 upstream_error: %s", body)
	}
	if !strings.Contains(body, "上游服务异常") {
		t.Errorf("文案应说明是上游故障: %s", body)
	}
	// 上游原文必须带出来（排障靠它）。
	if !strings.Contains(body, "xunfei") {
		t.Errorf("响应里丢了上游错误体原文: %s", body)
	}
	// 换号次数必须超过默认 MaxRotate(3) —— 这正是"抖动被放大成故障"的修正。
	if got := pv.calls.Load(); got <= 3 {
		t.Errorf("只换了 %d 次号（want >3，MaxRotate+serverRetryExtra）—— server 类错误的延长重试没生效", got)
	}
}

// TestUpstream5xxBurstThenRecover 前三次 502、第四次成功 → 请求必须成功
// （短抖动要能被换号跨过去，而不是把用户打死在第 3 个号上）。
func TestUpstream5xxBurstThenRecover(t *testing.T) {
	pv := &scriptedProvider{id: "codearts", steps: []struct {
		status int
		body   string
	}{{502, xunfei502}, {502, xunfei502}, {502, xunfei502}, {200, sseOK}}}
	h, _ := handlerWithAccounts(t, 6, pv)

	code, body := chatOnce(t, h, "codearts/glm-5.3-flash")
	if code != 200 {
		t.Fatalf("code=%d want 200（抖动后第 4 次成功应被采纳）body=%s", code, body)
	}
	if got := pv.calls.Load(); got != 4 {
		t.Errorf("调用次数=%d want 4", got)
	}
}

// TestEmptyPoolStill503 池子真的没号时仍是 503（别把改动做成"什么都变 502"）。
func TestEmptyPoolStill503(t *testing.T) {
	pv := &scriptedProvider{id: "codearts"}
	h, _ := handlerWithAccounts(t, 0, pv)

	code, body := chatOnce(t, h, "codearts/glm-5.3-flash")
	if code != 503 {
		t.Fatalf("code=%d want 503 body=%s", code, body)
	}
	if !strings.Contains(body, "no_healthy_account") {
		t.Errorf("空池必须报 no_healthy_account: %s", body)
	}
}

// TestEmptyPoolHintInsteadOfServer502 —— 空池但分类器可用时也不能被误判成 502。
func TestPoolHintMentionsNoAccounts(t *testing.T) {
	if got := poolUnavailableHintOf(nil); !strings.Contains(got, "暂无账号") {
		t.Errorf("空池提示: %q", got)
	}
	_ = httptest.NewRequest // 保持 httptest 依赖（chatOnce 已用）
}

// TestUpstream5xxAllAccountsTrippedStill502 打的是**另一条出口**（用户实测的
// 真正来源）：上游 5xx 喂 NoteError → 熔断把号逐个摘出候选 → 某轮 `acct == nil`
// 提前 break。旧代码这条路径无条件 503（连 lastKind 都不看），于是
// 「上游抖 502、池里号全健康」被报成「账号全不可用」。
//
// 构造：2 个号 + 熔断阈值 1（一次 5xx 即摘）→ 第 3 轮必然选不出号。
func TestUpstream5xxAllAccountsTrippedStill502(t *testing.T) {
	pv := &scriptedProvider{id: "codearts", steps: []struct {
		status int
		body   string
	}{{502, xunfei502}}}
	p := pool.New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetDefaultProvider("codearts")
	p.SetBreaker(1, time.Hour, time.Hour) // 一次 5xx 即熔断
	secrets := map[string]any{}
	var auths []*auth.Auth
	for _, uid := range []string{"a-uid", "b-uid"} {
		auths = append(auths, &auth.Auth{UID: uid, Nickname: uid})
		secrets[uid] = &fakeCodeartsSecret{AK: "AK-" + uid}
		p.SetCredits(uid, 100000)
	}
	p.SyncToDirWithSecrets("codearts", auths, secrets)
	p.SetMaxInFlight(50)

	h := NewHandler(Config{
		Pool:              p,
		Provider:          &scriptedRouter{pv: pv, uid: "codearts"},
		DefaultProvider:   "codearts",
		OwnedBy:           "codearts",
		DefaultClassifier: defaultClassifierOf(),
	})
	code, body := chatOnce(t, h, "codearts/glm-5.3-flash")

	if code != 502 {
		t.Fatalf("code=%d want 502（号被上游 5xx 熔断摘出 ≠ 账号不可用）body=%s", code, body)
	}
	if !strings.Contains(body, "upstream_error") {
		t.Errorf("错误码应为 upstream_error: %s", body)
	}
	// 上游原文必须仍在（这轮它是被熔断摘号，不是没号）。
	if !strings.Contains(body, "xunfei") {
		t.Errorf("丢了上游 502 原文: %s", body)
	}
	if got := pv.calls.Load(); got != 2 {
		t.Errorf("两个号各试一次（第 3 轮无号可选）: calls=%d", got)
	}
}
