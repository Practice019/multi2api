// openai_compat_test.go —— Key 型渠道（OpenAI 兼容）的守卫测试。
//
// ⚠ 背景：实现（openai_compat.go）由子代理产出但**没带测试**，这四条由主脑补写 ——
// T4 的验收标准里它们不是可选项：
//
//	① Chat **逐字节原样透传** body（我们是中转站，不改协议；这条不钉住，将来任何"顺手美化"
//	   都会静默改变上游看到的内容）
//	② Models 真的解析 data[].id
//	③ 上游非 2xx ⇒ **显式错误**且带状态码、且**不含 key 明文**（含 key 的错误会进日志/回显）
//	④ 已取消的 ctx 不 panic；凭证里没有 key 时在**打上游之前**就失败
package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gw "workbuddy2api/internal/gateway"
)

const testKey = "sk-t4-secret-key"

func testCred() gw.Credential {
	return gw.Credential{Provider: "openai-compat", UID: "u1", Secret: OpenAICompatSecret{APIKey: testKey}}
}

// ① 逐字节透传 + Bearer 头 + URL 拼接正确。
func TestOpenAICompatChatPassesBodyVerbatim(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"ok\":1}\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAICompat("t4", srv.URL+"/v1")
	// 故意用"不规整"的 body（空格/换行/key 顺序）—— 透传的实现必须逐字节相同
	body := []byte("{\n  \"model\" : \"pub-x\",   \"stream\": true,\n  \"messages\" : []\n}")
	cs, err := p.Chat(context.Background(), testCred(), body)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer cs.Body.Close()

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("URL 应拼成 /v1/chat/completions，实际上游收到 %q", gotPath)
	}
	if gotAuth != "Bearer "+testKey {
		t.Fatalf("Authorization 应为 Bearer <key>，实际 %q", gotAuth)
	}
	if string(gotBody) != string(body) {
		t.Fatalf("body 必须逐字节透传：\n  发送 %q\n  收到 %q", body, gotBody)
	}
	raw, _ := io.ReadAll(cs.Body)
	if string(raw) != "data: {\"ok\":1}\n\n" {
		t.Fatalf("流内容应原样回传，实际 %q", raw)
	}
	if cs.Status != 200 {
		t.Fatalf("Status 应为上游状态码 200，实际 %d", cs.Status)
	}
}

// ② Models 解析 data[].id。
func TestOpenAICompatModelsParsesIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("应请求 /v1/models，实际 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer srv.Close()

	p := NewOpenAICompat("t4", srv.URL+"/v1")
	ms, err := p.Models(context.Background(), testCred())
	if err != nil {
		t.Fatalf("Models 失败: %v", err)
	}
	if len(ms) != 2 || ms[0].ID != "m1" || ms[1].ID != "m2" {
		t.Fatalf("应解析出 [m1 m2]，实际 %+v", ms)
	}
}

// ③ 上游 401 ⇒ 显式错误、带状态码、**不含 key 明文**。
func TestOpenAICompatUpstreamErrorIsExplicitAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte("unauthorized: bad key"))
	}))
	defer srv.Close()

	p := NewOpenAICompat("t4", srv.URL+"/v1")
	_, err := p.Chat(context.Background(), testCred(), []byte(`{}`))
	if err == nil {
		t.Fatal("上游 401 必须返回错误，不能静默成功")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Fatalf("错误信息应带状态码 401，实际 %q", msg)
	}
	if strings.Contains(msg, testKey) {
		t.Fatalf("错误信息**不得**包含 key 明文，实际 %q", msg)
	}
	// 上游的一句话应该被带出来一点（否则排查只剩状态码）
	if !strings.Contains(msg, "unauthorized") {
		t.Fatalf("错误信息应带上游响应片段，实际 %q", msg)
	}

	// Models 同样不能吞错
	if _, err := p.Models(context.Background(), testCred()); err == nil {
		t.Fatal("Models 遇到 401 也应返回错误")
	}
}

// ④ 取消的 ctx 不 panic（契约要求）；缺 key 时**在打上游之前**失败。
func TestOpenAICompatCancelledCtxAndMissingKey(t *testing.T) {
	var hit int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p := NewOpenAICompat("t4", srv.URL+"/v1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Chat(ctx, testCred(), []byte(`{}`)); err == nil {
		t.Fatal("已取消的 ctx 上 Chat 应返回错误")
	}
	if _, err := p.Models(ctx, testCred()); err == nil {
		t.Fatal("已取消的 ctx 上 Models 应返回错误")
	}

	// 凭证里没有 key：必须在我们这边失败，而不是发一个空 Bearer 去换上游 401
	// （那会把"我们没装上凭证"误报成"上游拒绝这条渠道"，排查方向直接跑偏）
	empty := gw.Credential{Provider: "openai-compat", UID: "u1"}
	if _, err := p.Chat(context.Background(), empty, []byte(`{}`)); err == nil {
		t.Fatal("空凭证应显式报错")
	}
	if _, err := p.Models(context.Background(), empty); err == nil {
		t.Fatal("空凭证应显式报错")
	}
	if hit != 0 {
		t.Fatalf("空凭证时不该打到上游，实际打了 %d 次", hit)
	}

	// 能力位：两个都真实现，所以都声明
	if c := p.Caps(); !c.Has(gw.CapChat) || !c.Has(gw.CapModels) {
		t.Fatalf("Caps 应含 CapChat|CapModels，实际 %v", c.Names())
	}
}
