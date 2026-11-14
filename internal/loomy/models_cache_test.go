// models_cache_test.go 目录缓存与并集刷新的判据。
//
// 背景（2026-11-14 用户报障）：loomy 只显示 5 个模型，用户另有一个实例显示 10 个，
// 问「不同的电脑不可能检测不一样吧」。实测根因是**上游按账号下发不同模型集**，
// 而我们只采了池子里抽到的那一个账号。
//
// 这里的每条测试都是那份诊断的**可复跑证据**。
package loomy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// writeAuthFile 在 dir 里放一份凭证（文件名遵循 LoadDir 的 loomy-*.json 约定）。
func writeAuthFile(t *testing.T, dir, uid, session string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 authDir 失败: %v", err)
	}
	b, err := json.Marshal(map[string]any{"session": session, "uid": uid, "nickname": uid})
	if err != nil {
		t.Fatalf("序列化凭证失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "loomy-"+uid+".json"), b, 0o600); err != nil {
		t.Fatalf("写凭证失败: %v", err)
	}
}

// fakeCatalog 起一个假上游：按 `token:` 头返回该账号的模型集。
//
// ⚠ 用 `token:` 而不是 Bearer —— 这是本上游的真实契约（client.go:152 的实测记录：
// 目录端点认 token，用 Authorization 会得到 HTTP 200 + 「缺少 token」）。
func fakeCatalog(t *testing.T, bySession map[string][]string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ModelsPath {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		ids, ok := bySession[r.Header.Get("token")]
		if !ok {
			_, _ = w.Write([]byte(`{"code":"100002","desc":"缺少 token","data":{}}`))
			return
		}
		type item struct {
			ID     string `json:"id"`
			MaxOut int    `json:"max_output_tokens"`
			MaxTok int    `json:"max_tokens"`
		}
		var data []item
		for _, id := range ids {
			data = append(data, item{ID: id, MaxOut: 1000})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	return ts, &hits
}

func credFor(session string) gateway.Credential {
	return gateway.Credential{Provider: providerID, Secret: &Auth{Session: session, UID: "uid-a"}}
}

// ── ① 并集去重：大小写不敏感 ────────────────────────────────────────────────

// TestUnionModelsDedupIgnoresCase 上游对同一模型的大小写不稳，按字节比较会把
// 它当成两个模型列两行 —— 用户在另一个实例上看到的 11 行正是这个症状
// （`Hy-Image-3.5-preview` 出现两次）。
func TestUnionModelsDedupIgnoresCase(t *testing.T) {
	got := unionModels([][]Model{
		{{ID: "Hy-Image-3.5-preview"}},
		{{ID: "hy-image-3.5-preview"}, {ID: "spark-x2.6"}},
	})
	if len(got) != 2 {
		t.Fatalf("大小写不同应视为同一个模型，期望 2 条，实得 %d: %+v", len(got), got)
	}
	if got[0].ID != "Hy-Image-3.5-preview" {
		t.Errorf("应保留首次出现的写法，实得 %q", got[0].ID)
	}
}

// TestUnionModelsMergesMetadata 某个号只回了 id（字段缺失），后面的号回了完整字段
// —— 必须合并成完整条目，而不是"先到先得"留下空字段。
func TestUnionModelsMergesMetadata(t *testing.T) {
	got := unionModels([][]Model{
		{{ID: "m1"}}, // 只有 id
		{{ID: "M1", ContextWindow: 1048576, MaxOutputTokens: 131072, Name: "M One"}},
	})
	if len(got) != 1 {
		t.Fatalf("期望合并成 1 条，实得 %d", len(got))
	}
	if got[0].ContextWindow != 1048576 || got[0].MaxOutputTokens != 131072 || got[0].Name != "M One" {
		t.Errorf("元数据应取首个非零值，实得 %+v", got[0])
	}
}

// ── ② 缓存保质期 = 24h（用户指定）────────────────────────────────────────────

func TestCatalogCacheTTLIs24h(t *testing.T) {
	if CatalogCacheTTL != 24*time.Hour {
		t.Fatalf("TTL 应为 24h（用户 2026-11-14 指定），实得 %v", CatalogCacheTTL)
	}
	now := time.Now()
	if !(&CatalogSnapshot{FetchedAt: now.Add(-23 * time.Hour)}).Fresh(now) {
		t.Error("23 小时前的快照应仍新鲜")
	}
	if (&CatalogSnapshot{FetchedAt: now.Add(-25 * time.Hour)}).Fresh(now) {
		t.Error("25 小时前的快照应已过期")
	}
	if (&CatalogSnapshot{}).Fresh(now) {
		t.Error("零值 FetchedAt 不该算新鲜")
	}
}

// ── ③ 刷新 = 多账号并集（本次报障的核心判据）────────────────────────────────

func TestRefreshCatalogUnionsAccounts(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "a", "sess-a")
	writeAuthFile(t, dir, "b", "sess-b")
	ts, _ := fakeCatalog(t, map[string][]string{
		"sess-a": {"spark-x2.6", "GLM-5.3-Flash"},
		"sess-b": {"mimo-v2.5", "auto", "spark-x2.6"}, // 与 A 有一个交集
	})
	defer ts.Close()

	p := NewWithConfig(Config{Client: NewWithBase(ts.URL), AuthDir: dir})
	snap, err := p.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	// 2 + 3 - 1(交集) = 4
	if snap.Len() != 4 {
		t.Fatalf("并集去重后应为 4 条，实得 %d: %+v", snap.Len(), snap.Models)
	}
	if len(snap.Sources) != 2 {
		t.Errorf("两个账号都应有贡献，实得 %v", snap.Sources)
	}
	seen := map[string]bool{}
	for _, m := range snap.Models {
		if seen[m.ID] {
			t.Errorf("并集里不应有重复: %q", m.ID)
		}
		seen[m.ID] = true
	}
}

// TestRefreshCatalogSingleAccountIsIncomplete 反证"单账号不够"：
// 只放一个号时，拿到的必然只有那一个号的子集 —— 这正是修之前的行为。
func TestRefreshCatalogSingleAccountIsIncomplete(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "a", "sess-a") // 只放一个号
	ts, _ := fakeCatalog(t, map[string][]string{
		"sess-a": {"spark-x2.6-low", "spark-x2.6-4b"},  // A 专属的两个
		"sess-b": {"DeepSeek-V4.1-Flash", "mimo-v2.5"}, // B 才有的
	})
	defer ts.Close()

	p := NewWithConfig(Config{Client: NewWithBase(ts.URL), AuthDir: dir})
	snap, err := p.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if snap.Len() != 2 {
		t.Fatalf("单号只能看到自己的 2 条，实得 %d", snap.Len())
	}
	for _, m := range snap.Models {
		if m.ID == "DeepSeek-V4.1-Flash" {
			t.Fatal("单号不该看到 B 账号的模型 —— 这条成立才说明并集是必需的")
		}
	}
}

// ── ④ 全失败保留旧快照（绝不清空）──────────────────────────────────────────

func TestRefreshCatalogKeepsOldSnapshotOnTotalFailure(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "a", "sess-a")
	// 指向一个必然连不上的地址
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1"), AuthDir: dir})
	old := &CatalogSnapshot{
		Schema:    catalogCacheSchema,
		FetchedAt: time.Now().Add(-48 * time.Hour),
		Models:    []Model{{ID: "old-1"}, {ID: "old-2"}},
	}
	p.catalog = old

	snap, err := p.RefreshCatalog(context.Background())
	if err == nil {
		t.Fatal("全部账号失败时必须报错（否则运维看不到问题）")
	}
	if snap == nil || snap.Len() != 2 {
		t.Fatalf("旧快照必须保留（目录静默变空比不全更糟），实得 %+v", snap)
	}
	if got := p.Catalog(); got == nil || got.Len() != 2 {
		t.Fatalf("内存里的旧快照也必须还在，实得 %+v", got)
	}
}

// ── ⑤ 落盘 + 重启读回（冷启动不该是空目录）──────────────────────────────────

func TestCatalogSnapshotPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "a", "sess-a")
	ts, _ := fakeCatalog(t, map[string][]string{"sess-a": {"m1", "m2"}})
	defer ts.Close()

	p := NewWithConfig(Config{Client: NewWithBase(ts.URL), AuthDir: dir})
	if _, err := p.RefreshCatalog(context.Background()); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, catalogCacheFileName)); err != nil {
		t.Fatalf("快照应落盘到 %s: %v", catalogCacheFileName, err)
	}

	// 新实例 + 打不通的上游 = 模拟重启
	p2 := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1"), AuthDir: dir})
	got := p2.Catalog()
	if got == nil || got.Len() != 2 {
		t.Fatalf("重启后应从磁盘读回 2 条，实得 %+v", got)
	}
}

// TestCatalogIgnoresUnreadableCacheFile 坏缓存当没有缓存（不 panic、不阻断目录）。
func TestCatalogIgnoresUnreadableCacheFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, catalogCacheFileName), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewWithConfig(Config{AuthDir: dir})
	if got := p.Catalog(); got != nil {
		t.Fatalf("坏缓存应视为没有缓存，实得 %+v", got)
	}
}

// TestCatalogIgnoresSchemaMismatch 不认识的 schema 版本不采信。
func TestCatalogIgnoresSchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	b, _ := json.Marshal(map[string]any{"schema": 999, "fetched_at": time.Now(), "models": []Model{{ID: "x"}}})
	if err := os.WriteFile(filepath.Join(dir, catalogCacheFileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewWithConfig(Config{AuthDir: dir})
	if got := p.Catalog(); got != nil {
		t.Fatalf("schema 不匹配时应忽略，实得 %+v", got)
	}
}

// ── ⑥ 请求路径只读缓存，零出站（性能梯级 L5 的判据）────────────────────────

func TestModelsReadsCacheWithoutUpstream(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "a", "sess-a")
	ts, hits := fakeCatalog(t, map[string][]string{"sess-a": {"live-1"}})
	defer ts.Close()

	p := NewWithConfig(Config{Client: NewWithBase(ts.URL), AuthDir: dir})
	p.catalog = &CatalogSnapshot{
		Schema:    catalogCacheSchema,
		FetchedAt: time.Now(),
		Models:    []Model{{ID: "cached-1"}, {ID: "cached-2"}},
	}

	infos, err := p.Models(context.Background(), credFor("sess-a"))
	if err != nil {
		t.Fatalf("Models() 失败: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("应返回缓存里的 2 条，实得 %d", len(infos))
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("读缓存时不该打上游，实际打了 %d 次 —— 这会把一次白烧变成每次白烧", n)
	}
}

// TestModelsFallsBackToStaticWhenNoCache 连缓存都没有时才回落静态基线。
func TestModelsFallsBackToStaticWhenNoCache(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "a", "sess-a")
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1"), AuthDir: dir})

	infos, err := p.Models(context.Background(), credFor("sess-a"))
	if err != nil {
		t.Fatalf("Models() 失败: %v", err)
	}
	if len(infos) == 0 {
		t.Fatal("缓存空 + 上游不可达时应回落静态基线（空目录会让客户端判定模型不存在）")
	}
}
