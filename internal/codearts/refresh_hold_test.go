// refresh_hold_test.go —— 后台续期失败退避（用户报的 bug：死 token 每分钟重试）。
//
// # bug 形态（实测）
//
// codearts 的 refresh_token 是**消费型**的：一旦被服务端消费
// （STS5.1806 'the refresh token has been used'），之后每次刷新都注定失败。
// 修复前 runRefresh 失败只 log 不挂退避 → 每 60s 扫描都命中 NeedsRefresh
// （token 一直没更新 → 一直临近过期）→ 每分钟一次失败请求，永不停歇。
//
// 修复：失败挂退避（永久错误 24h / 抖动 10min），凭证文件被外部更新
// （重新登录写盘）时提前解除。
package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunRefreshNotifiesOutcomeHook 失败/成功会通知装配层 hook
// （pool.NoteRefreshFailure 的接线点，借鉴 one-api/LiteLLM 的渠道禁用可见性）。
func TestRunRefreshNotifiesOutcomeHook(t *testing.T) {
	var failCalls, okCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"STS5.1806","error_msg":"invalid refresh token: 'the refresh token has been used'"}`))
	}))
	defer srv.Close()

	kp, err := NewDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	jwkRaw, err := json.Marshal(kp.PrivateJWK())
	if err != nil {
		t.Fatal(err)
	}

	c := New()
	c.STSBase = srv.URL
	p := NewWithConfig(Config{
		Client:           c,
		OnRefreshFailure: func(uid string) { failCalls++ },
		OnRefreshSuccess: func(uid string) { okCalls++ },
	})
	p.accounts = func() []*Auth {
		return []*Auth{{
			UID:               "u1",
			RefreshToken:      "rt",
			DPoPPrivateKeyJWK: jwkRaw,
			ExpiresAt:         time.Now().Add(1 * time.Minute).Unix(),
		}}
	}

	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh: %v", err)
	}
	if failCalls != 1 {
		t.Errorf("失败应通知 hook 1 次，实际 %d", failCalls)
	}
	if okCalls != 0 {
		t.Errorf("失败轮不应通知成功 hook，实际 %d", okCalls)
	}
}

// TestIsPermanentRefreshError 永久性凭证错误必须被识别（决定退避时长）。
func TestIsPermanentRefreshError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("refresh_failed: http 400: {\"error_code\":\"STS5.1806\",\"error_msg\":\"invalid refresh token: 'the refresh token has been used'\"}"), true},
		{fmt.Errorf("refresh_failed: 无 refresh_token（需重新登录）"), true},
		{fmt.Errorf("refresh_failed: 缺 DPoP 私钥（无法签名，需重新登录）"), true},
		{fmt.Errorf("refresh 成功但写回失败（旧 refresh_token 已被服务端消费，需重新登录）: boom"), true},
		{fmt.Errorf("refresh_failed: 请求领取: EOF"), false},          // 网络抖动 → 短退避
		{fmt.Errorf("refresh_failed: http 500: internal"), false}, // 5xx → 短退避
		{nil, false},
	}
	for _, c := range cases {
		if got := isPermanentRefreshError(c.err); got != c.want {
			t.Errorf("isPermanentRefreshError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestRefreshHoldSkipsAccountUntilFileUpdate 退避期间不重试；凭证文件更新后恢复。
func TestRefreshHoldSkipsAccountUntilFileUpdate(t *testing.T) {
	p := NewWithConfig(Config{})
	dir := t.TempDir()
	path := filepath.Join(dir, "codearts-u1.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Auth{UID: "u1", RefreshToken: "rt", FilePath: path}

	// 永久错误 → 24h 退避
	p.holdRefresh(a, fmt.Errorf("refresh_failed: http 400: STS5.1806 has been used"))
	if !p.onRefreshHold(a) {
		t.Fatal("永久错误后应立即进入退避")
	}

	// 抖动错误 → 短退避（文件 mtime 未变，仍早于 holdAt → 不提前解除）
	p.holdRefresh(a, fmt.Errorf("refresh_failed: EOF"))
	if !p.onRefreshHold(a) {
		t.Fatal("抖动错误也应挂退避")
	}
	// 直接改内部表把到期时间拨到过去，模拟退避窗口过期
	p.refreshHoldMu.Lock()
	e := p.refreshHold["u1"]
	e.until = time.Now().Add(-time.Minute)
	p.refreshHold["u1"] = e
	p.refreshHoldMu.Unlock()
	if p.onRefreshHold(a) {
		t.Fatal("退避窗口过期后应解除")
	}
	if _, ok := p.refreshHold["u1"]; ok {
		t.Fatal("解除后不应残留表项")
	}

	// 凭证文件在进入退避**之后**被更新（重新登录写盘）→ 提前解除
	p.holdRefresh(a, fmt.Errorf("refresh_failed: http 400: STS5.1806 has been used"))
	if !p.onRefreshHold(a) {
		t.Fatal("重新挂退避后应立即进入退避")
	}
	if err := os.Chtimes(path, time.Now().Add(2*time.Hour), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if p.onRefreshHold(a) {
		t.Fatal("凭证文件更新后应解除退避（用户已重新登录）")
	}
}

// TestRunRefreshStopsAfterPermanentFailure 死 token 账号第一轮失败后，
// 第二轮 runRefresh 不再打上游（修复的回归点）。
func TestRunRefreshStopsAfterPermanentFailure(t *testing.T) {
	// 假上游：token 端点恒回 STS5.1806（token 已被消费）
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/oauth2/tokens") {
			http.NotFound(w, r)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"STS5.1806","error_msg":"invalid refresh token: 'the refresh token has been used'"}`))
	}))
	defer srv.Close()

	// 有效 DPoP 私钥：RefreshToken 会在发请求前用它对请求签名（没有它请求根本发不出去）
	kp, err := NewDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	jwkRaw, err := json.Marshal(kp.PrivateJWK())
	if err != nil {
		t.Fatal(err)
	}

	c := New()
	c.STSBase = srv.URL // refreshAttempt 用 stsBase() + TokenPath，必须指到假上游
	p := NewWithConfig(Config{Client: c})
	// 让 runRefresh 通过 localAccounts 看到这个账号
	p.accounts = func() []*Auth {
		return []*Auth{{
			UID:               "u1",
			RefreshToken:      "rt",
			DPoPPrivateKeyJWK: jwkRaw,
			ExpiresAt:         time.Now().Add(1 * time.Minute).Unix(),
		}}
	}

	// 第一轮：临近过期 → 尝试刷新 → 失败（STS5.1806）→ 挂 24h 退避
	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 第一轮: %v", err)
	}
	if hits != 1 {
		t.Fatalf("第一轮应恰好 1 次请求，实际 %d", hits)
	}
	// 第二轮：退避中 → 不尝试 → 无请求
	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 第二轮: %v", err)
	}
	if hits != 1 {
		t.Fatalf("退避期间不应再打上游，实际请求 %d 次（bug 回归：死 token 每分钟重试）", hits)
	}
}
