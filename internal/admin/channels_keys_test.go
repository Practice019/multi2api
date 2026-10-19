// channels_keys_test.go /admin/channels/keys 系列端点的守卫。
//
// 用户的原话：「我需要保证可以一个渠道多 apikey、多 apikey 轮询使用、apikey 不能用了
// 自动禁用、有一个 apikey 管理可以手动禁用启用」。这个文件守后半段（手动管理）。
//
// 四条要钉住的：
//
//	① GET 列表回掩码、状态、原因 —— **绝不回明文**（列表是会被反复刷新的）
//	② ★ index=0 必须能操作（后端用 *int 接收 index，就是防"0 被当成没传"）
//	③ 追加去重（重复 key 会让轮询连续选中同一条）
//	④ 手动只能设 1/2；3（自动停用）是系统写的，人手设它没有意义
package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// chanKeysCreate 建一条带两个 key 的渠道，回 id。
func chanKeysCreate(t *testing.T, h *Handler, extra string) string {
	t.Helper()
	body := `{"name":"多key渠道","base_url":"https://a.example.com/v1","models":["m1"],` +
		`"keys":["sk-first-0001","sk-second-0002"]` + extra + `}`
	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", body); code != http.StatusOK {
		t.Fatalf("建渠道失败 %d: %s", code, resp)
	}
	code, resp := doJSON(t, h, http.MethodGet, "/admin/channels", "")
	if code != http.StatusOK {
		t.Fatalf("列表失败 %d: %s", code, resp)
	}
	var list struct {
		Channels []struct {
			ID string `json:"id"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(resp), &list); err != nil || len(list.Channels) == 0 {
		t.Fatalf("解析列表失败: %v (%s)", err, resp)
	}
	return list.Channels[len(list.Channels)-1].ID
}

func TestChannelsKeysListMasksAndMetadata(t *testing.T) {
	s := chanStore(t)
	h := chanHandler(t, s)
	id := chanKeysCreate(t, h, `,"key_selection_mode":"round_robin"`)

	code, resp := doJSON(t, h, http.MethodGet, "/admin/channels/keys?id="+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET keys 应 200，实际 %d: %s", code, resp)
	}
	// ① 绝不泄露明文
	if strings.Contains(resp, "sk-first-0001") || strings.Contains(resp, "sk-second-0002") {
		t.Fatalf("★ 列表泄露了明文 key: %s", resp)
	}
	var kv struct {
		KeyCount         int    `json:"key_count"`
		KeySelectionMode string `json:"key_selection_mode"`
		Keys             []struct {
			Index  int    `json:"index"`
			Mask   string `json:"mask"`
			Status int    `json:"status"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(resp), &kv); err != nil {
		t.Fatalf("解析失败: %v (%s)", err, resp)
	}
	if kv.KeyCount != 2 || len(kv.Keys) != 2 {
		t.Fatalf("应有 2 个 key，实际 key_count=%d len=%d", kv.KeyCount, len(kv.Keys))
	}
	if kv.KeySelectionMode != "round_robin" {
		t.Fatalf("选择模式应能读回 round_robin，实际 %q", kv.KeySelectionMode)
	}
	if kv.Keys[0].Mask == "" || kv.Keys[0].Status != 1 {
		t.Fatalf("第一条应是启用 + 有掩码，实际 %+v", kv.Keys[0])
	}
}

// ★ ② index=0 必须能停用/启用（指针判空的回归测试）。
func TestChannelsKeysStatusIndexZero(t *testing.T) {
	s := chanStore(t)
	h := chanHandler(t, s)
	id := chanKeysCreate(t, h, "")

	if code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/keys/status",
		`{"id":"`+id+`","index":0,"status":2}`); code != http.StatusOK {
		t.Fatalf("★ index=0 停用失败 %d: %s", code, resp)
	}
	code, resp := doJSON(t, h, http.MethodGet, "/admin/channels/keys?id="+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET 失败 %d", code)
	}
	var kv struct {
		Keys []struct {
			Index  int    `json:"index"`
			Status int    `json:"status"`
			Reason string `json:"reason"`
		} `json:"keys"`
	}
	_ = json.Unmarshal([]byte(resp), &kv)
	if len(kv.Keys) != 2 || kv.Keys[0].Status != 2 {
		t.Fatalf("第一个 key 应变成手动停用(2)，实际 %+v", kv.Keys)
	}
	if kv.Keys[0].Reason != "manual" {
		t.Fatalf("手动停用应记 reason=manual，实际 %q", kv.Keys[0].Reason)
	}
	if kv.Keys[1].Status != 1 {
		t.Fatalf("第二个 key 不能被连带停用，实际 %+v", kv.Keys[1])
	}

	// 再启用回来
	if code, _ := doJSON(t, h, http.MethodPost, "/admin/channels/keys/status",
		`{"id":"`+id+`","index":0,"status":1}`); code != http.StatusOK {
		t.Fatalf("重新启用失败 %d", code)
	}
	_, resp = doJSON(t, h, http.MethodGet, "/admin/channels/keys?id="+id, "")
	if strings.Contains(resp, `"reason":"manual"`) {
		t.Fatalf("启用后应清空 reason，实际 %s", resp)
	}
}

// ③ 追加去重 + ④ 非法 status 拒绝。
func TestChannelsKeysAddDedupesAndRejectsSystemStatus(t *testing.T) {
	s := chanStore(t)
	h := chanHandler(t, s)
	id := chanKeysCreate(t, h, "")

	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/keys/add",
		`{"id":"`+id+`","keys":["sk-first-0001","sk-third-0003"]}`)
	if code != http.StatusOK {
		t.Fatalf("追加失败 %d: %s", code, resp)
	}
	var added struct {
		Added int `json:"added"`
		Keys  []struct {
			Index int `json:"index"`
		} `json:"keys"`
	}
	_ = json.Unmarshal([]byte(resp), &added)
	if added.Added != 1 {
		t.Fatalf("★ 应只新增 1 个（sk-first 已存在），实际 %d: %s", added.Added, resp)
	}
	if len(added.Keys) != 3 {
		t.Fatalf("应有 3 个 key，实际 %d", len(added.Keys))
	}

	// 人手不能设 3（自动停用）
	if code, _ := doJSON(t, h, http.MethodPost, "/admin/channels/keys/status",
		`{"id":"`+id+`","index":0,"status":3}`); code != http.StatusBadRequest {
		t.Fatalf("人手设 status=3 应被拒（400），实际 %d", code)
	}
	// 越界 index
	if code, _ := doJSON(t, h, http.MethodPost, "/admin/channels/keys/status",
		`{"id":"`+id+`","index":99,"status":1}`); code != http.StatusBadRequest {
		t.Fatalf("越界 index 应被拒，实际 %d", code)
	}
	// 缺 index
	if code, _ := doJSON(t, h, http.MethodPost, "/admin/channels/keys/status",
		`{"id":"`+id+`","status":1}`); code != http.StatusBadRequest {
		t.Fatalf("缺 index 应被拒，实际 %d", code)
	}
}

// 删除：只动指定下标，其余原样。
func TestChannelsKeysDelete(t *testing.T) {
	s := chanStore(t)
	h := chanHandler(t, s)
	id := chanKeysCreate(t, h, "")

	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels/keys/delete",
		`{"id":"`+id+`","index":0}`)
	if code != http.StatusOK {
		t.Fatalf("删除失败 %d: %s", code, resp)
	}
	var after struct {
		Keys []struct {
			Mask string `json:"mask"`
		} `json:"keys"`
	}
	_ = json.Unmarshal([]byte(resp), &after)
	if len(after.Keys) != 1 {
		t.Fatalf("删一个后应剩 1 个，实际 %d", len(after.Keys))
	}
	// 剩下的应该是原来第二个（掩码末尾 0002）
	if !strings.HasSuffix(after.Keys[0].Mask, "0002") {
		t.Fatalf("删掉的应是第一个，剩下那个的掩码应以 0002 结尾，实际 %q", after.Keys[0].Mask)
	}
}

// ★ reveal 的边界：默认只有掩码，显式 reveal=1 才回明文。
//
// 这条守的是安全边界而不是功能：列表端点 /admin/channels 是每 30 秒轮询的，
// 一旦明文混进默认响应，每次轮询都会把全部渠道的 key 发一遍。
// Key 管理窗口不是轮询的（只在打开时请求一次），所以那里可以要明文 —— 用户要"看全"。
func TestChannelsKeysRevealBoundary(t *testing.T) {
	s := chanStore(t)
	h := chanHandler(t, s)
	id := chanKeysCreate(t, h, "")

	code, resp := doJSON(t, h, http.MethodGet, "/admin/channels/keys?id="+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET keys 应 200，实际 %d", code)
	}
	if strings.Contains(resp, "sk-first-0001") {
		t.Fatalf("★ 默认响应不能带明文 key: %s", resp)
	}
	if !strings.Contains(resp, `"revealed":false`) {
		t.Fatalf("默认应标 revealed:false: %s", resp)
	}

	code, resp = doJSON(t, h, http.MethodGet, "/admin/channels/keys?id="+id+"&reveal=1", "")
	if code != http.StatusOK {
		t.Fatalf("reveal=1 应 200，实际 %d", code)
	}
	if !strings.Contains(resp, "sk-first-0001") || !strings.Contains(resp, "sk-second-0002") {
		t.Fatalf("reveal=1 应回完整明文: %s", resp)
	}
	if !strings.Contains(resp, `"revealed":true`) {
		t.Fatalf("reveal=1 应标 revealed:true: %s", resp)
	}
}

// 不存在的渠道 ⇒ 400/404，不崩。
func TestChannelsKeysUnknownChannel(t *testing.T) {
	s := chanStore(t)
	h := chanHandler(t, s)
	if code, _ := doJSON(t, h, http.MethodGet, "/admin/channels/keys?id=ch-nope", ""); code != http.StatusNotFound {
		t.Fatalf("不存在的渠道 GET 应 404，实际 %d", code)
	}
	if code, _ := doJSON(t, h, http.MethodPost, "/admin/channels/keys/status",
		`{"id":"ch-nope","index":0,"status":1}`); code != http.StatusBadRequest {
		t.Fatalf("不存在的渠道设状态应 400，实际 %d", code)
	}
}
