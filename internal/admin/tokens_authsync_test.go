package admin

// tokens_authsync_test.go —— T81 的不变量：**/admin/tokens 写过的令牌，必须立刻能在网关鉴权**。
//
// # 为什么单独一条（它抓的是 CRUD 面完全看不见的缺陷）
//
// 2026-10-26 独立终验实测：用 /admin/tokens 新建的令牌打 /v1/chat/completions 与 /v1/models
// 一律 401 invalid_api_key —— 而列表有它、详情对、明文也能取。根因是网关鉴权只问
// apikey.Store 的内存索引，而本包的 CRUD 直写 tokens 表；那个索引只在接管 SQL 时
// 全量重建一次，所以"重启后又自己好了"。
//
// 本文件用 `apikey.Store.Validate` 作为"网关会不会放行"的**等价判据**：handler.go 的
// 401 invalid_api_key 就是它返回非 nil（validBearer → validCredential）。

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/store"
)

// tokenAuthFixture 造一套**生产形态**的装配：同一个 SQL 库 + 已接管 SQL 的 apikey.Store
// + admin.Handler。生产里这三者就是这样接的（main.go:1289 建 Store、:1313 Migrate、
// :1617 把 Store 注入 admin.Config.APIKeys）。
func tokenAuthFixture(t *testing.T) (*Handler, *apikey.Store) {
	t.Helper()
	dir := t.TempDir()
	if err := store.Open(filepath.Join(dir, "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		_ = store.Close()
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keys, err := apikey.NewWithStore(filepath.Join(dir, "apikeys.json"))
	if err != nil {
		t.Fatalf("apikey.NewWithStore: %v", err)
	}
	return New(Config{AdminToken: testAdminToken, ListensLoopback: true, APIKeys: keys}), keys
}

// gatewayValidate 报出"网关会不会放行"（nil = 放行）。
func gatewayValidate(keys *apikey.Store, bearer string) error {
	_, err := keys.Validate(bearer)
	return err
}

// TestAdminCreatedTokenAuthenticatesOnGateway 五步：建 ⇒ 能用；禁用 ⇒ 立刻拒；
// 启用 ⇒ 又能用；过期 ⇒ 立刻拒；删除 ⇒ 立刻拒。
func TestAdminCreatedTokenAuthenticatesOnGateway(t *testing.T) {
	h, keys := tokenAuthFixture(t)

	// ① 建一条（unlimited 以免被额度判据干扰；本用例只验"索引有没有跟上"）
	code, body := doJSON(t, h, "POST", "/admin/tokens", createBody(t, "网关可用", map[string]any{"unlimited_quota": true}))
	if code != 200 {
		t.Fatalf("创建失败 code=%d body=%s", code, body)
	}
	items, _ := decodeMap(t, body)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items 数量 = %d，want 1（body=%s）", len(items), body)
	}
	id := int(items[0].(map[string]any)["id"].(float64))
	rec := tokenRecord(t, h, "POST", fmt.Sprintf("/admin/tokens/%d/key", id), "")
	if rec.Code != 200 {
		t.Fatalf("取明文失败 code=%d", rec.Code)
	}
	plain, _ := decodeMap(t, rec.Body.String())["key"].(string)
	if plain == "" {
		t.Fatal("明文 key 为空")
	}
	if err := gatewayValidate(keys, plain); err != nil {
		t.Fatalf("★ 新建的令牌必须立刻能过网关鉴权，实际 %v（这就是「CRUD 能写、网关 401」的症状）", err)
	}

	// ② 禁用 ⇒ 网关立刻拒（ErrDisabled ⇒ handler 回 401）
	code, body = doJSON(t, h, "POST", "/admin/tokens/update", tokenJSON(t, map[string]any{"id": id, "status_only": true, "status": 2}))
	if code != 200 {
		t.Fatalf("禁用失败 code=%d body=%s", code, body)
	}
	if err := gatewayValidate(keys, plain); !errors.Is(err, apikey.ErrDisabled) {
		t.Fatalf("禁用后网关必须立刻拒（ErrDisabled），实际 %v", err)
	}

	// ③ 启用回来 ⇒ 又能用（证明不是"一禁就永久失效"）
	code, body = doJSON(t, h, "POST", "/admin/tokens/update", tokenJSON(t, map[string]any{"id": id, "status_only": true, "status": 1}))
	if code != 200 {
		t.Fatalf("启用失败 code=%d body=%s", code, body)
	}
	if err := gatewayValidate(keys, plain); err != nil {
		t.Fatalf("重新启用后应当又能用，实际 %v", err)
	}

	// ④ 过期（全量更新里给一个过去的 expired_time）⇒ 立刻拒（ErrExpired）
	past := time.Now().Add(-time.Hour).Unix()
	code, body = doJSON(t, h, "POST", "/admin/tokens/update", tokenJSON(t, map[string]any{
		"id": id, "name": "网关可用", "automatic": false, "groups": []string{"default"},
		"unlimited_quota": true, "expired_time": past,
	}))
	if code != 200 {
		t.Fatalf("设置过期时间失败 code=%d body=%s", code, body)
	}
	if err := gatewayValidate(keys, plain); !errors.Is(err, apikey.ErrExpired) {
		t.Fatalf("过期后网关必须立刻拒（ErrExpired），实际 %v", err)
	}

	// ⑤ 删除 ⇒ 立刻拒（ErrUnknown）；软删的行不许留在索引里
	code, body = doJSON(t, h, "POST", "/admin/tokens/delete", tokenJSON(t, map[string]any{"id": id}))
	if code != 200 {
		t.Fatalf("删除失败 code=%d body=%s", code, body)
	}
	if err := gatewayValidate(keys, plain); !errors.Is(err, apikey.ErrUnknown) {
		t.Fatalf("删除后网关必须立刻拒（ErrUnknown），实际 %v", err)
	}
}

// TestAdminTokenMutationRoutesAreAllIndexSynced 路由清单守卫。
//
// # 为什么要有它（防止"将来又加一条只写 store 不通知索引"的路由）
//
// 上面那条用例只能覆盖**今天存在**的写路由。将来有人加一条（比如 /admin/tokens/import）
// 而忘了接 syncAuthIndex，鉴权又会静默失效。所以这里把 POST /admin/tokens* 的**全集**
// 钉住：新增一条会让这条红 ⇒ 作者必须判断它是读还是写；写就必须接 syncAuthIndex，
// 并把它加进上一条用例的覆盖表。
func TestAdminTokenMutationRoutesAreAllIndexSynced(t *testing.T) {
	h := tokenTestHandler(t) // patterns 由生产那一行 registerTokens() 产生
	want := []string{
		"POST /admin/tokens",            // 建（写）
		"POST /admin/tokens/update",     // 改名/改状态/改额度/改过期（写）
		"POST /admin/tokens/delete",     // 删（写）
		"POST /admin/tokens/{id}/key",   // 单条取明文（**读**：不改状态）
		"POST /admin/tokens/batch/keys", // 批量取明文（**读**：不改状态）
	}
	got := make([]string, 0, len(want))
	for _, pat := range h.patterns {
		if strings.HasPrefix(pat, "POST /admin/tokens") {
			got = append(got, pat)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("POST /admin/tokens* 路由集合变了：\n  实际 %v\n  期望 %v\n"+
			"新增写路由时**必须**接 h.syncAuthIndex（见 tokens.go 的注释），并把它加进 TestAdminCreatedTokenAuthenticatesOnGateway 的覆盖表；"+
			"只读的新端点请更新本期望值并在注释里写明它是读。", got, want)
	}
}
