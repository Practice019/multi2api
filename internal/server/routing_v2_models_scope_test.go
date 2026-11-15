// routing_v2_models_scope_test.go v2 令牌的 `/v1/models` 必须**双向**正确。
//
// # 这条判据钉的是什么
//
//	① 正向：令牌自己分组里的模型**必须出现**
//	② 反向：不在它分组里的模型**必须不出现**
//
// 只测①会放过"返回全部模型"的实现；只测②会放过"返回空目录"的实现。
// 两侧都要有，这条判据才咬得住。
//
// # 一个被推翻的假设（留档）
//
// 动手前我怀疑这里有 bug：`channelPublicNames` 里那句
//
//	abilities := s.EnabledModels(channelstore.DefaultGroup)   // 写死 default
//
// 看起来会让"分组不是 default 的 v2 令牌"拿到空目录（abilities ∧ allowed = ∅）。
// 实测**不成立** —— 渠道声明的模型还有另一条装配路径（`appendChannelModels`
// 直接追加渠道声明的名字，不经 abilities），所以模型照样可见。
//
// 结论：口径写死 default 这件事**目前没有造成可见缺陷**，但它是脆的 ——
// 以后若有人把 `appendChannelModels` 那条路径收掉，缺陷就会显形。
// 这条判据放在这里，就是为了让那次改动立刻红。
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// newV2ScopeHandler 造两条渠道：ch-a 进 g-a、ch-b 不进任何组（只有 default）。
//
// 返回 handler 与 v2 令牌的明文（只在本进程内用，不外发）。
func newV2ScopeHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	cs := entryStore(t, "https://example.invalid/v1", 1)
	// ch-a：只属于 g-a，服务 pub-y
	if err := cs.Upsert(&channelstore.Channel{
		ID: "ch-a", Name: "a", Kind: "standard", Status: 1,
		Models: []string{"real-y"}, ModelMap: map[string]string{"pub-y": "real-y"},
	}); err != nil {
		t.Fatal(err)
	}
	// ch-b：只属于 default，服务 pub-z
	if err := cs.Upsert(&channelstore.Channel{
		ID: "ch-b", Name: "b", Kind: "standard", Status: 1,
		Models: []string{"real-z"}, ModelMap: map[string]string{"pub-z": "real-z"},
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
	tok := &store.Token{
		Name: "v2", Status: 1, UnlimitedQuota: true, PolicyVersion: 2,
		Group:              store.EncodeTokenGroups([]string{g.ID}),
		ModelLimitsEnabled: false, // 不收窄白名单：只看分组
		ExpiredTime:        -1,
	}
	if err := store.CreateToken(tok); err != nil {
		t.Fatal(err)
	}
	if err := keys.SyncTokenByID(tok.ID); err != nil {
		t.Fatal(err)
	}
	p := testPoolWith()
	t.Cleanup(func() { _ = p.Close() })
	h := NewHandler(Config{Pool: p, APIKeys: keys, Channels: cs, Groups: gs, MaxBodyMB: 1,
		ChannelProvider: func(string, string) gateway.Provider { return &recordingProvider{id: "standard"} }})
	return h, tok.Key
}

// modelsBody 取一次 `/v1/models` 的响应体。
func modelsBody(t *testing.T, h *Handler, key string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// ★ ① 正向：令牌自己分组里的模型必须出现。
func TestModelsV2TokenSeesItsOwnGroup(t *testing.T) {
	h, key := newV2ScopeHandler(t)
	body := modelsBody(t, h, key)
	if !strings.Contains(body, "pub-y") {
		t.Fatalf("★ v2 令牌看不到它自己分组里的 pub-y ——\n"+
			"客户端据此建不了模型索引，等于不能用。\nbody=%s", body)
	}
}

// ★ ② 反向：不在它分组里的模型必须不出现。
//
// 这条是"权限边界"的那一半：v2 的分组就是令牌的可见范围，
// 目录里多给一个模型，客户端就会去调它，然后拿到 403。
func TestModelsV2TokenDoesNotSeeOtherGroups(t *testing.T) {
	h, key := newV2ScopeHandler(t)
	body := modelsBody(t, h, key)
	if strings.Contains(body, "pub-z") {
		t.Fatalf("★ v2 令牌看到了 ch-b（default 组）的 pub-z —— 它不在该令牌的分组里，\n"+
			"列出来会让客户端调一个必然 403 的模型。\nbody=%s", body)
	}
}

// ★ ③ 无令牌 ⇒ 401（不是"全量视角"）。
//
// ⚠ 我起先以为无令牌会得到"全部模型"的全量视角，实测是 **401**
// （注入了 APIKeys 就强制鉴权）。所以它不能当 ② 的对照。
//
// ② 的"非真空"保障由 ① 提供：① 断言 pub-y **确实出现**，
// 说明这份列表非空；在一个非空列表里断言"没有 pub-z"才有意义。
//
// 本条的用途变成钉住鉴权边界本身 —— 目录是权限面的一部分，
// 未鉴权不该拿到任何模型名。
func TestModelsWithoutTokenIsUnauthorized(t *testing.T) {
	h, _ := newV2ScopeHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，实际 %d：%s\n"+
			"目录是权限面的一部分 —— 未鉴权不该拿到任何模型名", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "pub-y") || strings.Contains(body, "pub-z") {
		t.Fatalf("★ 401 响应里漏出了模型名：%s", body)
	}
}
