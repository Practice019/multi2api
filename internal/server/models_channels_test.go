// models_channels_test.go 渠道视角与对外目录的关系（**撤收紧之后**的语义）。
//
// # 2026-10-20 的语义变更（用户决定）
//
// 原设计（T5）：对外目录 = 已启用渠道声明的模型 ∩ 令牌白名单 → 反向映射成公开名。
// 用户实测后看到「除新建的那条渠道外，其余 12 个上游全是 0 个模型」——那正是收紧的表现。
// 用户的决定：**不要模型对外的那些修改，直接展示全部的，渠道的要保留**。
//
// ⇒ 现在的分工（这份测试守的就是这条分工）：
//
//	· `/v1/models` 走既有口径（上游实时目录 + 令牌白名单），**渠道不再决定它有什么**；
//	· 渠道视角（channelPublicNames）**保留**，供管理页「对外模型」页签显示"渠道这边声明了什么"；
//	· 渠道对请求的作用只剩一条：chat 请求的名字命中某条启用渠道 ⇒ 走渠道转发。
//
// 三条守卫：
//
//	① 没配渠道 ⇒ 渠道视角不接管（返回 false）
//	② 配了渠道且渠道声明了模型 ⇒ 目录里**不出现**这些名字（撤收紧的核心证据）
//	③ 停用渠道 ⇒ 目录**不变**（它本来就不由渠道驱动）
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
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
// ⚠ 为什么必须把既有那套（Pool/Upstream/Provider）都配上：要证明"撤收紧后目录回到既有口径"，
// 就得让既有口径**真的产出东西** —— 否则目录为空也能把"里面没有 pub-y"这条断言混过去
// （空集合当然不含 pub-y）。这是"断言通过"与"断言有意义"的差别，也是我这一路最容易自己骗自己的地方。
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

// ① 没配渠道 ⇒ 渠道视角不接管（返回 false ⇒ 调用方落回既有路径）。
func TestChannelPublicNamesInactiveWithoutStore(t *testing.T) {
	h := NewHandler(Config{})
	names, active := h.channelPublicNames(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if active || names != nil {
		t.Fatalf("未配渠道时不该接管对外目录，实际 active=%v names=%v", active, names)
	}
}

// ② ★ 撤收紧的核心证据：渠道声明了模型，但对外目录**不因此**改变。
func TestChannelDoesNotDrivePublicList(t *testing.T) {
	s := testChannelStore(t)
	// 一条启用渠道：两个模型，其中上游名 real-x 对外叫 pub-y
	//
	// ⚠ 方向：ModelMap 是 **公开名 → 上游名**（与 new-api 的 channel.model_mapping 同向）。
	// 我第一版把它写反了，于是目录出的是 real-x —— 测试当场红。这条方向错了不会编译报错，
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

	ids := publicModelIDs(t, h)
	// 既有口径确实产出了东西 —— 否则下面那条断言无意义（空集合当然不含 pub-y）
	if len(ids) == 0 {
		t.Fatalf("既有口径应产出目录（这条成立，下面那条断言才有意义）")
	}
	for _, id := range ids {
		switch id {
		case "pub-y", "real-x", "plain":
			t.Fatalf("撤收紧后渠道声明的名字不该出现在对外目录，实际 %v（这正是不该再发生的那种收紧）", ids)
		}
	}
	// 序列化形状仍是 OpenAI 的 list
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("响应不是合法 JSON: %s", rec.Body.String())
	}

	// 渠道视角本身**仍在**（供管理页「对外模型」页签看"渠道这边声明了什么"）
	names, active := h.channelPublicNames(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !active {
		t.Fatalf("配了渠道 ⇒ 渠道视角应可算（它只是不再驱动 /v1/models）")
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "pub-y") || !strings.Contains(joined, "plain") {
		t.Fatalf("渠道视角应含映射后的公开名 pub-y 与未映射的 plain，实际 %v", names)
	}
}

// ③ 停用渠道 ⇒ 对外目录**不变**（它不由渠道驱动，所以停用也不该影响它）。
func TestChannelStatusDoesNotChangePublicList(t *testing.T) {
	s := testChannelStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-2", Name: "临时", BaseURL: "https://a.example.com/v1",
		Status: channelstore.StatusEnabled, Models: []string{"m1"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	h := channelAwareHandler(t, s)
	before := publicModelIDs(t, h)
	if err := s.SetStatus("ch-2", channelstore.StatusManuallyDisabled); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	after := publicModelIDs(t, h)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("停用渠道不该改变对外目录（原 %v → 现 %v）", before, after)
	}
	// 但渠道视角要立刻反映停用（能力由启用状态推导）—— 这条仍属于渠道自己的语义
	names, _ := h.channelPublicNames(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if len(names) != 0 {
		t.Fatalf("停用后渠道视角应清空，实际 %v", names)
	}
}
