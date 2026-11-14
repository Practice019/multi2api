package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

func serveWithCleanup(ctx context.Context, srv *http.Server, grace time.Duration, cleanup func()) error {
	return serveHTTPWithCleanup(ctx, srv, grace, cleanup, srv.ListenAndServe)
}

func serveHTTPWithCleanup(ctx context.Context, srv *http.Server, grace time.Duration, cleanup func(), serve func() error) error {
	var mu sync.Mutex
	var active sync.WaitGroup
	stopping := false
	handler := srv.Handler
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if stopping {
			mu.Unlock()
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		active.Add(1)
		mu.Unlock()
		defer active.Done()
		handler.ServeHTTP(w, r)
	})
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
