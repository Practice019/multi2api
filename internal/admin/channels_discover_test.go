// channels_discover_test.go 「模型自动发现」的守卫。
//
// 用**本地假上游**（httptest）验四件事：
//
//	① 正常：上游返回的模型名被原样回填，并按字典序稳定排序
//	② 新建表单的用法：只给 base_url + key 也能问（不需要先存渠道）
//	③ save=true 时写回渠道 models；上游删掉的模型**不残留**（覆盖而非合并）
//	④ 失败路径可读：上游 401 ⇒ 502 且错误里**不含 key 明文**；没有可用 key ⇒ 400 且说清原因
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/openaicompat"
)

// discoverHandler 搭一个已注入 ChannelProvider 的 Handler。
// ⚠ 工厂是**组合根**的职责；测试里注入具体上游是允许的（守卫用 go list -deps 分析非测试依赖）。
func discoverHandler(t *testing.T, s *channelstore.Store) *Handler {
	t.Helper()
	return New(Config{
		AdminToken: testAdminToken, ListensLoopback: true, Channels: s,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})
}

func doDiscover(t *testing.T, h *Handler, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/channels/discover", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.channelsDiscover(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ① 正常路径 + 稳定排序。
func TestDiscoverReturnsUpstreamModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("应打 /v1/models，实际 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"z-model"},{"id":"a-model"},{"id":"m-model"}]}`))
	}))
	defer srv.Close()

	s := chanStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-d", Name: "发现用渠道", BaseURL: srv.URL + "/v1", Status: channelstore.StatusEnabled,
		Keys: []channelstore.ChannelKey{},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	// 渠道没 key ⇒ 走请求里带 key 的形态，同时覆盖"已存渠道 + 显式 key"
	code, out := doDiscover(t, discoverHandler(t, s), `{"id":"ch-d","key":"sk-d"}`)
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %v", code, out)
	}
	got, _ := out["models"].([]any)
	if len(got) != 3 {
		t.Fatalf("应回 3 个模型，实际 %v", got)
	}
	if got[0] != "a-model" || got[1] != "m-model" || got[2] != "z-model" {
		t.Fatalf("应按字典序稳定排序，实际 %v", got)
	}
}

// ② 新建表单用法：还没保存也能问。
func TestDiscoverWorksBeforeSave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"only-one"}]}`))
	}))
	defer srv.Close()
	s := chanStore(t)
	code, out := doDiscover(t, discoverHandler(t, s),
		`{"base_url":"`+srv.URL+`/v1","key":"sk-new"}`)
	if code != http.StatusOK {
		t.Fatalf("未保存的渠道也应能试查，实际 %d: %v", code, out)
	}
	if out["count"] != float64(1) {
		t.Fatalf("应回 1 个模型，实际 %v", out)
	}
}

// ③ save=true 覆盖写回；上游删掉的模型不残留。
func TestDiscoverSaveOverwritesModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"fresh"}]}`))
	}))
	defer srv.Close()
	s := chanStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-s", Name: "回填用", BaseURL: srv.URL + "/v1", Status: channelstore.StatusEnabled,
		Models: []string{"stale-one", "stale-two"},
		Keys:   []channelstore.ChannelKey{{Key: "sk-s", Status: channelstore.StatusEnabled}},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	code, out := doDiscover(t, discoverHandler(t, s), `{"id":"ch-s","save":true}`)
	if code != http.StatusOK || out["saved"] != true {
		t.Fatalf("应 200 且 saved=true，实际 %d %v", code, out)
	}
	got, _ := s.Get("ch-s")
	if len(got.Models) != 1 || got.Models[0] != "fresh" {
		t.Fatalf("应覆盖为上游当前的模型（旧的不残留），实际 %v", got.Models)
	}
}

// ④ 失败路径：可读、且不含 key 明文。
func TestDiscoverFailuresAreReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()
	s := chanStore(t)
	secret := "sk-super-secret-value"
	code, out := doDiscover(t, discoverHandler(t, s),
		`{"base_url":"`+srv.URL+`/v1","key":"`+secret+`"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("上游 401 应映射成 502，实际 %d: %v", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "向上游查询模型失败") {
		t.Fatalf("错误应说明是查询失败，实际 %q", msg)
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("★ 错误里不得出现 key 明文，实际 %q", msg)
	}
	if out["hint"] == nil {
		t.Fatalf("应给可执行的 hint（Base URL 是否指向 /v1），实际 %v", out)
	}

	// 没有可用 key ⇒ 400 且说清（不是空列表、也不是 502）
	s2 := chanStore(t)
	if err := s2.Upsert(&channelstore.Channel{
		ID: "ch-nokey", Name: "没key", BaseURL: srv.URL + "/v1", Status: channelstore.StatusEnabled,
		Keys: []channelstore.ChannelKey{{Key: "sk-off", Status: channelstore.StatusManuallyDisabled}},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	code2, out2 := doDiscover(t, discoverHandler(t, s2), `{"id":"ch-nokey"}`)
	if code2 != http.StatusBadRequest {
		t.Fatalf("只有停用 key 时应 400，实际 %d: %v", code2, out2)
	}
	if !strings.Contains(out2["error"].(string), "没有可用的 key") {
		t.Fatalf("应说清是'没有可用 key'，实际 %v", out2["error"])
	}
}
