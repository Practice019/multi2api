// models_channels_test.go 渠道口径对外目录的集成守卫（真 store + 真 handler + 真文件）。
//
// 第 42 轮把接线做完了，但证据只有编译与既有测试 —— 那**证明不了**新路径真的生效。
// 这份测试走完整路径：临时目录建 store → 建渠道 → 打 /v1/models → 看响应里的 id。
//
// 四条：
//
//	① 没配渠道 ⇒ channelPublicNames 返回 (nil,false) ⇒ 走既有上游路径（不改既有部署）
//	② 配了渠道 ⇒ 目录**只**含能力索引里的模型（不是上游那些）
//	③ 映射反向：渠道声明的上游名 real-x + 映射 real-x→pub-y ⇒ 目录里出 pub-y
//	④ 停用渠道 ⇒ 目录立刻清空（默认收紧：能力由启用状态推导）
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
)

func testChannelStore(t *testing.T) *channelstore.Store {
	t.Helper()
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建渠道存储失败: %v", err)
	}
	return s
}

// modelIDsFromChannelPath 打 /v1/models，回 id 列表（含 owned_by）。
func modelIDsFromChannelPath(t *testing.T, h *Handler) ([]string, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	ids := make([]string, 0, len(body.Data))
	owners := map[string]string{}
	for _, d := range body.Data {
		ids = append(ids, d.ID)
		owners[d.ID] = d.OwnedBy
	}
	return ids, owners
}

// ① 没配渠道 ⇒ 不接管（返回 false ⇒ 调用方落回既有路径）。
func TestChannelPublicNamesInactiveWithoutStore(t *testing.T) {
	h := NewHandler(Config{})
	names, active := h.channelPublicNames(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if active || names != nil {
		t.Fatalf("未配渠道时不该接管对外目录，实际 active=%v names=%v", active, names)
	}
}

// ② 配了渠道 ⇒ 目录只含能力索引里的模型；③ 映射反向生效。
func TestChannelModelsDrivePublicList(t *testing.T) {
	s := testChannelStore(t)
	// 一条启用渠道：两个模型，其中上游名 real-x 对外叫 pub-y
	//
	// ⚠ 方向：ModelMap 是 **公开名 → 上游名**（与 new-api 的 channel.model_mapping 同向）。
	// 我第一版把它写反了（写成 {"real-x":"pub-y"}），于是目录出的是 real-x —— 测试当场红。
	// 记在这里：这条方向错了不会编译报错，只会在"目录里的名字和用户填的对不上"时才暴露。
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-1", Name: "我的Key渠道", BaseURL: "https://api.example.com/v1",
		Status: channelstore.StatusEnabled, Priority: 10,
		Models:   []string{"real-x", "plain"},
		ModelMap: map[string]string{"pub-y": "real-x"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	h := NewHandler(Config{Channels: s})

	ids, owners := modelIDsFromChannelPath(t, h)
	if len(ids) != 2 {
		t.Fatalf("目录应只有能力索引里的 2 条，实际 %v", ids)
	}
	// ③ 映射反向：出公开名、不出上游名
	var hasPub, hasReal, hasPlain bool
	for _, id := range ids {
		switch id {
		case "pub-y":
			hasPub = true
		case "real-x":
			hasReal = true
		case "plain":
			hasPlain = true
		}
	}
	if !hasPub || hasReal {
		t.Fatalf("real-x 应对外显示为 pub-y（且不得出现上游名），实际 %v", ids)
	}
	if !hasPlain {
		t.Fatalf("未映射的模型应原样出现，实际 %v", ids)
	}
	// owned_by 是渠道名（目录显示的来源应=请求实际会走的那条）
	if owners["pub-y"] != "我的Key渠道" {
		t.Fatalf("owned_by 应为渠道名，实际 %q", owners["pub-y"])
	}
	// 序列化形状仍是 OpenAI 的 list
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("响应不是合法 JSON: %s", rec.Body.String())
	}
}

// ④ 停用渠道 ⇒ 目录立刻清空（默认收紧在对外目录上的可见后果）。
func TestChannelDisabledEmptiesPublicList(t *testing.T) {
	s := testChannelStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-2", Name: "临时", BaseURL: "https://a.example.com/v1",
		Status: channelstore.StatusEnabled, Models: []string{"m1"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	h := NewHandler(Config{Channels: s})
	if ids, _ := modelIDsFromChannelPath(t, h); len(ids) != 1 {
		t.Fatalf("启用时目录应有 1 条，实际 %v", ids)
	}
	if err := s.SetStatus("ch-2", channelstore.StatusManuallyDisabled); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	ids, _ := modelIDsFromChannelPath(t, h)
	if len(ids) != 0 {
		t.Fatalf("停用后对外目录应清空（能力由启用状态推导），实际 %v", ids)
	}
}
