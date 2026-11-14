// client_catalog_fallback_test.go 主列表失败时的 /v3/config 兜底。
//
// 背景（2026-11-14 用户报障「workbuddy 国际没有用？模型全没了」）：
//
//	上游把 intl 的目录路径改掉了 —— 原 `/v2/enterprises/personal/models`
//	现在 404 `{"error_msg":"404 Route Not Found"}`。
//	而主列表一失败，FetchModels 就 return error ⇒ 该上游模型全部消失，
//	页面上连供应商行都不出现（供应商行是 aggregateCatalogVendors(models) 反推的）。
//
//	同一时间 `/v3/config` 仍然 200，且给出比主列表更全的模型集
//	（实测 intl：data.models 23 个）。它原来只被当作"锦上添花"的追加来源，
//	这里把它提升为**主列表失败时的兜底**。
//
// 这两条测试就是这个兜底的可复跑证据。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// intlAuthFor 造一份 intl 渠道凭证（Domain 决定 originRefererFor 的头）。
func intlAuthFor() *auth.Auth {
	return &auth.Auth{AccessToken: "tok", Channel: auth.ChannelIntl, Domain: "example.com"}
}

// TestFetchModelsFallsBackToV3Config 主列表 404 → 用 /v3/config 的目录。
func TestFetchModelsFallsBackToV3Config(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/enterprises/personal/models"),
			strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
		case strings.HasSuffix(r.URL.Path, modelsCatalogPath):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"models":[` +
				`{"id":"gpt-6-astra"},{"id":"kimi-k3"},{"id":"hy-image-3.5"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.ChatBaseIntl = srv.URL

	ms, err := c.FetchModels(intlAuthFor())
	if err != nil {
		t.Fatalf("主列表 404 时必须回退到 %s，实际报错: %v", modelsCatalogPath, err)
	}
	if len(ms) != 3 {
		t.Fatalf("应返回 /v3/config 的 3 个模型，实得 %d: %+v", len(ms), ms)
	}
	if ms[0].ID != "gpt-6-astra" {
		t.Errorf("模型 ID 应原样透传，实得 %q", ms[0].ID)
	}
}

// TestFetchModelsFallbackStillErrorsWhenCatalogAlsoFails 兜底也失败时才报错
// （错误不能被静默吞掉，否则运维看不到"到底是哪一步坏了"）。
func TestFetchModelsFallbackStillErrorsWhenCatalogAlsoFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error_msg":"404 Route Not Found"}`))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.ChatBaseIntl = srv.URL

	if _, err := c.FetchModels(intlAuthFor()); err == nil {
		t.Fatal("主列表与 /v3/config 都失败时必须返回错误")
	}
}

// TestFetchModelsMainListWinsWhenAvailable 主列表可用时优先用它（带上下文字段），
// 兜底只在失败时生效 —— 不能因为加了兜底就把正常路径换掉。
func TestFetchModelsMainListWinsWhenAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/enterprises/personal/models"),
			strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["main-1"]}],` +
				`"models":[{"id":"main-1","name":"Main One","maxInputTokens":1000,"maxOutputTokens":100}]}}`))
		case strings.HasSuffix(r.URL.Path, modelsCatalogPath):
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"cat-only"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.ChatBaseIntl = srv.URL

	ms, err := c.FetchModels(intlAuthFor())
	if err != nil {
		t.Fatalf("主列表可用时不该报错: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	main, ok := byID["main-1"]
	if !ok {
		t.Fatalf("主列表的 main-1 必须保留，实得 %+v", ms)
	}
	if main.ContextWindow != 1000 || main.MaxTokens != 100 {
		t.Errorf("主列表条目的上下文字段必须保留（这是主列表相对 /v3/config 的优势），实得 %+v", main)
	}
	if _, ok := byID["cat-only"]; !ok {
		t.Error("/v3/config 里多出的模型仍应被合并进来（原行为不变）")
	}
}
