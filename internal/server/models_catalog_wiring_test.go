package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// catalogBody 是 /v3/config 的最小真实形状（带 credits 系数串）。
const catalogBody = `{"code":0,"msg":"OK","data":{"models":[
  {"id":"deepseek-v4-pro","name":"DeepSeek V4 Pro","credits":"x0.51 credits","maxInputTokens":131072},
  {"id":"gpt-5.2","name":"GPT 5.2","credits":"x5.00 credits","maxInputTokens":400000},
  {"id":"hy4-preview-f","name":"HY4 Preview F","credits":"x0.00 credits","maxInputTokens":131072}
]}}`

// resetCatalogCache 清空目录缓存，让每条用例从干净状态开始
// （与 handler_test.go 里清 dynamicModelsCache 的写法同款）。
func resetCatalogCache() {
	modelCatalogCache.Lock()
	modelCatalogCache.cat = nil
	modelCatalogCache.fetched = time.Time{}
	modelCatalogCache.lastFail = time.Time{}
	modelCatalogCache.Unlock()
}

// catalogResp 造一个 /v3/config 响应；hits 可选，用来数上游命中次数。
func catalogResp(status int, body string, hits *int) *http.Response {
	if hits != nil {
		*hits++
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// newCatalogUpstream 返回一个 /v3/config 走假响应的 upstream.Client。
func newCatalogUpstream(status int, body string, hits *int) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return catalogResp(status, body, hits), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// B1.0 的核心验收：包级 ModelCatalog() 走通「Pick 账号 → FetchModelCatalog → 缓存」，
// 第二次调用命中缓存不再打上游。
func TestModelCatalogFetchesAndCaches(t *testing.T) {
	resetCatalogCache()
	var hits int
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newCatalogUpstream(200, catalogBody, &hits)})

	cat := ModelCatalog()
	if cat == nil {
		t.Fatal("ModelCatalog() 返回 nil —— 生产调用点没接线")
	}
	if len(cat.Models) != 3 {
		t.Fatalf("models=%d want 3", len(cat.Models))
	}
	if v, ok := cat.Multiplier("deepseek-v4-pro"); !ok || v != 0.51 {
		t.Errorf("deepseek-v4-pro=(%v,%v) want (0.51,true)", v, ok)
	}
	if _, ok := cat.Multiplier("hy4-preview-f"); ok {
		t.Error("0 系数模型不该命中 Multiplier")
	}
	if hits != 1 {
		t.Fatalf("上游命中 %d 次 want 1", hits)
	}

	// 第二次：命中 1h 正缓存，不应再打上游。
	if again := ModelCatalog(); again == nil || len(again.Models) != 3 {
		t.Fatalf("第二次 ModelCatalog() 未命中缓存: %+v", again)
	}
	if hits != 1 {
		t.Errorf("缓存未生效：上游命中 %d 次 want 1", hits)
	}
	_ = h
}

// 状态查询必须是只读的：即便缓存里有数据也不触发上游；缓存为空时同样不触发。
func TestModelCatalogStateIsReadOnly(t *testing.T) {
	resetCatalogCache()
	var hits int
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	_ = NewHandler(Config{Pool: p, Upstream: newCatalogUpstream(200, catalogBody, &hits)})

	// 空缓存：unavailable，且一次上游都不打
	st := ModelCatalogState()
	if st.State != "unavailable" {
		t.Errorf("空缓存 state=%q want unavailable", st.State)
	}
	if hits != 0 {
		t.Fatalf("状态查询触发了 %d 次上游请求（统计轮询会变成打上游）", hits)
	}

	// 填充缓存后：ok
	if ModelCatalog() == nil {
		t.Fatal("ModelCatalog() 应有数据")
	}
	st = ModelCatalogState()
	if st.State != "ok" || st.Models != 3 || st.Stale {
		t.Errorf("state=%+v want {ok 3 false}", st)
	}
	if hits != 1 {
		t.Errorf("状态查询后上游命中 %d 次 want 1（只应有 ModelCatalog 那一次）", hits)
	}
}

// 超过 1h TTL 后状态必须变成 stale（而不是继续报 ok），
// 且缓存内容仍在 —— 让前端能把「过期系数」和「没有系数」分开显示。
func TestModelCatalogStateStaleAfterTTL(t *testing.T) {
	resetCatalogCache()
	var hits int
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	_ = NewHandler(Config{Pool: p, Upstream: newCatalogUpstream(200, catalogBody, &hits)})

	if ModelCatalog() == nil {
		t.Fatal("ModelCatalog() 应有数据")
	}
	// 把成功时间拨回 2h 前（> dynamicModelsTTL）——缓存内容保留，只动时间戳。
	modelCatalogCache.Lock()
	modelCatalogCache.fetched = time.Now().Add(-2 * time.Hour)
	modelCatalogCache.Unlock()

	st := ModelCatalogState()
	if st.State != "stale" || !st.Stale {
		t.Errorf("state=%+v want stale", st)
	}
	if st.Models != 3 {
		t.Errorf("stale 状态下仍应报告目录里有几个模型, got %d", st.Models)
	}
	// 仍然只读：不能因为过期就在状态查询里回源。
	if hits != 1 {
		t.Errorf("stale 状态查询触发了上游请求（hits=%d）", hits)
	}
}

// 上游全失败：ModelCatalog() 返回 nil、进入 5min 负缓存、状态报 unavailable + cooldown，
// 且**不 panic、不影响任何请求路径**。
func TestModelCatalogFailureNegativeCache(t *testing.T) {
	resetCatalogCache()
	var hits int
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newCatalogUpstream(500, `{"code":500,"msg":"boom"}`, &hits)
	_ = NewHandler(Config{Pool: p, Upstream: up})

	if cat := ModelCatalog(); cat != nil {
		t.Fatalf("上游 500 时应返回 nil, got %+v", cat)
	}
	// 失败换号：MaxRotate 默认 3，但池里只有 1 个账号 → 只试一次。
	if hits != 1 {
		t.Errorf("上游命中 %d 次 want 1（池里只有 1 个号）", hits)
	}

	st := ModelCatalogState()
	if st.State != "unavailable" || !st.Cooldown {
		t.Errorf("state=%+v want {unavailable cooldown:true}", st)
	}
	// 负缓存生效：再来一次不应再次打上游。
	if cat := ModelCatalog(); cat != nil {
		t.Fatalf("负缓存期内应返回 nil, got %+v", cat)
	}
	if hits != 1 {
		t.Errorf("负缓存未生效：上游命中 %d 次 want 1", hits)
	}
}

// 失败换号：池里第一个号总失败、第二个号成功 → 应当拿到目录（而不是整体失败）。
func TestModelCatalogRotatesOnFailure(t *testing.T) {
	resetCatalogCache()
	var badHits, goodHits int
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // 确定性随机源 → bad 先被选中
	p.SetCredits("good", 1000)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") == "Bearer at-bad" {
				badHits++
				return catalogResp(500, `{"code":500}`, nil), nil
			}
			goodHits++
			return catalogResp(200, catalogBody, nil), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	_ = NewHandler(Config{Pool: p, Upstream: up})

	cat := ModelCatalog()
	if cat == nil || len(cat.Models) != 3 {
		t.Fatalf("换号后应拿到目录, got %+v", cat)
	}
	if badHits != 1 || goodHits != 1 {
		t.Errorf("badHits=%d goodHits=%d want 1/1", badHits, goodHits)
	}
	// 失败的号要吃到 NoteError（与 fetchDynamicModels 同口径）
	st, ok := p.Status("bad")
	if !ok || st.ErrTotal == 0 {
		t.Errorf("失败账号未被惩罚: %+v ok=%v", st, ok)
	}
}

// ResetModelsCache 必须把目录缓存一起清掉：否则「刷新模型」会留下旧系数。
func TestResetModelsCacheAlsoClearsCatalog(t *testing.T) {
	resetCatalogCache()
	var hits int
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	_ = NewHandler(Config{Pool: p, Upstream: newCatalogUpstream(200, catalogBody, &hits)})

	if ModelCatalog() == nil {
		t.Fatal("应有数据")
	}
	if ModelCatalogState().State != "ok" {
		t.Fatal("填充后应为 ok")
	}
	ResetModelsCache()
	if st := ModelCatalogState(); st.State != "unavailable" {
		t.Errorf("ResetModelsCache 后 state=%q want unavailable", st.State)
	}
	// 再取一次应重新回源（hits 从 1 变 2）
	if ModelCatalog() == nil {
		t.Fatal("重置后应能重新取到")
	}
	if hits != 2 {
		t.Errorf("hits=%d want 2（重置后应重新回源）", hits)
	}
}

// 未配置 Upstream/Pool 时不得 panic（NewHandler(Config{}) 的路径）。
func TestModelCatalogWithoutDeps(t *testing.T) {
	resetCatalogCache()
	_ = NewHandler(Config{})
	if cat := ModelCatalog(); cat != nil {
		t.Errorf("无依赖时应返回 nil, got %+v", cat)
	}
	if st := ModelCatalogState(); st.State != "unavailable" {
		t.Errorf("state=%q want unavailable", st.State)
	}
}
