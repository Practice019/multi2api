package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

func TestRoutingV2MixedChannelsAndIntersection(t *testing.T) {
	cs := entryStore(t, "https://example.invalid/v1", 1)
	standard := &recordingProvider{id: "standard"}
	builtin := &recordingProvider{id: "fake"}
	if err := cs.Upsert(&channelstore.Channel{ID: "ch-z", Name: "builtin", Kind: "builtin", ProviderID: "fake", Status: 1, Models: []string{"real-x"}, ModelMap: map[string]string{"pub-y": "real-x"}}); err != nil {
		t.Fatal(err)
	}
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gs.SaveRouting("", "route", "", []string{"ch-entry", "ch-z"})
	if err != nil {
		t.Fatal(err)
	}
	keys := newKeyStore(t)
	tok := &store.Token{Name: "v2", Status: 1, UnlimitedQuota: true, PolicyVersion: 2, Group: store.EncodeTokenGroups([]string{g.ID}), ModelLimitsEnabled: true, ModelLimits: "pub-y", ExpiredTime: -1}
	if err := store.CreateToken(tok); err != nil {
		t.Fatal(err)
	}
	if err := keys.SyncTokenByID(tok.ID); err != nil {
		t.Fatal(err)
	}
	p := testPoolWith()
	t.Cleanup(func() { _ = p.Close() })
	p.SyncToDirWithSecrets("fake", []*auth.Auth{{UID: "account-a"}, {UID: "account-b"}}, map[string]any{"account-a": "fake-secret-a", "account-b": "fake-secret-b"})
	router := &multiRouter{def: "fake", reg: map[string]*recordingProvider{"fake": builtin}, p: p}
	h := NewHandler(Config{Pool: p, Provider: router, APIKeys: keys, Channels: cs, Groups: gs, MaxBodyMB: 1, ChannelProvider: func(string, string) gateway.Provider { return routingHTTPErrorProvider{standard} }})
	send := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok.Key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i, want := range []string{"ch-entry", "ch-z", "ch-entry", "ch-z"} {
		rec := send("/v1/chat/completions", `{"model":"pub-y","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != 200 || rec.Header().Get("X-Channel-Id") != want {
			t.Fatalf("attempt %d: %d %s channel=%s", i, rec.Code, rec.Body.String(), rec.Header().Get("X-Channel-Id"))
		}
	}
	if standard.chatCalls.Load() != 2 || builtin.chatCalls.Load() != 2 {
		t.Fatalf("not mixed RR: %d %d", standard.chatCalls.Load(), builtin.chatCalls.Load())
	}
	if builtin.lastSecret.Load() != "fake-secret-b" {
		t.Fatalf("builtin internal RR did not alternate: %v", builtin.lastSecret.Load())
	}
	if !strings.Contains(builtin.lastBody.Load().(string), `"real-x"`) {
		t.Fatal("builtin model not rewritten")
	}
	denied := send("/v1/chat/completions", `{"model":"other","messages":[]}`)
	if denied.Code != 403 {
		t.Fatalf("out of group: %d", denied.Code)
	}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Data) != 1 || catalog.Data[0].ID != "pub-y" {
		t.Fatalf("catalog: %s", rec.Body.String())
	}
	standard.status, standard.body = 503, `{"error":"temporary"}`
	before := builtin.chatCalls.Load()
	if rec = send("/v1/chat/completions", `{"model":"pub-y","messages":[]}`); rec.Code != 200 || builtin.chatCalls.Load() != before+1 {
		t.Fatalf("retry did not reach builtin: %d", rec.Code)
	}
	standard.status, standard.body = 400, `{"error":"invalid"}`
	before = builtin.chatCalls.Load()
	if rec = send("/v1/chat/completions", `{"model":"pub-y","messages":[]}`); rec.Code != 400 || builtin.chatCalls.Load() != before {
		t.Fatalf("terminal error retried: %d", rec.Code)
	}
	standard.status, standard.body = 503, `{"error":"temporary"}`
	builtin.status, builtin.body = 503, `{"error":"temporary"}`
	attemptsBefore := standard.chatCalls.Load() + builtin.chatCalls.Load()
	if rec = send("/v1/chat/completions", `{"model":"pub-y","messages":[]}`); rec.Code != 503 {
		t.Fatalf("failure returned %d", rec.Code)
	}
	if count := standard.chatCalls.Load() + builtin.chatCalls.Load() - attemptsBefore; count > 3 {
		t.Fatalf("retry budget exceeded: %d", count)
	}
	tok.ModelLimits = ""
	if err = store.SaveToken(tok); err != nil {
		t.Fatal(err)
	}
	if err = keys.SyncTokenByID(tok.ID); err != nil {
		t.Fatal(err)
	}
	if rec = send("/v1/chat/completions", `{"model":"pub-y","messages":[]}`); rec.Code != 403 {
		t.Fatalf("empty whitelist allowed: %d", rec.Code)
	}
	if rec = send("/v1/images/generations", `{"model":"pub-y","prompt":"test"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized image endpoint fell back: %d", rec.Code)
	}
	tok.ModelLimits = "pub-y"
	if err = store.SaveToken(tok); err != nil {
		t.Fatal(err)
	}
	if err = keys.SyncTokenByID(tok.ID); err != nil {
		t.Fatal(err)
	}
	streaming := &routingBrokenStreamProvider{}
	h.cfg.ChannelProvider = func(string, string) gateway.Provider { return streaming }
	g, err = gs.SaveRouting(g.ID, g.Name, g.Description, []string{"ch-entry"})
	if err != nil {
		t.Fatal(err)
	}
	if rec = send("/v1/chat/completions", `{"model":"pub-y","stream":true,"messages":[]}`); rec.Code != 200 || streaming.calls != 1 || !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("stream replayed or lost output: %d calls=%d %s", rec.Code, streaming.calls, rec.Body.String())
	}
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"pub-y","messages":[]}`))
	request.Header.Set("Authorization", "Bearer "+tok.Key)
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), request.WithContext(ctx))
	if streaming.calls != 1 {
		t.Fatal("cancelled request reached upstream")
	}
}

type routingHTTPErrorProvider struct{ *recordingProvider }

func (p routingHTTPErrorProvider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	stream, err := p.recordingProvider.Chat(ctx, cred, body)
	if stream.Status >= 400 {
		_ = stream.Body.Close()
		return gateway.ChatStream{}, &gateway.UpstreamHTTPError{Status: stream.Status}
	}
	return stream, err
}

type routingBrokenStreamProvider struct{ calls int }

func (*routingBrokenStreamProvider) ID() string               { return "broken" }
func (*routingBrokenStreamProvider) Caps() gateway.Capability { return gateway.CapChat }
func (*routingBrokenStreamProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (p *routingBrokenStreamProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	p.calls++
	return gateway.ChatStream{Status: 200, Body: &routingBrokenReader{}}, nil
}

type routingBrokenReader struct{ sent bool }

func (r *routingBrokenReader) Read(buffer []byte) (int, error) {
	if r.sent {
		return 0, errors.New("fake broken stream")
	}
	r.sent = true
	return copy(buffer, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), nil
}
func (*routingBrokenReader) Close() error { return nil }

var _ io.ReadCloser = (*routingBrokenReader)(nil)
