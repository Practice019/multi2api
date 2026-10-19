// channels_entry_test.go **入口级**端到端：从 POST /v1/chat/completions 进，
// 而不是直调 forwardViaChannel（那是上一轮的证据，证明不了入口那段 if 走得通）。
//
// # 三个用例 + 一个陷阱
//
//	① 公开名 pub-y ⇒ 200，且**假上游收到的 model 是上游名 real-x**
//	② 能力索引外的名字 ⇒ 404，且**假上游一次都没被打到**（专门防"偷偷回落账号池"）
//	③ 渠道停用 ⇒ 同样 404，且假上游没被打到
//
// 陷阱：池那边的 Upstream 被替换成一个"一旦被调用就记一笔"的假对象，
// 并在每个用例末尾断言它**没被调用过** —— 因为"回落到账号池"正是最该防的错误行为，
// 而它很容易在重构中被悄悄引入（表现是"模型还能用"，看起来一切正常）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/openaicompat"
)

// entryStore 建一条启用渠道，BaseURL 指向假上游。
func entryStore(t *testing.T, baseURL string, status int) *channelstore.Store {
	t.Helper()
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建 store 失败: %v", err)
	}
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-entry", Name: "入口测试渠道", BaseURL: baseURL,
		Status: status, Priority: 10,
		Models:   []string{"real-x"},
		ModelMap: map[string]string{"pub-y": "real-x"}, // 公开名 → 上游名
		Keys:     []channelstore.ChannelKey{{Key: "sk-entry-key", Status: channelstore.StatusEnabled}},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	return s
}

// entryHandler 搭一个"渠道模式已开启"的 Handler。
//
// ⚠ MaxBodyMB 必须 >0：入口按 int64(MaxBodyMB)<<20 设上限，0 会让**每个**请求都 413。
// 这是个很容易在测试里踩空、且报错信息完全不指向原因的地方。
//
// Pool 仍然给一个（withAuth 要用），但渠道模式下它不该被用到。
func entryHandler(t *testing.T, s *channelstore.Store, poolHits *int32) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		atomic.AddInt32(poolHits, 1) // ★ 账号池被用到就是错
		return 200, baselineDynamicBody(), false
	})
	return NewHandler(Config{
		Pool: p, Upstream: up,
		Provider:        newTestRouter(true),
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
		MaxBodyMB:       1,
		Channels:        s,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})
}

func postEntryChat(t *testing.T, h *Handler, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ① 入口走通：客户端发公开名，上游收到上游名。
func TestEntryForwardsViaChannel(t *testing.T) {
	var gotModel any
	var gotAuth string
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		gotModel = m["model"]
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	}))
	defer srv.Close()

	var poolHits int32
	h := entryHandler(t, entryStore(t, srv.URL+"/v1", channelstore.StatusEnabled), &poolHits)
	code, body := postEntryChat(t, h, `{"model":"pub-y","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if code != http.StatusOK {
		t.Fatalf("入口转发应 200，实际 %d: %s", code, body)
	}
	if hits != 1 {
		t.Fatalf("假上游应被调用 1 次，实际 %d", hits)
	}
	if gotModel != "real-x" {
		t.Fatalf("★ 上游收到的 model 应是上游名 real-x（不是客户端发的 pub-y），实际 %v", gotModel)
	}
	if gotAuth != "Bearer sk-entry-key" {
		t.Fatalf("Authorization 应是渠道 key，实际 %q", gotAuth)
	}
	if !strings.Contains(body, "hi") {
		t.Fatalf("上游响应应原样回传给客户端，实际 %s", body)
	}
	if n := atomic.LoadInt32(&poolHits); n != 0 {
		t.Fatalf("★ 渠道模式下**不该**碰账号池，实际被调用 %d 次", n)
	}
}

// ② 能力索引外的名字 ⇒ 404，且假上游一次都没被打到（防"偷偷回落账号池"）。
func TestEntryRejectsModelOutsideAbilityIndex(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	var poolHits int32
	h := entryHandler(t, entryStore(t, srv.URL+"/v1", channelstore.StatusEnabled), &poolHits)

	code, body := postEntryChat(t, h, `{"model":"not-in-index","messages":[]}`)
	if code != http.StatusNotFound {
		t.Fatalf("索引外的模型应 404（默认收紧），实际 %d: %s", code, body)
	}
	if hits != 0 {
		t.Fatalf("★ 被拒的请求**不该**打到渠道上游，实际 %d 次", hits)
	}
	if n := atomic.LoadInt32(&poolHits); n != 0 {
		t.Fatalf("★ 更不该回落到账号池，实际被调用 %d 次", n)
	}
}

// ③ 渠道停用 ⇒ 404，且两条上游都没被打到。
func TestEntryRejectsWhenChannelDisabled(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	var poolHits int32
	h := entryHandler(t, entryStore(t, srv.URL+"/v1", channelstore.StatusManuallyDisabled), &poolHits)

	code, body := postEntryChat(t, h, `{"model":"pub-y","messages":[]}`)
	if code != http.StatusNotFound {
		t.Fatalf("停用渠道应 404，实际 %d: %s", code, body)
	}
	if hits != 0 || atomic.LoadInt32(&poolHits) != 0 {
		t.Fatalf("停用后两条上游都不该被打到（渠道 %d 次 / 池 %d 次）", hits, atomic.LoadInt32(&poolHits))
	}
}

// 没配渠道存储 ⇒ 入口那段 if 整体跳过（既有部署行为不变的**入口侧**证据）。
func TestEntryIgnoresChannelModeWithoutStore(t *testing.T) {
	var poolHits int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		atomic.AddInt32(&poolHits, 1)
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up, Provider: newTestRouter(true),
		DefaultProvider: "workbuddy", OwnedBy: "workbuddy", MaxBodyMB: 1,
	})
	code, body := postEntryChat(t, h, `{"model":"workbuddy/auto","stream":true,"messages":[]}`)
	// 断言"走到了既有账号池路径"，而不是"上游 HTTP 被调用过"：
	// 我第一版用 poolHits>0 当证据，结果红 —— 因为这个假池里的账号不健康，
	// 池在**取号阶段**就回了 503 no_healthy_account，根本走不到发上游那一步。
	// 池自己的错误码才是"确实进了池"的直接证据（比"HTTP 被打过"更贴近事实）。
	if code == http.StatusNotFound {
		t.Fatalf("没配渠道时不该出现渠道口径的 404，实际 %d: %s", code, body)
	}
	if !strings.Contains(body, "no_healthy_account") {
		t.Fatalf("没配渠道时应照旧交给账号池判断（期望池自己的 no_healthy_account），实际 %d: %s", code, body)
	}
	if strings.Contains(body, "未对当前渠道开放") {
		t.Fatalf("这条响应的文案属于渠道口径 —— 说明没配渠道时那段分支没被正确跳过: %s", body)
	}
}
