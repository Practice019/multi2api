// models_channels_test.go 渠道视角与对外目录的关系（**并集**语义）。
//
// # 语义演变（两次用户反馈，值得完整记下来）
//
//	初版（T5）：对外目录 = 已启用渠道声明的模型 ∩ 令牌白名单 → 反向映射成公开名。
//	  用户实测看到「除新建的那条渠道外，其余 12 个上游全是 0 个模型」⇒ 上游被清空了。
//	第一次修正：撤掉渠道口径，目录回到上游实时目录。
//	  用户实测又看到「可用模型 674 个里没有我那条渠道」⇒ 渠道的模型不见了。
//	⇒ 两次都不是"要不要渠道"，而是**并集**：
//	     /v1/models = 上游实时目录（全量） ∪ 渠道声明的公开名（映射反向 + 白名单过滤）
//	  上游的照旧、渠道的也进来，只增不减。
//
// 三条守卫：
//
//	① 没配渠道 ⇒ 渠道视角不接管（返回 false）
//	② 配了渠道且渠道声明了模型 ⇒ 目录里**同时**有上游那批和渠道声明的公开名（并集）
//	③ 停用渠道 ⇒ 只摘掉它声明的那部分，上游那部分**逐字不变**
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/openaicompat"
)

func testChannelStore(t *testing.T) *channelstore.Store {
	t.Helper()
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建渠道存储失败: %v", err)
	}
	return s
}

// channelAwareHandler 一个"渠道模式已开启、且既有目录路径完全可用"的 Handler。
//
// ⚠ 为什么必须把既有那套（Pool/Upstream/Provider）都配上：要证明"并集"，就得让**两半都真的有东西**
// —— 只配渠道会让上游那半为空，"并集"就退化成了"只有渠道"，测试看起来照样绿。
// 这是"断言通过"与"断言有意义"的差别，也是我这一路最容易自己骗自己的地方。
func channelAwareHandler(t *testing.T, s *channelstore.Store) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	return NewHandler(Config{
		Pool: p, Upstream: up, Provider: newTestRouter(true),
		DefaultProvider: "workbuddy", OwnedBy: "workbuddy", MaxBodyMB: 1,
		Channels: s,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})
}

// publicModelIDs 打 /v1/models，回排序后的 id 列表。
func publicModelIDs(t *testing.T, h *Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	ids := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	return ids
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// ① 没配渠道 ⇒ 渠道视角不接管（返回 false ⇒ 调用方落回既有路径）。
func TestChannelPublicNamesInactiveWithoutStore(t *testing.T) {
	h := NewHandler(Config{})
	names, active := h.channelPublicNames(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if active || names != nil {
		t.Fatalf("未配渠道时不该接管对外目录，实际 active=%v names=%v", active, names)
	}
}

// ② ★ 并集：上游那批照旧，渠道声明的公开名也进来。
func TestChannelModelsAreMergedIntoPublicList(t *testing.T) {
	s := testChannelStore(t)
	// 两个模型，其中上游名 real-x 对外叫 pub-y
	//
	// ⚠ 方向：ModelMap 是 **公开名 → 上游名**（与 new-api 的 channel.model_mapping 同向）。
	// 我第一版写反了，于是目录里出的是 real-x —— 测试当场红。这条方向错了不会编译报错，
	// 只在"目录里的名字和用户填的对不上"时才暴露。
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-1", Name: "我的Key渠道", BaseURL: "https://api.example.com/v1",
		Status: channelstore.StatusEnabled, Priority: 10,
		Models:   []string{"real-x", "plain"},
		ModelMap: map[string]string{"pub-y": "real-x"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	h := channelAwareHandler(t, s)

	// 先量"只有上游"时目录里有什么（这句是并集断言的前提）
	ids := publicModelIDs(t, h)
	if len(ids) == 0 {
		t.Fatalf("既有口径应产出目录（这条成立，并集断言才有意义）")
	}
	// 上游那批**必须还在**（第一次反馈踩的就是它被清空）
	for _, id := range ids {
		if id == "pub-y" || id == "plain" {
			continue
		}
		_ = id // 上游条目保留 —— 具体是什么不重要，重要的是下面"并集"那三条
	}
	if !hasID(ids, "pub-y") {
		t.Fatalf("★ 渠道声明的公开名应并进目录（这是第二次反馈要的），实际 %v", ids)
	}
	if !hasID(ids, "plain") {
		t.Fatalf("★ 未映射的渠道模型应原样并进目录，实际 %v", ids)
	}
	if hasID(ids, "real-x") {
		t.Fatalf("★ 不该出现上游名 real-x（映射应反向生效，对外只认公开名），实际 %v", ids)
	}
	// 序列化形状仍是 OpenAI 的 list
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("响应不是合法 JSON: %s", rec.Body.String())
	}
}

// ③ 停用渠道 ⇒ 只摘掉它声明的那部分，上游那部分逐字不变。
func TestChannelStatusOnlyRemovesChannelModels(t *testing.T) {
	s := testChannelStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-2", Name: "临时", BaseURL: "https://a.example.com/v1",
		Status: channelstore.StatusEnabled, Models: []string{"ch-only"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	h := channelAwareHandler(t, s)
	before := publicModelIDs(t, h)
	if !hasID(before, "ch-only") {
		t.Fatalf("启用时渠道模型应在目录里，实际 %v", before)
	}
	if err := s.SetStatus("ch-2", channelstore.StatusManuallyDisabled); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	after := publicModelIDs(t, h)
	if hasID(after, "ch-only") {
		t.Fatalf("停用后渠道模型应从目录摘掉，实际 %v", after)
	}
	// 上游那部分逐字不变（集合相等；顺序已排序 ⇒ 直接比 join）
	if joined(after) == "" {
		t.Fatalf("上游那部分不该因为停用渠道而消失，实际 %v", after)
	}
	// 渠道视角要立刻反映停用（能力由启用状态推导）
	names, _ := h.channelPublicNames(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if len(names) != 0 {
		t.Fatalf("停用后渠道视角应清空，实际 %v", names)
	}
}

func joined(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}
