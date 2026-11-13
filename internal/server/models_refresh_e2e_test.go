// models_refresh_e2e_test.go —— 「目录不是实时的」这条报障的**端到端**判据。
//
// # 用户原话
//
//	「反代模型目录有 bug，感觉列表目录都不是实时的。
//	  前端读取本地缓存，但是本地缓存需要有办法可以更新。」
//
// # 这条文件钉的是三层，顺序就是排查顺序
//
//	① 后端 TTL（1h）是**故意的**，不许用"干脆不缓存"来糊 —— 回归锁：
//	   不显式刷新时上游**只被问一次**（否则"刷新生效"的代价是把上游打爆）；
//	② 显式刷新（POST /admin/models/refresh → server.ResetModelsCache）必须让上游
//	   **被重新问一次**、目录内容跟着变；
//	③ 页面取数端点（/ui/models/data）必须**自带上游取数时刻**（catalog 字段）——
//	   否则「这份目录是不是实时」永远只能靠感觉，刷新有没有生效也无从判断。
//	   而页面原来的"更新于 HH:MM:SS"是**页面这次请求的时刻**，与上游缓存无关。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/store"
	"workbuddy2api/internal/upstream"
)

// modelsRefreshAdminToken 本文件统一用的管理令牌（uiIdentify 认它）。
const modelsRefreshAdminToken = "test-admin-token-models-refresh"

// dynModelsBody 造一份"每个 tag 一个模型"的上游目录响应。
// tag 变 ⇒ 目录内容变，用来证明"刷新之后拿到的是**新**目录"，而不只是"又发了一次请求"。
func dynModelsBody(tag string) string {
	return `{"code":0,"data":{"models":[{"id":"dyn-` + tag + `","maxInputTokens":131072,"maxOutputTokens":8192}],"agents":[{"name":"cli","models":["dyn-` + tag + `"]}]}}`
}

// modelsRefreshFixture 造一个"数**目录**上游命中次数 + 每次回新目录"的最小夹具。
//
// ⚠ 只数「目录那一条 URL」（实测是 /console/enterprises/personal/models）：
// 一次 /v1/models 还会顺带打 /v3/config（成本系数目录，见 handler.go 的
// modelCatalogCache 注释），把两条数在一起时"TTL 内不重复请求上游"这条断言
// 数的就不是同一件事了 —— 那是假绿/假红的高发处。
type modelsRefreshFixture struct {
	h       *Handler
	dirHits int
}

// isModelsDirPath 判「这条上游 URL 是不是模型目录本身」。
//
// 上游目录请求实测形如 GET https://<host>/console/enterprises/personal/models，
// 而成本系数目录是 /v3/config ⇒ 后缀判据足够且不会误伤（后者不以 /models 结尾）。
func isModelsDirPath(p string) bool { return strings.HasSuffix(p, "/models") }

func newModelsRefreshFixture(t *testing.T) *modelsRefreshFixture {
	t.Helper()
	resetDynamicCache()
	t.Cleanup(resetDynamicCache)

	f := &modelsRefreshFixture{}
	up := upstreamAdapter{c: &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := catalogBody // 成本系数目录：本用例不关心它，给一份形状合法的即可
			if isModelsDirPath(r.URL.Path) {
				f.dirHits++
				body = dynModelsBody(fmt.Sprintf("v%d", f.dirHits))
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}}
	f.h = NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		AdminToken: modelsRefreshAdminToken,
	})
	return f
}

// list 打一次 /v1/models，返回响应体。
func (f *modelsRefreshFixture) list(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// pageData 打一次 /ui/models/data（带管理令牌，uiIdentify 认它）。
func (f *modelsRefreshFixture) pageData(t *testing.T) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ui/models/data", nil)
	req.Header.Set("Authorization", "Bearer "+modelsRefreshAdminToken)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/models/data = %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 /ui/models/data 失败: %v body=%s", err, rec.Body.String())
	}
	return out
}

// TestExplicitRefreshReasksUpstreamWhileTTLStillHolds 是"刷新到底有没有穿透"的判据。
//
// 正例与**反锁**在同一个用例里，因为它们是同一件事的两面：
// 只断言"刷新后上游被重新问了"会放过"把缓存取消掉"那种实现
// （缓存一取消，任何时候都回源，上游被打爆而这断言照样绿）。
func TestExplicitRefreshReasksUpstreamWhileTTLStillHolds(t *testing.T) {
	f := newModelsRefreshFixture(t)

	first := f.list(t)
	if f.dirHits != 1 {
		t.Fatalf("冷启动应只问上游一次，实际 %d", f.dirHits)
	}
	if !strings.Contains(first, "dyn-v1") {
		t.Fatalf("目录内容不对（应有 dyn-v1）: %s", first)
	}

	// ① 回归锁：不显式刷新 ⇒ TTL（1h）内**不得**再问上游。
	second := f.list(t)
	if f.dirHits != 1 {
		t.Errorf("★ TTL 内上游被问了 %d 次（want 1）—— 正缓存被取消了？"+
			"那是「每次请求都打上游」，代价是把上游打爆", f.dirHits)
	}
	if second != first {
		t.Errorf("TTL 内目录内容变了（缓存没起作用）:\n%s\n%s", first, second)
	}

	// ② 显式刷新：POST /admin/models/refresh 打的就是这个包级函数。
	ResetModelsCache()
	third := f.list(t)
	if f.dirHits != 2 {
		t.Errorf("★ 显式刷新后上游命中 %d 次 want 2 —— 刷新没穿透到上游", f.dirHits)
	}
	if !strings.Contains(third, "dyn-v2") {
		t.Errorf("★ 显式刷新后拿到的还是旧目录（这是用户报障的形态）: %s", third)
	}
}

// TestModelsPageDataReportsCatalogFetchTime 钉住 item ③：页面能看出"这份目录什么时候取的"。
func TestModelsPageDataReportsCatalogFetchTime(t *testing.T) {
	f := newModelsRefreshFixture(t)

	// 先把目录灌热（页面首次打开时上游就是热的）
	_ = f.list(t)

	data := f.pageData(t)
	raw, ok := data["catalog"]
	if !ok {
		t.Fatalf("★ /ui/models/data 没有 catalog 字段 —— 页面无法回答「这份目录是什么时候取的」，"+
			"于是「是不是实时」只能靠感觉（用户报障的原话）。现有字段: %v", keysOf(data))
	}
	cat, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("catalog 字段类型应为对象，实际 %T", raw)
	}
	age, ok := cat["age_seconds"].(float64)
	if !ok {
		t.Fatalf("catalog.age_seconds 缺失/类型不对: %v", cat)
	}
	if age < 0 || age > 5 {
		t.Errorf("catalog.age_seconds=%v want 0~5（刚刚才取过）", age)
	}
	if ttl, _ := cat["ttl_seconds"].(float64); int64(ttl) != int64(dynamicModelsTTL.Seconds()) {
		t.Errorf("catalog.ttl_seconds=%v want %v（必须与后端的 TTL 同源，不许前端各写一份）",
			cat["ttl_seconds"], int64(dynamicModelsTTL.Seconds()))
	}
	if stale, _ := cat["stale"].(bool); stale {
		t.Error("刚取过就报 stale=true")
	}
	fetched, _ := cat["fetched_at"].(string)
	ts, err := time.Parse(time.RFC3339, fetched)
	if err != nil {
		t.Fatalf("catalog.fetched_at=%q 不是 RFC3339: %v", fetched, err)
	}
	if d := time.Since(ts); d < -5*time.Second || d > 10*time.Second {
		t.Errorf("catalog.fetched_at=%v 距现在 %v，want ~0（它该是**这份目录被取回的时刻**）", ts, d)
	}
}

// TestCatalogFreshnessTakesOldestSnapshotAndFlagsStale 是"取哪一份时刻"的判据。
//
// 为什么取**最旧**的那一份：目录是多上游合并的，只要有一份过了 TTL，
// 「这份目录是不是实时的」答案就是"不是"。取最新的一份会把 90 分钟前的旧目录
// 报成"1 秒前取的"——那比没有这个字段更坏（假新鲜）。
func TestCatalogFreshnessTakesOldestSnapshotAndFlagsStale(t *testing.T) {
	resetDynamicCache()
	t.Cleanup(resetDynamicCache)

	// 空缓存：不许假装有值
	if got := catalogFreshness(); got.AgeSeconds != -1 || got.FetchedAt != "" {
		t.Errorf("冷缓存时 catalogFreshness=%+v want {FetchedAt:\"\" AgeSeconds:-1}（不知道就说不知道）", got)
	}

	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []DefaultModel{{ID: "dyn-old"}}
	dynamicModelsCache.fetched = time.Now().Add(-90 * time.Minute)
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	// 另一个上游 1 秒前刚取过：不许因为"有一份是新的"就报成新鲜
	storeProviderModels("other-provider", []gateway.ModelInfo{{ID: "x"}})

	got := catalogFreshness()
	if got.AgeSeconds < 89*60 || got.AgeSeconds > 91*60 {
		t.Errorf("age_seconds=%d want ~5400（取**最旧**的一份：90 分钟前那次）", got.AgeSeconds)
	}
	if !got.Stale {
		t.Error("★ 最旧的一份已过 TTL，stale 必须为 true —— 否则页面上这份旧目录看起来是新鲜的")
	}
	if got.TTLSeconds != int64(dynamicModelsTTL.Seconds()) {
		t.Errorf("ttl_seconds=%d want %d", got.TTLSeconds, int64(dynamicModelsTTL.Seconds()))
	}
}

// TestModelsPageDataDoesNotFakeFreshnessForStoreCatalog 是"SQL 目录"那一档的纪律。
//
// store 档的行里**没有**取数时刻（models 表只存名字/端点）。如实报"不知道"（-1），
// 不许用 0 或 now 把它包装成"刚刚取的" —— 那正是假新鲜。
func TestModelsPageDataDoesNotFakeFreshnessForStoreCatalog(t *testing.T) {
	resetDynamicCache()
	t.Cleanup(resetDynamicCache)

	// 独立临时库，用完关掉（store.DB 是包级单例，纪律见 models_channels_test.go）。
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := store.SeedModelCatalog([]store.CatalogEntry{
		{Vendor: "fixture-provider", Name: "fixture-provider/only-model", Endpoints: []string{"chat"}},
	}); err != nil {
		t.Fatalf("seed 目录失败: %v", err)
	}

	f := newModelsRefreshFixture(t)
	// ⚠ 先把内存里那两份缓存**灌热**：这才能证明 SQL 档的 -1 不是「恰好缓存是冷的」，
	//   而是真的没有把内存缓存的取数时刻冒充到 SQL 目录上。
	_ = f.list(t)
	data := f.pageData(t)
	if data["source"] != "store" {
		t.Fatalf("source=%v want store（已 seed 的库应走 SQL 档）", data["source"])
	}
	cat, _ := data["catalog"].(map[string]any)
	age, ok := cat["age_seconds"].(float64)
	if !ok || age != -1 {
		t.Errorf("SQL 档的 catalog.age_seconds=%v want -1（没有取数时刻就说不知道，不许报 0）", cat["age_seconds"])
	}
}

// keysOf 只在报错信息里用：把响应字段名列出来，省得排查时再手打一遍。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
