package main

import (
	"context"
	"errors"
	"net"
	"net/http"
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
