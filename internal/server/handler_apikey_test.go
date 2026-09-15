// handler_apikey_test.go —— API key 管理（对标 new-api 令牌体系）的鉴权与用量。
//
// 覆盖：config 管理 key 兼容 / 未配置不鉴权兼容 / 普通 key 通过并记用量 /
// 额度用尽 402 / 禁用 401 / 限速 429。
package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/auth"
)

func newAPIKeyStore(t *testing.T) *apikey.Store {
	t.Helper()
	s, err := apikey.New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func chatReq() *http.Request {
	return httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
}

// TestAPIKeyNoAuthCompat 未配置任何鉴权 → 恒通过（旧行为）。
func TestAPIKeyNoAuthCompat(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if rec.Code != 200 {
		t.Fatalf("未配置鉴权应通过: code=%d", rec.Code)
	}
}

// TestAPIKeyAdminKeyCompat config.api_key 管理 key 兼容（不限额、不记用量）。
func TestAPIKeyAdminKeyCompat(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "admin-secret",
	})
	req := chatReq()
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("管理 key 应通过: code=%d body=%s", rec.Code, rec.Body)
	}
	// 错误 key 且无 store → 401
	req2 := chatReq()
	req2.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("错误 key 应 401: code=%d", rec2.Code)
	}
}

// TestAPIKeyStoreAuthAndUsage 普通 key 通过且用量被记录。
func TestAPIKeyStoreAuthAndUsage(t *testing.T) {
	store := newAPIKeyStore(t)
	k, err := store.Create("app-1", 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKeys:  store,
	})
	req := chatReq()
	req.Header.Set("Authorization", "Bearer "+k.ID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("普通 key 应通过: code=%d body=%s", rec.Code, rec.Body)
	}
	v := store.List()[0]
	if v.Requests != 1 {
		t.Errorf("Requests=%d want 1", v.Requests)
	}
	if v.Used <= 0 {
		t.Errorf("Used=%d want >0（sseOK 含 usage，应累计 token）", v.Used)
	}
}

// TestAPIKeyQuotaExhausted 额度用尽 → 402。
func TestAPIKeyQuotaExhausted(t *testing.T) {
	store := newAPIKeyStore(t)
	k, _ := store.Create("tiny", 5, 0)
	store.BumpUsage(k.ID, 5, true) // 直接用完
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKeys:  store,
	})
	req := chatReq()
	req.Header.Set("Authorization", "Bearer "+k.ID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("额度用尽应 402: code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "额度") {
		t.Errorf("402 body 应说明额度: %s", rec.Body)
	}
}

// TestAPIKeyDisabled 禁用 → 401。
func TestAPIKeyDisabled(t *testing.T) {
	store := newAPIKeyStore(t)
	k, _ := store.Create("off", 0, 0)
	store.Toggle(k.ID, false)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKeys:  store,
	})
	req := chatReq()
	req.Header.Set("Authorization", "Bearer "+k.ID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("禁用应 401: code=%d", rec.Code)
	}
}

// TestAPIKeyRateLimit RPM 超限 → 429。
func TestAPIKeyRateLimit(t *testing.T) {
	store := newAPIKeyStore(t)
	k, _ := store.Create("burst", 0, 2)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKeys:  store,
	})
	for i := 0; i < 2; i++ {
		req := chatReq()
		req.Header.Set("Authorization", "Bearer "+k.ID)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("第 %d 次应通过: code=%d", i+1, rec.Code)
		}
	}
	req := chatReq()
	req.Header.Set("Authorization", "Bearer "+k.ID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超速应 429: code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestAPIKeyErrMapping Store 哨兵错误与 HTTP 映射一致（admin 侧 writeStoreError）。
func TestAPIKeyErrMapping(t *testing.T) {
	store := newAPIKeyStore(t)
	if _, err := store.Validate("sk-nope"); !errors.Is(err, apikey.ErrUnknown) {
		t.Errorf("want ErrUnknown: %v", err)
	}
}
