package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newRefreshServer 起一个假 token 端点。
//
// 关键行为：**每次成功换发都会把旧 refresh_token 标记为已用** ——
// 这正是真实服务端的语义（STS5.1806 'the refresh token has been used'）。
// 用它才能测出"并发双消费"这个缺陷。
func newRefreshServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	var mu sync.Mutex
	used := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		_ = r.ParseForm()
		rt := r.Form.Get("refresh_token")

		mu.Lock()
		alreadyUsed := used[rt]
		if !alreadyUsed {
			used[rt] = true
		}
		mu.Unlock()

		if alreadyUsed {
			// 真实服务端的拒绝形态
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error_code":"STS5.1806","error_msg":"invalid refresh token: 'the refresh token has been used'"}`)
			return
		}

		// 换发一套新凭证（新 AK + 新 refresh_token）
		resp := map[string]any{
			"credentials": map[string]any{
				"access_key_id":     fmt.Sprintf("AK_NEW_%d", n),
				"secret_access_key": fmt.Sprintf("SK_NEW_%d", n),
				"security_token":    fmt.Sprintf("ST_NEW_%d", n),
				"expiration":        time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
			},
			"refresh_token": fmt.Sprintf("RT_NEW_%d", n),
		}
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	return srv, &calls
}

func newTestAuth(t *testing.T, dir string) *Auth {
	t.Helper()
	doc := credFile{
		Auth: credBody{
			AccessKey: "AK_OLD", SecretKey: "SK_OLD", SecurityToken: "ST_OLD",
			ExpiresAt:    time.Now().Add(30 * time.Minute).Unix(),
			RefreshToken: "RT_OLD",
		},
		Account:  accountBlock{UID: "u1"},
		DPoP:     dpopBlock{PrivateKeyJWK: mustJWK(t)},
		ClientID: "vscode-codebot",
	}
	raw, _ := json.Marshal(doc)
	p := filepath.Join(dir, "codearts-u1.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := ParseCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = p
	return a
}

func mustJWK(t *testing.T) json.RawMessage {
	t.Helper()
	kp, err := NewDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(kp.PrivateJWK())
	return b
}

// TestConcurrentRefreshConsumesTokenOnce 是本模块**最重要**的测试。
//
// 场景：后台调度器与请求路径同时发现凭证将过期，双双调用 RefreshToken。
//
// 若没有 refreshMu：
//   - 两个 goroutine 各拿到 RT_OLD 发出去
//   - 服务端只会让第一个成功，第二个收到 STS5.1806
//   - 且第二个可能把内存里的 RefreshToken 覆盖成已作废的值（取决于时序）
//
// 加锁后：两者串行，第二个进来时第一个已换发成功、内存里是新 token，
// 于是第二个也用**新** token 成功续期（或看到已新鲜而跳过）。
// 断言：绝不出现 "the refresh token has been used" 这类失败。
func TestConcurrentRefreshConsumesTokenOnce(t *testing.T) {
	srv, _ := newRefreshServer(t)
	defer srv.Close()

	dir := t.TempDir()
	a := newTestAuth(t, dir)

	c := New()
	c.STSBase = srv.URL

	const N = 8
	var wg sync.WaitGroup
	errs := make([]error, N)
	start := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量同时起跑，最大化竞争
			errs[i] = c.RefreshToken(a)
		}(i)
	}
	close(start)
	wg.Wait()

	// 任何 "token has been used" 都说明发生了双消费
	for i, err := range errs {
		if err != nil && strings.Contains(err.Error(), "has been used") {
			t.Errorf("goroutine %d 发生双消费: %v", i, err)
		}
	}

	// 最终凭证必须是可用的（有 AK 且没被写坏）
	if a.AccessKey == "" || a.SecretKey == "" {
		t.Fatalf("并发续期后凭证被写坏: ak=%q sk=%q", a.AccessKey, a.SecretKey)
	}
	t.Logf("并发 %d 次续期后 AK=%s（无双消费）", N, a.AccessKey)
}

// TestRefreshSerializedWritesConsistentFile 确认并发下磁盘文件不会半更新。
func TestRefreshSerializedWritesConsistentFile(t *testing.T) {
	srv, _ := newRefreshServer(t)
	defer srv.Close()

	dir := t.TempDir()
	a := newTestAuth(t, dir)
	c := New()
	c.STSBase = srv.URL

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = c.RefreshToken(a)
		}()
	}
	close(start)
	wg.Wait()

	// 磁盘文件必须始终是合法凭证
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("并发续期后磁盘文件损坏: %v", err)
	}
	if back.AccessKey == "" || back.SecurityToken == "" {
		t.Errorf("磁盘凭证不完整: %+v", back)
	}
	// .tmp 不应残留
	if _, err := os.Stat(a.FilePath + ".tmp"); err == nil {
		t.Error("残留 .tmp 文件，说明写盘未收尾")
	}
}

// TestSchedulerRefreshesExpiringAccount 验证调度器会主动续期临近过期的账号。
func TestSchedulerRefreshesExpiringAccount(t *testing.T) {
	srv, calls := newRefreshServer(t)
	defer srv.Close()

	dir := t.TempDir()
	a := newTestAuth(t, dir)
	// 让它"4 分钟后过期" —— 落在默认 5 分钟窗口内
	a.ExpiresAt = time.Now().Add(4 * time.Minute).Unix()

	// 用真实 Backend，但指向假 server
	b := &Backend{Client: New(), byUID: map[string]*Auth{a.UID: a}, byPtr: map[*auth.Auth]*Auth{}}
	b.Client.STSBase = srv.URL

	// 把投影对象放进 byPtr，让 syncByUID 有东西可更新
	proj := b.project(a)

	sch := NewRefreshScheduler(b)
	sch.tick() // 直接跑一次，不等 ticker

	if got := atomic.LoadInt32(calls); got == 0 {
		t.Fatal("调度器未发起续期")
	}
	if a.AccessKey == "AK_OLD" {
		t.Error("续期后 AK 未更新")
	}
	// 投影对象也应同步（否则管理台显示旧过期时间）
	if proj.ExpiresAt != a.ExpiresAt {
		t.Errorf("投影对象过期时间未同步: proj=%d codearts=%d", proj.ExpiresAt, a.ExpiresAt)
	}
}

// TestSchedulerSkipsFreshAccount 确认没过期的账号不会被无谓续期。
//
// 这很重要：refresh_token 一次性，无谓续期等于白白消耗它。
func TestSchedulerSkipsFreshAccount(t *testing.T) {
	srv, calls := newRefreshServer(t)
	defer srv.Close()

	dir := t.TempDir()
	a := newTestAuth(t, dir)
	a.ExpiresAt = time.Now().Add(25 * time.Minute).Unix() // 远未到期

	b := &Backend{Client: New(), byUID: map[string]*Auth{a.UID: a}, byPtr: map[*auth.Auth]*Auth{}}
	b.Client.STSBase = srv.URL
	b.project(a)

	sch := NewRefreshScheduler(b)
	sch.tick()

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("不应续期新鲜凭证，实际调用了 %d 次", got)
	}
}

// TestSchedulerStopsOnContextCancel 确认 ctx 取消后 goroutine 正常退出。
func TestSchedulerStopsOnContextCancel(t *testing.T) {
	srv, _ := newRefreshServer(t)
	defer srv.Close()

	dir := t.TempDir()
	a := newTestAuth(t, dir)
	b := &Backend{Client: New(), byUID: map[string]*Auth{a.UID: a}, byPtr: map[*auth.Auth]*Auth{}}
	b.Client.STSBase = srv.URL

	sch := NewRefreshScheduler(b)
	sch.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sch.Run(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// 正常退出
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后调度器未退出（goroutine 泄漏）")
	}
}

// TestSchedulerFailureDoesNotStopOthers 确认单账号失败不影响其它账号。
func TestSchedulerFailureDoesNotStopOthers(t *testing.T) {
	var mu sync.Mutex
	okCalled := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rt := r.Form.Get("refresh_token")
		if strings.HasPrefix(rt, "BAD") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error_code":"STS5.1806","error_msg":"invalid refresh token"}`)
			return
		}
		mu.Lock()
		okCalled++
		mu.Unlock()
		b, _ := json.Marshal(map[string]any{
			"credentials": map[string]any{
				"access_key_id": "AK_OK", "secret_access_key": "SK", "security_token": "ST",
				"expiration": time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
			},
			"refresh_token": "RT_OK2",
		})
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	dir := t.TempDir()

	bad := newTestAuth(t, dir)
	bad.UID = "bad"
	bad.RefreshToken = "BAD_RT"
	bad.ExpiresAt = time.Now().Add(time.Minute).Unix()

	good := newTestAuth(t, dir)
	good.UID = "good"
	good.AccessKey = "AK_G"
	good.RefreshToken = "GOOD_RT"
	good.ExpiresAt = time.Now().Add(time.Minute).Unix()

	b := &Backend{Client: New(), byUID: map[string]*Auth{"bad": bad, "good": good}, byPtr: map[*auth.Auth]*Auth{}}
	b.Client.STSBase = srv.URL

	sch := NewRefreshScheduler(b)
	sch.tick()

	mu.Lock()
	defer mu.Unlock()
	if okCalled == 0 {
		t.Error("坏账号失败后，好账号也没被续期 —— 失败污染了整轮扫描")
	}
}
