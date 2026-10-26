// channels_sql_boundary_test.go —— B1 的**黑盒**边界守卫（只管一条：明文不许从列表/详情漏出去）。
//
// # 为什么要有它（比包内断言更接近真实风险）
//
// channelstore 包内的断言只能证明"我返回的对象里没有明文"，证明不了
// "HTTP 响应体里没有明文" —— 后者才是用户/攻击者能看到的东西，而它取决于
// 上游序列化路径（viewOf / keys 面板 / 错误体…）。所以这一组直接打管理面端点，
// 逐字节检查响应体。
//
// # 四条判据（主脑口径）
//
//	① 列表响应体不含明文 key
//	② 详情/Keys 面板（默认）响应体不含明文 key
//	③ 只有显式入口含明文：POST /admin/channels/reveal 与 GET keys?reveal=1
//	④ 用 ch-xxxx 外部 id 走完「编辑 / 停用 / 管理 Key / 删除」四条链路都不 404
//
// ⚠ 本文件只**新建**，不改 admin 里任何既有文件（那是别人的地盘）。用到的
// doJSON / testAdminToken 由同包既有测试文件提供，这里只读取不修改。
package admin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/store"
)

// sqlProbeKey 是贯穿本文件的"明文指纹"：只要它在响应体里出现，就是漏了一次。
const sqlProbeKey = "sk-b1-boundary-secret-0001"

// sqlBoundaryHarness 起一套**跑在 SQL 上**的管理台：临时库 + channelstore + Handler。
//
// 为什么不复用 chanStore(t)：那个 helper 建的是"JSON 时代的"存储（不打开 DB），
// 而 B1 之后 New 要求库已打开。这里自带一套，避免改别人的文件。
func sqlBoundaryHarness(t *testing.T) *Handler {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("channelstore.New: %v", err)
	}
	cfg := Config{AdminToken: testAdminToken, ListensLoopback: true}
	cfg.Channels = s
	return New(cfg)
}

// sqlBoundaryCreate 建一条带明文 key 的渠道，返回它的外部 id。
func sqlBoundaryCreate(t *testing.T, h *Handler) string {
	t.Helper()
	body := map[string]any{
		"name": "B1边界渠道", "type": 0, "base_url": "https://b1.example.invalid/v1",
		"keys": []string{sqlProbeKey}, "models": []string{"m-b1"},
	}
	raw, _ := json.Marshal(body)
	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", string(raw))
	if code != http.StatusOK {
		t.Fatalf("建渠道应 200，实际 %d: %s", code, resp)
	}
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.Channel.ID == "" {
		t.Fatalf("建渠道响应里没有 id: %v (%s)", err, resp)
	}
	return out.Channel.ID
}

// TestB1ListAndKeysNeverLeakPlaintext 判据 ①②③：列表与默认 Keys 面板绝不带明文；只有显式入口带。
func TestB1ListAndKeysNeverLeakPlaintext(t *testing.T) {
	h := sqlBoundaryHarness(t)
	id := sqlBoundaryCreate(t, h)

	// ① 列表（30 秒轮询的那个接口）：不得出现明文，但要有掩码。
	code, list := doJSON(t, h, http.MethodGet, "/admin/channels", "")
	if code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", code)
	}
	if strings.Contains(list, sqlProbeKey) {
		t.Fatalf("★ 列表响应体里有明文 key：%s", list)
	}
	if !strings.Contains(list, "key_masks") {
		t.Fatalf("列表应回掩码字段：%s", list)
	}

	// ② Keys 面板默认（不带 reveal=1）：不得出现明文。
	code, keys := doJSON(t, h, http.MethodGet, "/admin/channels/keys?id="+id, "")
	if code != http.StatusOK {
		t.Fatalf("keys 面板应 200，实际 %d: %s", code, keys)
	}
	if strings.Contains(keys, sqlProbeKey) {
		t.Fatalf("★ Keys 面板默认响应里有明文：%s", keys)
	}
	if !strings.Contains(keys, `revealed":false`) {
		t.Fatalf("默认应标 revealed:false：%s", keys)
	}

	// ③ 两个显式入口**必须**能看到明文（否则功能被误关）。
	code, revealed := doJSON(t, h, http.MethodPost, "/admin/channels/reveal", `{"id":"`+id+`"}`)
	if code != http.StatusOK || !strings.Contains(revealed, sqlProbeKey) {
		t.Fatalf("reveal 必须回明文（这是显式入口），实际 %d: %s", code, revealed)
	}
	code, keysReveal := doJSON(t, h, http.MethodGet, "/admin/channels/keys?id="+id+"&reveal=1", "")
	if code != http.StatusOK || !strings.Contains(keysReveal, sqlProbeKey) {
		t.Fatalf("keys?reveal=1 必须回明文，实际 %d: %s", code, keysReveal)
	}
	if !strings.Contains(keysReveal, `revealed":true`) {
		t.Fatalf("reveal=1 应标 revealed:true：%s", keysReveal)
	}
}

// TestB1ExternalIDRoundTrip 判据 ④：ch-xxxx 走完编辑 / 停用 / 管理 Key / 删除，四条链路都不 404。
//
// 这条守的是 B1 最容易静默坏掉的地方：字符串 id ⇄ int64 主键的翻译。
// 任何一处漏翻译，表现就是"界面上的操作点了没反应"（404），而不是报错。
func TestB1ExternalIDRoundTrip(t *testing.T) {
	h := sqlBoundaryHarness(t)
	id := sqlBoundaryCreate(t, h)
	if !strings.HasPrefix(id, "ch-") {
		t.Fatalf("外部 id 应仍是 ch- 形状（admin 生成），实际 %q", id)
	}

	// 编辑：带同一个 id 走一次更新（改名 + 改模型）。
	edit := map[string]any{
		"id": id, "name": "B1边界渠道·改", "base_url": "https://b1.example.invalid/v1",
		"models": []string{"m-b1", "m-b2"},
	}
	raw, _ := json.Marshal(edit)
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", string(raw)); code != http.StatusOK {
		t.Fatalf("编辑应 200，实际 %d: %s", code, resp)
	}
	// 停用：1 → 2（走 status 端点，内部委托 store.SetChannelStatus）。
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/status",
		`{"id":"`+id+`","status":2}`); code != http.StatusOK {
		t.Fatalf("停用应 200，实际 %d: %s", code, resp)
	}
	// 恢复启用。
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/status",
		`{"id":"`+id+`","status":1}`); code != http.StatusOK {
		t.Fatalf("恢复启用应 200，实际 %d: %s", code, resp)
	}
	// 管理 Key：加一把 → 停用第 0 把 → 删掉第 0 把。三条都按**外部 id** 定位。
	raw, _ = json.Marshal(map[string]any{"id": id, "keys": []string{"sk-b1-second-0002"}})
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/keys/add", string(raw)); code != http.StatusOK {
		t.Fatalf("加 Key 应 200，实际 %d: %s", code, resp)
	}
	raw, _ = json.Marshal(map[string]any{"id": id, "index": 0, "status": 2})
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/keys/status", string(raw)); code != http.StatusOK {
		t.Fatalf("停用 Key 应 200，实际 %d: %s", code, resp)
	}
	raw, _ = json.Marshal(map[string]any{"id": id, "index": 0})
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/keys/delete", string(raw)); code != http.StatusOK {
		t.Fatalf("删 Key 应 200，实际 %d: %s", code, resp)
	}
	// 删除渠道。
	raw, _ = json.Marshal(map[string]any{"id": id})
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/delete", string(raw)); code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d: %s", code, resp)
	}
	// 删完必须真的没了（否则说明删的是另一个 id —— 翻译错了但恰好没报错）。
	_, after := doJSON(t, h, http.MethodGet, "/admin/channels", "")
	if strings.Contains(after, id) {
		t.Fatalf("删除后列表里仍有 %s：%s", id, after)
	}
}
