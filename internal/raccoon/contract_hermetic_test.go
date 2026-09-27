// contract_hermetic_test.go 契约测试与假上游 —— **判据 2 的 enforcement**。
//
// 全程 hermetic：扫码 code 由客户端本地生成、服务端接受任意自造 code，
// 所以**不需要真实账号**就能把整条链路跑通。
package raccoon

import (
	"context"
	"encoding/base64"
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

// fixtureJWT 造一个带指定 exp 的假 JWT（只需 base64url 的 payload 段）。
func fixtureJWT(expUnix int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expUnix)))
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

// fakeUpstream 假 Raccoon 上游。
//
// qrStatuses 按顺序返回扫码轮询的状态（用完后固定 success）。
func fakeUpstream(t *testing.T, qrStatuses []string) (*httptest.Server, *fakeCalls) {
	t.Helper()
	calls := &fakeCalls{}
	exp := time.Now().Add(3 * time.Hour).Unix()
	token := fixtureJWT(exp)

	mux := http.NewServeMux()
	idx := int32(0)

	// ── 扫码轮询 ───────────────────────────────────────────────────────
	mux.HandleFunc(authPrefix+"/login_with_qrcode_code", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls.qrPoll, 1)
		_ = n
		// ⚠ 本端点**不带 Authorization**
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"qr login must not carry Authorization"}`))
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		code := body["qrcode_code"]
		if len(code) != 32 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"qrcode_code must be 32 hex"}`))
			return
		}
		i := atomic.AddInt32(&idx, 1) - 1
		st := "success"
		if int(i) < len(qrStatuses) {
			st = qrStatuses[i]
		}
		switch st {
		case "pending":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"status":"pending"}}`))
		case "logging":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"status":"logging","expired_at":"2030-01-01T00:00:00Z"}}`))
		case "canceled":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"status":"canceled"}}`))
		case "weird":
			// 未知状态 → 调用方必须降级为 pending
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"status":"totally_unknown"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"status":"success","access_token":"` + token + `","refresh_token":"rt-1"}}`))
		}
	})

	// ── 用户信息 ───────────────────────────────────────────────────────
	mux.HandleFunc(authPrefix+"/user_info", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// X-Org-Code 必须总是发（个人账号为空串也照发）
		if _, ok := r.Header["X-Org-Code"]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"X-Org-Code must always be sent"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"id":"7445120","name":"RaccoonAva","office_identity":"personal","phone":"13800001100"}}`))
	})

	// ── 续期 ───────────────────────────────────────────────────────────
	mux.HandleFunc(authPrefix+"/refresh", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.refresh, 1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"refresh_token required"}`))
			return
		}
		if body["refresh_token"] == "rt-expired" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":200003,"message":"登录态已过期"}`))
			return
		}
		// ⚠ 刻意**只返回新 access_token**（实测形态）：
		// 不保留旧 refresh_token 会让续期一次就把账号变成不可续期。
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"access_token":"` + fixtureJWT(time.Now().Add(6*time.Hour).Unix()) + `"}}`))
	})

	// ── 模型目录 ───────────────────────────────────────────────────────
	mux.HandleFunc(llmPrefix+"/model_catalog", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.catalog, 1)
		// ⚠ 本端点不发 Content-Type、不发 X-Client-Platform
		if r.Header.Get("Content-Type") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"catalog must not send Content-Type"}`))
			return
		}
		if r.Header.Get("X-Client-Platform") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"catalog must not send X-Client-Platform"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"categories":[
			{"type":"image","models":[]},
			{"type":"chat","models":[
				{"name":"sn-sensenova-6-8-flash","description":"SenseNova-6.8-Flash","visible":true,
				 "params":{"context_window":256000,"max_tokens":63999},
				 "billing_effective_multiplier":0,"billing_multiplier":0.5,"tags":["vision"]},
				{"name":"sn-kimi-k3","description":"Kimi-K3","visible":true,
				 "params":{"context_window":1000000,"max_tokens":100000},
				 "billing_effective_multiplier":1,"billing_multiplier":1,"tags":["vision"]},
				{"name":"raccoon-internal","description":"internal","visible":false,
				 "params":{"context_window":1000,"max_tokens":100}},
				{"name":"sn-new-model","description":"New Model","visible":true,
				 "params":{"context_window":500000,"max_tokens":50000}}
			]}
		]}}`))
	})

	// ── 聊天（标准 SSE）────────────────────────────────────────────────
	mux.HandleFunc(llmPrefix+"/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.chat, 1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// 聊天必须带 X-Client-Platform，但**不带** X-Client-Version
		if r.Header.Get("X-Client-Platform") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"chat requires X-Client-Platform"}`))
			return
		}
		if r.Header.Get("X-Client-Version") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"chat must not send X-Client-Version"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		if obj["stream"] != true {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"stream must be forced"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"成功\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	// ── 余额 ───────────────────────────────────────────────────────────
	mux.HandleFunc(pointsPrefix+"/balance", func(w http.ResponseWriter, r *http.Request) {
		// 完整业务头（含 platform/version）
		if r.Header.Get("X-Client-Platform") == "" || r.Header.Get("X-Client-Version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"balance requires platform+version"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"available_points":12345,"reward_points":3000,"daily_points":300,"monthly_points":0,"topup_points":0}}`))
	})

	// ── 登录奖励 ───────────────────────────────────────────────────────
	mux.HandleFunc(desktopPrefix+"/login/points/grant", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.grant, 1)
		// ⚠ X-Client-Platform 是**准入条件**
		if r.Header.Get("X-Client-Platform") != desktopPlatformExpected {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"X-Client-Platform required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"granted":true,"popup":{"source":"desktop_login","points":3000}}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, calls
}

// desktopPlatformExpected 假上游期望的 platform 取值。
const desktopPlatformExpected = "desktop-windows"

type fakeCalls struct {
	qrPoll  int32
	refresh int32
	catalog int32
	chat    int32
	grant   int32
}

func newContractProvider(base string) gateway.Provider {
	return NewWithConfig(Config{Client: NewWithBase(base)})
}

func newContractCredential(uid string) gateway.Credential {
	a := &Auth{
		AccessToken:    fixtureJWT(time.Now().Add(3 * time.Hour).Unix()),
		RefreshToken:   "rt-1",
		OfficeIdentity: "personal",
		UserID:         uid,
		Nickname:       "RaccoonAva",
		Phone:          "13800001100",
	}
	return gateway.Credential{Provider: providerID, UID: uid, Nickname: a.DisplayUID(), Secret: a}
}

// TestContract 契约测试 —— **判据 2 的正式入口**。
func TestContract(t *testing.T) {
	srv, _ := fakeUpstream(t, nil)
	gateway.RunProviderContract(t, func() gateway.Provider {
		return newContractProvider(srv.URL)
	}, gateway.WithCredential(newContractCredential("7445120")))
}

// TestContractIsNotConditionallySkipped 契约必须无凭证也跑。
func TestContractIsNotConditionallySkipped(t *testing.T) {
	t.Setenv("RACCOON_ACCESS_TOKEN", "")
	t.Setenv("RACCOON_TEST_CRED", "")

	srv, _ := fakeUpstream(t, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gateway.RunProviderContract(t, func() gateway.Provider {
			return newContractProvider(srv.URL)
		}, gateway.WithCredential(newContractCredential("7445120")))
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("契约在无凭证环境下未完成 —— CI 里会变成永远挂住")
	}
}

// ── 逐条钉住本 provider 特有的硬判据 ────────────────────────────────────

// TestQRCodeIsLocallyGeneratedAndAccepted 扫码 code 本地生成、服务端接受。
//
// 这是本 provider 能纯程序化登录的**前提**：不需要官方回调链路。
func TestQRCodeIsLocallyGeneratedAndAccepted(t *testing.T) {
	code, err := GenerateQRCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 32 {
		t.Fatalf("code 应为 32 位 hex，实际 %q（%d 字符）", code, len(code))
	}
	for _, r := range code {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("code 必须是小写 hex，出现 %q", r)
		}
	}

	// 承载 URL 形态
	u := QRLoginURL(code)
	if !strings.HasPrefix(u, DefaultAPIBase+"/login/mp?") {
		t.Errorf("承载 URL 前缀不对: %s", u)
	}
	if !strings.Contains(u, "code="+code) {
		t.Errorf("承载 URL 必须带 code: %s", u)
	}
	// ⚠ appname 必须 URL-encode（中文）
	if !strings.Contains(u, "appname=") || strings.Contains(u, "appname=商汤") {
		t.Errorf("appname 必须 URL-encode: %s", u)
	}

	// 假上游接受自造 code
	srv, _ := fakeUpstream(t, []string{"pending"})
	c := NewWithBase(srv.URL)
	res, err := c.PollQRCode(context.Background(), code)
	if err != nil {
		t.Fatalf("服务端应接受自造 code: %v", err)
	}
	if res.Status != QRStatusPending {
		t.Errorf("status = %v, want pending", res.Status)
	}
}

// TestUnknownQRStatusDegradesToPending 未知状态必须降级为 pending。
//
// ⚠ 降级方向的理由：误判成 success → 拿到空 token 卡死；
// 误判成 canceled → 用户正在扫的码被无故刷新。
func TestUnknownQRStatusDegradesToPending(t *testing.T) {
	cases := []struct {
		in   string
		want QRStatus
	}{
		{"pending", QRStatusPending},
		{"logging", QRStatusLogging},
		{"canceled", QRStatusCanceled},
		{"success", QRStatusSuccess},
		{"totally_unknown", QRStatusPending}, // 未知 → pending
		{"", QRStatusPending},
		{"PENDING", QRStatusPending}, // 大小写不敏感
	}
	for _, c := range cases {
		if got := NormalizeQRStatus(c.in); got != c.want {
			t.Errorf("NormalizeQRStatus(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// 端到端：假上游回未知状态，客户端必须当 pending（不是 success/canceled）
	srv, _ := fakeUpstream(t, []string{"weird"})
	cl := NewWithBase(srv.URL)
	res, err := cl.PollQRCode(context.Background(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("未知状态不该报错: %v", err)
	}
	if res.Status != QRStatusPending {
		t.Errorf("未知状态应降级为 pending，实际 %v", res.Status)
	}
}

// TestSuccessWithEmptyTokenIsPending success 但 token 为空 → pending。
func TestSuccessWithEmptyTokenIsPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"status":"success"}}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	res, err := c.PollQRCode(context.Background(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != QRStatusPending {
		t.Errorf("success 但无 access_token 必须视为 pending（不产出半截凭据），实际 %v", res.Status)
	}
}

// TestQRLoginCarriesNoAuthorization 扫码端点不带 Authorization。
func TestQRLoginCarriesNoAuthorization(t *testing.T) {
	srv, _ := fakeUpstream(t, []string{"pending"})
	c := NewWithBase(srv.URL)
	// 即使传入凭证，也不该带 Authorization（客户端实现里根本没传）
	if _, err := c.PollQRCode(context.Background(), "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("扫码轮询不该失败（假上游会拒绝带 Authorization 的请求）: %v", err)
	}
}

// TestLoginEndToEnd 扫码登录端到端（含 canceled 换码）。
func TestLoginEndToEnd(t *testing.T) {
	// 先 canceled（触发换码），再 success
	srv, _ := fakeUpstream(t, []string{"canceled", "logging"})
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	lf, ok := p.LoginFlow()
	if !ok {
		t.Fatal("Provider 必须实现 LoginFlow")
	}
	state, url, err := lf.Start()
	if err != nil {
		t.Fatalf("Start 报错: %v", err)
	}
	if !strings.Contains(url, "code="+state) {
		t.Errorf("返回的 URL 应含 state（qrcode_code）: %s", url)
	}

	var cred gateway.Credential
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		c, perr := lf.Poll(state)
		if perr == nil {
			cred = c
			break
		}
		if perr != gateway.ErrLoginPending {
			t.Fatalf("轮询报错: %v", perr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if cred.UID == "" {
		t.Fatal("25 秒内未拿到凭证")
	}
	if cred.UID != "7445120" {
		t.Errorf("uid = %q, want 7445120", cred.UID)
	}
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		t.Fatalf("Secret 类型: %T", cred.Secret)
	}
	if a.RefreshToken != "rt-1" {
		t.Errorf("refresh_token = %q, want rt-1", a.RefreshToken)
	}
	// 展示名应含手机号后 4 位（服务端昵称不可区分多账号）
	if !strings.Contains(cred.Nickname, "1100") {
		t.Errorf("展示名应含手机号后 4 位，实际 %q", cred.Nickname)
	}
}

// TestRefreshKeepsOldRefreshToken 续期保留旧 refresh_token。
//
// ⚠ 实测服务端**可能只返回新 access_token**。不保留旧值会让续期一次
// 就把账号变成不可续期 —— 这是本 provider 最容易犯的错。
func TestRefreshKeepsOldRefreshToken(t *testing.T) {
	srv, calls := fakeUpstream(t, nil)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	cred := newContractCredential("7445120")
	a := cred.Secret.(*Auth)
	oldRT := a.RefreshToken
	oldExp := a.ExpiresAt

	if err := p.RefreshCredential(cred); err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if atomic.LoadInt32(&calls.refresh) != 1 {
		t.Fatal("续期请求应发 1 次")
	}
	if a.RefreshToken != oldRT {
		t.Errorf("服务端没返回新 refresh_token 时必须保留旧值 %q，实际 %q", oldRT, a.RefreshToken)
	}
	// expires_at 应由新 JWT 重算（假上游给的是 +6h，比原来的 +3h 晚）
	if a.ExpiresAtMS() <= 0 || a.ExpiresAt == oldExp {
		t.Errorf("expires_at 应由新 token 的 JWT exp 重算，old=%q new=%q", oldExp, a.ExpiresAt)
	}
	// 展示字段必须保留
	if a.OfficeIdentity != "personal" || a.Phone != "13800001100" {
		t.Errorf("服务端不返回的字段必须保留: %+v", a)
	}
}

// TestRefreshExpiredIsSentinel 续期终态必须是哨兵错误。
func TestRefreshExpiredIsSentinel(t *testing.T) {
	srv, _ := fakeUpstream(t, nil)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	// HTTP 401 → 终态
	cred := gateway.Credential{Provider: providerID, UID: "u",
		Secret: &Auth{AccessToken: "a", RefreshToken: "rt-expired"}}
	err := p.RefreshCredential(cred)
	if err == nil || !isExpiredErr(err) {
		t.Errorf("HTTP 401 应返回终态错误，实际 %v", err)
	}

	// 无 refresh_token → 终态
	cred2 := gateway.Credential{Provider: providerID, UID: "u", Secret: &Auth{AccessToken: "a"}}
	if err := p.RefreshCredential(cred2); err == nil || !isExpiredErr(err) {
		t.Errorf("无 refresh_token 应返回终态错误，实际 %v", err)
	}
}

func isExpiredErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrRefreshExpired.Error())
}

// TestExpiryFallsBackToJWT 过期判定必须回退到 JWT exp。
//
// ⚠ 只读 expires_at 会让判定**恒为 false**，于是 refreshAll 永远跳过这些账号 ——
// 表现为"凭据悄悄过期、续期从不触发"，**静默失效、无任何报错**。
func TestExpiryFallsBackToJWT(t *testing.T) {
	now := time.Now()

	// 有 expires_at：用它
	a1 := &Auth{AccessToken: fixtureJWT(now.Add(10*time.Hour).Unix()),
		ExpiresAt: fmt.Sprintf("%d", now.Add(-time.Hour).UnixMilli())}
	if !a1.IsExpired(now) {
		t.Error("expires_at 已过期时应判过期（即便 JWT 还没到期）")
	}

	// **无 expires_at：必须回退到 JWT**
	a2 := &Auth{AccessToken: fixtureJWT(now.Add(-time.Hour).Unix())}
	if !a2.IsExpired(now) {
		t.Error("无 expires_at 时必须回退到 JWT exp 判过期 —— 否则续期会静默失效")
	}

	// 无任何过期信息 → 保守视为不过期
	a3 := &Auth{AccessToken: "not-a-jwt"}
	if a3.IsExpired(now) {
		t.Error("无过期信息时应保守视为不过期（交给服务端 401 判定）")
	}

	// JWT 未过期
	a4 := &Auth{AccessToken: fixtureJWT(now.Add(time.Hour).Unix())}
	if a4.IsExpired(now) {
		t.Error("JWT 未过期时不该判过期")
	}
}

// TestPhoneEncryption AES-128-CFB 加密手机号。
func TestPhoneEncryption(t *testing.T) {
	ct, err := EncryptPhone("13800001100")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatalf("输出不是 Base64: %v", err)
	}
	// 16 字节 iv + 11 字节手机号（CFB 是流密码，密文长度 = 明文长度）
	if len(raw) != 16+11 {
		t.Errorf("密文长度 = %d, want 27（16 iv + 11 明文）", len(raw))
	}
	// 两次加密的 iv 不同 → 密文不同
	ct2, _ := EncryptPhone("13800001100")
	if ct == ct2 {
		t.Error("两次加密应产生不同 iv（从而不同密文）")
	}
}

// TestDisplayNameRules 展示名规则（含「1 倍也要显示」）。
func TestDisplayNameRules(t *testing.T) {
	cases := []struct {
		desc      string
		base, eff float64
		want      string
	}{
		{"Kimi-K3", 1, 1, "Kimi-K3 · x1"},                 // ⚠ 1 倍也要显示
		{"GLM-5-3", 0.75, 0.75, "GLM-5-3 · x0.75"},
		{"GLM-5-3-Flash", 0.2, 0.1, "GLM-5-3-Flash · x0.2→x0.1"}, // 折扣
		{"Free-1", 0.5, 0, "Free-1 · 免费"},                // 0 = 免费
		{"No-Info", 0, -1, "No-Info"},                     // 负数 = 取不到 → 不加后缀
		{"No-Info2", 0, 0.0, "No-Info2 · 免费"},            // 0 是"免费"，不是"取不到"
	}
	for _, c := range cases {
		if got := DisplayName("id", c.desc, c.base, c.eff); got != c.want {
			t.Errorf("DisplayName(%q, base=%v, eff=%v) = %q, want %q",
				c.desc, c.base, c.eff, got, c.want)
		}
	}
	// NaN 不加后缀
	if got := DisplayName("id", "X", 0, nan()); got != "X" {
		t.Errorf("NaN 倍率不该加后缀（那才是取不到），实际 %q", got)
	}
	// description 为空时用 id
	if got := DisplayName("the-id", "", 0, 0.5); got != "the-id · x0.5" {
		t.Errorf("description 为空应回退 id，实际 %q", got)
	}
}

func nan() float64 {
	var z float64
	return z / z
}

// TestModelCatalogMerge 目录合并（远端优先 + 兜底补齐 + 过滤 invisible）。
func TestModelCatalogMerge(t *testing.T) {
	srv, calls := fakeUpstream(t, nil)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	models, err := p.Models(context.Background(), newContractCredential("7445120"))
	if err != nil {
		t.Fatalf("Models 报错: %v", err)
	}
	if atomic.LoadInt32(&calls.catalog) == 0 {
		t.Fatal("必须调用 model_catalog")
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	// 远端 visible:true 的必须在
	for _, want := range []string{"sn-sensenova-6-8-flash", "sn-kimi-k3", "sn-new-model"} {
		if !ids[want] {
			t.Errorf("远端模型 %s 缺位", want)
		}
	}
	// ⚠ visible:false 必须被过滤
	if ids["raccoon-internal"] {
		t.Error("visible=false 的模型必须被过滤")
	}
	// 兜底表里远端没下发的也要保留（上游临时少下发不该让模型消失）
	for _, want := range []string{"sn-glm-5-3", "sn-deepseek-v4-1-flash"} {
		if !ids[want] {
			t.Errorf("兜底表模型 %s 应保留", want)
		}
	}
	// Raccoon-Auto 绝不该出现（发了会 404）
	if ids["Raccoon-Auto"] {
		t.Error("Raccoon-Auto 不在远端目录里，绝不能暴露（发了会 404）")
	}
}

// TestModelCatalogFailureFallsBack 目录失败时回退兜底表。
func TestModelCatalogFailureFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	models, err := p.Models(context.Background(), newContractCredential("7445120"))
	if err != nil {
		t.Fatalf("目录失败不该让 Models 报错: %v", err)
	}
	if len(models) != len(fallbackModels) {
		t.Errorf("应回退兜底表（%d 条），实际 %d 条", len(fallbackModels), len(models))
	}
}

// TestFallbackTableNamesMatchDisplayName 兜底表 name 必须与 DisplayName 形态一致。
//
// ⚠ 不一致会让远端/回退两条路径显示不同形态。
func TestFallbackTableNamesMatchDisplayName(t *testing.T) {
	// 表里的 name 应含倍率后缀（· 免费 / · x…）
	for _, m := range fallbackModels {
		if !strings.Contains(m.Name, " · ") {
			t.Errorf("兜底表 %s 的 name=%q 应含倍率后缀", m.ID, m.Name)
		}
	}
	// 逐条与 DisplayName 的输出对齐（用表里的倍率反推）
	expect := map[string]struct{ base, eff float64 }{
		"sn-sensenova-6-8-flash":      {0.5, 0},
		"sn-sensenova-6-8-flash-lite": {0.5, 0},
		"sn-glm-5-3":                  {0.75, 0.75},
		"sn-kimi-k3":                  {1, 1},
		"sn-glm-5-3-flash":            {0.2, 0.1},
		"sn-deepseek-v4-1-flash":      {0.25, 0.25},
	}
	for _, m := range fallbackModels {
		e, ok := expect[m.ID]
		if !ok {
			t.Errorf("兜底表出现未登记的模型 %s", m.ID)
			continue
		}
		// 去掉 name 前缀（描述部分）只比对后缀形态
		got := DisplayName(m.ID, strings.Split(m.Name, " · ")[0], e.base, e.eff)
		if got != m.Name {
			t.Errorf("%s:\n  兜底表 name = %q\n  DisplayName = %q\n  两者必须一致",
				m.ID, m.Name, got)
		}
	}
}

// TestBalanceRequiresAvailablePoints 余额缺 available_points 即判失败。
func TestBalanceRequiresAvailablePoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"reward_points":3000}}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	if _, err := c.FetchBalance(context.Background(), &Auth{AccessToken: "a"}); err == nil {
		t.Error("缺 available_points 必须报错（0 是「已用光」的语义，不能编造）")
	}
}

// TestLoginGrantNeedsPlatform X-Client-Platform 是领取奖励的准入条件。
func TestLoginGrantNeedsPlatform(t *testing.T) {
	srv, calls := fakeUpstream(t, nil)
	c := NewWithBase(srv.URL)
	res, err := c.ClaimLoginGrant(context.Background(), &Auth{AccessToken: "a", OfficeIdentity: "personal"})
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if !res.Claimed || res.Points != 3000 {
		t.Errorf("应领到 3000 积分，实际 %+v", res)
	}
	if atomic.LoadInt32(&calls.grant) != 1 {
		t.Error("应发 1 次 grant 请求")
	}
}

// TestErrorClassification 错误分类（含 200 + 业务失败）。
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{401, ``, gateway.ErrKindAuth},
		{429, ``, gateway.ErrKindSoftRate},
		{400, ``, gateway.ErrKindClient},
		{500, ``, gateway.ErrKindServer},
		{402, ``, gateway.ErrKindHardCredit},
		// ⚠ 业务失败可能是 HTTP 200 + 非 0 code
		{200, `{"code":402030,"message":"insufficient points"}`, gateway.ErrKindHardCredit},
		{200, `{"code":0,"message":"success"}`, gateway.ErrKindNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestCredentialRoundTrip 凭证落盘与回读一致。
func TestCredentialRoundTrip(t *testing.T) {
	a := &Auth{
		AccessToken: fixtureJWT(1790412721), RefreshToken: "rt",
		ExpiresAt: "1790412721000", OfficeIdentity: "personal",
		UserID: "7445120", Nickname: "RaccoonAva", Phone: "13800001100",
		DeviceID: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
	}
	raw, err := MarshalAuthFile(a)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if back.AccessToken != a.AccessToken || back.RefreshToken != a.RefreshToken ||
		back.UserID != a.UserID || back.Phone != a.Phone ||
		back.OfficeIdentity != a.OfficeIdentity || back.DeviceID != a.DeviceID {
		t.Errorf("回读不一致:\n  want %+v\n  got  %+v", a, back)
	}
	if back.UID() != "7445120" {
		t.Errorf("uid = %q, want 7445120", back.UID())
	}
}

// TestParseCredentialAcceptsFlatForm 扁平形也认。
func TestParseCredentialAcceptsFlatForm(t *testing.T) {
	a, err := ParseCredential([]byte(`{"access_token":"tok","refresh_token":"rt","user_id":"u9"}`))
	if err != nil {
		t.Fatalf("扁平形应被接受: %v", err)
	}
	if a.UID() != "u9" {
		t.Errorf("uid = %q, want u9", a.UID())
	}
	if _, err := ParseCredential([]byte(`{"refresh_token":"rt"}`)); err == nil {
		t.Error("缺 access_token 应报错")
	}
}

// TestPrepareBodyForcesStream 请求体强制 stream + 裁剪越界 max_tokens。
func TestPrepareBodyForcesStream(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"x","stream":false,"max_tokens":99999999}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream 必须被强制为 true")
	}
	if got := obj["max_tokens"].(float64); got != maxOutputTokensUpperBound {
		t.Errorf("max_tokens 应收敛到 %d，实际 %v", maxOutputTokensUpperBound, got)
	}
	// 非法值删掉
	out2 := PrepareBody([]byte(`{"model":"x","max_tokens":-5}`))
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	if _, still := obj2["max_tokens"]; still {
		t.Error("负数 max_tokens 应被删掉")
	}
	// 非法 JSON 原样返回
	raw := []byte(`not json`)
	if string(PrepareBody(raw)) != string(raw) {
		t.Error("非法 JSON 必须原样返回")
	}
}

// TestProviderDoesNotPanicOnBadCredential 错型凭证返回错误而非 panic。
func TestProviderDoesNotPanicOnBadCredential(t *testing.T) {
	p := NewWithConfig(Config{})
	bad := gateway.Credential{Provider: providerID, UID: "u", Secret: "not-an-auth"}
	if _, err := p.Chat(context.Background(), bad, []byte(`{}`)); err == nil {
		t.Error("错型 Secret 应返回错误")
	}
	if _, err := p.Models(context.Background(), bad); err != nil {
		t.Errorf("Models 不该因 Secret 错型而失败（无凭证也要给兜底目录）: %v", err)
	}
}
