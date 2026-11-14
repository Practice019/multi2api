package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReliabilityServeFailureWaitsCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	failure := errors.New("stub listen failure")
	done := make(chan error, 1)
	srv := &http.Server{Handler: http.NotFoundHandler()}
	go func() {
		done <- serveHTTPWithCleanup(context.Background(), srv, time.Second, func() { close(entered); <-release }, func() error { return failure })
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup not started")
	}
	select {
	case <-done:
		t.Fatal("returned before cleanup")
	default:
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup not joined")
	}
}

func TestReliabilityShutdownTimeoutWaitsHandler(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	cleaned := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
	})}
	done := make(chan error, 1)
	go func() {
		done <- serveHTTPWithCleanup(ctx, srv, 20*time.Millisecond, func() { close(cleaned) }, func() error { return srv.Serve(ln) })
	}()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, e := client.Get("http://" + ln.Addr().String())
		if e == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not started")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("forced close did not cancel request")
	}
	select {
	case <-cleaned:
		t.Fatal("cleanup raced handler")
	default:
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked")
	}
	<-requestDone
}

// TestDrainExemptsProbesButCutsBusiness 排空期：探针放行、业务端点切断。
//
// # 为什么这条判据要直接测 drainGate 而不是"取消 ctx 之后抢时间窗"
//
// 真实关停的时间窗极短（`stopping=true` 到 `srv.Shutdown()` 完成之间只有毫秒级），
// 靠 cancel 之后立刻发请求去抓那个窗口会时绿时红 —— 那种判据等于没有。
// 所以把判定逻辑抽成 drainGate（见 lifecycle.go），这里直接驱动它。
//
// # 两侧都必须钉住
//
//	放行侧：/livez、/readyz 在排空期**必须继续可答**
//	         · /livez 若被切断 ⇒ 编排系统判容器不健康 ⇒ 在排空途中杀掉它，
//	           正好打断我们想好好做完的那些在途请求；
//	         · /readyz 若被切断（被这里的纯文本 503 抢先拦掉）⇒ 调用方看不到
//	           draining / reason 字段，分不清"排空"还是"账号耗尽"。
//	切断侧：业务端点必须 503。排空的定义就是"不再接新活"。
//
// ⚠ /healthz 刻意**不在**豁免名单里：它的语义是"能受理对话"（看账号池），
// 排空期正确答案就是 503。新的存活探针是 /livez —— 那正是本轮 P0-2 拆分的意义。
func TestDrainExemptsProbesButCutsBusiness(t *testing.T) {
	var mu sync.Mutex
	var active sync.WaitGroup
	stopping := false
	var served []string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = append(served, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("inner"))
	})
	gate := drainGate(inner, &mu, &stopping, &active)

	hit := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}

	// ① 未排空：一切照常进到内层
	for _, p := range []string{"/livez", "/readyz", "/healthz", "/v1/chat/completions", "/admin/accounts"} {
		code, body := hit(p)
		if code != http.StatusOK || body != "inner" {
			t.Fatalf("未排空时 %s = %d/%q，want 200/inner", p, code, body)
		}
	}

	// ② 进入排空
	mu.Lock()
	stopping = true
	mu.Unlock()
	// 清掉第 ① 步（未排空时合法进入内层）的记录 —— 后面的「不该进入内层」
	// 只看排空之后发生的事，否则会把合法记录当成违规。
	served = nil

	// ③ 探针必须继续可答（具体状态码由各自的处理器决定）
	for _, p := range []string{"/livez", "/readyz"} {
		code, body := hit(p)
		if code != http.StatusOK || body != "inner" {
			t.Errorf("排空期 %s = %d/%q —— 探针**必须**放行到处理器：\n"+
				"  · livez 被切断 ⇒ 编排系统在排空途中杀掉容器，打断在途请求\n"+
				"  · readyz 被切断 ⇒ 调用方看不到 draining/reason，分不清排空与账号耗尽", p, code, body)
		}
	}

	// ④ 业务端点必须被切断
	for _, p := range []string{
		"/v1/chat/completions", "/v1/models", "/v1/messages",
		"/admin/accounts", "/admin/logs", "/ui", "/status",
		"/healthz", // 刻意不豁免：语义是"能受理对话"，排空期答案就是 503
	} {
		code, body := hit(p)
		if code != http.StatusServiceUnavailable {
			t.Errorf("排空期 %s = %d，want 503（不再接新活）", p, code)
		}
		if !strings.Contains(body, "shutting down") {
			t.Errorf("排空期 %s body=%q，want 含 shutting down", p, body)
		}
	}

	// ⑤ 切断的请求不该进入内层（否则"切断"只是回了个 503 但活还在干）
	for _, sv := range served {
		if sv == "/v1/chat/completions" || sv == "/healthz" {
			t.Errorf("排空期 %s 仍进入了内层处理器 —— 切断必须发生在进入之前", sv)
		}
	}
}
