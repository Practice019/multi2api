package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

type boundedChannelBody struct {
	reader io.Reader
	chunk  int
	read   int
	closed atomic.Int32
}

func (b *boundedChannelBody) Read(p []byte) (int, error) {
	if b.chunk > 0 && len(p) > b.chunk {
		p = p[:b.chunk]
	}
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *boundedChannelBody) Close() error {
	b.closed.Add(1)
	return nil
}

func TestChannelPendingBound(t *testing.T) {
	for _, scrub := range []bool{false, true} {
		for _, prefix := range []string{"", "data: ", "{\"content\":\""} {
			for _, chunk := range []int{4093, 16384} {
				body := &boundedChannelBody{reader: io.MultiReader(strings.NewReader(prefix), io.LimitReader(zeroChannelReader{}, maxChannelPendingBytes*2)), chunk: chunk}
				w := httptest.NewRecorder()
				h := &Handler{}
				h.writeChannelStream(w, httptest.NewRequest("POST", "/", nil), channelForward{Stream: gateway.ChatStream{Status: http.StatusAccepted, Body: body}}, newStreamUsage(time.Now()), scrub)
				if body.closed.Load() != 1 || body.read != maxChannelPendingBytes+1 {
					t.Fatalf("scrub=%v prefix=%q chunk=%d: closed=%d read=%d", scrub, prefix, chunk, body.closed.Load(), body.read)
				}
				if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
					t.Fatalf("unexpected status/body: %d/%d", w.Code, w.Body.Len())
				}
			}
		}
	}
}

type zeroChannelReader struct{}

func (zeroChannelReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestChannelPendingBoundaryAndCompatibility(t *testing.T) {
	for _, size := range []int{maxChannelPendingBytes - 1, maxChannelPendingBytes} {
		input := "data: " + strings.Repeat("x", size-len("data: \n")) + "\n"
		body := &boundedChannelBody{reader: strings.NewReader(input)}
		w := httptest.NewRecorder()
		(&Handler{}).writeChannelStream(w, httptest.NewRequest("POST", "/", nil), channelForward{Stream: gateway.ChatStream{Body: body}}, nil, false)
		if w.Body.String() != input || body.closed.Load() != 1 {
			t.Fatal("boundary stream changed")
		}
	}
	for _, scrub := range []bool{false, true} {
		input := ": heartbeat\r\n\r\n" + sseOK
		body := &boundedChannelBody{reader: strings.NewReader(input), chunk: 1}
		w := httptest.NewRecorder()
		usage := newStreamUsage(time.Now())
		(&Handler{}).writeChannelStream(w, httptest.NewRequest("POST", "/", nil), channelForward{Stream: gateway.ChatStream{Body: body}, PublicModel: "public"}, usage, scrub)
		if !strings.Contains(w.Body.String(), "你好") || !strings.Contains(w.Body.String(), "data: [DONE]") {
			t.Fatal("fragmented SSE lost content")
		}
		if !scrub && w.Body.String() != input {
			t.Fatal("passthrough changed")
		}
		if scrub && strings.Contains(w.Body.String(), "glm-5.2") {
			t.Fatal("model not scrubbed")
		}
		if tokens, _, source := usage.Result(); tokens != 2 || source != usageFromLastFrame {
			t.Fatalf("usage=%d/%s", tokens, source)
		}
	}
	input := strings.Repeat(": heartbeat\n\n", maxChannelPendingBytes/10)
	body := &boundedChannelBody{reader: strings.NewReader(input), chunk: 17}
	w := httptest.NewRecorder()
	(&Handler{}).writeChannelStream(w, httptest.NewRequest("POST", "/", nil), channelForward{Stream: gateway.ChatStream{Body: body}}, nil, true)
	if w.Body.String() != input {
		t.Fatal("heartbeat stream truncated")
	}
}

func TestChannelPendingOverflowAfterOutput(t *testing.T) {
	prefix := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"
	body := &boundedChannelBody{reader: io.MultiReader(strings.NewReader(prefix), io.LimitReader(zeroChannelReader{}, 2*maxChannelPendingBytes)), chunk: len(prefix)}
	w := httptest.NewRecorder()
	(&Handler{}).writeChannelStream(w, httptest.NewRequest("POST", "/", nil), channelForward{Stream: gateway.ChatStream{Status: http.StatusAccepted, Body: body}}, nil, false)
	if w.Code != http.StatusAccepted || w.Body.String() != prefix || body.closed.Load() != 1 {
		t.Fatal("overflow changed committed response or did not close body")
	}
}

type failedChannelWriter struct{ http.ResponseWriter }

func (failedChannelWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestChannelDownstreamFailureClosesBody(t *testing.T) {
	body := &boundedChannelBody{reader: strings.NewReader(sseOK), chunk: 64}
	w := failedChannelWriter{httptest.NewRecorder()}
	(&Handler{}).writeChannelStream(w, httptest.NewRequest("POST", "/", nil), channelForward{Stream: gateway.ChatStream{Body: body}}, nil, false)
	if body.closed.Load() != 1 || body.read >= len(sseOK) {
		t.Fatal("downstream failure did not stop upstream reading")
	}
}

type blockingChannelBody struct {
	started chan struct{}
	closed  chan struct{}
	closes  atomic.Int32
}

func (b *blockingChannelBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingChannelBody) Close() error {
	if b.closes.Add(1) == 1 {
		close(b.closed)
	}
	return nil
}

func TestChannelStreamCancellationClosesBlockedBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &blockingChannelBody{started: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Handler{}).writeChannelStream(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil).WithContext(ctx), channelForward{Stream: gateway.ChatStream{Body: body}}, nil, true)
	}()
	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock read")
	}
	if body.closes.Load() != 1 {
		t.Fatal("body not closed exactly once")
	}
}

type cancelChannelProvider struct {
	okProvider
	chat func(context.Context) (gateway.ChatStream, error)
}

func (p *cancelChannelProvider) Chat(ctx context.Context, _ gateway.Credential, _ []byte) (gateway.ChatStream, error) {
	return p.chat(ctx)
}

func TestChannelCancellationStopsAttempts(t *testing.T) {
	for _, mode := range []string{"before", "factory", "error", "success"} {
		t.Run(mode, func(t *testing.T) {
			s := timeoutSwitchStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, factories := 0, 0
			body := &boundedChannelBody{reader: strings.NewReader("data: [DONE]\n\n")}
			h := NewHandler(Config{Channels: s, ChannelProvider: func(id, _ string) gateway.Provider {
				factories++
				if mode == "factory" {
					cancel()
				}
				return &cancelChannelProvider{okProvider: okProvider{id: id}, chat: func(got context.Context) (gateway.ChatStream, error) {
					calls++
					if got != ctx {
						t.Fatal("request context not propagated")
					}
					cancel()
					if mode == "error" {
						return gateway.ChatStream{Body: body}, errors.New("stub failure")
					}
					return gateway.ChatStream{Status: 200, Body: body}, nil
				}}
			}})
			if mode == "before" {
				cancel()
			}
			_, err := h.forwardViaChannel(ctx, "pub-z", []byte(`{"model":"pub-z"}`))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v", err)
			}
			wantCalls := 1
			if mode == "before" || mode == "factory" {
				wantCalls = 0
			}
			if calls != wantCalls || factories > 1 {
				t.Fatalf("calls=%d factories=%d", calls, factories)
			}
			if body.closed.Load() != int32(wantCalls) {
				t.Fatalf("closes=%d", body.closed.Load())
			}
		})
	}
}
