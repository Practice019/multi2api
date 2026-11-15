// catalog_snapshot_wiring_test.go U-04 的**接线判据** —— 获取与读取真的分开了吗。
//
// # 为什么必须单独测「读不写」
//
// catalog_snapshot.go 的不变量（Fail 保留旧快照、Sequence 只在成功时前进）已经有
// 单元判据了。但那些判据**测不到**接线错误：
//
//	`/admin/channels/catalog` 明明拿到了快照持有者，却在读路径里又调了一次
//	BuildCatalog + Publish —— 单元判据全绿，实际每次 GET 都推进一次 sequence。
//
// 于是面板上"这份目录是第 3 次刷新得到的"会随着访问次数漂移，
// 而**没有任何运行期信号**说明它是漂的。
//
// 所以这里断言的是**计数**：连续读 → 计数不动；显式刷新 → 计数才动。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// catalogEnvelope /admin/channels/catalog 的响应形状。
type catalogEnvelope struct {
	Catalog struct {
		Version string `json:"snapshot_version"`
		Models  []struct {
			ID string `json:"id"`
		} `json:"models"`
		Conflicts []struct {
			Public string `json:"public"`
			Reason string `json:"reason"`
		} `json:"conflicts"`
	} `json:"catalog"`
	Sequence    int64  `json:"sequence"`
	Attempts    int64  `json:"attempts"`
	Failed      int64  `json:"failed"`
	PublishedAt string `json:"published_at"`
	LastError   string `json:"last_error"`
}

// newCatalogTestHandler 建一个可用渠道库的 Handler。
//
// ⚠ channelstore.New 要求 store.DB 已经 Open + Migrate（B1 之后渠存在 SQL 里）。
// store.DB 是**包级单例**，所以每个用例自己开一个 t.TempDir() 下的库并在 Cleanup 里关 ——
// 不关的话下一个用例会在同一个库上跑，"空库"这个前提会静默失效。
func newCatalogTestHandler(t *testing.T) (*Handler, *channelstore.Store) {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cs, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("channelstore.New: %v", err)
	}
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatalf("groupstore.New: %v", err)
	}
	return New(Config{AdminToken: testAdminToken, ListensLoopback: true, Channels: cs, Groups: gs}), cs
}

func readCatalog(t *testing.T, h *Handler) catalogEnvelope {
	t.Helper()
	rec := tokenRecord(t, h, http.MethodGet, "/admin/channels/catalog", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET catalog: %d %s", rec.Code, rec.Body.String())
	}
	var out catalogEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 catalog 响应失败: %v\n%s", err, rec.Body.String())
	}
	return out
}

// TestCatalogReadsDoNotAdvanceSequence 连续读不推进计数 ——「获取与读取分开」。
//
// ⚠ 这是本文件最重要的一条：把读路径改回"每次现算并发布"时它会立刻红。
func TestCatalogReadsDoNotAdvanceSequence(t *testing.T) {
	h, cs := newCatalogTestHandler(t)
	if err := cs.Upsert(&channelstore.Channel{
		ID: "a", Name: "a", Kind: "standard", Status: channelstore.StatusEnabled,
		Models: []string{"real"}, Keys: []channelstore.ChannelKey{{Key: "k", Status: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	first := readCatalog(t, h)
	if first.Sequence != 1 || first.Attempts != 1 {
		t.Fatalf("首次读应惰性发布一次（seq=1 att=1），实际 seq=%d att=%d", first.Sequence, first.Attempts)
	}
	if len(first.Catalog.Models) != 1 || first.Catalog.Models[0].ID != "real" {
		t.Fatalf("首次快照内容不对：%+v", first.Catalog.Models)
	}
	if first.Catalog.Version == "" {
		t.Fatal("快照必须有版本号")
	}
	if first.PublishedAt == "" {
		t.Fatal("快照必须有发布时间")
	}

	// 读 5 次：计数一次都不该动。
	for i := 0; i < 5; i++ {
		again := readCatalog(t, h)
		if again.Sequence != first.Sequence {
			t.Fatalf("第 %d 次重复读把 Sequence 从 %d 推到了 %d —— "+
				"读路径还在写（现算 + 发布），计数会随访问次数漂移",
				i+2, first.Sequence, again.Sequence)
		}
		if again.Attempts != first.Attempts {
			t.Fatalf("第 %d 次重复读把 Attempts 从 %d 推到了 %d", i+2, first.Attempts, again.Attempts)
		}
		if again.Catalog.Version != first.Catalog.Version {
			t.Fatalf("重复读拿到了不同版本：%q → %q", first.Catalog.Version, again.Catalog.Version)
		}
	}
}

// TestCatalogRefreshAdvancesSequence 显式刷新才推进计数。
func TestCatalogRefreshAdvancesSequence(t *testing.T) {
	h, cs := newCatalogTestHandler(t)
	if err := cs.Upsert(&channelstore.Channel{
		ID: "a", Name: "a", Kind: "standard", Status: channelstore.StatusEnabled,
		Models: []string{"m1"}, Keys: []channelstore.ChannelKey{{Key: "k", Status: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	first := readCatalog(t, h)

	rec := tokenRecord(t, h, http.MethodPost, "/admin/channels/catalog/refresh", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	after := readCatalog(t, h)
	if after.Sequence != first.Sequence+1 {
		t.Fatalf("刷新后 Sequence 应 +1（%d → %d），实际 %d", first.Sequence, first.Sequence+1, after.Sequence)
	}
	// 内容没变 ⇒ 版本不变、模型不变。
	if after.Catalog.Version != first.Catalog.Version {
		t.Fatalf("内容未变但版本变了：%q → %q", first.Catalog.Version, after.Catalog.Version)
	}

	// 刷新是**显式**动作：没调它之前，配置改了也不该出现在快照里。
	if err := cs.Upsert(&channelstore.Channel{
		ID: "b", Name: "b", Kind: "standard", Status: channelstore.StatusEnabled,
		Models: []string{"m2"}, Keys: []channelstore.ChannelKey{{Key: "k", Status: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	stale := readCatalog(t, h)
	if len(stale.Catalog.Models) != 1 {
		t.Fatalf("未刷新前快照不该反映新渠道（这是快照语义），实际 %d 个模型", len(stale.Catalog.Models))
	}
	rec = tokenRecord(t, h, http.MethodPost, "/admin/channels/catalog/refresh", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次 refresh: %d %s", rec.Code, rec.Body.String())
	}
	fresh := readCatalog(t, h)
	if len(fresh.Catalog.Models) != 2 {
		t.Fatalf("刷新后应看到 2 个模型，实际 %d 个", len(fresh.Catalog.Models))
	}
	if fresh.Catalog.Version == stale.Catalog.Version {
		t.Fatal("内容变了但版本没变")
	}
}

// TestCatalogSurfacesConflicts 映射歧义通过管理端点可见（而不是被静默吞掉）。
func TestCatalogSurfacesConflicts(t *testing.T) {
	h, cs := newCatalogTestHandler(t)
	for _, ch := range []*channelstore.Channel{
		// A：把 pub 显式映射到 gpt-4
		{ID: "a", Name: "a", Kind: "standard", Status: channelstore.StatusEnabled,
			Models: []string{"gpt-4"}, ModelMap: map[string]string{"pub": "gpt-4"},
			Keys: []channelstore.ChannelKey{{Key: "k", Status: 1}}},
		// B：把 pub 当真实模型直接声明 ⇒ 隐式撞名
		{ID: "b", Name: "b", Kind: "standard", Status: channelstore.StatusEnabled,
			Models: []string{"pub"}, Keys: []channelstore.ChannelKey{{Key: "k", Status: 1}}},
	} {
		if err := cs.Upsert(ch); err != nil {
			t.Fatal(err)
		}
	}

	got := readCatalog(t, h)
	if len(got.Catalog.Conflicts) != 1 {
		t.Fatalf("应通过管理端点看到 1 条映射歧义，实际 %d 条：%+v", len(got.Catalog.Conflicts), got.Catalog.Conflicts)
	}
	c := got.Catalog.Conflicts[0]
	if c.Public != "pub" || c.Reason != channelstore.ConflictImplicitCollision {
		t.Fatalf("冲突内容不对：%+v", c)
	}
	// 歧义名不发布 —— 管理端看到的目录里不能有它。
	for _, m := range got.Catalog.Models {
		if m.ID == "pub" {
			t.Fatal("歧义公开名 pub 出现在了管理端目录里 —— 它必须被拒绝发布")
		}
	}
}

// TestCatalogHandlerIsolatesInstances 两个 Handler 不共享快照。
//
// ⚠ 与 adminToken 同一条理由：快照放在包级的话，测试与真实部署会互相污染，
// 表现是"测试里 publish 过一次，真实实例启动就已经 Ready"。
func TestCatalogHandlerIsolatesInstances(t *testing.T) {
	h1, cs := newCatalogTestHandler(t)
	if err := cs.Upsert(&channelstore.Channel{
		ID: "a", Name: "a", Kind: "standard", Status: channelstore.StatusEnabled,
		Models: []string{"m1"}, Keys: []channelstore.ChannelKey{{Key: "k", Status: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	first := readCatalog(t, h1)
	if first.Sequence != 1 {
		t.Fatalf("h1 首次读应 seq=1，实际 %d", first.Sequence)
	}
	// h1 再读两次 —— 计数不动（已由另一条判据覆盖，这里只为制造"h1 已经发布过"）。
	readCatalog(t, h1)
	readCatalog(t, h1)

	// 同一个渠道库、**另一个 Handler 实例**：它必须有自己的第一份快照。
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	h2 := New(Config{AdminToken: testAdminToken, ListensLoopback: true, Channels: cs, Groups: gs})
	got := readCatalog(t, h2)
	if got.Sequence != 1 {
		t.Fatalf("h2 首次读应 seq=1（它自己的第一份），实际 %d —— 快照持有者被跨实例共享了。"+
			"包级单例会让「测试里发布过一次、真实实例启动就已 Ready」", got.Sequence)
	}
	if got.Attempts != 1 {
		t.Fatalf("h2 首次读 Attempts 应为 1，实际 %d", got.Attempts)
	}
}

// 让 httptest 的 import 有用（helper 里可能不直接引用）。
var _ = httptest.NewRequest
