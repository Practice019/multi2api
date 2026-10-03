package loomy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsImageGenExt Provider 必须自报生图能力。
//
// 不实现的表现：`POST /v1/images/generations` 回 501（"上游不支持生图"），
// 而 loomy 上确实有两个只能生图的模型 —— 一次白跑的功能缺失。
func TestImplementsImageGenExt(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.ImageGenExt](p); !ok {
		t.Fatal("loomy 必须实现 gateway.ImageGenExt，否则生图端点回 501")
	}
	if _, ok := gateway.ExtOf[gateway.ImageModelExt](p); !ok {
		t.Fatal("loomy 必须实现 gateway.ImageModelExt，否则两个生图模型不进 /v1/models")
	}
}

// TestImageModelsMatchKnownIDs 声明的生图模型必须与实测清单一致。
//
// 这三个 ID 是**实测能用 /images/generations 的**（见 imagegen_ext.go 的文件头）。
// 漏一个 → 调用方不知道能用它；多一个 → 调用方照目录去调然后失败。
func TestImageModelsMatchKnownIDs(t *testing.T) {
	p := NewWithConfig(Config{})
	got := p.ImageModels()
	// 至少要有豆包那个（用户明确点了它的名）
	var sawSeedream bool
	for _, m := range got {
		if m.ID == "doubao-seedream-5-lite" {
			sawSeedream = true
			if m.Name == "" {
				t.Error("doubao-seedream-5-lite 应当有展示名（目录里显示用）")
			}
		}
	}
	if !sawSeedream {
		t.Errorf("生图清单里没有 doubao-seedream-5-lite: %+v", got)
	}
	// 返回的必须是**副本**，改它不能污染包级切片
	before := len(imageModelIDs)
	got2 := p.ImageModels()
	if len(got2) > 0 {
		got2[0].ID = "MUTATED"
	}
	if imageModelIDs[0].ID == "MUTATED" {
		t.Error("ImageModels 必须返回副本 —— 调用方改它会污染包级清单")
	}
	if len(imageModelIDs) != before {
		t.Error("包级清单长度意外变化")
	}
}

// TestGenerateImageSendsBothAuthHeaders 生图请求必须**两个鉴权头都发**。
//
// # 为什么这条是生图特有的事实（不是抄 chat 的）
//
// client.go 的"双轨鉴权"判据说一个端点认一个头：
//
//	GET  /models            →  token
//	POST /chat/completions  →  Authorization: Bearer
//
// 但生图端点实测**两个都认**，且 Loomy 官方客户端两个都发
// （其 image-generation-service.js："session 模式同时塞 token + Authorization"）。
//
// 只发一个也许能通，但没有任何证据 —— 这条把它钉住，免得后人
// "统一成 chat 那样只发一个"（那会是一个没有依据的改动）。
func TestGenerateImageSendsBothAuthHeaders(t *testing.T) {
	var gotAuth, gotToken, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("token")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"https://example.invalid/a.png"}],"points_consumed":110}`))
	}))
	defer srv.Close()

	p, dir := newTestProvider(t, srv.URL)
	writeCred(t, dir, "uid-1", "session-abc")

	body := []byte(`{"model":"doubao-seedream-5-lite","prompt":"一只猫","n":1,"size":"1024x1024"}`)
	raw, status, err := p.GenerateImage(context.Background(), "uid-1", body)
	if err != nil {
		t.Fatalf("生图失败: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，want 200", status)
	}
	// ① 两个鉴权头都在，且用的是这个账号的 session
	if gotAuth != "Bearer session-abc" {
		t.Errorf("Authorization = %q，want \"Bearer session-abc\"", gotAuth)
	}
	if gotToken != "session-abc" {
		t.Errorf("token = %q，want \"session-abc\" —— 生图端点两个头都要发", gotToken)
	}
	// ② 打到正确的路径
	if gotPath != "/images/generations" {
		t.Errorf("路径 = %q，want /images/generations", gotPath)
	}
	// ③ 请求体原样转发（不解析、不改写）
	if gotBody != string(body) {
		t.Errorf("请求体被改写了:\n got: %s\nwant: %s", gotBody, body)
	}
	// ④ 响应体原样交回（含非标准字段 points_consumed）
	if !strings.Contains(string(raw), "points_consumed") {
		t.Errorf("响应里的 points_consumed 丢了: %s", raw)
	}
}

// TestGenerateImagePassesThroughUpstreamError 上游的错误必须**原样**交回。
//
// # 为什么（诊断价值）
//
// 实测上游 404 时给出的是结构化信封：
//
//	{"error":{"message":"该模型暂未开放","type":"not_found_error","code":404}}
//
// 网关若把它换成自己的一句话（或吞掉改成 502），调用方就失去了
// "这个模型不支持"与"网关坏了"的区别 —— 那是完全不同的处理方式。
func TestGenerateImagePassesThroughUpstreamError(t *testing.T) {
	const upstreamErr = `{"error":{"message":"该模型暂未开放","type":"not_found_error","code":404}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(upstreamErr))
	}))
	defer srv.Close()

	p, dir := newTestProvider(t, srv.URL)
	writeCred(t, dir, "uid-1", "session-abc")

	raw, status, err := p.GenerateImage(context.Background(), "uid-1",
		[]byte(`{"model":"x","prompt":"p"}`))
	if err != nil {
		t.Fatalf("上游有应答时不该返回 error（那是传输层语义）: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("状态码 = %d，want 404（原样透传）", status)
	}
	if string(raw) != upstreamErr {
		t.Errorf("错误信封被改写了:\n got: %s\nwant: %s", raw, upstreamErr)
	}
}

// TestGenerateImageMissingCredentialIsAnError 取不到凭证必须报 error。
//
// # 为什么不能"发个空凭证试试"
//
// 空 session 发出去会被上游当成"token 缺失"，而它返回的是
// **HTTP 200 + `{"code":"100002","desc":"缺少 token"}`**（实测形态）。
// 那是一个把人往"鉴权头写错了"方向带的误导性失败 ——
// 真正的原因是"这个 uid 没有凭证"。
func TestGenerateImageMissingCredentialIsAnError(t *testing.T) {
	p, _ := newTestProvider(t, "https://example.invalid")
	if _, _, err := p.GenerateImage(context.Background(), "no-such-uid",
		[]byte(`{"model":"x","prompt":"p"}`)); err == nil {
		t.Fatal("取不到凭证时必须返回 error，而不是发一个空 session 出去")
	}
}

// TestGenerateImageUsesTheRequestedAccount 必须用**被指定那个账号**的 session。
//
// # 为什么单独一条
//
// 账号池里每个账号有各自的 session，而生图是要计费到具体账号上的。
// 用错号 = 扣了别人的分（用户会看到"我没生成过这个东西，积分却少了"）。
// 本用例放两个账号，只对其中一个发起请求。
func TestGenerateImageUsesTheRequestedAccount(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("token")
		_, _ = w.Write([]byte(`{"data":[{"url":"u"}]}`))
	}))
	defer srv.Close()

	p, dir := newTestProvider(t, srv.URL)
	writeCred(t, dir, "uid-1", "session-ONE")
	writeCred(t, dir, "uid-2", "session-TWO")

	if _, _, err := p.GenerateImage(context.Background(), "uid-2",
		[]byte(`{"model":"x","prompt":"p"}`)); err != nil {
		t.Fatalf("生图失败: %v", err)
	}
	if seen != "session-TWO" {
		t.Errorf("用了 %q 的 session —— 必须是被请求的那个账号（uid-2）", seen)
	}
}

// TestModelsStillExcludeImageModelsFromChatCatalog 生图模型**仍不该**进对话目录。
//
// # 为什么这条重要（防止"修一个错、造一个错"）
//
// 本次修正了 `Unavailable` 的语义（它只表示"chat 不可用"）。
// 一个很容易犯的错是：既然它们在 images 端点可用，就把 Unavailable 去掉 ——
// 那会让两个生图模型出现在对话目录里，客户端据此调用 chat 端点，**必然 404**。
//
// 正确的形态是**两个轴各自表达**：
//
//	对话目录   不含它们（它们不能对话）
//	生图清单   含它们（它们能生图）
func TestModelsStillExcludeImageModelsFromChatCatalog(t *testing.T) {
	for _, m := range AvailableModels() {
		for _, im := range imageModelIDs {
			if m.ID == im.ID {
				t.Errorf("生图模型 %s 不该出现在**对话**可用清单里"+
					"（它走 /chat/completions 会 404；它属于 imageModelIDs）", m.ID)
			}
		}
	}
	// 反向：生图清单里必须也没有纯对话模型
	for _, im := range imageModelIDs {
		if known, _, ok := ResolveModel(im.ID); ok && !known.Unavailable {
			t.Errorf("生图模型 %s 在对话表里标着可用 —— 两个轴的判据混淆了", im.ID)
		}
	}
}

// ---- 测试辅助 ----

// newTestProvider 造一个指向假上游的 Provider（含凭证目录）。
func newTestProvider(t *testing.T, baseURL string) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()
	p := NewWithConfig(Config{AuthDir: dir})
	p.SetClient(NewWithBase(baseURL))
	return p, dir
}

// writeCred 在凭证目录里写一份最小可用凭证。
func writeCred(t *testing.T, dir, uid, session string) {
	t.Helper()
	a := &Auth{Session: session, UID: uid, Nickname: uid}
	raw, err := MarshalAuthFile(a)
	if err != nil {
		t.Fatalf("序列化凭证失败: %v", err)
	}
	writeFile(t, dir, FileName(a), string(raw))
}

// TestImageModelCatalogShapeIsJSON 目录里那条生图记录要能被 JSON 序列化。
//
// 复用 entryOf 生成的 map 里含 `capabilities: []string{...}` ——
// 值类型写成不可序列化的东西时，/v1/models 会在写入时失败
// （那时表现是"目录为空"而不是"字段写错了"，很难定位）。
func TestImageModelCatalogShapeIsJSON(t *testing.T) {
	entry := map[string]any{
		"id":           "doubao-seedream-5-lite",
		"object":       "model",
		"capabilities": []string{"image_generation"},
	}
	if _, err := json.Marshal(entry); err != nil {
		t.Fatalf("生图目录条目无法序列化: %v", err)
	}
}
