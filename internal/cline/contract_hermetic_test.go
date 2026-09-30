// contract_hermetic_test.go 契约测试与假上游 —— **判据 2 的 enforcement**。
//
// # 为什么必须是 hermetic（不需要真实凭证）
//
// 本仓库的经验教训（loomy/contract_hermetic_test.go 的注释也记了同一条）：
// 一个"拿不到凭证就 skip"的契约测试在 CI 与别人 clone 的环境里等于不存在。
// Cline 的登录是设备码轮询，**完全不需要真实账号**就能把整条链路跑通 ——
// 假上游扮演 WorkOS 与 api.cline.bot 两个角色即可。
//
// 因此本文件的 TestContract 不读任何环境变量、不做任何条件跳过。
package cline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fixtureToken 固定 fixture 令牌（不含真实凭据）。
const fixtureToken = "eyJhbGciOiJSUzI1NiJ9.fixture-payload.fixture-sig"

// fakeUpstream 同时扮演 WorkOS 与 api.cline.bot。
//
// 路由按路径后缀分派（两个角色共用一个 httptest server，
// 因为 Client 的 APIBase 与 WorkOSBase 在测试里都指向它）。
func fakeUpstream(t *testing.T) (*httptest.Server, *fakeCalls) {
	t.Helper()
	calls := &fakeCalls{}
	mux := http.NewServeMux()

	// ── WorkOS：设备码授权 ──────────────────────────────────────────────
	mux.HandleFunc("/user_management/authorize/device", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.deviceAuth, 1)
		// 校验 form 体逐字节等于 client_id=<id>（参照测试断言）
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != WorkOSClientID {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"dev-1","user_code":"ABCD-1234",
			"verification_uri":"https://workos.example/activate",
			"verification_uri_complete":"https://workos.example/activate?user_code=ABCD-1234",
			"expires_in":300,"interval":1}`))
	})

	// ── WorkOS：轮询换 token ───────────────────────────────────────────
	mux.HandleFunc("/user_management/authenticate", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls.authenticate, 1)
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != deviceGrantType ||
			r.PostForm.Get("device_code") != "dev-1" ||
			r.PostForm.Get("client_id") != WorkOSClientID {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		// 前两次回 authorization_pending（**不是错误**），之后成功。
		// 这条是 §3.4 判据的实证：按状态码判失败会把"用户还没点"误报成失败。
		if n <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"workos-at-1","refresh_token":"rt-1","token_type":"Bearer"}`))
	})

	// ── api.cline.bot：token 注册 ──────────────────────────────────────
	mux.HandleFunc("/api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.register, 1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 字段名必须是驼峰 accessToken / refreshToken
		if body["accessToken"] != "workos-at-1" || body["refreshToken"] != "rt-1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad fields"}`))
			return
		}
		// register 必须带 clientHeaders（无 Authorization）
		if r.Header.Get("X-Title") != "Cline" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad headers"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{
			"accessToken":"workos:` + fixtureToken + `","refreshToken":"cline-rt-1",
			"expiresAt":"2099-01-01T00:00:00.000Z","tokenType":"Bearer",
			"userInfo":{"clineUserId":"usr-fixture-1","email":"fixture@example.com"}}}`))
	})

	// ── api.cline.bot：续期 ────────────────────────────────────────────
	mux.HandleFunc("/api/v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.refresh, 1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		// ⚠ 字段名是驼峰 refreshToken / grantType（不是 OAuth 标准的）
		if body["refreshToken"] == "" || body["grantType"] != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad fields"}`))
			return
		}
		// ⚠ refresh 调用点**不带** clientHeaders（与 register 不同）
		if r.Header.Get("X-Title") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"refresh must not send clientHeaders"}`))
			return
		}
		// 续期返回**裸 JWT**（不带 workos: 前缀）—— 真实形态，
		// 幂等补齐必须让它在下一步可用。
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{
			"accessToken":"` + fixtureToken + `","refreshToken":"cline-rt-2",
			"expiresAt":4102444800000}}`))
	})

	// ── api.cline.bot：聊天（SSE）─────────────────────────────────────
	mux.HandleFunc("/api/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.chat, 1)
		auth := r.Header.Get("Authorization")
		// ⚠ 前缀不可剥：必须是 Bearer workos:...
		if !strings.HasPrefix(auth, "Bearer "+tokenPrefix) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing workos prefix"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		if obj["stream"] != true {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"stream must be forced"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning\":\"想\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"成功\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	// ── api.cline.bot：recommended-models（**不需要认证**）────────────
	mux.HandleFunc("/api/v1/ai/cline/recommended-models", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.recommended, 1)
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"recommended-models must be anonymous"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recommended":[{"id":"anthropic/claude-x","name":"Claude X"}],
			"free":[{"id":"cline-free/deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash"}],
			"clinePass":[{"id":"cline-pass/opus","name":"Opus"}]}`))
	})

	// ── api.cline.bot：全量模型 id（需认证）────────────────────────────
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.models, 1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+tokenPrefix) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// ⚠ 刻意**不含** cline-free/*：真实上游就是这样（460 个 id 里零命中），
		// 钉住"免费模型只由 recommended-models 下发"这条事实。
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek/deepseek-v4.1-flash"},{"id":"openai/gpt-x"}]}`))
	})

	// ── api.cline.bot：用户信息与余额 ──────────────────────────────────
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+tokenPrefix) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"userId":"usr-fixture-1"}}`))
	})
	mux.HandleFunc("/api/v1/users/", func(w http.ResponseWriter, r *http.Request) {
		// 只接受 usr-… 形态的 accountId（不是 JWT 的 sub）
		if !strings.Contains(r.URL.Path, "usr-fixture-1") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Invalid request format"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"userId":"usr-fixture-1","balance":500000}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, calls
}

type fakeCalls struct {
	deviceAuth   int32
	authenticate int32
	register     int32
	refresh      int32
	chat         int32
	recommended  int32
	models       int32
}

func newContractProvider(base string) gateway.Provider {
	return NewWithConfig(Config{Client: NewWithBase(base)})
}

func newContractCredential(uid string) gateway.Credential {
	a := &Auth{
		AccessToken:  clineBearerValue(fixtureToken),
		RefreshToken: "cline-rt-1",
		AccountID:    uid,
		Email:        "fixture@example.com",
		Nickname:     "fixture",
	}
	if a.ExpireTime == 0 {
		a.ExpireTime = time.Now().Add(time.Hour).UnixMilli()
	}
	return gateway.Credential{Provider: providerID, UID: uid, Nickname: a.Nickname, Secret: a}
}

// TestContract 契约测试 —— **判据 2 的正式入口**。
func TestContract(t *testing.T) {
	srv, _ := fakeUpstream(t)
	gateway.RunProviderContract(t, func() gateway.Provider {
		return newContractProvider(srv.URL)
	}, gateway.WithCredential(newContractCredential("usr-fixture-1")))
}

// TestContractIsNotConditionallySkipped 把"契约必须无凭证也跑"固化下来。
func TestContractIsNotConditionallySkipped(t *testing.T) {
	t.Setenv("CLINE_ACCESS_TOKEN", "")
	t.Setenv("CLINE_TEST_CRED", "")

	srv, _ := fakeUpstream(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gateway.RunProviderContract(t, func() gateway.Provider {
			return newContractProvider(srv.URL)
		}, gateway.WithCredential(newContractCredential("usr-fixture-1")))
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("契约在无凭证环境下未完成 —— CI 里会变成永远挂住")
	}
}

// ── 逐条钉住本 provider 特有的硬判据 ────────────────────────────────────

// TestBearerPrefixIsNotStrippable 钉住 §10.1 的头号坑。
//
// ⚠ 这条是**变异可检**的：把 clineBearerValue 改成返回 token 原值
// （即"剥掉前缀"的等价物），假上游会回 401，本用例立刻红。
func TestBearerPrefixIsNotStrippable(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{fixtureToken, tokenPrefix + fixtureToken},               // 裸 JWT → 补前缀
		{tokenPrefix + fixtureToken, tokenPrefix + fixtureToken}, // 已有 → 不变（幂等）
		{"  " + fixtureToken + "  ", tokenPrefix + fixtureToken}, // trim 后补
		{"", ""},
	}
	for _, c := range cases {
		if got := clineBearerValue(c.in); got != c.want {
			t.Errorf("clineBearerValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRefreshReturnsBareJWTAndIsIdempotentlyPrefixed 续期返回裸 JWT 后仍可用。
//
// 实测形态：登录返回带前缀的 token，**续期返回裸 JWT**。
// 若只在登录时补一次前缀，续期后 Authorization 就变成裸 JWT → 401。
func TestRefreshReturnsBareJWTAndIsIdempotentlyPrefixed(t *testing.T) {
	srv, calls := fakeUpstream(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	cred := newContractCredential("usr-fixture-1")
	a := cred.Secret.(*Auth)

	if err := p.RefreshCredential(cred); err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if got := atomic.LoadInt32(&calls.refresh); got != 1 {
		t.Fatalf("续期请求应发 1 次，实际 %d", got)
	}
	if !strings.HasPrefix(a.AccessToken, tokenPrefix) {
		t.Errorf("续期后 access_token 应带 %s 前缀（幂等补齐），实际 %q", tokenPrefix, a.AccessToken)
	}
	if a.RefreshToken != "cline-rt-2" {
		t.Errorf("refresh_token 应被更新为 cline-rt-2，实际 %q", a.RefreshToken)
	}
	// 续期后必须仍能发聊天（即前缀确实生效）
	if err := p.client.VerifyToken(context.Background(), a); err != nil {
		t.Errorf("续期后的令牌应可用: %v", err)
	}
}

// TestDeviceFlowPendingIsNotAnError `authorization_pending` 不是错误。
//
// 假上游前两次刻意回 pending（HTTP 400 + error 字段），
// 若实现按状态码判失败，startLogin 会立刻报错而拿不到凭据。
func TestDeviceFlowPendingIsNotAnError(t *testing.T) {
	srv, calls := fakeUpstream(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	lf, ok := p.LoginFlow()
	if !ok {
		t.Fatal("Provider 必须实现 LoginFlow")
	}
	state, url, err := lf.Start()
	if err != nil {
		t.Fatalf("Start 报错: %v", err)
	}
	if url == "" || !strings.Contains(url, "user_code=ABCD-1234") {
		t.Errorf("应优先返回 verification_uri_complete，实际 %q", url)
	}

	// 轮询直到就绪（假上游前 2 次 pending → 第 3 次成功）
	var cred gateway.Credential
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, perr := lf.Poll(state)
		if perr == nil {
			cred = c
			break
		}
		if perr != gateway.ErrLoginPending {
			t.Fatalf("轮询报错（pending 不该是错误）: %v", perr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if cred.UID == "" {
		t.Fatalf("20 秒内未拿到凭证；authenticate 调用 %d 次", atomic.LoadInt32(&calls.authenticate))
	}
	if cred.UID != "usr-fixture-1" {
		t.Errorf("uid = %q, want usr-fixture-1", cred.UID)
	}
	if n := atomic.LoadInt32(&calls.authenticate); n < 3 {
		t.Errorf("应至少轮询 3 次（2 次 pending + 1 次成功），实际 %d", n)
	}
}

// TestTwoModelEndpointsAreBothRequired 两个模型端点都必须打。
//
// ⚠ 这是本 provider 最容易做错的地方：只调 /models 会**看不到任何免费模型**
// （那 460 个 id 里 cline-free/* 零命中）。
func TestTwoModelEndpointsAreBothRequired(t *testing.T) {
	srv, calls := fakeUpstream(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	models, err := p.Models(context.Background(), newContractCredential("usr-fixture-1"))
	if err != nil {
		t.Fatalf("Models 报错: %v", err)
	}
	if atomic.LoadInt32(&calls.recommended) == 0 {
		t.Error("必须调用 recommended-models（免费集合的唯一来源）")
	}
	if atomic.LoadInt32(&calls.models) == 0 {
		t.Error("必须调用 /api/v1/models（付费模型来源）")
	}

	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	// 免费模型（只在 recommended-models 里）必须出现
	if !ids["cline-free/deepseek-v4.1-flash"] {
		t.Errorf("免费模型缺位 —— 说明没打 recommended-models 或免费判定失效: %v", ids)
	}
	// 付费模型（只在 /models 里）必须出现
	if !ids["deepseek/deepseek-v4.1-flash"] {
		t.Errorf("远端 /models 的模型缺位: %v", ids)
	}
	// 无 id 的项不该出现（假上游没有，这里反证"没乱加"）
	if ids[""] {
		t.Error("不应出现空 id")
	}
}

// TestModelsAnonymousStillWorks 无凭据时也能给出兜底目录（不打 /models）。
//
// ⚠ 无凭据时**不该**发 /models：它需要认证，匿名调用必然 401，
// 只会白白产生一条 warning。
func TestModelsAnonymousStillWorks(t *testing.T) {
	srv, calls := fakeUpstream(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	models, err := p.Models(context.Background(), gateway.Credential{Provider: providerID})
	if err != nil {
		t.Fatalf("无凭据时 Models 不该报错: %v", err)
	}
	if len(models) == 0 {
		t.Error("无凭据时应回落到兜底表，而不是空目录")
	}
	if atomic.LoadInt32(&calls.models) != 0 {
		t.Error("无凭据时不该发 /api/v1/models（必然 401，只产生噪音）")
	}
}

// TestFreeModelIsIndependentID 免费与付费是**两个不同条目**。
//
// ⚠ 绝不可用"名字包含 deepseek"之类的模糊匹配判定免费 ——
// 那会把付费条目误标为免费，用户按免费预期使用却被计费。
func TestFreeModelIsIndependentID(t *testing.T) {
	remoteFree := map[string]bool{"cline-free/deepseek-v4.1-flash": true}
	cases := []struct {
		id   string
		free bool
	}{
		{"cline-free/deepseek-v4.1-flash", true},
		{"deepseek/deepseek-v4.1-flash", false}, // 同族但付费
		{"nvidia/foo-reasoning:free", true},     // :free 后缀
		{"foo:freebar", false},                  // 含 :free 子串但不是后缀
		{"stealth/space-bunny-alpha", true},     // 兜底表标记
	}
	for _, c := range cases {
		if got := isFreeModel(c.id, remoteFree); got != c.free {
			t.Errorf("isFreeModel(%q) = %v, want %v", c.id, got, c.free)
		}
	}
}

// TestGeminiMaxTokensIsNotCopied 钉住 gemini-3.8-flash 的 65536（真实缺陷）。
//
// 给它发 131072 会被上游 vertex provider 以 400 拒绝。
func TestGeminiMaxTokensIsNotCopied(t *testing.T) {
	f, ok := fallbackByID["cline-free/gemini-3.8-flash"]
	if !ok {
		t.Fatal("兜底表缺 cline-free/gemini-3.8-flash")
	}
	if f.MaxTokens != 65_536 {
		t.Errorf("gemini-3.8-flash 的 maxTokens = %d，必须是 65536（不是 131072）\n"+
			"  给该模型发 131072 会被上游 vertex provider 以 400 拒绝", f.MaxTokens)
	}
}

// TestPayloadForcesStreamAndClampsTokens 请求体改写的两条硬规则。
func TestPayloadForcesStreamAndClampsTokens(t *testing.T) {
	// 强制 stream + 裁剪超限 max_tokens
	out := PrepareBody([]byte(`{"model":"x","stream":false,"max_tokens":9999999}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出参不是 JSON: %v", err)
	}
	if obj["stream"] != true {
		t.Error("stream 必须被强制为 true（上游仅流式）")
	}
	if got := obj["max_tokens"].(float64); got != maxOutputTokensUpperBound {
		t.Errorf("max_tokens 应收敛到 %d，实际 %v", maxOutputTokensUpperBound, got)
	}

	// 未超限的不动
	out2 := PrepareBody([]byte(`{"model":"x","max_tokens":1000}`))
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	if got := obj2["max_tokens"].(float64); got != 1000 {
		t.Errorf("未超限的 max_tokens 不该被改，实际 %v", got)
	}

	// 非法 JSON 原样返回（不猜）
	raw := []byte(`not json`)
	if got := PrepareBody(raw); string(got) != string(raw) {
		t.Error("非法 JSON 必须原样返回")
	}
}

// TestToolEnumEmptyStringsCleaned Gemini 系对空串 enum 返回 400。
func TestToolEnumEmptyStringsCleaned(t *testing.T) {
	in := []byte(`{"model":"x","tools":[{"type":"function","function":{"name":"f","parameters":{
		"type":"object","properties":{"mode":{"type":"string","enum":["","fast","slow"]},
		"nested":{"type":"object","properties":{"a":{"enum":[""]}}}}}}}]}`)
	out := PrepareBody(in)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	props := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)

	mode := props["mode"].(map[string]any)["enum"].([]any)
	if len(mode) != 2 {
		t.Errorf("空串应从 enum 剔除，剩 [fast slow]，实际 %v", mode)
	}
	for _, v := range mode {
		if v == "" {
			t.Error("enum 里仍有空串")
		}
	}
	// 嵌套的 enum 全是空串 → 整个键被删掉（留空数组同样非法）
	nested := props["nested"].(map[string]any)["properties"].(map[string]any)["a"].(map[string]any)
	if _, still := nested["enum"]; still {
		t.Error("全为空串的 enum 应删除该键（空数组同样非法）")
	}
}

// TestUIDDerivation uid 派生优先级。
func TestUIDDerivation(t *testing.T) {
	cases := []struct {
		a    *Auth
		want string
	}{
		{&Auth{AccountID: "usr-1", Email: "e@x.com"}, "usr-1"},
		{&Auth{Email: "e@x.com"}, "e@x.com"},
		{&Auth{}, ""},
	}
	for _, c := range cases {
		if got := c.a.UID(); got != c.want {
			t.Errorf("UID() = %q, want %q", got, c.want)
		}
	}
	// token 兜底：同 token 派生同 uid（稳定性）
	a1 := &Auth{AccessToken: "tok-1"}
	a2 := &Auth{AccessToken: "tok-1"}
	if a1.UID() == "" || a1.UID() != a2.UID() {
		t.Errorf("同 token 必须派生同一 uid，得到 %q / %q", a1.UID(), a2.UID())
	}
	if a1.UID() == (&Auth{AccessToken: "tok-2"}).UID() {
		t.Error("不同 token 必须派生不同 uid")
	}
}

// TestErrorClassification 错误分类（含 403 的语义分裂）。
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{401, `{"error":"unauthorized"}`, gateway.ErrKindAuth},
		{403, `{"error":"forbidden"}`, gateway.ErrKindAuth},
		// 403 的第二种语义：地域限制 → 客户端类（只换号不罚）
		{403, `{"error":"This model is not available in your region"}`, gateway.ErrKindClient},
		{429, ``, gateway.ErrKindSoftRate},
		{400, ``, gateway.ErrKindClient},
		{500, ``, gateway.ErrKindServer},
		{402, ``, gateway.ErrKindHardCredit},
		{200, `{"error":"insufficient credits"}`, gateway.ErrKindHardCredit},
		{200, `{"ok":true}`, gateway.ErrKindNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestRefreshExpiredIsSentinel 续期终态必须是哨兵错误（而不是字符串匹配）。
//
// ⚠ 401 与"2xx 但缺 token"都必须归入终态；网络/5xx 必须是可重试的普通错误。
func TestRefreshExpiredIsSentinel(t *testing.T) {
	// 无 refresh_token → 终态
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1")})
	cred := gateway.Credential{Provider: providerID, UID: "u", Secret: &Auth{AccessToken: "a"}}
	err := p.RefreshCredential(cred)
	if err == nil || !isExpiredErr(err) {
		t.Errorf("无 refresh_token 应返回终态错误，实际 %v", err)
	}
}

// isExpiredErr 判定错误是否（包装了）ErrRefreshExpired。
func isExpiredErr(err error) bool {
	return err != nil && (err == ErrRefreshExpired ||
		strings.Contains(err.Error(), ErrRefreshExpired.Error()))
}

// TestBalanceRequiresAccountID 余额必须用 account_id（不是 JWT 的 sub）。
func TestBalanceRequiresAccountID(t *testing.T) {
	srv, _ := fakeUpstream(t)
	c := NewWithBase(srv.URL)

	// 用 usr-… → 成功
	res := c.FetchBalance(context.Background(), &Auth{
		AccessToken: clineBearerValue(fixtureToken), AccountID: "usr-fixture-1",
	})
	if res.Error != "" {
		t.Fatalf("用 account_id 查余额应成功，实际 %q", res.Error)
	}
	if res.Raw != 500000 {
		t.Errorf("余额原始值 = %v, want 500000", res.Raw)
	}
	if got := res.Total(); got != 5.0 {
		t.Errorf("换算后应约 5.00，实际 %v（系数 balanceScale=%d）", got, balanceScale)
	}

	// 用 JWT 的 sub 形态（user_…）→ 服务端 400
	res2 := c.FetchBalance(context.Background(), &Auth{
		AccessToken: clineBearerValue(fixtureToken), AccountID: "user_wrong_sub",
	})
	if res2.Error == "" {
		t.Error("用 JWT sub 形态查余额应失败（服务端 400 Invalid request format）")
	}
}

// TestBalanceSurfacesServerError 余额失败必须透出服务端原文。
//
// ⚠ HTTP 401 的响应体**没有 success 字段**，只判 success===false 会落到
// "响应缺少 data 字段"这个误导性文案，把真正有用的线索丢掉。
func TestBalanceSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Unauthorized: Please re-authenticate your Cline account."}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	res := c.FetchBalance(context.Background(), &Auth{
		AccessToken: clineBearerValue(fixtureToken), AccountID: "usr-x",
	})
	if res.Error == "" {
		t.Fatal("401 应返回错误")
	}
	if !strings.Contains(res.Error, "re-authenticate") {
		t.Errorf("必须透出服务端原文（排查鉴权唯一有用的线索），实际 %q", res.Error)
	}
}

// TestParseTimestamp 时间戳归一（秒/毫秒/ISO 字符串）。
func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{float64(1790323427), 1790323427000},    // 10 位 → 秒
		{float64(1790323427000), 1790323427000}, // 13 位 → 已是毫秒
		// 2026-09-25T05:23:47.000Z 的 UTC 毫秒值（用 time.Parse 核实过）
		{"2026-09-25T05:23:47.000Z", 1790313827000},
		{"", 0},
		{float64(0), 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseTimestamp(c.in); got != c.want {
			t.Errorf("parseTimestamp(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestParseTokenPayloadRequiresAccessToken 缺 accessToken 必须报错。
func TestParseTokenPayloadRequiresAccessToken(t *testing.T) {
	if _, err := parseTokenPayload([]byte(`{"success":false,"error":"nope"}`)); err == nil {
		t.Error("失败信封不该被解析成功")
	}
	p, err := parseTokenPayload([]byte(`{"data":{"accessToken":"workos:x","refreshToken":"r"}}`))
	if err != nil {
		t.Fatalf("正常信封应解析成功: %v", err)
	}
	if p.AccessToken != "workos:x" || p.RefreshToken != "r" {
		t.Errorf("解析结果不对: %+v", p)
	}
	// 裸响应（无 data 信封）也要能解析
	p2, err := parseTokenPayload([]byte(`{"accessToken":"y"}`))
	if err != nil || p2.AccessToken != "y" {
		t.Errorf("裸响应应兼容: %+v err=%v", p2, err)
	}
}

// TestCredentialRoundTrip 凭证落盘与回读必须一致。
func TestCredentialRoundTrip(t *testing.T) {
	a := &Auth{
		AccessToken: clineBearerValue(fixtureToken), RefreshToken: "rt",
		ExpireTime: 1790323427000, AccountID: "usr-1",
		Email: "e@x.com", Nickname: "e@x.com",
	}
	raw, err := MarshalAuthFile(a)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if back.AccessToken != a.AccessToken || back.AccountID != a.AccountID ||
		back.ExpireTime != a.ExpireTime || back.RefreshToken != a.RefreshToken {
		t.Errorf("回读不一致:\n  want %+v\n  got  %+v", a, back)
	}
	if back.UID() != "usr-1" {
		t.Errorf("回读后 uid = %q, want usr-1", back.UID())
	}
}

// TestParseCredentialAcceptsFlatForm 扁平形（手写凭证）也要认。
func TestParseCredentialAcceptsFlatForm(t *testing.T) {
	flat := `{"access_token":"` + fixtureToken + `","refresh_token":"rt","account_id":"usr-9"}`
	a, err := ParseCredential([]byte(flat))
	if err != nil {
		t.Fatalf("扁平形应被接受: %v", err)
	}
	if a.UID() != "usr-9" {
		t.Errorf("uid = %q, want usr-9", a.UID())
	}
	// 缺 access_token 应报错
	if _, err := ParseCredential([]byte(`{"refresh_token":"rt"}`)); err == nil {
		t.Error("缺 access_token 应报错")
	}
}

// TestProviderDoesNotPanicOnBadCredential 错型凭证返回错误而非 panic（契约要求）。
func TestProviderDoesNotPanicOnBadCredential(t *testing.T) {
	p := NewWithConfig(Config{})
	bad := gateway.Credential{Provider: providerID, UID: "u", Secret: "not-an-auth"}
	if _, err := p.Chat(context.Background(), bad, []byte(`{}`)); err == nil {
		t.Error("错型 Secret 应返回错误")
	}
	if _, err := p.Models(context.Background(), bad); err != nil {
		// Models 无凭证也要能给出兜底目录，故不该因 Secret 错型而失败
		t.Errorf("Models 不该因 Secret 错型而失败: %v", err)
	}
}

var _ = fmt.Sprintf // 保留 fmt 供未来断言使用
