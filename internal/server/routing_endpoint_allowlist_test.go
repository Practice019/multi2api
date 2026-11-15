// routing_endpoint_allowlist_test.go 钉住"新版令牌能调哪些端点"这份允许清单。
//
// # 为什么需要这条判据
//
// 清单写在 handler.go 里，形式是一串 `&& r.URL.Path != "..."`：
//
//	if version != 0 && r.URL.Path != "/v1/chat/completions" &&
//	   r.URL.Path != "/v1/models" && r.URL.Path != "/v1/images/generations" {
//		→ 501 routing_endpoint_unsupported
//	}
//
// 它有三个毛病，都是**静默**的：
//
//	① 加了端点（比如生图）却忘了加进清单 ⇒ 那条端点永远 501，而且没人会注意到
//	② 清单里留了一个已经删掉的端点 ⇒ 那个路径会一路往下跑到旧路由
//	③ 文档写"生图 501"，代码其实已经支持了 —— 计划 §8 就这么错了一版
//
// ⇒ 用一条**行为断言**把清单固定成"这三个能用、别的明确 501"。
//
//	加端点时这条判据会红，逼你同时改清单与文档。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// newV2AllowlistHandler 造一个 policy_version=2 的最小部署。
func newV2AllowlistHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	cs := entryStore(t, "https://example.invalid/v1", 1)
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gs.SaveRouting("", "route", "", []string{"ch-entry"})
	if err != nil {
		t.Fatal(err)
	}
	keys := newKeyStore(t)
	tok := &store.Token{
		Name: "v2", Status: 1, UnlimitedQuota: true, PolicyVersion: 2,
		Group:              store.EncodeTokenGroups([]string{g.ID}),
		ModelLimitsEnabled: true, ModelLimits: "pub",
		ExpiredTime: -1,
	}
	if err := store.CreateToken(tok); err != nil {
		t.Fatal(err)
	}
	if err := keys.SyncTokenByID(tok.ID); err != nil {
		t.Fatal(err)
	}
	p := testPoolWith()
	t.Cleanup(func() { _ = p.Close() })
	h := NewHandler(Config{Pool: p, APIKeys: keys, Channels: cs, Groups: gs, MaxBodyMB: 1})
	return h, tok.Key
}

// isUnsupportedEndpoint 判断响应是不是"该端点不在 v2 允许清单里"。
func isUnsupportedEndpoint(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusNotImplemented &&
		strings.Contains(rec.Body.String(), "routing_endpoint_unsupported")
}

// ★ 允许清单：这三个端点必须能用，别的一律明确 501。
func TestRoutingEndpointAllowlist(t *testing.T) {
	h, key := newV2AllowlistHandler(t)

	send := func(method, path, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// ── 允许清单内的三个：都不该返回"端点不支持" ──
	allowed := []struct {
		method, path, body string
	}{
		{"POST", "/v1/chat/completions", `{"model":"pub","messages":[{"role":"user","content":"hi"}]}`},
		{"GET", "/v1/models", ""},
		{"POST", "/v1/images/generations", `{"model":"pub","prompt":"a cat"}`},
	}
	for _, c := range allowed {
		rec := send(c.method, c.path, c.body)
		if isUnsupportedEndpoint(rec) {
			t.Fatalf("★ %s %s 在允许清单里却回了 routing_endpoint_unsupported ——\n"+
				"handler.go 里那串 `r.URL.Path != ...` 漏了它。\nbody=%s",
				c.method, c.path, rec.Body.String())
		}
	}

	// ── 清单外**已注册**的端点：必须明确 501，不能悄悄落到旧路由 ──
	//
	// ⚠ 只测"已注册"的路径。`/v1/embeddings` 这类压根没注册的会由 mux 直接 404 ——
	// 那道闸门在 `withAuth` 里，路径不存在时根本跑不到，测它只能测到 404。
	//
	// `GET /v1/model`（单数）是 `/v1/models` 的**旧别名**，注册了同一个 handler，
	// 但**不在**允许清单里 ⇒ v2 令牌用它会被 501。这是刻意的：v2 只认规范化路径，
	// 旧别名属于"旧路径"，不能成为绕过 v2 组与白名单的旁路。
	rec := send("GET", "/v1/model", "")
	if !isUnsupportedEndpoint(rec) {
		t.Fatalf("★ GET /v1/model（旧别名，已注册但不在允许清单）没回 routing_endpoint_unsupported（实际 %d）——\n"+
			"新版令牌不该被悄悄放行到旧路径：那会让权限模型绕过 v2 的组与白名单。\nbody=%s",
			rec.Code, rec.Body.String())
	}
}

// ★ 501 的文案必须与清单一致（文档与代码不能再各说一套）。
//
// 计划 §8 曾经写着「生图等端点明确 501」，而代码其实已经支持生图 ——
// 那条文案就是从这里来的。它与清单不一致时这条判据会红。
func TestRoutingUnsupportedMessageMatchesAllowlist(t *testing.T) {
	h, key := newV2AllowlistHandler(t)
	req := httptest.NewRequest("GET", "/v1/model", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, needle := range []string{"聊天", "模型列表", "生图"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("★ 501 文案应逐条说明允许的能力（含 %q），实际 %s\n"+
				"文案与 handler.go 的清单必须同步 —— 否则文档会照它写错（§8 已经错过一版）",
				needle, body)
		}
	}
}
