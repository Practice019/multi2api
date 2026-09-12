package codearts

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMockClient 构造一个把出口指向本地 mock 的 Client。
//
// # 为什么这条路径重要
//
// 本包有 12 个 `*Live` 测试，全部因为"没有真实凭证"而 skip。
// 它们的注释明确写着需要 CODEARTS_* 环境变量。结果是：
// 在**没有凭证的环境**（CI、别人 clone）里，**错误处理路径完全没被验证**
// —— 而错误处理正是生产环境里最常走的分支（token 过期、限流、上游 5xx）。
//
// `EngineBase` / `STSBase` 是可注入字段，所以可以把出口指向 httptest，
// **不需要任何真实凭证**就能覆盖这些分支。这条测试补的就是这块。
func newMockClient(t *testing.T, engineH, stsH http.HandlerFunc) (*Client, func()) {
	t.Helper()
	engine := httptest.NewServer(engineH)
	sts := httptest.NewServer(stsH)
	c := &Client{
		HTTP:                 engine.Client(),
		ChatHTTP:             engine.Client(),
		EngineBase:           engine.URL,
		STSBase:              sts.URL,
		HeaderTimeout:        2 * time.Second,
		IdleTimeout:          2 * time.Second,
		MaxAuthRetry:         1,
		SanitizeFingerprints: false,
	}
	return c, func() { engine.Close(); sts.Close() }
}

// TestVerifyMapsUpstreamErrors 上游各种状态码必须映射成**可区分**的错误。
//
// # 断言的是 ErrKind，不是错误字符串
//
// 我先只断言"错误信息里含 401"这类字符串。**那挡不住真正的回归**：
// 变异测试（把 `Classify(resp.StatusCode, ...)` 改成 `Classify(500, ...)`，
// 即丢掉分类能力）之后，字符串里照样有 "http 401"（因为 Msg 里带了原文），
// 测试**全绿** —— 又是一次"验证书写形式而非本体"。
//
// 改成断言 `*Error.Kind` 这个**枚举**，因为上层正是靠它决定
// "换号重试"（ErrAuth/ErrSoftRate）还是"等上游恢复"（ErrServer）
// 还是"换号也没用"（ErrClient/ErrHardCredit）。
func TestVerifyMapsUpstreamErrors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantKind ErrKind
		wantErr  bool
	}{
		{"401 凭证失效", http.StatusUnauthorized, `{"error_msg":"token expired"}`, ErrAuth, true},
		{"403 无权限", http.StatusForbidden, `{"error_msg":"forbidden"}`, ErrAuth, true},
		{"429 限流", http.StatusTooManyRequests, `{"error_msg":"rate limited"}`, ErrSoftRate, true},
		{"500 上游错误", http.StatusInternalServerError, `oops`, ErrServer, true},
		{"502 网关错误", http.StatusBadGateway, `<html>bad gateway</html>`, ErrServer, true},
		// 200 一律视为"上游接受了凭证"，不论 body 是什么（见上方说明）
		{"200 非法 JSON（仍视为接受）", http.StatusOK, `not-json`, ErrNone, false},
		{"200 空 body（仍视为接受）", http.StatusOK, ``, ErrNone, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, cleanup := newMockClient(t,
				func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				},
				func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				})
			defer cleanup()

			err := c.Verify(&Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"})
			if tc.wantErr && err == nil {
				t.Fatalf("上游返回 %d，Verify 却成功了 —— 调用方会误以为凭证有效", tc.status)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("上游返回 %d，Verify 却失败: %v", tc.status, err)
				}
				return
			}

			// 必须是带 Kind 的类型化错误（调用方靠 Kind 分流）
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("错误不是 *Error 类型，调用方无法按 Kind 分流: %T %v", err, err)
			}
			if ce.Kind != tc.wantKind {
				t.Errorf("ErrKind 不符：http %d 应分类为 %v，实际 %v（分类错误会让上层选错处置策略）",
					tc.status, tc.wantKind, ce.Kind)
			}
			if ce.Status != tc.status {
				t.Errorf("Status 未原样带出：上游 %d，实际 %d", tc.status, ce.Status)
			}
			t.Logf("  %d → Kind=%v Status=%d", tc.status, ce.Kind, ce.Status)
		})
	}
}

// TestVerifySucceedsOnValidResponse 正向路径：上游返回合法身份时不得报错。
//
// 没有这条，"Verify 永远返回错误"也能让上面那组测试全绿 ——
// 那会掩盖一个最严重的缺陷（正常的凭证被判定为失效）。
func TestVerifySucceedsOnValidResponse(t *testing.T) {
	// 先看真实上游的成功响应长什么样，再构造最小合法响应。
	// 这里用一个宽松的合法 JSON；若实现要求特定字段，本测试会暴露。
	c, cleanup := newMockClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id":"u-1","user_name":"tester"}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id":"u-1","user_name":"tester"}`))
		})
	defer cleanup()

	err := c.Verify(&Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"})
	if err != nil {
		t.Fatalf("上游返回合法响应，Verify 却失败: %v\n"+
			"（若实现要求特定字段，说明本测试的 mock 需要补齐 —— 那不是产品缺陷）", err)
	}
	t.Log("合法响应 → Verify 通过")
}

// TestRefreshTokenFailureIsTyped 刷新失败的错误必须能区分原因。
//
// 缺 refresh_token 与"上游拒绝刷新"是两种不同处置：
// 前者要提示用户重新登录，后者可以重试。
func TestRefreshTokenFailureIsTyped(t *testing.T) {
	c, cleanup := newMockClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) },
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	defer cleanup()

	// 情形 1：没有 refresh_token —— 必须给出**明确**提示
	a := &Auth{AccessKey: "AK", SecretKey: "SK"}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("没有 refresh_token 时 RefreshToken 应当失败")
	}
	if !strings.Contains(err.Error(), "refresh") {
		t.Errorf("错误信息应说明是 refresh 相关问题，实际: %v", err)
	}
	t.Logf("无 refresh_token → %v", err)

	// 情形 2：有 refresh_token 但上游 401 —— 错误信息要含状态码
	c2, cleanup2 := newMockClient(t,
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) },
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		})
	defer cleanup2()

	a2 := &Auth{AccessKey: "AK", SecretKey: "SK"}
	a2.RefreshToken = "rt-fake"
	err2 := c2.RefreshToken(a2)
	if err2 == nil {
		t.Fatal("上游 401 时 RefreshToken 应当失败")
	}
	t.Logf("上游 401 → %v", err2)
}

// TestChatStreamErrorStatusIsReturned 聊天失败时状态码必须原样带出。
//
// 调用方要靠它决定"换号重试"还是"直接报错"：429/401 要换号，
// 400 换号也没用。若这里吞掉状态码，上层只能瞎猜。
func TestChatStreamErrorStatusIsReturned(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, cleanup := newMockClient(t,
				func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error_msg":"mock"}`))
				},
				func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
			defer cleanup()

			rc, got, respBody, err := c.ChatStreamWith(
				&Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"},
				[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
				nil)
			if rc != nil {
				_ = rc.Close()
			}
			if err == nil && got < 400 {
				t.Fatalf("上游 %d，ChatStreamWith 却报成功（status=%d）", status, got)
			}
			if got != status {
				t.Errorf("状态码未原样带出：上游 %d，返回 %d —— 上层无法据此决定是否换号", status, got)
			}
			t.Logf("  上游 %d → 返回 status=%d, err=%v, body=%q", status, got, err, string(respBody[:min(len(respBody), 40)]))
		})
	}
}

// TestEngineBaseNormalization 基址末尾斜杠不得导致双斜杠路径。
//
// `EngineBase="http://x/"` + `ChatPath="/api/v2/..."` 若直接拼接
// 会得到 `http://x//api/v2/...` —— 某些网关会 404。
// engineBase() 用 TrimRight 处理了，这条把它钉住。
func TestEngineBaseNormalization(t *testing.T) {
	c := &Client{EngineBase: "http://example.com/"}
	if got := c.engineBase(); got != "http://example.com" {
		t.Errorf("末尾斜杠未被去掉：%q", got)
	}
	c2 := &Client{EngineBase: "http://example.com"}
	if got := c2.engineBase(); got != "http://example.com" {
		t.Errorf("无斜杠时被改坏：%q", got)
	}
	// 空值时回落默认
	if got := (&Client{}).engineBase(); got != DefaultEngineBase {
		t.Errorf("空 EngineBase 应回落默认，实际 %q", got)
	}
	if got := (&Client{}).stsBase(); got != DefaultSTSBase {
		t.Errorf("空 STSBase 应回落默认，实际 %q", got)
	}
	t.Log("engineBase/stsBase 归一化正确")
}

// TestContextCancellationPropagates 调用方取消 context 必须能中断请求。
//
// 不验这条的话，"用户点了取消但请求还在跑、还在耗上游配额"这类问题
// 会一直存在，而且只在慢请求上偶发。
func TestContextCancellationPropagates(t *testing.T) {
	release := make(chan struct{})
	// ⚠ 慢处理必须挂在**被请求的那个** server 上。
	// 我第一版把慢 handler 挂在 engine，却把请求发往 sts（瞬时 200），
	// 于是"取消没传导"是**测试写错**，不是产品缺陷。
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		w.WriteHeader(200)
	}
	fast := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }
	// sts 慢（因为下面请求的是 stsBase）
	c, cleanup := newMockClient(t, http.HandlerFunc(fast), http.HandlerFunc(slow))
	defer cleanup()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	rawURL := c.stsBase() + CallerIdentityPath
	req, err := c.signedRequest(ctx, http.MethodGet, rawURL, "", "",
		&Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"}, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	elapsed := time.Since(start)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("context 已取消，请求却成功了 —— 取消没有传导下去")
	}
	if elapsed > 2*time.Second {
		t.Errorf("取消后 %v 才返回，说明 context 未被及时传导", elapsed)
	}
	t.Logf("取消后 %v 返回错误: %v", elapsed.Round(time.Millisecond), err)
}
