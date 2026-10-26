// channels_test.go /admin/channels 的 handler 级守卫。
//
// 上一轮我如实标注了"端点已注册但无测试"，这份就是补它。四条要钉住的：
//
//	① 未注入渠道存储 ⇒ 回 enabled:false 空集（与 /admin/apikeys 同款），不是 501、不是崩
//	② **列表响应里不含任何明文 key** ← 最关键的一条：列表是轮询的，一旦漏出明文，
//	   每次轮询都会把全部渠道的 key 发一遍
//	③ 停用渠道 ⇒ 能力索引立刻不包含它的模型（这就是"默认收紧"在数据面的体现）
//	④ 映射环在**保存时**就被拒，不是留到请求时才炸
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/store"
)

const chanTestKey = "sk-channel-secret-plaintext"

func chanStore(t *testing.T) *channelstore.Store {
	t.Helper()
	// B1：SQL 是 channelstore 的后端 ⇒ 先开临时库。
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建渠道存储失败: %v", err)
	}
	return s
}

func chanHandler(t *testing.T, s *channelstore.Store) *Handler {
	t.Helper()
	cfg := Config{AdminToken: testAdminToken, ListensLoopback: true}
	cfg.Channels = s
	return New(cfg)
}

// doJSON 发一个带测试鉴权的请求并回 (状态码, 响应体原文)。
func doJSON(t *testing.T, h *Handler, method, path, body string) (int, string) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withTestAuth(req))
	return rec.Code, rec.Body.String()
}

// ① 未注入存储 ⇒ **不注册路由**（404）。
//
// 这是仓库既有约定：未注册 = 前端按 manifest 不渲染操作区，
// 而不是渲染一个点了会失败的按钮。所以这里断言 404，不是 200+enabled:false。
// （handler 里仍有 `s == nil` 的防御分支，属于纵深防御，路由门才是第一道。）
func TestChannelsNotRegisteredWithoutStore(t *testing.T) {
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true})
	code, body := doJSON(t, h, http.MethodGet, "/admin/channels", "")
	if code != http.StatusNotFound {
		t.Fatalf("未注入渠道存储时路由不该存在（约定：未注册 = 前端不渲染操作区），实际 %d: %s", code, body)
	}
}

// ②③④ 建 → 列表（无明文）→ reveal（有明文）→ 停用（能力消失）→ 删。
func TestChannelsCRUDAndSecretHygiene(t *testing.T) {
	h := chanHandler(t, chanStore(t))

	create := `{"name":"测试渠道","type":0,"base_url":"https://api.example.com/v1",` +
		`"keys":["` + chanTestKey + `"],"models":["pub-a","pub-b"],"priority":10,"weight":5}`
	code, body := doJSON(t, h, http.MethodPost, "/admin/channels", create)
	if code != http.StatusOK {
		t.Fatalf("建渠道应 200，实际 %d: %s", code, body)
	}
	var created struct {
		OK      bool `json:"ok"`
		Created bool `json:"created"`
		Channel struct {
			ID       string   `json:"id"`
			KeyMasks []string `json:"key_masks"`
		} `json:"channel"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("建渠道响应不是合法 JSON: %v (%s)", err, body)
	}
	if !created.OK || !created.Created || created.Channel.ID == "" {
		t.Fatalf("建渠道应回 ok/created/id，实际 %s", body)
	}
	id := created.Channel.ID

	// ② 列表：**绝不含明文**，但要有掩码
	code, body = doJSON(t, h, http.MethodGet, "/admin/channels", "")
	if code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", code)
	}
	if strings.Contains(body, chanTestKey) {
		t.Fatalf("★ 列表响应里出现了明文 key —— 列表是轮询的，这等于每次轮询都发一遍：%s", body)
	}
	if !strings.Contains(body, "key_masks") || len(created.Channel.KeyMasks) != 1 {
		t.Fatalf("列表应回掩码，实际 %s", body)
	}
	if !strings.Contains(body, "\"ability_count\":2") {
		t.Fatalf("两条模型 × 一个启用渠道 ⇒ ability_count 应为 2，实际 %s", body)
	}

	// reveal 是唯一回明文的端点
	code, body = doJSON(t, h, http.MethodPost, "/admin/channels/reveal", `{"id":"`+id+`"}`)
	if code != http.StatusOK || !strings.Contains(body, chanTestKey) {
		t.Fatalf("reveal 应回明文 key（供复制），实际 %d: %s", code, body)
	}

	// ③ 停用渠道 ⇒ 能力索引立刻不含它（默认收紧的数据面体现）
	code, body = doJSON(t, h, http.MethodPost, "/admin/channels/status", `{"id":"`+id+`","status":2}`)
	if code != http.StatusOK {
		t.Fatalf("停用应 200，实际 %d: %s", code, body)
	}
	_, body = doJSON(t, h, http.MethodGet, "/admin/channels", "")
	if !strings.Contains(body, "\"ability_count\":0") {
		t.Fatalf("停用后能力索引应清空（能力由启用状态推导），实际 %s", body)
	}

	// 删除
	code, body = doJSON(t, h, http.MethodPost, "/admin/channels/delete", `{"id":"`+id+`"}`)
	if code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d: %s", code, body)
	}
	_, body = doJSON(t, h, http.MethodGet, "/admin/channels", "")
	if strings.Contains(body, id) {
		t.Fatalf("删除后列表不该再有该渠道，实际 %s", body)
	}
}

// ④ 映射环在保存时就被拒（留到请求时才炸会让用户看到莫名其妙的上游 400）。
func TestChannelsRejectsMappingCycle(t *testing.T) {
	h := chanHandler(t, chanStore(t))
	body := `{"name":"环","base_url":"https://a.example.com/v1","models":["m"],` +
		`"model_mapping":{"a":"b","b":"a"}}`
	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", body)
	if code != http.StatusBadRequest {
		t.Fatalf("映射含环应 400，实际 %d: %s", code, resp)
	}
	if !strings.Contains(resp, "cycle") {
		t.Fatalf("错误信息应点明环（便于用户定位自己写的映射），实际 %s", resp)
	}
}

// 非法 status 直接 400，不当成 0 静默写库；缺 name/base_url 也要拦。
func TestChannelsRejectsBadInput(t *testing.T) {
	h := chanHandler(t, chanStore(t))
	if code, _ := doJSON(t, h, http.MethodPost, "/admin/channels", `{"name":"","base_url":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("缺 name 应 400，实际 %d", code)
	}
	if code, _ := doJSON(t, h, http.MethodPost, "/admin/channels", `{"name":"n"}`); code != http.StatusBadRequest {
		t.Fatalf("缺 base_url 应 400，实际 %d", code)
	}
	code, _ := doJSON(t, h, http.MethodPost, "/admin/channels", `{"name":"n","base_url":"https://a/v1"}`)
	if code != http.StatusOK {
		t.Fatalf("合法请求应 200，实际 %d", code)
	}
	_, body := doJSON(t, h, http.MethodGet, "/admin/channels", "")
	var list struct {
		Channels []struct {
			ID string `json:"id"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list.Channels) != 1 {
		t.Fatalf("应有一个渠道，实际 %s", body)
	}
	code, _ = doJSON(t, h, http.MethodPost, "/admin/channels/status", `{"id":"`+list.Channels[0].ID+`","status":9}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status=9 应 400，实际 %d", code)
	}
}
