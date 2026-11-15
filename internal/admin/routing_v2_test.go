package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/groupstore"
)

type routingTestProvider struct{}

func (routingTestProvider) ID() string               { return "fake-routing" }
func (routingTestProvider) Caps() gateway.Capability { return gateway.CapChat | gateway.CapModels }
func (routingTestProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{Status: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}
func (routingTestProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return []gateway.ModelInfo{{ID: "fake-model"}}, nil
}

func TestRoutingV2BuiltinBindingAndDiscovery(t *testing.T) {
	tokenTestHandler(t)
	cs, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry := gateway.NewRegistry()
	if err = registry.Register(routingTestProvider{}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true, Channels: cs, Registry: registry})
	rec := tokenRecord(t, h, "POST", "/admin/channels", `{"name":"builtin","kind":"builtin","provider_id":"fake-routing"}`)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := cs.List()[0].ID
	rec = tokenRecord(t, h, "POST", "/admin/channels/discover", tokenJSON(t, map[string]any{"id": id, "save": true}))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "fake-model") {
		t.Fatalf("discover: %d %s", rec.Code, rec.Body.String())
	}
	rec = tokenRecord(t, h, "GET", "/admin/channels", "")
	if !strings.Contains(rec.Body.String(), `"kind":"builtin"`) || !strings.Contains(rec.Body.String(), `"provider_id":"fake-routing"`) {
		t.Fatalf("metadata: %s", rec.Body.String())
	}
	for _, body := range []string{`{"name":"duplicate","kind":"builtin","provider_id":"fake-routing"}`, `{"name":"secret","kind":"builtin","provider_id":"fake-routing","keys":["fake-secret"]}`} {
		rec = tokenRecord(t, h, "POST", "/admin/channels", body)
		if rec.Code != 400 {
			t.Fatalf("invalid binding allowed: %d", rec.Code)
		}
	}
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest("GET", "/admin/channels/providers", nil))
	if unauth.Code == 200 {
		t.Fatal("provider list bypassed authentication")
	}
}

func TestRoutingCredentialDisclosureIsExplicit(t *testing.T) {
	f := newAdminKeyFixture(t)
	metadata := f.call(t, "GET", "/admin/apikeys/credentials", "")
	if metadata.Code != 200 || strings.Contains(metadata.Body.String(), f.h.currentAdminToken()) || strings.Contains(metadata.Body.String(), `"full"`) {
		t.Fatalf("metadata leaks: %d %s", metadata.Code, metadata.Body.String())
	}
	reveal := f.call(t, "POST", "/admin/apikeys/credentials/reveal", `{}`)
	if reveal.Code != 200 || !strings.Contains(reveal.Body.String(), `"full"`) || reveal.Header().Get("Cache-Control") != credentialCacheControl {
		t.Fatalf("explicit reveal: %d", reveal.Code)
	}
}

func TestRoutingV2AdminFlow(t *testing.T) {
	h := tokenTestHandler(t)
	cs, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	h = New(Config{AdminToken: testAdminToken, ListensLoopback: true, Channels: cs, Groups: gs})
	rec := tokenRecord(t, h, http.MethodGet, "/admin/channels/providers", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "providers") {
		t.Fatalf("providers: %d %s", rec.Code, rec.Body.String())
	}
	rec = tokenRecord(t, h, http.MethodPost, "/admin/channels", `{"name":"standard","kind":"standard","base_url":"https://example.invalid/v1","keys":["fake-key"],"models":["real"],"model_mapping":{"public":"real"}}`)
	if rec.Code != 200 {
		t.Fatalf("channel: %s", rec.Body.String())
	}
	id := cs.List()[0].ID
	rec = tokenRecord(t, h, http.MethodPost, "/admin/groups", tokenJSON(t, map[string]any{"name": "route", "policy_version": 2, "channel_ids": []string{id}}))
	if rec.Code != 200 {
		t.Fatalf("group: %s", rec.Body.String())
	}
	gid := gs.List()[0].ID
	rec = tokenRecord(t, h, http.MethodPost, "/admin/tokens/preview", tokenJSON(t, map[string]any{"policy_version": 2, "groups": []string{gid}, "model_limits_enabled": true, "model_limits": []string{"public"}}))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"public"`) {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	rec = tokenRecord(t, h, http.MethodPost, "/admin/tokens", createBody(t, "v2", map[string]any{"policy_version": 2, "groups": []string{gid}, "unlimited_quota": true, "model_limits_enabled": true, "model_limits": []string{"public"}}))
	if rec.Code != 200 {
		t.Fatalf("token: %d %s", rec.Code, rec.Body.String())
	}
	rec = tokenRecord(t, h, http.MethodGet, "/admin/tokens", "")
	if !strings.Contains(rec.Body.String(), `"policy_version":2`) {
		t.Fatalf("policy lost: %s", rec.Body.String())
	}
}
