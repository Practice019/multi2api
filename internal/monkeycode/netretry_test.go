// netretry_test.go 守「网络失败可解释」与「重试范围严格」两条。
//
// # 为什么这两条必须有测试
//
//	① 错误串是用户唯一的线索。用户实测拿到的原文是裸的
//	   `net/http: TLS handshake timeout`，而真因是「走代理那条路不通」
//	   —— 一条指不到原因的错误会让人去查上游、查账号、查版本，
//	   就是查不到出口上。本文件把"错误里必须出现出口"钉住。
//	   ⚠ 2026-11-01 更正：当时的推断是"国际站必须走代理"，用户随后实测推翻了它
//	   （零代理可直连）；现在文案只报"走的是哪个出口 + 直连/代理两条路"，不再下绝对结论。
//
//	② 重试必须**只**覆盖幂等方法。签到是 POST，重复提交在上游是错误码
//	   （checkin.go:54「已签必须早退」就是为它写的）；给 POST 加重试会制造
//	   "看起来成功、实际污染了历史"这种最难查的形态。
package monkeycode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyServer 第 1 次请求在传输层断开（模拟代理抖动），之后正常。
func flakyServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(hits, 1) == 1 {
			// Hijack 后直接关连接：客户端看到的是 EOF（传输层错误，可重试）
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cnAuth() *Auth { return &Auth{Region: RegionCN, Session: "s"} }

// GET 是幂等的 ⇒ 传输层抖动应当被一次重试吃掉。
func TestGetRetriesTransientNetworkError(t *testing.T) {
	var hits int32
	srv := flakyServer(t, &hits)
	c := NewClientWithProxy("")

	code, raw, err := c.doJSON(context.Background(), cnAuth(), http.MethodGet, srv.URL+"/users/status", nil, nil)
	if err != nil {
		t.Fatalf("第一次抖动应被重试吃掉，实际 err=%v", err)
	}
	if code != http.StatusOK || !strings.Contains(string(raw), "\"ok\"") {
		t.Fatalf("重试后应拿到 200 与响应体，实际 code=%d raw=%s", code, raw)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("应恰好打两次（1 次失败 + 1 次成功），实际 %d", got)
	}
}

// POST 不幂等 ⇒ **一次都不许重试**（否则签到可能被重复提交）。
func TestPostDoesNotRetry(t *testing.T) {
	var hits int32
	srv := flakyServer(t, &hits)
	c := NewClientWithProxy("")

	_, _, err := c.doJSON(context.Background(), cnAuth(), http.MethodPost, srv.URL+"/users/wallet/checkin", nil, []byte(`{}`))
	if err == nil {
		t.Fatal("传输层失败必须报错（不许假装成功）")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("★ POST 必须只发一次，实际 %d 次", got)
	}
}

// 错误里必须出现**出口**，且官方给的代理要出现在文案里。
//
// ★ 新口径（2026-11-01 用户实测）：国际站可直连，所以文案里**不许**再有
// "必须走代理"这种绝对化结论；但"走的是哪个出口"这条必须保留（排障全靠它）。
func TestNetErrMentionsEgress(t *testing.T) {
	c := NewClientWithProxy("http://127.0.0.1:10808")
	intl := &Auth{Region: RegionIntl, Session: "s"}

	wrapped := c.wrapNetErr(intl, http.MethodGet, "https://monkeycode-ai.net/api/v1/users/wallet/checkin",
		timeoutErr{})
	msg := wrapped.Error()
	for _, want := range []string{"国际站出口", "http://127.0.0.1:10808"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误文案应含 %q，实际 %q", want, msg)
		}
	}
	// 新口径：既要给出可操作的两条路（直连 / 代理），也不许把"走代理"写成必然。
	for _, want := range []string{"代理", "直连"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误文案应提示 %q 这条路（用户实测：国际站可直连），实际 %q", want, msg)
		}
	}
	for _, bad := range []string{"必须走代理", "必须配代理", "一定连不上"} {
		if strings.Contains(msg, bad) {
			t.Errorf("★ 错误文案仍有旧前提 %q（实测：零代理可直连国际站），实际 %q", bad, msg)
		}
	}
	// 国内站直连时不该出现代理提示
	cnMsg := c.wrapNetErr(cnAuth(), http.MethodGet, "https://monkeycode-ai.com/api/v1/users/status", timeoutErr{}).Error()
	if !strings.Contains(cnMsg, "国内站出口（直连）") {
		t.Errorf("国内站直连的错误文案不对: %q", cnMsg)
	}
	for _, bad := range []string{"必须走代理", "必须配代理"} {
		if strings.Contains(cnMsg, bad) {
			t.Errorf("国内站文案不该含 %q: %q", bad, cnMsg)
		}
	}
}

// wrapNetErr 必须保留 %w 链：上层要用 errors.Is 区分取消/超时。
func TestNetErrKeepsCauseChain(t *testing.T) {
	c := NewClientWithProxy("")
	err := c.wrapNetErr(cnAuth(), http.MethodGet, "https://monkeycode-ai.com/x", context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Error("★ 包装后必须仍能 errors.Is(context.Canceled) —— 换成 非 %w 之外的动词会让上层的取消判断静默失效")
	}
}

// transientNetErr 的判据（含"取消不算瞬时"这条最容易写反的）。
func TestTransientNetErrClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"TLS 握手超时", timeoutErr{}, true},
		{"连接被重置", errors.New("read tcp: connection reset by peer"), true},
		{"EOF", errors.New("unexpected EOF"), true},
		{"DNS 解析不出", errors.New("dial tcp: lookup x: no such host"), true},
		{"调用方取消", context.Canceled, false},
		{"上下文到点", context.DeadlineExceeded, false},
		{"证书错（确定性）", errors.New("x509: certificate signed by unknown authority"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := transientNetErr(c.err); got != c.want {
			t.Errorf("%s: transientNetErr = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// 慢失败不重试：代理不通时每次都要等满 20s 握手超时，重试会把
// 「全部签到」拖过任务上限（26 个账号 × 2 次 × 20s ≈ 18 分钟 > 15 分钟上限）。
func TestSlowFailureIsNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(retryCostCeiling + 200*time.Millisecond) // 慢失败
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
	}))
	defer srv.Close()
	c := NewClientWithProxy("")

	_, _, err := c.doJSON(context.Background(), cnAuth(), http.MethodGet, srv.URL+"/users/status", nil, nil)
	if err == nil {
		t.Fatal("慢失败也必须报错")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("★ 已经等过一次完整超时的失败不许再试（实际打了 %d 次）—— 批量签到会被拖过任务上限", got)
	}
}

// 用户实际看到的那条消息：签到失败的原因必须能指到**代理**上。
//
// 这条走的是真实调用链（Provider → Client → doJSON），而不是只测 wrapNetErr：
// 用户报的原文正是签到历史里那句话，所以判据必须落在同一条路径上。
func TestCheckinStatusErrorNamesProxy(t *testing.T) {
	// 指向一个**必然连不上**的端口（1）：拨号被拒 ⇒ 瞬时错误 ⇒ 重试一次后报错。
	p := New(Config{Proxy: "http://127.0.0.1:1"})
	intl := &Auth{Region: RegionIntl, Session: "s"}

	_, _, err := p.CheckinStatus(context.Background(), intl)
	if err == nil {
		t.Fatal("代理连不上时必须报错（不许静默当成『没签到』）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "国际站出口（代理 http://127.0.0.1:1）") {
		t.Errorf("★ 错误里必须写出走的是哪个代理，实际：%q", msg)
	}
	// ★ 新口径：不许再说"必须走代理"（用户实测零代理可直连国际站），
	//   但仍要给可操作建议 —— 代理这条路上失败时，用户应能想到"要么修代理、要么试直连"。
	if strings.Contains(msg, "必须走代理") {
		t.Errorf("★ 错误里仍有旧前提「必须走代理」（实测可直连），实际：%q", msg)
	}
	if !strings.Contains(msg, "直连") {
		t.Errorf("错误里应提示「直连」这条备选（国际站可直连），实际：%q", msg)
	}
	if !strings.Contains(msg, "monkeycode-ai.net") {
		t.Errorf("错误里应保留失败的主机名，实际：%q", msg)
	}
}

// timeoutErr 一个 net.Error 超时（模拟 net/http 的 TLS handshake timeout）。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "net/http: TLS handshake timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
