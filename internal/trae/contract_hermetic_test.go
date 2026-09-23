// contract_hermetic_test.go 让 gateway.RunProviderContract **在 CI 里真的跑起来**。
//
// 与 loomy 的 contract_hermetic_test.go 同一条修法：用 httptest 起一个
// 假上游（实现对话/模型/签到/额度/续期五个端点），把三个 host 全指过去，
// 于是无网络、无真实凭证也能让契约跑满全程。
package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fixtureToken 一个合成的 JWT（形态与真货一致：三段 base64 点分）。
// 契约只验证"形态与行为"，不验证"这个具体值能用"。
const fixtureToken = "eyJhbGciOiJIUzI1NiJ9.eyJ1aWQiOiJ0cmFlLWZpcyJ9.sig"

// fixtureRefreshToken 合成 refreshToken。
const fixtureRefreshToken = "rt-fixture-0123456789"

// fakeCalls 记录假上游被调用的情况。
type fakeCalls struct {
	chat, models, checkinStatus, checkinClaim, entUsage, exchange int
}

// fakeUpstream 起一个**严格执行鉴权**的假上游。
//
// 严格鉴权：对话/模型要 `Authorization: Cloud-IDE-JWT <token>`，缺了就 401
// （真上游行为，见 client.go 的 soloHeaders）。宽松的假上游会让鉴权测试 fail-open。
func fakeUpstream(t *testing.T, want *Auth) (*httptest.Server, *fakeCalls) {
	t.Helper()
	calls := &fakeCalls{}
	mux := http.NewServeMux()

	authOK := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Cloud-IDE-JWT "+want.AccessToken
	}
	deny := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"401","message":"login required"}`)
	}

	// 对话：返回 SOLO 自定义 SSE（output ×2 + token_usage + done）。
	mux.HandleFunc(EpChat, func(w http.ResponseWriter, r *http.Request) {
		calls.chat++
		if !authOK(r) {
			deny(w)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id:1\nevent:metadata\ndata:{\"model\":\"\"}\n\n")
		_, _ = io.WriteString(w, "event:output\ndata:{\"response\":\"你\",\"reasoning_content\":\"想\"}\n\n")
		_, _ = io.WriteString(w, "event:output\ndata:{\"response\":\"好\"}\n\n")
		_, _ = io.WriteString(w, "event:token_usage\ndata:{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}\n\n")
		_, _ = io.WriteString(w, "event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")
	})

	// 模型：config_info_list。
	mux.HandleFunc(EpModels, func(w http.ResponseWriter, r *http.Request) {
		calls.models++
		if !authOK(r) {
			deny(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"config_info_list":[`+
			`{"config_name":"glm-5.2","display_config":{"display_name":"GLM-5.2"}},`+
			`{"config_name":"kimi-k2.6","display_config":{"display_name":"Kimi K2.6"}},`+
			`{"config_name":"DeepSeek-V4-Flash","display_config":{"display_name":"DeepSeek V4 Flash"}}]}`)
	})

	// 签到状态。
	mux.HandleFunc(EpCheckinStatus, func(w http.ResponseWriter, r *http.Request) {
		calls.checkinStatus++
		if !authOK(r) {
			deny(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"checked_in":false,"credits":5,"enable":true}`)
	})

	// 签到领取。
	mux.HandleFunc(EpCheckinClaim, func(w http.ResponseWriter, r *http.Request) {
		calls.checkinClaim++
		if !authOK(r) {
			deny(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	// 额度：两个权益包 → 1500。
	mux.HandleFunc(EpEntUsage, func(w http.ResponseWriter, r *http.Request) {
		calls.entUsage++
		if !authOK(r) {
			deny(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"is_credits_billing":true,"user_entitlement_pack_list":[`+
			`{"entitlement_base_info":{"quota":{"credits_limit":1000}}},`+
			`{"entitlement_base_info":{"quota":{"credits_limit":500}}}]}`)
	})

	// 续期：ExchangeToken（OAuth 头，不看 Cloud-IDE-JWT）。
	mux.HandleFunc(EpExchange, func(w http.ResponseWriter, r *http.Request) {
		calls.exchange++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Result":{"Token":"`+fixtureToken+`2",`+
			`"TokenExpireAt":1786847930141,"RefreshToken":"`+fixtureRefreshToken+`2"}}`)
	})

	srv := httptest.NewServer(mux)
	return srv, calls
}

// newContractCredential 造一个契约测试用的凭证。
func newContractCredential(uid string) gateway.Credential {
	return gateway.Credential{
		Provider: providerID,
		UID:      uid,
		Nickname: "fixture",
		Secret: &Auth{
			AccessToken:  fixtureToken,
			RefreshToken: fixtureRefreshToken,
			ExpiresAt:    time.Now().Add(24 * time.Hour).Unix(),
			UID:          uid,
			Nickname:     "fixture",
		},
	}
}

// TestProbeHealth 主动健康检查（A2 移植）：探测成功/失败/凭证错。
func TestProbeHealth(t *testing.T) {
	want := &Auth{AccessToken: fixtureToken, UID: "u1"}
	srv, _ := fakeUpstream(t, want)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	// 成功：假上游的模型端点认这个 token。
	if err := p.ProbeHealth(context.Background(), newContractCredential("u1")); err != nil {
		t.Errorf("健康探测应成功: %v", err)
	}
	// 失败：token 不对 → 假上游 401。
	bad := newContractCredential("u1")
	bad.Secret = &Auth{AccessToken: "wrong-token", UID: "u1"}
	if err := p.ProbeHealth(context.Background(), bad); err == nil {
		t.Error("错误 token 的健康探测应失败")
	}
	// 凭证类型错：报错不 panic。
	if err := p.ProbeHealth(context.Background(), gateway.Credential{Secret: "nope"}); err == nil {
		t.Error("凭证类型错应报错")
	}
}

// TestContract 契约在假上游上全程执行（CI 里不跳过）。
func TestContract(t *testing.T) {
	want := &Auth{AccessToken: fixtureToken, UID: "contract-fixture"}
	srv, _ := fakeUpstream(t, want)
	gateway.RunProviderContract(t, func() gateway.Provider {
		return NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	}, gateway.WithCredential(newContractCredential("contract-fixture")))
}

// TestContractIsNotConditionallySkipped 契约必须无凭证也跑（与 loomy 同一条）。
func TestContractIsNotConditionallySkipped(t *testing.T) {
	t.Setenv("TRAE_TOKEN", "")
	t.Setenv("TRAE_TEST_CRED", "")
	want := &Auth{AccessToken: fixtureToken, UID: "no-env-fixture"}
	srv, _ := fakeUpstream(t, want)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gateway.RunProviderContract(t, func() gateway.Provider {
			return NewWithConfig(Config{Client: NewWithBase(srv.URL)})
		}, gateway.WithCredential(newContractCredential("no-env-fixture")))
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("契约在无凭证环境下未完成 —— CI 里会变成永远挂住")
	}
}

// TestChatConvertsSOLOToOpenAI 对话流必须被转成 OpenAI SSE。
func TestChatConvertsSOLOToOpenAI(t *testing.T) {
	want := &Auth{AccessToken: fixtureToken, UID: "u1"}
	srv, _ := fakeUpstream(t, want)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	cs, err := p.Chat(context.Background(), newContractCredential("u1"),
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != http.StatusOK {
		t.Fatalf("status=%d", cs.Status)
	}
	raw, _ := io.ReadAll(cs.Body)
	body := string(raw)
	if !strings.Contains(body, `"choices"`) {
		t.Errorf("输出不是 OpenAI chunk（缺 choices）: %s", body)
	}
	if !strings.Contains(body, `"content":"你"`) || !strings.Contains(body, `"content":"好"`) {
		t.Errorf("输出缺少 content 增量: %s", body)
	}
	if !strings.Contains(body, `"reasoning_content":"想"`) {
		t.Errorf("输出缺少 reasoning_content: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("输出缺少 [DONE]: %s", body)
	}
}

// TestChatBadCredentialReturnsError 凭证类型不对应报错，不 panic。
func TestChatBadCredentialReturnsError(t *testing.T) {
	want := &Auth{AccessToken: fixtureToken, UID: "u1"}
	srv, _ := fakeUpstream(t, want)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	if _, err := p.Chat(context.Background(), gateway.Credential{Secret: "not-an-auth"}, nil); err == nil {
		t.Fatal("凭证类型不对应报错")
	}
}

// TestSSEStreamErrorBecomesInBandError 流内 event:error → OpenAI 错误信封。
//
// 出口层（internal/server）认 choices 为空 + error 字段的帧为流内错误，
// 会交给本上游的 Classify 分类 —— 所以转换必须保留 code（1005 → 硬冷却）。
func TestSSEStreamErrorBecomesInBandError(t *testing.T) {
	var sb strings.Builder
	if err := ConvertSOLOToOpenAI(strings.NewReader(
		"event:error\ndata:{\"code\":1005,\"message\":\"plan limit\"}\n\n"), &sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, `"error"`) {
		t.Errorf("流内错误未转成错误信封: %s", out)
	}
	if !strings.Contains(out, "1005") {
		t.Errorf("错误信封应保留 code 1005: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("错误后缺 [DONE]: %s", out)
	}
}

// TestSSEAlwaysTerminatesWithDone 空流也要以 [DONE] 收尾（出口层契约）。
func TestSSEAlwaysTerminatesWithDone(t *testing.T) {
	var sb strings.Builder
	if err := ConvertSOLOToOpenAI(strings.NewReader(""), &sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "data: [DONE]") {
		t.Errorf("空流必须补 [DONE]: %q", sb.String())
	}
}

// TestClassifyPins 分类判据逐条钉住。
func TestClassifyPins(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{"1005 流内信封", http.StatusOK, `{"error":{"code":"1005","message":"plan limit"}}`, gateway.ErrKindHardCredit},
		{"1005 明文", http.StatusOK, `{"code":1005,"message":"plan limit"}`, gateway.ErrKindHardCredit},
		{"401 登录失效", http.StatusUnauthorized, `{"code":"401","message":"login required"}`, gateway.ErrKindSessionDead},
		{"429 限流", http.StatusTooManyRequests, `rate limited`, gateway.ErrKindSoftRate},
		{"404 模型不存在", http.StatusNotFound, `model not found`, gateway.ErrKindNotFound},
		{"500 上游故障", http.StatusInternalServerError, `oops`, gateway.ErrKindServer},
		{"400 参数错", http.StatusBadRequest, `bad request`, gateway.ErrKindClient},
		{"200 正常帧", http.StatusOK, `{"choices":[{"delta":{"content":"hi"}}]}`, gateway.ErrKindNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d, %q) = %v，want %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

// TestPrepareBodyPins 请求体改写逐条钉住。
func TestPrepareBodyPins(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("改写输出不是 JSON: %v（%s）", err, string(out))
	}
	if obj["stream"] != true {
		t.Error("stream 必须被强制为 true")
	}
	if obj["function"] != Function {
		t.Errorf("function = %v，want %q", obj["function"], Function)
	}
	if obj["config_name"] != "glm-5.2" {
		t.Errorf("config_name = %v", obj["config_name"])
	}
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	content := first["content"].([]any)
	text := content[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "hi" {
		t.Errorf("content 未转成 text 数组: %v", content)
	}
}

// TestQuotaRefresh 额度 = 权益包 credits_limit 求和（1000+500=1500）。
func TestQuotaRefresh(t *testing.T) {
	want := &Auth{AccessToken: fixtureToken, UID: "u1"}
	srv, _ := fakeUpstream(t, want)
	dir := t.TempDir()
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir})

	// 把凭证写进目录，让 authByUID 找得到。
	a := &Auth{AccessToken: fixtureToken, RefreshToken: fixtureRefreshToken, UID: "u1", Nickname: "n"}
	raw, err := MarshalAuthFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName(a)), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	qv, ok := p.RefreshQuota("u1")
	if !ok || !qv.HasData {
		t.Fatalf("应当有数据: %+v ok=%v", qv, ok)
	}
	if qv.Remaining != 1500 {
		t.Errorf("Remaining = %d，want 1500（1000+500）", qv.Remaining)
	}
}

// TestCredentialParseRoundTrip 嵌套形/扁平形解析 + 落盘回读。
func TestCredentialParseRoundTrip(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1786847930},` +
		`"account":{"uid":"u1","nickname":"n1"}}`)
	a, err := Parse(nested)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessToken != "at" || a.UID != "u1" || a.Nickname != "n1" {
		t.Fatalf("嵌套形解析错: %+v", a)
	}

	flat := []byte(`{"accessToken":"at2","refreshToken":"rt2","uid":"u2","machineId":"m1","deviceId":"d1"}`)
	a2, err := Parse(flat)
	if err != nil {
		t.Fatal(err)
	}
	if a2.AccessToken != "at2" || a2.MachineID != "m1" || a2.DeviceID != "d1" {
		t.Fatalf("扁平形解析错: %+v", a2)
	}

	raw, err := MarshalAuthFile(a2)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(raw)
	if err != nil {
		t.Fatalf("落盘形态回读失败: %v", err)
	}
	if back.AccessToken != "at2" || back.MachineID != "m1" {
		t.Fatalf("回读不一致: %+v", back)
	}

	if _, err := Parse([]byte(`{"foo":1}`)); err == nil {
		t.Error("缺 accessToken 的凭证应报错")
	}
}

// TestNormalizeExpiresAt 毫秒/秒归一化。
func TestNormalizeExpiresAt(t *testing.T) {
	if got := normalizeExpiresAt(1786847930141); got != 1786847930 {
		t.Errorf("毫秒归一化 = %d，want %d", got, 1786847930)
	}
	if got := normalizeExpiresAt(1786847930); got != 1786847930 {
		t.Errorf("秒值不应被改动: %d", got)
	}
}

// TestAuthNeedsRefresh 过期窗口判断。
func TestAuthNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	if !a.NeedsRefresh(10 * time.Minute) {
		t.Error("5 分钟后过期、窗口 10 分钟 → 应该需要刷新")
	}
	if a.NeedsRefresh(1 * time.Minute) {
		t.Error("5 分钟后过期、窗口 1 分钟 → 不该刷新")
	}
	a2 := &Auth{ExpiresAt: 0}
	if !a2.NeedsRefresh(time.Hour) {
		t.Error("无过期时间的凭证应视为需要刷新")
	}
}

// TestCredentialNewFieldsParse 新字段（clientId / refreshExpiresAt）的多形态解析与回读。
//
// 这两个字段是"账号突然过期"修复的一部分：ClientID 必须随凭证保存
// （ExchangeToken 要用签发它的那个 id），refreshToken 自身有效期必须跟踪
// （到期前预警，避免静默死亡后盲重试计数禁用）。
func TestCredentialNewFieldsParse(t *testing.T) {
	// 嵌套形：auth.clientId + auth.refreshExpiresAt。
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1786847930,` +
		`"refreshExpiresAt":1786847930000,"clientId":"cid-nested"},` +
		`"account":{"uid":"u1","nickname":"n1"}}`)
	a, err := Parse(nested)
	if err != nil {
		t.Fatal(err)
	}
	if a.ClientID != "cid-nested" {
		t.Errorf("嵌套形 clientId = %q", a.ClientID)
	}
	// 毫秒 → 秒归一化。
	if a.RefreshExpiresAt != 1786847930 {
		t.Errorf("嵌套形 refreshExpiresAt = %d, want 1786847930", a.RefreshExpiresAt)
	}

	// 顶层 clientId / clientID / client_id 三种命名都认（Trae2api-cn 导出的形态）。
	flat := []byte(`{"accessToken":"at2","refreshToken":"rt2","uid":"u2","clientId":"cid-flat","refreshExpireAt":1786847930}`)
	a2, err := Parse(flat)
	if err != nil {
		t.Fatal(err)
	}
	if a2.ClientID != "cid-flat" {
		t.Errorf("扁平形 clientId = %q", a2.ClientID)
	}
	if a2.RefreshExpiresAt != 1786847930 {
		t.Errorf("扁平形 refreshExpiresAt = %d", a2.RefreshExpiresAt)
	}

	// 落盘形态（嵌套）回读不丢字段。
	raw, err := MarshalAuthFile(a2)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(raw)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if back.ClientID != "cid-flat" || back.RefreshExpiresAt != 1786847930 {
		t.Errorf("回读丢字段: clientId=%q refreshExpiresAt=%d", back.ClientID, back.RefreshExpiresAt)
	}

	// RefreshTokenDead 判定：<=0 = 未知 → 不死；已过期 → 死。
	unknown := &Auth{RefreshToken: "rt", RefreshExpiresAt: 0}
	if unknown.RefreshTokenDead() {
		t.Error("RefreshExpiresAt=0（未知）不应判死")
	}
	dead := &Auth{RefreshToken: "rt", RefreshExpiresAt: time.Now().Unix() - 1}
	if !dead.RefreshTokenDead() {
		t.Error("已过期的 refreshToken 应判死")
	}
	alive := &Auth{RefreshToken: "rt", RefreshExpiresAt: time.Now().Unix() + 3600}
	if alive.RefreshTokenDead() {
		t.Error("未过期的 refreshToken 不应判死")
	}
}

// TestChat401AutoRefreshRetry 401 自愈：对话遇 401 → ExchangeToken 续期 → 原请求重试一次。
//
// 修复"账号突然过期"的核心路径：token 在两次刷新之间过期时，以前是
// 401 → SessionDead 计数 → 3 次禁用；现在同一账号先自救（续期+重试），
// 救得回来就继续服务。
func TestChat401AutoRefreshRetry(t *testing.T) {
	oldToken := fixtureToken
	newToken := "at-refreshed-001"

	var exchangeCalls, chatCalls401, chatCallsOK int
	mux := http.NewServeMux()
	// ExchangeToken：换新 token + 轮换 refreshToken + 带 refreshExpiresAt。
	mux.HandleFunc(EpExchange, func(w http.ResponseWriter, r *http.Request) {
		exchangeCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Result":{"Token":"at-refreshed-001",`+
			`"TokenExpireAt":1786847930141,"RefreshToken":"rt-new","RefreshExpireAt":1786847930141}}`)
	})
	// 对话：旧 token 一律 401，新 token 才放行（200 SOLO SSE）。
	mux.HandleFunc(EpChat, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Cloud-IDE-JWT "+newToken {
			chatCallsOK++
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "id:1\nevent:metadata\ndata:{\"model\":\"\"}\n\n")
			_, _ = io.WriteString(w, "event:output\ndata:{\"response\":\"你好\"}\n\n")
			_, _ = io.WriteString(w, "event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")
			return
		}
		chatCalls401++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"401","message":"token expired"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := &Auth{
		AccessToken:  oldToken,
		RefreshToken: fixtureRefreshToken,
		ExpiresAt:    time.Now().Add(24 * time.Hour).Unix(),
	}
	// 模拟池子保管的凭证：同一对象在续期时被原地更新。
	cred := newContractCredential("u401")
	cred.Secret = a

	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	cs, err := p.Chat(context.Background(), cred,
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != http.StatusOK {
		t.Fatalf("401 自愈后 status=%d，want 200", cs.Status)
	}
	raw, _ := io.ReadAll(cs.Body)
	if !strings.Contains(string(raw), `"content":"你好"`) {
		t.Errorf("自愈重试的输出不是 OpenAI chunk: %s", raw)
	}
	if exchangeCalls != 1 {
		t.Errorf("ExchangeToken 调用次数 = %d, want 1", exchangeCalls)
	}
	if chatCalls401 != 1 || chatCallsOK != 1 {
		t.Errorf("对话调用 = 401:%d 成功:%d，want 401:1 成功:1", chatCalls401, chatCallsOK)
	}
	// 续期原地生效：凭证对象被更新为新 token。
	if a.AccessToken != newToken {
		t.Errorf("凭证未被续期: AccessToken=%q", a.AccessToken)
	}
	if a.RefreshToken != "rt-new" {
		t.Errorf("refreshToken 未轮换: %q", a.RefreshToken)
	}
	if a.RefreshExpiresAt != 1786847930 {
		t.Errorf("refreshExpiresAt 未落字段: %d", a.RefreshExpiresAt)
	}
}
