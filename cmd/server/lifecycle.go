package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"workbuddy2api/internal/server"
)

// drainExemptPaths 是排空期间**仍然放行**的路径。
//
// ⚠ 为什么必须豁免，以及为什么恰好是这两条：
//
//	/livez   存活探针问的是"进程还活着吗"。排空期间进程**本来就活着** ——
//	         它正在把在途请求做完。此时若回 503，编排系统会判容器不健康并把它杀掉，
//	         正好在我们想好好收尾的那一刻把连接全掐了。**存活探针在排空期必须 200。**
//	/readyz  就绪探针问的是"能接新流量吗"，排空期的正确答案**就是 503**；
//	         但它必须由 readyz 处理器自己用 JSON 形状回答（带 draining / reason 字段），
//	         而不是被下面那段纯文本 503 抢先拦掉 —— 否则调用方只能看到"服务正在关闭"，
//	         看不出是排空还是账号耗尽。
//
// 业务端点**不在**豁免里：排空期一律 503，那才是"不再接新活"。
//
// 判据：TestDrainExemptsProbesButCutsBusiness（cmd/server）与
// TestProbesSplitLivenessFromReadiness（internal/server）。
var drainExemptPaths = map[string]bool{
	"/livez":  true,
	"/readyz": true,
}

func serveWithCleanup(ctx context.Context, srv *http.Server, grace time.Duration, cleanup func()) error {
	return serveHTTPWithCleanup(ctx, srv, grace, cleanup, srv.ListenAndServe)
}

// drainGate 把"排空期间切断业务流量、但放行探针"这件事包成一个 http.Handler。
//
// 抽成独立函数而不是内联进 serveHTTPWithCleanup，是为了**可测**：内联的话只能靠
// "cancel 之后抢那个时间窗"去验证，而窗口极短 —— 那种用例会时绿时红，等于没有。
// 判据：TestDrainExemptsProbesButCutsBusiness。
//
// ⚠ mu 同时保护 stopping 与 active.Add(1)：关停侧的 `stopping = true` 与
// `active.Wait()` 之间**不能**插进新的 Add，否则 Wait 可能漏掉一个刚登记的请求，
// 于是 cleanup() 会在仍有在途请求时执行。
func drainGate(handler http.Handler, mu *sync.Mutex, stopping *bool, active *sync.WaitGroup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if *stopping && !drainExemptPaths[r.URL.Path] {
			mu.Unlock()
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		active.Add(1)
		mu.Unlock()
		defer active.Done()
		handler.ServeHTTP(w, r)
	})
}

func serveHTTPWithCleanup(ctx context.Context, srv *http.Server, grace time.Duration, cleanup func(), serve func() error) error {
	var mu sync.Mutex
	var active sync.WaitGroup
	stopping := false
	handler := drainGate(srv.Handler, &mu, &stopping, &active)
	srv.Handler = handler
	serveDone := make(chan struct{})
	cleanupDone := make(chan struct{})
	var shutdownErr error
	go func() {
		defer close(cleanupDone)
		select {
		case <-ctx.Done():
		case <-serveDone:
		}
		mu.Lock()
		stopping = true
		mu.Unlock()
		// ★ 让 /readyz 在 srv.Shutdown **之前**就先转 503。
		//
		// 顺序很关键：编排系统必须先看到"我不ready了"从而**摘掉流量**，再等我们把
		// 在途请求做完。反过来（先关连接、再摘流量）会让关闭期间新到的请求
		// 打到已经不再接受连接的监听上 —— 调用方拿到的是连接错误，而不是一次
		// 干净的 503 重试信号。
		server.MarkDraining()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		shutdownErr = srv.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			log.Printf("http: 优雅关停失败，强制关闭连接并等待处理器退出: %v", shutdownErr)
			shutdownErr = errors.Join(shutdownErr, srv.Close())
		}
		active.Wait()
		cleanup()
	}()
	err := serve()
	close(serveDone)
	<-cleanupDone
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, shutdownErr)
}
