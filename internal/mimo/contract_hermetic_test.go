// contract_hermetic_test.go MiMo 的假上游契约测试（判据 2 的载体）。
//
// 设计红线对应"评审报告 §4.8 测试计划"，尤其：
//   - paid SSE **逐字节直通**（MiMo2API 重建流丢 tool_calls/usage 的反面）；
//   - **每请求不 bootstrap/不 refresh**（TRAE 恒真预检事故的回归防线）；
//   - free/oauth 的 401 自愈是**真的**（trae 曾"注释里存在"，别再犯）。
package mimo

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// ── 假上游 ──────────────────────────────────────────────────────────────

type fakeCalls struct {
	chat, models, bootstrap, oauthToken int32
}

type fakeUpstream struct {
	srv *httptest.Server
	c   *fakeCalls

	mu      sync.Mutex
	wantKey string // paid Bearer 期望值
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{c: &fakeCalls{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.c.chat, 1)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.Header.Get("api-key")
		}
		f.mu.Lock()
		want := f.wantKey
		f.mu.Unlock()
		if want != "" && tok != want {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key","param":"Please provide valid API Key","code":"401","type":"invalid_key"}}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if hasUnbackedToolHistory(raw) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Param Incorrect","param":"The reasoning_content in the thinking mode must be passed back to the API.","code":"400"}}`))
			return
		}
		if r.Header.Get("X-Mimo-Source") == "" || !strings.HasPrefix(r.Header.Get("User-Agent"), "mimocode/") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"missing client headers","code":"403"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"考\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_time\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.c.models, 1)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		want := f.wantKey
		f.mu.Unlock()
		if want != "" && tok != want {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key","code":"401","type":"invalid_key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"mimo-v2.6-pro"},{"id":"mimo-v2.6-flash"},{"id":"mimo-v2.5-pro[1m]"}]}`)
	})

	mux.HandleFunc("/api/free-ai/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.c.bootstrap, 1)
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		fp, _ := body["client"].(string)
		if len(fp) != 64 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		jwt := makeTestJWT(time.Now().Add(time.Hour).Unix())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jwt":"`+jwt+`"}`)
	})

	mux.HandleFunc("/api/free-ai/openai/chat", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.c.chat, 1)
		// free 面：接受一切能过 parse 的 JWT（自愈测试用旧票=过期即 401）。
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if exp := jwtExpiry(tok); exp == 0 || time.Now().Unix() >= exp {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"jwt expired","code":"401"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"free答\"}}]}\n\ndata: [DONE]\n\n")
	})

	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.c.oauthToken, 1)
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"oa-2","refresh_token":"rt-2","expires_in":7200}`)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// setWant 更新 paid 期望 key（oauth 续期测试里 want 会换）。
func (f *fakeUpstream) setWant(k string) {
	f.mu.Lock()
	f.wantKey = k
	f.mu.Unlock()
}

// lastRequestHeaders 抓取下一次 paid chat 请求的完整头（api-key 两态钉）。
func (f *fakeUpstream) captureNextChatHeaders() (chan http.Header, func(http.ResponseWriter, *http.Request)) {
	ch := make(chan http.Header, 1)
	return ch, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		ch <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}
}

func makeTestJWT(exp int64) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := fmt.Sprintf(`{"iss":"xiaomi-ai","iat":%d,"exp":%d}`, exp-3600, exp)
	return hdr + "." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

func hasUnbackedToolHistory(raw []byte) bool {
	var doc struct {
		Messages []struct {
			Role             string `json:"role"`
			ToolCalls        []any  `json:"tool_calls"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	for _, m := range doc.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ReasoningContent == "" {
			return true
		}
	}
	return false
}

func paidCred(key string) gateway.Credential {
	return gateway.Credential{Provider: providerID, UID: "u-paid", Nickname: "t",
		Secret: &Auth{Channel: ChannelPaid, Type: TypeAPI, Key: key, UID: "u-paid"}}
}

func newPaidProvider(f *fakeUpstream) *Provider {
	return NewWithConfig(Config{Client: NewWithBase(f.srv.URL)})
}

// ── 契约本体 ────────────────────────────────────────────────────────────

func TestContract(t *testing.T) {
	gateway.RunProviderContract(t, NewProvider,
		gateway.WithCredential(paidCred("sk-contract0001")))
}

// ── paid 直通与帧完整性 ─────────────────────────────────────────────────

func TestPaidChatPassthroughKeepsAllFrames(t *testing.T) {
	f := newFakeUpstream(t)
	key := "sk-testkey0000000001"
	f.setWant(key)
	p := newPaidProvider(f)
	body := []byte(`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	cs, err := p.Chat(context.Background(), paidCred(key), body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	raw, _ := io.ReadAll(cs.Body)
	s := string(raw)
	for _, frag := range []string{`"tool_calls"`, `"usage"`, "想", "考", "data: [DONE]"} {
		if !strings.Contains(s, frag) {
			t.Errorf("直通丢了 %q:\n%s", frag, s)
		}
	}
}

// ── reasoning 方言闭环 ─────────────────────────────────────────────────

func TestReasoningBackfillRoundTrip(t *testing.T) {
	f := newFakeUpstream(t)
	key := "sk-testkey0000000002"
	f.setWant(key)
	p := newPaidProvider(f)

	first := []byte(`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"几点了"}],"stream":true}`)
	cs, err := p.Chat(context.Background(), paidCred(key), first)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(cs.Body)
	_ = cs.Body.Close() // Close → 旁路聚合收尾 → (anchor, call_1)→"思考" 进缓存

	second := []byte(`{"model":"mimo-v2.6-pro","messages":[
		{"role":"user","content":"几点了"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"a\":1}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"12:00"},
		{"role":"user","content":"谢谢"}],"stream":true}`)
	cs2, err := p.Chat(context.Background(), paidCred(key), second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs2.Body.Close() }()
	if cs2.Status != 200 {
		raw, _ := io.ReadAll(cs2.Body)
		t.Fatalf("回注未生效：status=%d body=%s", cs2.Status, raw)
	}
}

func TestReasoningDowngradeOnColdCache(t *testing.T) {
	f := newFakeUpstream(t)
	key := "sk-testkey0000000003"
	f.setWant(key)
	p := newPaidProvider(f)
	body := []byte(`{"model":"mimo-v2.6-pro","messages":[
		{"role":"user","content":"几点了"},
		{"role":"assistant","tool_calls":[{"id":"call_UNKNOWN","type":"function","function":{"name":"get_time","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_UNKNOWN","content":"12:00"},
		{"role":"user","content":"谢谢"}],"stream":true}`)
	cs, err := p.Chat(context.Background(), paidCred(key), body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != 200 {
		t.Fatalf("降级重发应成功，得到 %d", cs.Status)
	}
	if got := atomic.LoadInt32(&f.c.chat); got != 2 {
		t.Errorf("出站次数 = %d, want 2（一次 400 + 一次降级成功）", got)
	}
}

// ── 预检回归钉（TRAE 事故形状）──────────────────────────────────────────

func TestNoBootstrapPerChat(t *testing.T) {
	f := newFakeUpstream(t)
	key := "sk-testkey0000000004"
	f.setWant(key)
	p := NewWithConfig(Config{Client: NewWithBase(f.srv.URL), FreeEnabled: boolp(true)})
	body := []byte(`{"model":"mimo-v2.6-pro","messages":[{"role":"user","content":"x"}],"stream":true}`)
	for i := 0; i < 5; i++ {
		cs, err := p.Chat(context.Background(), paidCred(key), body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(cs.Body)
		_ = cs.Body.Close()
	}
	if boot := atomic.LoadInt32(&f.c.bootstrap); boot != 0 {
		t.Errorf("paid 对话触发了 %d 次 bootstrap（应 0）—— 恒真预检回归！", boot)
	}
	if ot := atomic.LoadInt32(&f.c.oauthToken); ot != 0 {
		t.Errorf("paid 对话触发了 %d 次 oauth 刷新（应 0）", ot)
	}
}

func TestRefreshSkewTrulyDisablesPreflight(t *testing.T) {
	p := NewWithConfig(Config{})
	skew, has := p.RefreshSkew(gateway.Credential{Provider: providerID, UID: "x",
		Secret: &Auth{Channel: ChannelPaid, Type: TypeAPI, Key: "sk-x"}})
	if !has {
		t.Fatal("ok=false 会回落核心 10m 兜底窗口 = 恒真预检（TRAE 事故形状），必须 ok=true")
	}
	if skew > 0 {
		t.Errorf("skew=%v 应为 <=0", skew)
	}
}

// ── free 轨 401 自愈 ────────────────────────────────────────────────────

func TestFreeChat401SelfHeal(t *testing.T) {
	f := newFakeUpstream(t)
	old := &Auth{Channel: ChannelFree, Fingerprint: strings.Repeat("a", 64), UID: "u-free"}
	old.AccessToken = makeTestJWT(time.Now().Add(-time.Hour).Unix()) // 过期票
	old.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	cred := gateway.Credential{Provider: providerID, UID: "u-free", Secret: old}

	p := NewWithConfig(Config{
		Client:      NewWithBase(f.srv.URL),
		FreeEnabled: boolp(true),
	})
	// 让 free chat 第一趟必 401（模拟"缓存票被服务端吊销"），之后放行 ——
	// 这才是自愈路径本身：bootstrap→401→DropCachedJWT→重bootstrap→重试。
	var freeHits int32
	orig := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/free-ai/openai/chat" && atomic.AddInt32(&freeHits, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"jwt revoked","code":"401"}}`))
			return
		}
		orig.ServeHTTP(w, r)
	})

	body := []byte(`{"model":"mimo-v2.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	cs, err := p.Chat(context.Background(), cred, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != 200 {
		t.Fatalf("free 401 自愈失败：status=%d", cs.Status)
	}
	if boot := atomic.LoadInt32(&f.c.bootstrap); boot != 2 {
		t.Errorf("bootstrap 次数 = %d, want 2（首取 + 自愈重取）", boot)
	}
	if got := atomic.LoadInt32(&freeHits); got != 2 {
		t.Errorf("free chat 次数 = %d, want 2（首趟 401 + 自愈重试）", got)
	}
	if strings.Count(old.AccessToken, ".") != 2 || jwtExpiry(old.AccessToken) < time.Now().Unix() {
		t.Errorf("活 secret 未原地换新票: %q", old.AccessToken)
	}
}

// ── oauth 轨刷新与轮换 ──────────────────────────────────────────────────

func TestOAuthRefreshAndRotate(t *testing.T) {
	f := newFakeUpstream(t)
	a := &Auth{Channel: ChannelPaid, Type: TypeOAuth,
		AccessToken: "oa-old", RefreshToken: "rt-1",
		ExpiresAt: time.Now().Add(-time.Minute).Unix(), UID: "u-oa"}
	p := newPaidProvider(f)
	if !a.Renewable() {
		t.Fatal("oauth+refresh 应判为可刷")
	}
	if err := p.RefreshCredential(gateway.Credential{Provider: providerID, UID: "u-oa", Secret: a}); err != nil {
		t.Fatal(err)
	}
	if a.AccessToken != "oa-2" || a.RefreshToken != "rt-2" {
		t.Errorf("刷新未轮换: %+v", a)
	}
	if a.ExpiresAt <= time.Now().Unix() {
		t.Errorf("ExpiresAt 未更新: %d", a.ExpiresAt)
	}
	if ot := atomic.LoadInt32(&f.c.oauthToken); ot != 1 {
		t.Errorf("token 端点次数 = %d, want 1", ot)
	}
	// 纯 sk：不可刷、no-op 不报错。
	sk := &Auth{Channel: ChannelPaid, Type: TypeAPI, Key: "sk-x", UID: "u-sk"}
	if sk.Renewable() {
		t.Error("纯 sk 不应可刷")
	}
	if err := p.RefreshCredential(gateway.Credential{Provider: providerID, UID: "u-sk", Secret: sk}); err != nil {
		t.Errorf("纯 sk no-op 续期不应报错: %v", err)
	}
}

// oauth 轨对话 401 自愈：旧 access 打上游 401 → 刷新 → 新 access 重试成功。
func TestOAuthChat401SelfHeal(t *testing.T) {
	f := newFakeUpstream(t)
	f.setWant("oa-2") // 上游只认刷新后的新票
	a := &Auth{Channel: ChannelPaid, Type: TypeOAuth,
		AccessToken: "oa-old", RefreshToken: "rt-1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), // 未过期（预检不刷，靠 401 自愈）
		UID:       "u-oa"}
	cred := gateway.Credential{Provider: providerID, UID: "u-oa", Secret: a}
	p := newPaidProvider(f)
	body := []byte(`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	cs, err := p.Chat(context.Background(), cred, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != 200 {
		t.Fatalf("oauth 401 自愈失败：status=%d", cs.Status)
	}
	if atomic.LoadInt32(&f.c.oauthToken) != 1 {
		t.Error("应恰好刷一次")
	}
	if a.AccessToken != "oa-2" {
		t.Errorf("活 secret 未更新: %q", a.AccessToken)
	}
}

// ── 鉴权头两态 ──────────────────────────────────────────────────────────

func TestAuthHeaderTwoModes(t *testing.T) {
	// bearer 态
	f := newFakeUpstream(t)
	key := "sk-testkey0000000007"
	f.setWant(key)
	p := newPaidProvider(f)
	// api-key 态：换一个只回头的假 server，检查两态各自形态。
	ch, handler := f.captureNextChatHeaders()
	srv2 := httptest.NewServer(http.HandlerFunc(handler))
	defer srv2.Close()
	c := New()
	c.PaidBase = srv2.URL + "/v1"
	c.AuthHeader = "api-key"
	p2 := NewWithConfig(Config{Client: c})
	go func() {
		cs, err := p2.Chat(context.Background(), paidCred("tp-abc123def456gh"),
			[]byte(`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"x"}],"stream":true}`))
		if err == nil {
			_, _ = io.ReadAll(cs.Body)
			_ = cs.Body.Close()
		}
	}()
	select {
	case h := <-ch:
		if h.Get("api-key") != "tp-abc123def456gh" {
			t.Errorf("api-key 头缺失: %v", h.Get("api-key"))
		}
		if h.Get("Authorization") != "" {
			t.Error("api-key 态必须删掉 Authorization 头（OmniProxy 纪律）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没抓到请求")
	}
	// bearer 态自检（同 server 直接成功过一次即证明默认头正确）。
	cs, err := p.Chat(context.Background(), paidCred(key),
		[]byte(`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"x"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = cs.Body.Close()
}

// ── Models 与别名 ───────────────────────────────────────────────────────

func TestModelsLiveAndStaticFallback(t *testing.T) {
	f := newFakeUpstream(t)
	key := "sk-testkey0000000008"
	f.setWant(key)
	p := newPaidProvider(f)
	ms, err := p.Models(context.Background(), paidCred(key))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range ms {
		ids[m.ID] = true
	}
	if !ids["mimo-v2.6-pro"] || !ids["mimo-v2.5-pro[1m]"] {
		t.Errorf("实时目录不符: %v", ids)
	}
	if aliasModel("mimo-2.5-pro") != "mimo-v2.5-pro[1m]" {
		t.Error("别名归一失败")
	}
	if aliasModel("mimo-v2.6-flash") != "mimo-v2.6-flash" {
		t.Error("非别名不该被改")
	}
}

// ── 错误矩阵（分类器）──────────────────────────────────────────────────

func TestClassifyMatrix(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{"401 invalid_key", 401, `{"error":{"message":"Invalid API Key","code":"401","type":"invalid_key"}}`, gateway.ErrKindSessionDead},
		{"400 421 审查", 400, `{"error":{"message":"x","code":"421","param":"sensitive"}}`, gateway.ErrKindNone},
		{"400 441 风控", 400, `{"error":{"message":"x","code":"441"}}`, gateway.ErrKindSoftRate},
		{"HTTP 441 直发", 441, `risk`, gateway.ErrKindSoftRate},
		{"402 quota", 402, `{"error":{"message":"Quota exceeded. Check your plan","code":"insufficient_quota"}}`, gateway.ErrKindHardCredit},
		{"402 balance(实测原文)", 402, `{"error":{"code":"402","message":"Insufficient account balance","type":"insufficient_balance"}}`, gateway.ErrKindHardCredit},
		{"400 balance(防包装形态)", 400, `{"error":{"code":"400","message":"no credits","type":"insufficient_balance"}}`, gateway.ErrKindHardCredit},
		{"流内 quota 词", 500, `{"type":"error","error":{"code":"insufficient_quota","message":"Quota exceeded."}}`, gateway.ErrKindHardCredit},
		{"429 裸", 429, `too many requests`, gateway.ErrKindSoftRate},
		{"FreeUsageLimit", 400, `{"error":{"code":"FreeUsageLimitError"}}`, gateway.ErrKindHardCredit},
		{"reasoning 400", 400, `{"error":{"message":"Param Incorrect","param":"The reasoning_content in the thinking mode must be passed back to the API.","code":"400"}}`, gateway.ErrKindNone},
		{"Spring 异形体", 405, `{"timestamp":"2026-09-24T00:00:00","status":405,"error":"Method Not Allowed","path":"/v1/models"}`, gateway.ErrKindClient},
		{"空 body 5xx", 502, ``, gateway.ErrKindServer},
		{"403 软处理", 403, `{"error":{"message":"Forbidden","code":"403"}}`, gateway.ErrKindNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d,…)=%v want %v", c.name, c.status, got, c.want)
		}
	}
}

func TestSoftRateParse(t *testing.T) {
	now := time.Now()
	if at, ok := ParseSoftRateReset(429, `{"error":{"message":"please Try again in 2h 5m"}}`, now); !ok || at.Sub(now) < 2*time.Hour {
		t.Errorf("try-again-in 解析失败: %v %v", at, ok)
	}
	if at, ok := ParseSoftRateReset(429, `plain`, now); !ok || at.Sub(now) < 4*time.Minute {
		t.Errorf("兜底 5min 失效: %v", at.Sub(now))
	}
}

// ── OAuth 解密链（真密码学往返）─────────────────────────────────────────

func TestDecryptCallbackURealCrypto(t *testing.T) {
	sess, err := newOAuthSession()
	if err != nil {
		t.Fatal(err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := eph.ECDH(sess.priv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256(shared)
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	plain := []byte(`{"sk":"sk-real-123","uid":456,"url":"https://token-plan-cn.xiaomimimo.com/v1"}`)
	sealed := gcm.Seal(nil, nonce, plain, nil) // ct‖tag
	u := append(append(append([]byte{}, eph.PublicKey().Bytes()...), nonce...), sealed...)

	sk, uid, base, err := sess.DecryptCallbackU(base64.RawURLEncoding.EncodeToString(u))
	if err != nil {
		t.Fatal(err)
	}
	if sk != "sk-real-123" || uid != "456" || !strings.HasPrefix(base, "https://token-plan") {
		t.Errorf("解密结果不符: sk=%q uid=%q base=%q", sk, uid, base)
	}
}

// ── auth.json 三态导入归一 ──────────────────────────────────────────────

func TestClientAuthJSONThreeShapes(t *testing.T) {
	officialAPI := `{"providers":{"xiaomi":{"type":"api","key":"sk-a1","metadata":{"uid":"111","base_url":"https://api.xiaomimimo.com/v1"}}}}`
	officialOAuth := `{"providers":{"xiaomi":{"type":"oauth","access":"oa-a","refresh":"rt-a","expires":1893456000}}}`
	desktopOld := `{"providers":{"xiaomi":{"type":"oauth","access_token":"oa-b","refresh_token":"rt-b","expires_at":1893456000123}}}`
	cases := []struct {
		raw    string
		key    string
		access string
		exp    int64
	}{
		{officialAPI, "sk-a1", "", 0},
		{officialOAuth, "", "oa-a", 1893456000},
		{desktopOld, "", "oa-b", 1893456000}, // 毫秒归一为秒
	}
	for i, c := range cases {
		entries, err := ParseClientAuthJSON([]byte(c.raw))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if len(entries) != 1 {
			t.Fatalf("case %d: entries=%d", i, len(entries))
		}
		a := entries[0]
		if a.Key != c.key || a.AccessToken != c.access {
			t.Errorf("case %d: 归一错 %+v", i, a)
		}
		if c.exp != 0 && a.ExpiresAt != c.exp {
			t.Errorf("case %d: ExpiresAt=%d want %d", i, a.ExpiresAt, c.exp)
		}
	}
}

// ── 凭证落盘与重载闭环（文件名铁律）─────────────────────────────────────

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{Channel: ChannelPaid, Type: TypeAPI, Key: "sk-roundtrip0000001",
		UID: DeriveUID("sk-roundtrip0000001"), Nickname: "rt", Source: "import",
		LoggedInAt: time.Now().UTC().Format(time.RFC3339)}
	a.FilePath = dir + "/" + FileName(a)
	name := a.FilePath[strings.LastIndexByte(a.FilePath, '/')+1:]
	if !strings.HasPrefix(name, "mimo-") || !strings.HasSuffix(name, ".json") {
		t.Fatalf("文件名必须 mimo-<uid>.json: %s", name)
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	list, err := LoadDir(dir)
	if err != nil || len(list) != 1 {
		t.Fatalf("重载失败: %v %d", err, len(list))
	}
	if list[0].Key != a.Key {
		t.Error("回读 key 不一致")
	}
	// DeriveUID 稳定性：同一 key 永远同 uid（重载不产生重复账号的前提）。
	if DeriveUID("sk-roundtrip0000001") != DeriveUID("sk-roundtrip0000001") {
		t.Error("DeriveUID 不稳定")
	}
}

// ── NeverExpires 三态 ───────────────────────────────────────────────────

func TestNeverExpiresOnlyForPureSK(t *testing.T) {
	p := NewWithConfig(Config{})
	ok := p.NeverExpires(gateway.Credential{Secret: &Auth{Channel: ChannelPaid, Type: TypeAPI, Key: "sk-x"}})
	if !ok {
		t.Error("纯 sk 应报「长期有效」（UI 不显示 — 被读成功能缺失）")
	}
	if p.NeverExpires(gateway.Credential{Secret: &Auth{Channel: ChannelPaid, Type: TypeOAuth, RefreshToken: "rt"}}) {
		t.Error("oauth 有到期时刻，不该报永久")
	}
	if e, ok := p.TokenExpiry(gateway.Credential{Secret: &Auth{Channel: ChannelPaid, Type: TypeAPI, Key: "sk-x"}}); ok || e != 0 {
		t.Error("sk 的 TokenExpiry 必须是 (0,false) —— 造 0 会被渲染成 1970 年过期")
	}
}

// ── import handler（多行/注释/去重/auth_json）───────────────────────────

func TestImportHandler(t *testing.T) {
	dir := t.TempDir()
	p := NewWithConfig(Config{AuthDir: dir})
	reqBody := `{"keys_text":"tp-one1111111111111 主号\nsk-two2222222222222\n# 注释行\ntp-one1111111111111\n短行"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/mimo/import", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	p.handleImport(rec, req)
	var resp struct {
		OK       bool `json:"ok"`
		Imported int  `json:"imported"`
		Results  []struct {
			Index  int    `json:"index"`
			OK     bool   `json:"ok"`
			UID    string `json:"uid"`
			Error  string `json:"error"`
			KeyTyp string `json:"key_type"`
		} `json:"results"`
	}
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("回执解析失败: %v body=%s", err, rec.Body.String())
	}
	// tp / sk 两条成功，坏行一条失败，重复静默去重（不占条）。
	if len(resp.Results) != 3 {
		t.Fatalf("results=%d:\n%s", len(resp.Results), rec.Body.String())
	}
	if !resp.Results[0].OK || resp.Results[0].KeyTyp != "tokenplan" {
		t.Errorf("第一条应成功且判型 tokenplan: %+v", resp.Results[0])
	}
	if resp.Results[1].KeyTyp != "pay" {
		t.Errorf("第二条应判型 pay: %+v", resp.Results[1])
	}
	if resp.Results[2].OK {
		t.Error("短行应失败")
	}
	list, _ := LoadDir(dir)
	if len(list) != 2 {
		t.Errorf("落盘账号数 = %d, want 2", len(list))
	}
}

func boolp(b bool) *bool { return &b }

// ── 手动 code 粘贴全链（服务器部署逃生路径）─────────────────────────────

func TestManualLoginCompleteFullChain(t *testing.T) {
	// manual 模式不要求本机回调端口可达 —— 这是"网关在服务器上"的核心诉求。
	p := NewWithConfig(Config{OAuthRedirectMode: "manual"})
	if _, instr := p.ManualLogin(); instr == "" {
		t.Fatal("ManualLogin 必须带用户指引文案")
	}
	state, authURL, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authURL, "/authorize?") || !strings.Contains(authURL, "pk=") {
		t.Fatalf("authURL 形态不对: %s", authURL)
	}
	// v2 探测报告 §7.2 的官方参数集逐钉：kn 固定 mimocode、key_name 是生成名、app=MiMo。
	if !strings.Contains(authURL, "kn=mimocode") {
		t.Errorf("kn 必须是固定渠道名 mimocode: %s", authURL)
	}
	if !strings.Contains(authURL, "key_name=mimo-code-cli-key-") {
		t.Errorf("缺 key_name 或格式不对: %s", authURL)
	}
	if !strings.Contains(authURL, "app=MiMo") {
		t.Errorf("缺 app=MiMo（v2 报告实参）: %s", authURL)
	}
	if !strings.Contains(authURL, url.QueryEscape("code/callback")) &&
		!strings.Contains(authURL, "code%2Fcallback") {
		t.Errorf("manual 模式 redirect_uri 应指平台 code/callback: %s", authURL)
	}

	// pk 必须是 base64url(SPKI DER)（用户教程 §3.2 的 MCowBQYDK2VuAyEA… 前缀）——
	// 平台按 SPKI 解析，我们发 raw32 会让真链路解密必败（假上游闭环测不出来）。
	uu, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	pkB64 := uu.Query().Get("pk")
	if !strings.HasPrefix(pkB64, "MCowBQYDK2VuAyEA") {
		t.Fatalf("pk 不是 SPKI DER 编码（应含 X25519 SPKI 前缀 MCowBQYDK2VuAyEA）: %.20s…", pkB64)
	}
	pkRaw, err := base64.RawURLEncoding.DecodeString(pkB64)
	if err != nil {
		t.Fatalf("pk 不是 base64url: %v", err)
	}
	if len(pkRaw) != 12+32 {
		t.Fatalf("pk 应为 SPKI(12B 头)+raw32，共 44 字节，实为 %d", len(pkRaw))
	}
	srvPub, err := ecdh.X25519().NewPublicKey(pkRaw[12:])
	if err != nil {
		t.Fatalf("pk 去掉 SPKI 头后不是合法 X25519 raw32: %v", err)
	}
	// 扮演平台：临时密钥 ECDH → SHA256 → AES-256-GCM 封 {sk,uid}。
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := eph.ECDH(srvPub)
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256(shared)
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	sealed := gcm.Seal(nil, nonce, []byte(`{"sk":"sk-manual-1","uid":"777","url":"https://token-plan-cn.xiaomimimo.com/v1"}`), nil)
	u := append(append(append([]byte{}, eph.PublicKey().Bytes()...), nonce...), sealed...)
	// 用户粘贴的是**完整回跳 URL**（浏览器地址栏形态）—— extractUParam 要能剥。
	pasted := "http://127.0.0.1:18081/?u=" + url.QueryEscape(base64.RawURLEncoding.EncodeToString(u))

	body, _ := json.Marshal(map[string]string{"state": state, "code": pasted})
	req := httptest.NewRequest(http.MethodPost, "/admin/mimo/login/complete", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.handleCompleteLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete 回执 %d: %s", rec.Code, rec.Body.String())
	}

	// Poll 应拿到真凭证（含账号专属网关）。
	cred, err := p.Poll(state)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	af, ok := cred.Secret.(*authFile)
	if !ok || af.a == nil {
		t.Fatalf("Secret 类型: %T", cred.Secret)
	}
	if af.a.Key != "sk-manual-1" || af.a.UID != "777" ||
		!strings.HasPrefix(af.a.BaseURL, "https://token-plan") {
		t.Errorf("凭证内容不对: %+v", af.a)
	}
	name, raw, err := af.MarshalAuthFile()
	if err != nil || name != "mimo-777.json" {
		t.Fatalf("落盘形态: name=%q err=%v", name, err)
	}
	back, err := Parse(raw)
	if err != nil || back.BaseURL != af.a.BaseURL {
		t.Errorf("回读丢 baseUrl: %v %+v", err, back)
	}
}

func TestAutoModeKeepsLocalCallback(t *testing.T) {
	// 默认（auto）redirect_uri 必须仍是本机 127.0.0.1:port —— 同机部署的
	// 零粘贴体验不能因 manual 模式回归。
	p := NewWithConfig(Config{CallbackPort: "19313"})
	_, authURL, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authURL, url.QueryEscape("127.0.0.1:19313")) {
		t.Errorf("auto 模式 redirect_uri 应指本机回调: %s", authURL)
	}
}

// ── route 通道（桌面网关 Cookie 反代）───────────────────────────────────
//
// 假上游严格复刻 2026-09-24 探测报告实锤的三端点与 Cookie 语义。

func newFakeRouteUpstream(t *testing.T) (*httptest.Server, *fakeCalls) {
	t.Helper()
	c := &fakeCalls{}
	mux := http.NewServeMux()
	ok := func(r *http.Request) bool {
		ck := r.Header.Get("Cookie")
		return strings.Contains(ck, "serviceToken=svc-good") && strings.Contains(ck, "userId=3146522385") &&
			r.Header.Get("x-client-version") != "" && r.Header.Get("x-mimo-source") != ""
	}
	mux.HandleFunc("/api/route/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c.chat, 1)
		if !ok(r) {
			// 令牌失效的真实形态：302 跳 SSO（客户端必须停下折算 401）。
			w.Header().Set("Location", "https://account.xiaomi.com/pass/serviceLogin")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"思\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"成功\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/api/model/list", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c.models, 1)
		if !ok(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"data":[{"modelName":"mimo-v2.6-flash","modelType":"TEXT"},{"modelName":"mimo-route-only-model","modelType":"TEXT"}]}`)
	})
	mux.HandleFunc("/api/user/xiaomi/me", func(w http.ResponseWriter, r *http.Request) {
		if !ok(r) {
			w.Header().Set("Location", "https://account.xiaomi.com/pass/serviceLogin")
			w.WriteHeader(http.StatusFound)
			return
		}
		io.WriteString(w, `{"data":{"nickname":"妖精七七","region":"CN"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, c
}

func routeCred() gateway.Credential {
	return gateway.Credential{Provider: providerID, UID: "3146522385", Secret: &Auth{
		Channel: ChannelRoute, Type: TypeAPI, ServiceToken: "svc-good", UID: "3146522385",
		Slh: "s1", Ph: "p1",
	}}
}

func TestRouteChatCookiePassthrough(t *testing.T) {
	srv, c := newFakeRouteUpstream(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	body := []byte(`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"只回复两个字：成功"}],"stream":true}`)
	cs, err := p.Chat(context.Background(), routeCred(), body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != 200 {
		t.Fatalf("status=%d", cs.Status)
	}
	raw, _ := io.ReadAll(cs.Body)
	if !strings.Contains(string(raw), "成功") || !strings.Contains(string(raw), "思") {
		t.Errorf("直通内容缺失: %s", raw)
	}
	_ = c
}

// TestRouteStaleTokenFoldsTo401 302→SSO 必须折算成 401（不能跟到登录页 HTML
// 伪装 200 —— 那是"令牌死了却显示正常"的最阴险形态）。
func TestRouteStaleTokenFoldsTo401(t *testing.T) {
	srv, _ := newFakeRouteUpstream(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	bad := routeCred()
	bad.Secret.(*Auth).ServiceToken = "svc-expired"
	body := []byte(`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"x"}],"stream":true}`)
	cs, err := p.Chat(context.Background(), bad, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != 401 {
		t.Fatalf("302 应折算 401，实得 %d", cs.Status)
	}
	// route 不可刷 → 不会触发任何 refresh，直接上交分类。
	if got := Classify(cs.Status, string(readAllShort(cs.Body))); got != gateway.ErrKindSessionDead {
		t.Errorf("route 401 应判 SessionDead（令牌到期，重抓 Cookie），得 %v", got)
	}
}

func readAllShort(rc io.Reader) []byte {
	raw, _ := io.ReadAll(io.LimitReader(rc, 512))
	return raw
}

func TestRouteModelsCatalog(t *testing.T) {
	srv, _ := newFakeRouteUpstream(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	ms, err := p.Models(context.Background(), routeCred())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range ms {
		ids[m.ID] = true
	}
	// mimo-route-only-model 故意不在静态表里 —— 若 2xx 解析再次坏掉，
	// Models() 会静默回落静态表，这个 id 就消失，测试立刻红。
	if !ids["mimo-v2.6-flash"] || !ids["mimo-route-only-model"] {
		t.Errorf("route 目录（modelName 字段）解析不符: %v", ids)
	}
}

func TestRouteCookieImportFlow(t *testing.T) {
	srv, _ := newFakeRouteUpstream(t)
	dir := t.TempDir()
	p := NewWithConfig(Config{AuthDir: dir, Client: NewWithBase(srv.URL)})
	// 好 Cookie：验活过 + 真昵称 + 落盘 mimo-<uid>.json
	req := httptest.NewRequest(http.MethodPost, "/admin/mimo/import",
		strings.NewReader(`{"keys_text":"serviceToken=svc-good; userId=3146522385; mimopc_slh=s1; mimopc_ph=p1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.handleImport(rec, req)
	var resp struct {
		Results []struct {
			OK       bool   `json:"ok"`
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
			Channel  string `json:"channel"`
			Error    string `json:"error"`
		} `json:"results"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil || len(resp.Results) != 1 {
		t.Fatalf("回执解析: %s", rec.Body.String())
	}
	r0 := resp.Results[0]
	if !r0.OK || r0.UID != "3146522385" || r0.Channel != "route" || r0.Nickname != "妖精七七" {
		t.Fatalf("route 导入回执不符: %+v", r0)
	}
	list, _ := LoadDir(dir)
	if len(list) != 1 || list[0].Channel != ChannelRoute || list[0].Slh != "s1" {
		t.Fatalf("落盘回读不符: %+v", list)
	}
	if FileName(list[0]) != "mimo-3146522385.json" {
		t.Errorf("route 文件名铁律: %s", FileName(list[0]))
	}
}
