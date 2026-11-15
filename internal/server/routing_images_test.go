package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/openaicompat"
	"workbuddy2api/internal/store"
)

type routingImageRouter struct {
	*multiRouter
	calls  int
	status int
	model  string
}

func (r *routingImageRouter) ImageGen(_ context.Context, _ string, _ string, body []byte) ([]byte, int, bool, error) {
	r.calls++
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	r.model = payload.Model
	status := r.status
	if status == 0 {
		status = 200
	}
	return []byte(`{"data":[{"b64_json":"fake"}]}`), status, true, nil
}

func TestRoutingImagesMixedAndNoReplay(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer sk-entry-key" {
			t.Errorf("wrong endpoint/auth: %s", r.URL.Path)
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Model != "real-x" {
			t.Error("model not rewritten")
		}
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"fake"}]}`))
	}))
	defer upstream.Close()
	cs := entryStore(t, upstream.URL+"/v1", 1)
	standard, _ := cs.Get("ch-entry")
	standard.ModelEndpoints = map[string][]string{"real-x": {"images"}}
	if err := cs.Upsert(standard); err != nil {
		t.Fatal(err)
	}
	if err := cs.Upsert(&channelstore.Channel{ID: "ch-z", Name: "builtin-image", Kind: "builtin", ProviderID: "fake", Status: 1, Models: []string{"real-x"}, ModelMap: map[string]string{"pub-y": "real-x"}, ModelEndpoints: map[string][]string{"real-x": {"images"}}}); err != nil {
		t.Fatal(err)
	}
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	group, err := gs.SaveRouting("", "image", "", []string{"ch-entry", "ch-z"})
	if err != nil {
		t.Fatal(err)
	}
	keys := newKeyStore(t)
	token := &store.Token{Name: "image", PolicyVersion: 2, UnlimitedQuota: true, Status: 1, ExpiredTime: -1, Group: store.EncodeTokenGroups([]string{group.ID})}
	if err = store.CreateToken(token); err != nil {
		t.Fatal(err)
	}
	if err = keys.SyncTokenByID(token.ID); err != nil {
		t.Fatal(err)
	}
	p := testPoolWith()
	t.Cleanup(func() { _ = p.Close() })
	p.SyncToDirWithSecrets("fake", []*auth.Auth{{UID: "img-a"}}, map[string]any{"img-a": "fake"})
	router := &routingImageRouter{multiRouter: &multiRouter{def: "fake", reg: map[string]*recordingProvider{"fake": {id: "fake"}}, p: p}}
	h := NewHandler(Config{APIKeys: keys, Channels: cs, Groups: gs, Pool: p, Provider: router, MaxBodyMB: 1, ChannelProvider: func(id, url string) gateway.Provider { return openaicompat.NewOpenAICompat(id, url) }})
	send := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"pub-y","prompt":"fake","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+token.Key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, id := range []string{"ch-entry", "ch-z"} {
		rec := send("/v1/images/generations")
		if rec.Code != 200 || rec.Header().Get("X-Channel-Id") != id {
			t.Fatalf("image RR %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if calls != 1 || router.calls != 1 || router.model != "real-x" {
		t.Fatal("mixed images not executed")
	}
	if rec := send("/v1/chat/completions"); rec.Code != 403 {
		t.Fatalf("image-only model accepted chat: %d", rec.Code)
	}
	if _, err = gs.SaveRouting(group.ID, group.Name, "", []string{"ch-z"}); err != nil {
		t.Fatal(err)
	}
	router.status = 503
	if rec := send("/v1/images/generations"); rec.Code != 503 || router.calls != 2 {
		t.Fatalf("image replayed: %d calls=%d", rec.Code, router.calls)
	}
	token.ModelLimitsEnabled = true
	token.ModelLimits = ""
	if err = store.SaveToken(token); err != nil {
		t.Fatal(err)
	}
	if err = keys.SyncTokenByID(token.ID); err != nil {
		t.Fatal(err)
	}
	if rec := send("/v1/images/generations"); rec.Code != 403 || router.calls != 2 {
		t.Fatal("image whitelist bypass")
	}
}
