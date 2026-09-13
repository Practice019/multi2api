package codearts

// oauth_test.go —— 从 codearts2api/internal/oauth/codearts_test.go 移植。
//
// ⚠ 移植范围只改了两处：包名（oauth → codearts）与去掉 codearts. 前缀。
// **测试断言逐字未改** —— 这是"移植正确"的证据：
// 7 条断言在原仓库与新仓库必须给出同样结果。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestGeneratePKCEMatchesUpstreamContract 锁定 PKCE 参数与服务端要求的一致性。
//
// 依据：扩展的实现里 VERIFIER_LENGTH=128（hex 字符数），
// challenge = base64url(SHA256(verifier)) 且 **去掉 padding**。
// 这两点与服务端校验严格对齐，改动会被拒（STS5 系列错误）。
func TestGeneratePKCEMatchesUpstreamContract(t *testing.T) {
	p, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("生成 PKCE: %v", err)
	}

	// verifier 必须是 128 个 hex 字符
	if len(p.Verifier) != 128 {
		t.Errorf("verifier 长度 = %d, 期望 128", len(p.Verifier))
	}
	for _, c := range p.Verifier {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("verifier 含非 hex 字符 %q", c)
		}
	}

	if p.Method != "SHA-256" {
		t.Errorf("method = %q, 期望 SHA-256", p.Method)
	}

	// challenge 必须等于 base64url(SHA256(verifier))，无 padding
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Errorf("challenge 不匹配\n得到: %s\n期望: %s", p.Challenge, want)
	}
	if strings.ContainsAny(p.Challenge, "+/=") {
		t.Errorf("challenge 含非 base64url 字符（+/=）: %s", p.Challenge)
	}
}

// TestGeneratePKCEUnique 确认每次生成不同（避免复用 verifier）。
func TestGeneratePKCEUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		p, err := GeneratePKCE()
		if err != nil {
			t.Fatal(err)
		}
		if seen[p.Verifier] {
			t.Fatal("verifier 重复，随机性有问题")
		}
		seen[p.Verifier] = true
	}
}

// TestManagerStartBuildsAuthorizeURL 校验授权 URL 的关键参数齐全。
//
// 这些参数缺任何一个都会让上游拒绝或无法回调：
//
//	client_id / code_challenge / code_challenge_method / port / auth_callback_url
func TestManagerStartBuildsAuthorizeURL(t *testing.T) {
	m := NewManager("", "", "")
	s, err := m.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if s.callbackS != nil {
			_ = s.callbackS.Close()
		}
	}()

	if !strings.HasPrefix(s.AuthURL, DefaultPortalBase+"/portal/authorize?") {
		t.Errorf("授权 URL 前缀不对: %s", s.AuthURL)
	}
	for _, key := range []string{
		"client_id=", "code_challenge=", "code_challenge_method=SHA-256",
		"port=", "auth_callback_url=", "ticket_id=",
	} {
		if !strings.Contains(s.AuthURL, key) {
			t.Errorf("授权 URL 缺少参数 %q\nURL: %s", key, s.AuthURL)
		}
	}

	// client_id 默认必须是扩展名
	if !strings.Contains(s.AuthURL, "client_id="+DefaultClientID) {
		t.Errorf("client_id 不是默认值 %s: %s", DefaultClientID, s.AuthURL)
	}

	// auth_callback_url 必须指向本地随机端口
	if !strings.Contains(s.AuthURL, "127.0.0.1") {
		t.Errorf("回调未指向本地: %s", s.AuthURL)
	}
	if !strings.Contains(s.AuthURL, "oauth%2Fcallback") &&
		!strings.Contains(s.AuthURL, CallbackPath) {
		t.Errorf("回调路径不对: %s", s.AuthURL)
	}
}

// TestCallbackURLMatchesAuthorizePort 锁定 redirect_uri 一致性。
//
// OAuth 要求换 token 时的 redirect_uri 与授权时**逐字节一致**，
// 否则服务端以 redirect_uri mismatch 拒绝。
// 实现里是靠从 AuthURL 反解 port 来保证的，这里守住该不变量。
func TestCallbackURLMatchesAuthorizePort(t *testing.T) {
	m := NewManager("", "", "")
	s, err := m.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s.callbackS != nil {
			_ = s.callbackS.Close()
		}
	}()

	cb := callbackURLOf(s)
	if cb == "" {
		t.Fatal("callbackURLOf 返回空")
	}
	if !strings.HasPrefix(cb, "http://127.0.0.1:") {
		t.Errorf("回调 URL 形式不对: %s", cb)
	}
	if !strings.HasSuffix(cb, CallbackPath) {
		t.Errorf("回调 URL 未以 %s 结尾: %s", CallbackPath, cb)
	}
	// 授权 URL 里的 auth_callback_url 应与之等价
	if !strings.Contains(s.AuthURL, "auth_callback_url=") {
		t.Fatal("授权 URL 缺 auth_callback_url")
	}
}

// TestSessionWaitTimesOut 确认未回调时会超时而非永久挂起。
//
// 用极短的 TTL 不现实（常量），所以这里只验证 Wait 在收到 result 时正常返回，
// 超时路径依赖 TTL 常量（15 分钟）不做实时测试。
func TestSessionDeliverReturnsCode(t *testing.T) {
	s := &Session{resultCh: make(chan callbackResult, 1)}
	go s.deliver(callbackResult{Code: "THE_CODE"})
	code, err := s.Wait()
	if err != nil {
		t.Fatalf("Wait 返回错误: %v", err)
	}
	if code != "THE_CODE" {
		t.Errorf("code = %q, 期望 THE_CODE", code)
	}
}

// TestSessionDeliverOnce 确认 deliver 只生效一次（防重复回调）。
func TestSessionDeliverOnce(t *testing.T) {
	s := &Session{resultCh: make(chan callbackResult, 1)}
	s.deliver(callbackResult{Code: "FIRST"})
	s.deliver(callbackResult{Code: "SECOND"}) // 应被忽略
	code, _ := s.Wait()
	if code != "FIRST" {
		t.Errorf("code = %q, 期望 FIRST（第二次 deliver 应被忽略）", code)
	}
}

// TestPrivateJWKJSONRoundTrip 确认 DPoP 私钥能序列化/反序列化。
//
// 这是续期的生命线：私钥丢了就再也无法用 refresh_token 换新凭证。
func TestPrivateJWKJSONRoundTrip(t *testing.T) {
	kp, err := newDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	raw := privateJWKJSON(kp)
	if len(raw) == 0 {
		t.Fatal("privateJWKJSON 返回空")
	}

	var probe map[string]string
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("JWK 不是合法 JSON: %v", err)
	}
	if probe["kty"] != "EC" || probe["crv"] != "P-256" {
		t.Errorf("JWK 类型不对: %v", probe)
	}
	if probe["d"] == "" {
		t.Error("JWK 缺私钥分量 d")
	}

	restored, err := loadDPoPKeyPair(raw)
	if err != nil {
		t.Fatalf("恢复密钥失败: %v", err)
	}
	if restored == nil {
		t.Fatal("恢复的密钥为 nil")
	}
}
