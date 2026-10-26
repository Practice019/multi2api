// channels_parity_test.go —— B3：渠道管理端点对齐 hyxapi 的**逐条判据**。
//
// # 为什么这些断言落在 Go 层（而不是只靠端到端）
//
// 端到端（隔离实例 + curl/浏览器）能证明"这条链路通"，但证明不了**边界**：
// 分页上限、状态字典、fail-closed 的字段分类、批量删除里"没找到的 id"、
// 优先级重排后的确定顺序 —— 这些正是"点了没反应/少删了一条"这类故障的藏身处。
//
// # 与既有测试的分工（不要重复）
//
//	channels_sql_boundary_test.go —— 明文边界（**不许改**，本文件不碰它）
//	channels_page_test.go         —— T13 的列表分页/筛选（本文件只补 B3 新增的维度）
//	channels_keys_test.go         —— 单把 key 的三个动作（本文件补批量）
//	本文件                        —— B3 的 14 组新行为 + 字段分类表的守卫
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
)

// ── 测试底座 ───────────────────────────────────────────────────────────────

// parityProvider 一个可控的假上游：只实现 gateway.Provider 的四件套。
//
// 与 checkin_all_test.go 的 plainProvider 同一取舍（假上游越小，测的东西越清楚）：
// 本文件要验的是"渠道端点在拿到上游目录后怎么算/怎么落库"，不是"上游怎么回答"。
type parityProvider struct {
	models []string
	err    error
	delay  time.Duration
	calls  int
}

func (p *parityProvider) ID() string               { return "parity" }
func (p *parityProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *parityProvider) Chat(_ context.Context, _ gateway.Credential, _ []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{Status: http.StatusNotImplemented, Body: io.NopCloser(strings.NewReader(""))}, nil
}
func (p *parityProvider) Models(_ context.Context, _ gateway.Credential) ([]gateway.ModelInfo, error) {
	p.calls++
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	if p.err != nil {
		return nil, p.err
	}
	out := make([]gateway.ModelInfo, 0, len(p.models))
	for _, m := range p.models {
		out = append(out, gateway.ModelInfo{ID: m})
	}
	return out, nil
}

// parityHandler 起一套带假上游的管理台。
func parityHandler(t *testing.T, p gateway.Provider) (*Handler, *channelstore.Store) {
	t.Helper()
	s := chanStore(t)
	cfg := Config{
		AdminToken: testAdminToken, ListensLoopback: true,
		Channels: s,
	}
	if p != nil {
		cfg.ChannelProvider = func(id, baseURL string) gateway.Provider { return p }
	}
	return New(cfg), s
}

// doJSONMap 发请求并把响应解成 map（测试里大量只需要"某个字段是什么"）。
func doJSONMap(t *testing.T, h *Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	code, raw := doJSON(t, h, method, path, body)
	var m map[string]any
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return code, m
}

// parityCreate 建一条渠道并回它的外部 id。
func parityCreate(t *testing.T, h *Handler, payload map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(payload)
	code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels", string(raw))
	if code != http.StatusOK {
		t.Fatalf("建渠道应 200，实际 %d: %v", code, out)
	}
	ch, _ := out["channel"].(map[string]any)
	id, _ := ch["id"].(string)
	if id == "" {
		t.Fatalf("建渠道响应里没有 channel.id: %v", out)
	}
	return id
}

// ── 1) 字段分类表的守卫（fail-closed 的"忘记登记必须红"）───────────────────

// TestChannelRequestFieldsAreAllRegistered 是**守卫的守卫**：
// channelReq 上新增一个字段却忘了登记分类表 ⇒ 这条必须红。
//
// 为什么它比"运行期 fail-closed"更靠前：把未登记字段拒掉只能保护**运行期**，
// 而"新字段压根没登记"这件事本身会在开发期被忽略 —— 直到用户来报"保存了没反应"。
func TestChannelRequestFieldsAreAllRegistered(t *testing.T) {
	fields := channelRequestJSONFields()
	if len(fields) == 0 {
		t.Fatal("反射没拿到任何字段 —— 守卫本身失效了（比漏登记更糟）")
	}
	known := map[string]bool{}
	for _, f := range fields {
		known[f] = true
		if channelFieldRegistry[f] == "" {
			t.Errorf("★ channelReq 的字段 %q **没有登记**在 channelFieldRegistry —— "+
				"新增/改名字段必须同步登记；否则运行期它会被 fail-closed 拒掉（用户看到的是「保存了没反应」）", f)
		}
	}
	// 反向：登记表里的可改/敏感字段必须真的还在结构体上（改名后留下死登记同样要抓）。
	for name, class := range channelFieldRegistry {
		if class == channelFieldServerManaged {
			continue
		}
		if !known[name] {
			t.Errorf("登记表里的 %q（%s）不在 channelReq 上 —— 字段改名/删除后忘了同步登记表", name, class)
		}
	}
	// 钉住"哪些是敏感的"：这三个是凭据/上游地址，回显与日志都不该带它们的值。
	for _, want := range []string{"keys", "base_url", "model_mapping"} {
		if got := channelFieldRegistry[want]; got != channelFieldSensitive {
			t.Errorf("%q 的分类 = %q，want %q", want, got, channelFieldSensitive)
		}
	}
}

// TestChannelsUpsertRejectsUnregisteredFields fail-closed：未登记与服务端维护字段都 400。
func TestChannelsUpsertRejectsUnregisteredFields(t *testing.T) {
	h, _ := parityHandler(t, nil)

	// ① 未登记字段（bogus_field）⇒ 400 且点名它
	body := `{"name":"x","base_url":"https://x.example.invalid/v1","bogus_field":1}`
	code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels", body)
	if code != http.StatusBadRequest {
		t.Fatalf("未登记字段应 400（fail-closed），实际 %d: %v", code, out)
	}
	rej, _ := out["rejected_fields"].([]any)
	if len(rej) != 1 || rej[0] != "bogus_field" {
		t.Errorf("rejected_fields 应点名 bogus_field，实际 %v", out["rejected_fields"])
	}

	// ② 服务端维护字段（balance）⇒ 400，且报错里点名它
	body = `{"name":"x","base_url":"https://x.example.invalid/v1","balance":12.5}`
	code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels", body)
	if code != http.StatusBadRequest {
		t.Fatalf("客户端直写 balance 应 400，实际 %d: %v", code, out)
	}
	if s, _ := out["error"].(string); !strings.Contains(s, "balance") {
		t.Errorf("报错应点名 balance，实际 %q", s)
	}

	// ③ 合法字段照旧通过（别把 fail-closed 做成"什么都不能存"）
	id := parityCreate(t, h, map[string]any{
		"name": "ok-ch", "base_url": "https://ok.example.invalid/v1",
		"keys": []string{"sk-ok-1"}, "models": []string{"m1"},
	})
	if id == "" {
		t.Fatal("合法请求应能建渠道")
	}
}

// ── 2) 新增三模式（single / batch / multi_to_single）────────────────────────

func TestChannelsUpsertCreateModes(t *testing.T) {
	h, _ := parityHandler(t, nil)

	// ① single：keys 都挂在同一条渠道上（既有行为）
	single := parityCreate(t, h, map[string]any{
		"name": "single-ch", "base_url": "https://s.example.invalid/v1",
		"keys": []string{"sk-s1", "sk-s2"},
	})
	code, out := doJSONMap(t, h, http.MethodGet, "/admin/channels/keys?id="+single, "")
	if code != http.StatusOK || int(out["key_count"].(float64)) != 2 {
		t.Fatalf("single 应把 2 把 key 挂在一条渠道上，实际 %d %v", code, out["key_count"])
	}

	// ② batch：**一行一个 key 建一条**（数组元素里带换行也要拆）
	raw, _ := json.Marshal(map[string]any{
		"mode": "batch", "name": "batch-ch", "base_url": "https://b.example.invalid/v1",
		"keys": []string{"sk-b1\nsk-b2", "sk-b3"},
	})
	code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels", string(raw))
	if code != http.StatusOK {
		t.Fatalf("batch 新增应 200，实际 %d: %v", code, out)
	}
	chs, _ := out["channels"].([]any)
	if len(chs) != 3 {
		t.Fatalf("batch 应建 3 条（2 个元素拆成 3 把 key），实际 %d", len(chs))
	}
	if got := out["mode"]; got != "batch" {
		t.Errorf("响应 mode=%v want batch", got)
	}
	for _, c := range chs {
		m, _ := c.(map[string]any)
		if kc, _ := m["key_count"].(float64); kc != 1 {
			t.Errorf("batch 每条渠道应只有 1 把 key，实际 %v（%v）", m["key_count"], m["name"])
		}
	}

	// ③ multi_to_single：多把 key 合成**一条**
	raw, _ = json.Marshal(map[string]any{
		"mode": "multi_to_single", "name": "merged-ch", "base_url": "https://m.example.invalid/v1",
		"keys": []string{"sk-m1", "sk-m2", "sk-m3"},
	})
	code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels", string(raw))
	if code != http.StatusOK {
		t.Fatalf("multi_to_single 应 200，实际 %d: %v", code, out)
	}
	ch, _ := out["channel"].(map[string]any)
	if kc, _ := ch["key_count"].(float64); kc != 3 {
		t.Errorf("multi_to_single 应合成一条 3 key 渠道，实际 %v", ch["key_count"])
	}

	// ④ 非法 mode ⇒ 400（不是静默按 single 处理）
	code, _ = doJSONMap(t, h, http.MethodPost, "/admin/channels",
		`{"mode":"nope","name":"x","base_url":"https://x.example.invalid/v1"}`)
	if code != http.StatusBadRequest {
		t.Errorf("非法 mode 应 400，实际 %d", code)
	}
}

// TestChannelsUpsertBatchKeyPrefixName 批量新增时把 key 的**掩码**前缀拼进名称。
func TestChannelsUpsertBatchKeyPrefixName(t *testing.T) {
	h, _ := parityHandler(t, nil)
	raw, _ := json.Marshal(map[string]any{
		"mode": "batch", "name": "pref", "base_url": "https://p.example.invalid/v1",
		"batch_add_set_key_prefix_2_name": true,
		"keys":                            []string{"sk-aaaa1111bbbb"},
	})
	code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels", string(raw))
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %v", code, out)
	}
	chs, _ := out["channels"].([]any)
	m, _ := chs[0].(map[string]any)
	name, _ := m["name"].(string)
	if !strings.HasPrefix(name, "pref ") {
		t.Errorf("名称应带前缀，实际 %q", name)
	}
	// ★ 名称里**不得**出现明文 key：渠道名是长期可见文本（列表/日志/导出）
	if strings.Contains(name, "sk-aaaa1111bbbb") {
		t.Errorf("★ 渠道名里出现了明文 key：%q", name)
	}
}

// ── 3) 更新：key_mode 的 append / replace ──────────────────────────────────

func TestChannelsUpsertKeyModeAppendAndReplace(t *testing.T) {
	h, _ := parityHandler(t, nil)
	id := parityCreate(t, h, map[string]any{
		"name": "km-ch", "base_url": "https://km.example.invalid/v1",
		"keys": []string{"sk-keep", "sk-drop"},
	})
	// 把第二把停用：replace 语义里"同一字符串要保留原状态"才有可验的差别。
	raw, _ := json.Marshal(map[string]any{"id": id, "index": 1, "status": 2})
	if code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/keys/status", string(raw)); code != http.StatusOK {
		t.Fatalf("停用第 2 把 key 应 200，实际 %d: %v", code, out)
	}

	// ① append（缺省）：旧 key 保留，新 key 追加
	raw, _ = json.Marshal(map[string]any{
		"id": id, "name": "km-ch", "base_url": "https://km.example.invalid/v1",
		"keys": []string{"sk-new"},
	})
	if code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels", string(raw)); code != http.StatusOK {
		t.Fatalf("append 更新应 200，实际 %d: %v", code, out)
	}
	_, kv := doJSONMap(t, h, http.MethodGet, "/admin/channels/keys?id="+id, "")
	if kc, _ := kv["key_count"].(float64); kc != 3 {
		t.Fatalf("append 后应有 3 把 key，实际 %v", kv["key_count"])
	}

	// ② replace：只剩请求里给的那把；且**同一字符串的旧 key 保留其停用状态**
	raw, _ = json.Marshal(map[string]any{
		"id": id, "name": "km-ch", "base_url": "https://km.example.invalid/v1",
		"key_mode": "replace", "keys": []string{"sk-drop"},
	})
	if code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels", string(raw)); code != http.StatusOK {
		t.Fatalf("replace 更新应 200，实际 %d: %v", code, out)
	}
	_, kv = doJSONMap(t, h, http.MethodGet, "/admin/channels/keys?id="+id, "")
	if kc, _ := kv["key_count"].(float64); kc != 1 {
		t.Fatalf("replace 后应只剩 1 把 key，实际 %v", kv["key_count"])
	}
	keys, _ := kv["keys"].([]any)
	first, _ := keys[0].(map[string]any)
	if st, _ := first["status"].(float64); int(st) != 2 {
		t.Errorf("★ replace 后同一个 key 应保留它原来的停用状态(2)，实际 %v —— "+
			"否则每次保存都会把上游判废的 key 重新启用", first["status"])
	}
	// ③ 非法 key_mode ⇒ 400
	raw, _ = json.Marshal(map[string]any{
		"id": id, "name": "km-ch", "base_url": "https://km.example.invalid/v1", "key_mode": "wipe",
	})
	if code, _ := doJSONMap(t, h, http.MethodPost, "/admin/channels", string(raw)); code != http.StatusBadRequest {
		t.Errorf("非法 key_mode 应 400，实际 %d", code)
	}
}

// ── 4) 列表：新增的筛选维度与排序 ──────────────────────────────────────────

// TestChannelsListGroupTagModeAndSort B3 新增的三个维度：group / tag_mode / sort_by。
func TestChannelsListGroupTagModeAndSort(t *testing.T) {
	h, _ := parityHandler(t, nil)
	a := parityCreate(t, h, map[string]any{"name": "aaa", "base_url": "https://a.example.invalid/v1",
		"group": "vip", "priority": 1, "models": []string{"m-a"}})
	parityCreate(t, h, map[string]any{"name": "bbb", "base_url": "https://b.example.invalid/v1",
		"group": "default", "priority": 9, "models": []string{"m-b"}})
	c := parityCreate(t, h, map[string]any{"name": "ccc", "base_url": "https://c.example.invalid/v1",
		"group": "default", "priority": 5, "models": []string{"m-c"}})

	// ① group：只命中 vip
	if got := parityIDs(t, h, "?group=vip"); len(got) != 1 || got[0] != a {
		t.Errorf("group=vip 应只命中 1 条（%s），实际 %v", a, got)
	}

	// ② sort_by=name&sort_order=asc ⇒ 第一条是 aaa；desc ⇒ 第一条是 ccc
	nameAsc := parityIDs(t, h, "?sort_by=name&sort_order=asc")
	if len(nameAsc) != 3 || nameAsc[0] != a {
		t.Errorf("sort_by=name&asc 应 aaa 打头，实际 %v", nameAsc)
	}
	nameDesc := parityIDs(t, h, "?sort_by=name&sort_order=desc")
	if len(nameDesc) != 3 || nameDesc[0] != c {
		t.Errorf("sort_by=name&desc 应 ccc 打头，实际 %v", nameDesc)
	}
	// ③ sort_by=priority（缺省降序）⇒ 优先级 9 的 bbb 打头
	if got := parityIDs(t, h, "?sort_by=priority"); len(got) == 0 || got[0] == a {
		t.Errorf("sort_by=priority 缺省降序应高优先级打头，实际 %v", got)
	}
	// ④ 不认识的 sort_by ⇒ 回落缺省（不报错）
	if got := parityIDs(t, h, "?sort_by=drop_table"); len(got) != 3 {
		t.Errorf("非法 sort_by 应回落缺省顺序，实际 %v", got)
	}

	// ⑤ tag_mode：先给 a 打标签，再按 tag_mode 精确匹配
	raw, _ := json.Marshal(map[string]any{"ids": []string{a}, "tag": "prod"})
	if code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/tag/batch", string(raw)); code != http.StatusOK {
		t.Fatalf("打标签应 200，实际 %d: %v", code, out)
	}
	if got := parityIDs(t, h, "?tag_mode=1&keyword=prod"); len(got) != 1 || got[0] != a {
		t.Errorf("tag_mode=1&keyword=prod 应只命中带该标签的 1 条，实际 %v", got)
	}
	// tag_mode 关掉时 keyword 是模糊搜索 ⇒ prod 不该命中任何渠道名
	if got := parityIDs(t, h, "?keyword=prod"); len(got) != 0 {
		t.Errorf("tag_mode 关时 keyword=prod 不该命中（没有渠道名含 prod），实际 %v", got)
	}
	// ⑥ tags 清单与 tag/models
	code, out := doJSONMap(t, h, http.MethodGet, "/admin/channels/tags", "")
	if code != http.StatusOK {
		t.Fatalf("tags 应 200，实际 %d", code)
	}
	tags, _ := out["tags"].([]any)
	if len(tags) != 1 || tags[0] != "prod" {
		t.Errorf("tags 应只有 prod，实际 %v", tags)
	}
	code, out = doJSONMap(t, h, http.MethodGet, "/admin/channels/tag/models?tag=prod", "")
	if code != http.StatusOK {
		t.Fatalf("tag/models 应 200，实际 %d", code)
	}
	ms, _ := out["models"].([]any)
	if len(ms) != 1 || ms[0] != "m-a" {
		t.Errorf("tag/models 应是该 tag 下渠道声明的模型并集 [m-a]，实际 %v", ms)
	}
	// ⑦ 标签三连的 编辑：把带 prod 的渠道 priority 改成 42
	raw, _ = json.Marshal(map[string]any{"tag": "prod", "priority": 42})
	if code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/tag/edit", string(raw)); code != http.StatusOK {
		t.Fatalf("tag/edit 应 200，实际 %d: %v", code, out)
	}
	code, out = doJSONMap(t, h, http.MethodGet, "/admin/channels/order", "")
	order, _ := out["order"].([]any)
	first, _ := order[0].(map[string]any)
	if code != http.StatusOK || first["id"] != a || int(first["priority"].(float64)) != 42 {
		t.Errorf("tag/edit 后 a 应是优先级最高(42)，实际 %v", order)
	}
	// ⑧ 标签三连的 停用（按 tag）
	raw, _ = json.Marshal(map[string]any{"tag": "prod", "status": 2})
	if code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/tag/status", string(raw)); code != http.StatusOK {
		t.Fatalf("tag/status 应 200，实际 %d: %v", code, out)
	} else if int(out["changed"].(float64)) != 1 {
		t.Errorf("tag/status 应报 changed=1（真的变了），实际 %v", out["changed"])
	}
	// ⑨ 再停一次：changed 必须是 0（"点了没反应"与"本来就是那个状态"要能分开）
	raw, _ = json.Marshal(map[string]any{"tag": "prod", "status": 2})
	_, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/tag/status", string(raw))
	if int(out["changed"].(float64)) != 0 {
		t.Errorf("重复停用应报 changed=0，实际 %v", out["changed"])
	}
	_ = c
}

// ── 5) 搜索：五条规则（含 key 全等）────────────────────────────────────────

func TestChannelsSearchFiveRules(t *testing.T) {
	h, _ := parityHandler(t, nil)
	id := parityCreate(t, h, map[string]any{
		"name": "srch-alpha", "base_url": "https://srch.example.invalid/v1",
		"keys": []string{"sk-search-secret-1"}, "models": []string{"m-search"},
	})

	// ① name 模糊 ② base_url 模糊 ③ models 模糊
	for _, kw := range []string{"srch-alpha", "srch.example", "m-search"} {
		code, out := doJSONMap(t, h, http.MethodGet, "/admin/channels/search?keyword="+kw, "")
		if code != http.StatusOK {
			t.Fatalf("search?keyword=%s 应 200，实际 %d", kw, code)
		}
		if int(out["total"].(float64)) != 1 {
			t.Errorf("keyword=%s 应命中 1 条，实际 %v", kw, out["total"])
		}
	}

	// ④ key **全等** 命中；子串**不该**命中（否则就成了 key 前缀探针）
	code, out := doJSONMap(t, h, http.MethodGet, "/admin/channels/search?keyword=sk-search-secret-1", "")
	if code != http.StatusOK || int(out["total"].(float64)) != 1 {
		t.Errorf("key 全等应命中 1 条，实际 %d %v", code, out["total"])
	}
	_, out = doJSONMap(t, h, http.MethodGet, "/admin/channels/search?keyword=sk-search", "")
	if int(out["total"].(float64)) != 0 {
		t.Errorf("★ key 只做全等匹配：子串 sk-search 不该命中，实际 %v", out["total"])
	}

	// ⑤ 响应体**不得**含明文 key（搜索命中 key 也不代表能把 key 发出去）
	_, raw := doJSON(t, h, http.MethodGet, "/admin/channels/search?keyword=sk-search-secret-1", "")
	if strings.Contains(raw, "sk-search-secret-1") {
		t.Errorf("★ 搜索响应体里出现明文 key：%s", raw)
	}

	// 未知关键词 ⇒ 0 条（而不是全量）
	_, out = doJSONMap(t, h, http.MethodGet, "/admin/channels/search?keyword=zzz-nope", "")
	if int(out["total"].(float64)) != 0 {
		t.Errorf("无命中应 0 条，实际 %v", out["total"])
	}
	_ = id
}

// ── 6) 状态：单条与批量（返回真正变化的条数）───────────────────────────────

func TestChannelsStatusSingleAndBatch(t *testing.T) {
	h, _ := parityHandler(t, nil)
	a := parityCreate(t, h, map[string]any{"name": "st-a", "base_url": "https://sa.example.invalid/v1"})
	b := parityCreate(t, h, map[string]any{"name": "st-b", "base_url": "https://sb.example.invalid/v1"})

	// 批量停用两条 ⇒ changed=2
	raw, _ := json.Marshal(map[string]any{"ids": []string{a, b}, "status": 2})
	code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/status-batch", string(raw))
	if code != http.StatusOK || int(out["changed"].(float64)) != 2 {
		t.Fatalf("批量停用应 changed=2，实际 %d %v", code, out)
	}
	// 其中一条已经停用 ⇒ changed=1；并且 missing 要如实回报
	raw, _ = json.Marshal(map[string]any{"ids": []string{a, b, "ch-does-not-exist"}, "status": 2})
	_, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/status-batch", string(raw))
	if int(out["changed"].(float64)) != 0 {
		t.Errorf("两条都已停用 ⇒ changed=0，实际 %v", out["changed"])
	}
	miss, _ := out["missing"].([]any)
	if len(miss) != 1 {
		t.Errorf("不存在的 id 应如实回报 missing，实际 %v", out["missing"])
	}
	// 启用一条 ⇒ changed=1
	raw, _ = json.Marshal(map[string]any{"ids": []string{a}, "status": 1})
	_, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/status-batch", string(raw))
	if int(out["changed"].(float64)) != 1 {
		t.Errorf("启用已停用的那条应 changed=1，实际 %v", out["changed"])
	}
	// 非法 status ⇒ 400
	raw, _ = json.Marshal(map[string]any{"ids": []string{a}, "status": 9})
	if code, _ := doJSONMap(t, h, http.MethodPost, "/admin/channels/status-batch", string(raw)); code != http.StatusBadRequest {
		t.Errorf("非法 status 应 400，实际 %d", code)
	}
}

// ── 7) 删除三连 ────────────────────────────────────────────────────────────

func TestChannelsDeleteTriple(t *testing.T) {
	h, _ := parityHandler(t, nil)
	a := parityCreate(t, h, map[string]any{"name": "del-a", "base_url": "https://da.example.invalid/v1"})
	b := parityCreate(t, h, map[string]any{"name": "del-b", "base_url": "https://db.example.invalid/v1"})
	c := parityCreate(t, h, map[string]any{"name": "del-c", "base_url": "https://dc.example.invalid/v1"})

	// ① 批量按 ids（含一个不存在的）
	raw, _ := json.Marshal(map[string]any{"ids": []string{a, b, "ch-nope"}})
	code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/batch-delete", string(raw))
	if code != http.StatusOK || int(out["deleted"].(float64)) != 2 {
		t.Fatalf("批量删除应 deleted=2，实际 %d %v", code, out)
	}
	if miss, _ := out["missing"].([]any); len(miss) != 1 {
		t.Errorf("missing 应回报 1 个，实际 %v", out["missing"])
	}
	// ② 按 tag 批量删除：先给 c 打标签
	raw, _ = json.Marshal(map[string]any{"ids": []string{c}, "tag": "tmp"})
	doJSONMap(t, h, http.MethodPost, "/admin/channels/tag/batch", string(raw))
	raw, _ = json.Marshal(map[string]any{"tag": "tmp"})
	if code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/batch-delete", string(raw)); code != http.StatusOK ||
		int(out["deleted"].(float64)) != 1 {
		t.Fatalf("按 tag 删除应 deleted=1，实际 %d %v", code, out)
	}
	if ids := parityIDs(t, h, ""); len(ids) != 0 {
		t.Fatalf("删完应一条不剩，实际 %v", ids)
	}
	// ③ 清空已禁用：建两条、禁一条、清空 ⇒ 只剩启用的那条
	keep := parityCreate(t, h, map[string]any{"name": "keep", "base_url": "https://k.example.invalid/v1"})
	gone := parityCreate(t, h, map[string]any{"name": "gone", "base_url": "https://g.example.invalid/v1"})
	raw, _ = json.Marshal(map[string]any{"ids": []string{gone}, "status": 2})
	doJSONMap(t, h, http.MethodPost, "/admin/channels/status-batch", string(raw))
	if code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/clear-disabled", ""); code != http.StatusOK ||
		int(out["deleted"].(float64)) != 1 {
		t.Fatalf("清空已禁用应 deleted=1，实际 %d %v", code, out)
	}
	left := parityIDs(t, h, "")
	if len(left) != 1 || left[0] != keep {
		t.Errorf("清空后应只剩启用的那条，实际 %v", left)
	}
	// ④ ids/tag 都不给 ⇒ 400
	if code, _ := doJSONMap(t, h, http.MethodPost, "/admin/channels/batch-delete", "{}"); code != http.StatusBadRequest {
		t.Errorf("ids/tag 都不给应 400，实际 %d", code)
	}
}

// ── 8) 复制 ────────────────────────────────────────────────────────────────

func TestChannelsCopyAssignsNewExternalID(t *testing.T) {
	h, _ := parityHandler(t, nil)
	src := parityCreate(t, h, map[string]any{
		"name": "copy-src", "base_url": "https://cp.example.invalid/v1",
		"keys": []string{"sk-copy-1"}, "models": []string{"m1"},
	})
	code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/copy", `{"id":"`+src+`"}`)
	if code != http.StatusOK {
		t.Fatalf("复制应 200，实际 %d: %v", code, out)
	}
	newID, _ := out["id"].(string)
	if newID == "" || newID == src {
		t.Fatalf("★ 复制出来的渠道必须有自己的外部 id（否则前端没法寻址），实际 %q（源 %q）", newID, src)
	}
	if rb, ok := out["reset_balance"].(bool); !ok || !rb {
		t.Errorf("reset_balance 缺省应为 true，实际 %v", out["reset_balance"])
	}
	// 复制出来的那条确实存在、名字带后缀、且 key 也复制过去了
	code, list := doJSONMap(t, h, http.MethodGet, "/admin/channels", "")
	items, _ := list["channels"].([]any)
	if code != http.StatusOK || len(items) != 2 {
		t.Fatalf("复制后应有 2 条渠道，实际 %d", len(items))
	}
	found := false
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["id"] == newID {
			found = true
			if name, _ := m["name"].(string); !strings.HasSuffix(name, "_复制") {
				t.Errorf("复制件名字应带 _复制 后缀，实际 %q", name)
			}
			if kc, _ := m["key_count"].(float64); kc != 1 {
				t.Errorf("复制件应带 1 把 key，实际 %v", m["key_count"])
			}
		}
	}
	if !found {
		t.Errorf("列表里找不到复制出来的 %s", newID)
	}
}

// ── 9) 修复能力 ────────────────────────────────────────────────────────────

func TestChannelsFixAbilitiesEndpoint(t *testing.T) {
	h, _ := parityHandler(t, nil)
	parityCreate(t, h, map[string]any{"name": "fix-a", "base_url": "https://f.example.invalid/v1",
		"models": []string{"m1", "m2"}})
	code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/fix-abilities", "")
	if code != http.StatusOK {
		t.Fatalf("fix-abilities 应 200，实际 %d: %v", code, out)
	}
	if _, ok := out["fixed"]; !ok {
		t.Errorf("响应必须带 fixed 条数（只说 ok 没法判断它到底修没修东西），实际 %v", out)
	}
	if _, ok := out["failed"]; !ok {
		t.Errorf("响应必须带 failed 条数，实际 %v", out)
	}
}

// ── 10) 优先级排序三件套 ───────────────────────────────────────────────────

func TestChannelsOrderReorderMove(t *testing.T) {
	h, _ := parityHandler(t, nil)
	a := parityCreate(t, h, map[string]any{"name": "ord-a", "base_url": "https://oa.example.invalid/v1", "priority": 1})
	b := parityCreate(t, h, map[string]any{"name": "ord-b", "base_url": "https://ob.example.invalid/v1", "priority": 2})
	c := parityCreate(t, h, map[string]any{"name": "ord-c", "base_url": "https://oc.example.invalid/v1", "priority": 3})

	orderIDs := func() []string {
		_, out := doJSONMap(t, h, http.MethodGet, "/admin/channels/order", "")
		items, _ := out["order"].([]any)
		ids := make([]string, 0, len(items))
		for _, it := range items {
			m, _ := it.(map[string]any)
			ids = append(ids, fmt.Sprint(m["id"]))
		}
		return ids
	}
	if got := orderIDs(); len(got) != 3 || got[0] != c {
		t.Fatalf("初始顺序应按 priority 降序（c,b,a），实际 %v", got)
	}
	// ① 整体重排：a,b,c
	raw, _ := json.Marshal(map[string]any{"ids": []string{a, b, c}})
	if code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/reorder", string(raw)); code != http.StatusOK {
		t.Fatalf("reorder 应 200，实际 %d: %v", code, out)
	}
	if got := orderIDs(); len(got) != 3 || got[0] != a || got[2] != c {
		t.Errorf("reorder 后顺序应为 a,b,c，实际 %v", got)
	}
	// ② 下移 a ⇒ b,a,c
	raw, _ = json.Marshal(map[string]any{"id": a, "direction": "down"})
	if code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/move", string(raw)); code != http.StatusOK {
		t.Fatalf("move down 应 200，实际 %d: %v", code, out)
	}
	if got := orderIDs(); got[0] != b || got[1] != a {
		t.Errorf("下移后应为 b,a,c，实际 %v", got)
	}
	// ③ 顶端再上移 ⇒ 不是错误，updated=0 + at_edge
	raw, _ = json.Marshal(map[string]any{"id": b, "direction": "up"})
	code, out := doJSONMap(t, h, http.MethodPost, "/admin/channels/move", string(raw))
	if code != http.StatusOK {
		t.Fatalf("在顶端上移不该报错，实际 %d", code)
	}
	if edge, ok := out["at_edge"].(bool); !ok || !edge {
		t.Errorf("在顶端上移应报 at_edge=true，实际 %v", out)
	}
	// ④ 非法 direction ⇒ 400
	raw, _ = json.Marshal(map[string]any{"id": a, "direction": "sideways"})
	if code, _ := doJSONMap(t, h, http.MethodPost, "/admin/channels/move", string(raw)); code != http.StatusBadRequest {
		t.Errorf("非法 direction 应 400，实际 %d", code)
	}
}

// ── 11) 多 Key 的批量动作 ──────────────────────────────────────────────────

func TestChannelsMultiKeyBatchEndpoints(t *testing.T) {
	h, _ := parityHandler(t, nil)
	id := parityCreate(t, h, map[string]any{
		"name": "mk-ch", "base_url": "https://mk.example.invalid/v1",
		"keys": []string{"sk-mk-1", "sk-mk-2", "sk-mk-3"},
	})
	// ① 汇总：3 把全启用
	code, out := doJSONMap(t, h, http.MethodGet, "/admin/channels/keys/summary?id="+id, "")
	if code != http.StatusOK || int(out["enabled"].(float64)) != 3 || int(out["disabled"].(float64)) != 0 {
		t.Fatalf("summary 应 3 启用 / 0 停用，实际 %d %v", code, out)
	}
	// ② 全禁 ⇒ changed=3
	raw, _ := json.Marshal(map[string]any{"id": id, "status": 2})
	if code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/keys/status-all", string(raw)); code != http.StatusOK ||
		int(out["changed"].(float64)) != 3 {
		t.Fatalf("全禁应 changed=3，实际 %d %v", code, out)
	}
	// ③ 再全禁一次 ⇒ changed=0（幂等）
	_, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/keys/status-all", string(raw))
	if int(out["changed"].(float64)) != 0 {
		t.Errorf("重复全禁应 changed=0，实际 %v", out["changed"])
	}
	// ④ 选择模式
	raw, _ = json.Marshal(map[string]any{"id": id, "mode": "round_robin"})
	if code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/keys/mode", string(raw)); code != http.StatusOK {
		t.Fatalf("keys/mode 应 200，实际 %d: %v", code, out)
	}
	if out["key_selection_mode"] != "round_robin" {
		t.Errorf("选择模式应为 round_robin，实际 %v", out["key_selection_mode"])
	}
	// ⑤ 先启用第 0 把，再"删所有已禁用" ⇒ 只删掉 2 把，剩下 1 把
	raw, _ = json.Marshal(map[string]any{"id": id, "index": 0, "status": 1})
	doJSONMap(t, h, http.MethodPost, "/admin/channels/keys/status", string(raw))
	raw, _ = json.Marshal(map[string]any{"id": id})
	if code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/keys/delete-disabled", string(raw)); code != http.StatusOK ||
		int(out["removed"].(float64)) != 2 {
		t.Fatalf("删已禁用应 removed=2，实际 %d %v", code, out)
	}
	if kc, _ := out["key_count"].(float64); kc != 1 {
		t.Errorf("删完应剩 1 把 key，实际 %v", out["key_count"])
	}
	// ⑥ 清空后：全启/全禁 不该报错
	raw, _ = json.Marshal(map[string]any{"id": id, "status": 2, "index": 0})
	doJSONMap(t, h, http.MethodPost, "/admin/channels/keys/status", string(raw))
	raw, _ = json.Marshal(map[string]any{"id": id})
	if code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/keys/delete-disabled", string(raw)); code != http.StatusOK ||
		int(out["key_count"].(float64)) != 0 {
		t.Errorf("删光后 key_count 应为 0，实际 %d %v", code, out["key_count"])
	}
}

// ── 12) 上游侧：测试渠道 / 余额 / 模型变更 ─────────────────────────────────

func TestChannelsUpstreamTestBalanceAndModelDiff(t *testing.T) {
	p := &parityProvider{models: []string{"m-a", "m-b"}}
	h, _ := parityHandler(t, p)
	id := parityCreate(t, h, map[string]any{
		"name": "up-ch", "base_url": "https://up.example.invalid/v1",
		"keys": []string{"sk-up-1"}, "models": []string{"m-a", "m-x"},
	})
	// postID 发一条"只带这个 id"的请求（用 %q 构造 JSON，避免在测试里写裸字符串字面量）
	postID := func(p0 string) (int, map[string]any) {
		return doJSONMap(t, h, http.MethodPost, p0, fmt.Sprintf("{\"id\":%q}", id))
	}
	// 取列表里那条渠道（用来断言 test_time / balance 这些**落库**的列）
	rowOf := func() map[string]any {
		_, list := doJSONMap(t, h, http.MethodGet, "/admin/channels", "")
		items, _ := list["channels"].([]any)
		if len(items) == 0 {
			t.Fatal("列表里应有一条渠道")
		}
		m, _ := items[0].(map[string]any)
		return m
	}

	// ① 测试渠道：连通 ⇒ ok + models 条数，并把 test_time/response_time 写回去
	code, out := postID("/admin/channels/test")
	if code != http.StatusOK {
		t.Fatalf("test 应 200，实际 %d: %v", code, out)
	}
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("单条测试应回 1 条结果，实际 %v", results)
	}
	r0, _ := results[0].(map[string]any)
	if okv, _ := r0["ok"].(bool); !okv {
		t.Fatalf("假上游返回成功，测试应 ok=true，实际 %v", r0)
	}
	if n, _ := r0["models"].(float64); int(n) != 2 {
		t.Errorf("测试应报告上游 2 个模型，实际 %v", r0["models"])
	}
	if m := rowOf(); func() bool { tt, _ := m["test_time"].(float64); return tt <= 0 }() {
		t.Errorf("测试后 test_time 应被写回（>0），实际 %v", rowOf()["test_time"])
	}

	// ② 余额：本仓渠道类型没有余额接口 ⇒ supported:false，且**一个字节都不写**
	code, out = postID("/admin/channels/balance")
	if code != http.StatusOK {
		t.Fatalf("balance 应 200，实际 %d: %v", code, out)
	}
	if sup, _ := out["supported"].(bool); sup {
		t.Errorf("OpenAI 兼容渠道不该声称支持余额查询，实际 supported=true")
	}
	m := rowOf()
	if b, _ := m["balance"].(float64); b != 0 {
		t.Errorf("★ 不该把「不支持」写成 0（或任何值），实际 balance=%v", m["balance"])
	}
	if but, _ := m["balance_updated_time"].(float64); but != 0 {
		t.Errorf("余额未支持时不该更新 balance_updated_time，实际 %v", but)
	}

	// ③ 模型变更检测：上游 [m-a,m-b]，声明 [m-a,m-x] ⇒ added=[m-b] removed=[m-x]
	code, out = postID("/admin/channels/models/check")
	if code != http.StatusOK {
		t.Fatalf("models/check 应 200，实际 %d: %v", code, out)
	}
	results, _ = out["results"].([]any)
	r0, _ = results[0].(map[string]any)
	added, _ := r0["added"].([]any)
	removed, _ := r0["removed"].([]any)
	if len(added) != 1 || added[0] != "m-b" || len(removed) != 1 || removed[0] != "m-x" {
		t.Errorf("added/removed 应为 [m-b]/[m-x]，实际 %v / %v", added, removed)
	}
	// 检查端点**只报不改**
	ms, _ := rowOf()["models"].([]any)
	if len(ms) != 2 || ms[0] != "m-a" {
		t.Errorf("models/check 不该改渠道声明，实际 %v", ms)
	}

	// ④ 应用变更：overwrite ⇒ 渠道声明变成上游那一份
	raw, _ := json.Marshal(map[string]any{"id": id, "mode": "overwrite"})
	if code, out = doJSONMap(t, h, http.MethodPost, "/admin/channels/models/apply", string(raw)); code != http.StatusOK ||
		int(out["applied"].(float64)) != 1 {
		t.Fatalf("models/apply overwrite 应 applied=1，实际 %d %v", code, out)
	}
	if ms, _ = rowOf()["models"].([]any); len(ms) != 2 {
		t.Fatalf("覆盖后应有 2 个模型，实际 %v", ms)
	}

	// ⑤ add 模式：上游多一个 m-c ⇒ 只并进来
	p.models = []string{"m-a", "m-b", "m-c"}
	raw, _ = json.Marshal(map[string]any{"id": id, "mode": "add"})
	doJSONMap(t, h, http.MethodPost, "/admin/channels/models/apply", string(raw))
	if ms, _ = rowOf()["models"].([]any); len(ms) != 3 {
		t.Errorf("add 模式后应有 3 个模型，实际 %v", ms)
	}

	// ⑥ 上游整体失败 ⇒ 端点本身仍 200、结果里 ok=false，且**不**清空声明
	p.err = fmt.Errorf("upstream boom")
	code, out = postID("/admin/channels/test")
	if code != http.StatusOK {
		t.Fatalf("上游失败时端点本身仍应 200（结果里带错误），实际 %d", code)
	}
	results, _ = out["results"].([]any)
	r0, _ = results[0].(map[string]any)
	if okv, _ := r0["ok"].(bool); okv {
		t.Errorf("上游失败时 ok 应为 false，实际 %v", r0)
	}
	if ms, _ = rowOf()["models"].([]any); len(ms) != 3 {
		t.Errorf("上游失败不该改动渠道声明，实际 %v", ms)
	}

	// ⑦ 非法 mode ⇒ 400
	raw, _ = json.Marshal(map[string]any{"id": id, "mode": "nuke"})
	if code, _ := doJSONMap(t, h, http.MethodPost, "/admin/channels/models/apply", string(raw)); code != http.StatusBadRequest {
		t.Errorf("非法 apply mode 应 400，实际 %d", code)
	}
}

// parityIDs 从列表响应里取全部渠道 id（顺序即列表顺序）。
func parityIDs(t *testing.T, h *Handler, query string) []string {
	t.Helper()
	code, out := doJSONMap(t, h, http.MethodGet, "/admin/channels"+query, "")
	if code != http.StatusOK {
		t.Fatalf("列表 %s 应 200，实际 %d", query, code)
	}
	items, _ := out["channels"].([]any)
	ids := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		if id, _ := m["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
