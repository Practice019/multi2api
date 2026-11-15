// catalog_version_header_test.go 目录版本响应头的判据（U-04 的对外价值）。
//
// # 为什么是"响应头"而不是"改列表内容"
//
// 把 `/v1/models` 的清单换成快照口径是**语义变更**：v2 令牌现在走 `ResolvePolicy`
// （按它自己的分组），而快照覆盖全部渠道 —— 直接切会让客户端看到它无权调用的模型。
//
// 目录版本头是纯增量的那部分价值：客户端能发现"目录变了"，不必把整个列表
// 拉下来自己 diff。**不碰任何现有语义。**
//
// # 三条判据
//
//	① 有渠道时必须有头，且是稳定值（两次调用一样）
//	② 渠道变了版本必须变（否则"发现变化"这个能力是假的）
//	③ 没有渠道时不设头（而不是给一个固定的空版本 —— 那会让客户端以为目录是空的）
//
// # 一条被推翻的前提（留档）
//
// 第一版我以为"不带令牌"就是旧路径，于是直接无令牌请求 `/v1/models` 取头 ——
// 实测 **401**（注入了 APIKeys 就强制鉴权）。旧路径必须用一个 **policy_version=0**
// 的令牌才会走到。下面的夹具因此同时发 v1 与 v2 两把。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// newCatalogFixture 造一个同时有 v1 与 v2 令牌的部署。
//
// 返回 handler、v1 令牌明文、v2 令牌明文（只在本进程内用，不外发）。
func newCatalogFixture(t *testing.T) (*Handler, string, string) {
	t.Helper()
	cs := entryStore(t, "https://example.invalid/v1", 1)
	if err := cs.Upsert(&channelstore.Channel{
		ID: "ch-a", Name: "a", Kind: "standard", Status: 1,
		Models: []string{"real-y"}, ModelMap: map[string]string{"pub-y": "real-y"},
	}); err != nil {
		t.Fatal(err)
	}
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gs.SaveRouting("", "route", "", []string{"ch-a"})
	if err != nil {
		t.Fatal(err)
	}
	keys := newKeyStore(t)

	// v2：走 ResolvePolicy 分支。
	v2 := &store.Token{
		Name: "v2", Status: 1, UnlimitedQuota: true, PolicyVersion: 2,
		Group: store.EncodeTokenGroups([]string{g.ID}), ExpiredTime: -1,
	}
	if err := store.CreateToken(v2); err != nil {
		t.Fatal(err)
	}
	// v1：走旧路径（policy_version = 0）。
	v1 := &store.Token{
		Name: "v1", Status: 1, UnlimitedQuota: true, PolicyVersion: 0, ExpiredTime: -1,
	}
	if err := store.CreateToken(v1); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []*store.Token{v1, v2} {
		if err := keys.SyncTokenByID(tok.ID); err != nil {
			t.Fatal(err)
		}
	}

	p := testPoolWith()
	t.Cleanup(func() { _ = p.Close() })
	h := NewHandler(Config{Pool: p, APIKeys: keys, Channels: cs, Groups: gs, MaxBodyMB: 1,
		ChannelProvider: func(string, string) gateway.Provider { return &recordingProvider{id: "standard"} }})
	return h, v1.Key, v2.Key
}

// catalogVersionOf 发一次 `/v1/models` 并取版本头。
func catalogVersionOf(t *testing.T, h *Handler, key string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	return rec.Header().Get(catalogVersionHeader)
}

// ★ ① 有渠道 ⇒ 有头，且稳定。
func TestCatalogVersionHeaderIsStable(t *testing.T) {
	h, _, v2key := newCatalogFixture(t)

	first := catalogVersionOf(t, h, v2key)
	if first == "" {
		t.Fatalf("★ 配了渠道却没有 %s 头 —— 客户端无法发现目录变化", catalogVersionHeader)
	}
	// 连发三次必须一致：Version 是内容哈希，排序遍历也是为这个。
	for i := 0; i < 3; i++ {
		if got := catalogVersionOf(t, h, v2key); got != first {
			t.Fatalf("★ 第 %d 次版本变了（%q → %q）—— 内容没变版本却在变，"+
				"快照比较会立刻失效", i+2, first, got)
		}
	}
}

// ★ ② 渠道变了 ⇒ 版本必须变。
func TestCatalogVersionChangesWithChannels(t *testing.T) {
	h, _, v2key := newCatalogFixture(t)
	before := catalogVersionOf(t, h, v2key)
	if before == "" {
		t.Fatal("前置条件不成立：应有版本头")
	}

	if err := h.cfg.Channels.Upsert(&channelstore.Channel{
		ID: "ch-new", Name: "new", Kind: "standard", Status: 1,
		Models: []string{"real-new"},
	}); err != nil {
		t.Fatal(err)
	}

	after := catalogVersionOf(t, h, v2key)
	if after == before {
		t.Fatalf("★ 加了渠道但版本没变（%q）—— 那「发现目录变化」这个能力是假的", before)
	}
}

// ★ ③ 没有渠道 ⇒ 不设头（而不是给一个固定值）。
//
// ⚠ 第一版用 `NewHandler(Config{})` —— 那会 **500 gateway_panic**（没有 Provider），
// 于是头当然是空的。判据**因为错误的原因通过**，反向验证怎么改都不红。
// 现在：先断言 200（不给 panic 留后门），再用"有 Provider、渠道表为空"的夹具。
func TestCatalogVersionHeaderAbsentWithoutChannels(t *testing.T) {
	// ⚠ 不能用 entryStore —— 它会 Upsert 一条「入口测试渠道」，这里要的正好是**没有渠道**。
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cs, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cs.List()); n != 0 {
		t.Fatalf("前置条件：渠道表必须为空，实际 %d 条", n)
	}
	keys := newKeyStore(t)
	tok := &store.Token{Name: "v2", Status: 1, UnlimitedQuota: true,
		PolicyVersion: 2, ExpiredTime: -1}
	if err := store.CreateToken(tok); err != nil {
		t.Fatal(err)
	}
	if err := keys.SyncTokenByID(tok.ID); err != nil {
		t.Fatal(err)
	}
	p := testPoolWith()
	t.Cleanup(func() { _ = p.Close() })
	h := NewHandler(Config{Pool: p, APIKeys: keys, Channels: cs, MaxBodyMB: 1,
		ChannelProvider: func(string, string) gateway.Provider { return &recordingProvider{id: "standard"} }})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// ★ 先钉住"请求真的成功了"—— 否则这条判据可以在 500 上通过。
	if rec.Code != http.StatusOK {
		t.Fatalf("★ 请求必须真的 200（实际 %d）—— 否则「头不存在」是因为处理失败，"+
			"不是因为没渠道。body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(catalogVersionHeader); got != "" {
		t.Fatalf("★ 没配渠道却给了版本头 %q —— 客户端会以为这是一个空的但确定的目录，"+
			"而实际是「这台网关没有渠道功能」", got)
	}
}

// 两条路径（v1 旧路径 / v2 策略路径）必须给同一个头。
func TestCatalogVersionHeaderOnBothPaths(t *testing.T) {
	h, v1key, v2key := newCatalogFixture(t)

	lv := catalogVersionOf(t, h, v1key) // policy_version = 0 ⇒ 旧路径
	vv := catalogVersionOf(t, h, v2key) // policy_version = 2 ⇒ ResolvePolicy 分支

	if lv == "" || vv == "" {
		t.Fatalf("两条路径都应有版本头，实际 legacy=%q v2=%q", lv, vv)
	}
	if lv != vv {
		t.Fatalf("★ 同一个目录给了两个版本（legacy=%q v2=%q）—— "+
			"客户端不该因为令牌是 v1 还是 v2 就失去发现变化的能力", lv, vv)
	}
}
