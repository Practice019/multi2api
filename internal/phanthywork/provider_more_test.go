// provider_more_test.go 第二组行为断言：目录、隔离、凭证读写、登录入口。
//
// 与 provider_test.go 分开的理由：这个文件覆盖的是"周边契约"
// （目录合并、多账号隔离、两条 Loader 同源、登录 URL/兜底粘贴），
// 而 provider_test.go 覆盖"五步链路本身"。
package phanthywork

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestModelsMergesRemoteAndFallback 远端优先、兜底补齐、**永不为空**。
func TestModelsMergesRemoteAndFallback(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	ms, err := p.Models(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if fake.pathCount(pathModels) != 1 {
		t.Error("有凭证时应当拉远端目录")
	}
	var found bool
	for _, m := range ms {
		if m.ID == "glm-5.3-flash" {
			found = true
			if m.ContextWindow != 131072 {
				t.Errorf("远端下发的 context window 应生效，得到 %d", m.ContextWindow)
			}
		}
	}
	if !found {
		t.Error("远端有、兜底表也有的模型必须出现")
	}
	if len(ms) < len(fallbackCatalog) {
		t.Errorf("合并后的目录条数 %d 少于兜底表 %d", len(ms), len(fallbackCatalog))
	}
}

// TestModelsAnonymousDoesNotCallRemote 无凭证不打远端（必然 401，只产生噪音）。
func TestModelsAnonymousDoesNotCallRemote(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	ms, err := p.Models(context.Background(), gateway.Credential{Provider: providerID})
	if err != nil {
		t.Fatalf("无凭证时不该报错: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("无凭证时也要返回兜底目录")
	}
	if fake.pathCount(pathModels) != 0 {
		t.Error("无凭证时不该打远端目录（必然 401）")
	}
}

// TestMultiAccountIsolation 多账号隔离（报告 §7.1 第 5 条，用户点名要求）。
//
// ⚠ 本包把三把密钥 / installationId / 推理令牌 / 时钟偏移 / 并发闸
// 全部放在 *Auth 上，所以"隔离"是类型的后果 —— 但这句话必须有用例证明。
func TestMultiAccountIsolation(t *testing.T) {
	a := newTestAuth("u-a", "rt-a")
	b := newTestAuth("u-b", "rt-b")

	ida, _ := a.installationID()
	idb, _ := b.installationID()
	if ida == idb {
		t.Fatal("两个账号的 installationId 必须不同（否则服务端认为是同一台设备）")
	}
	if a.Device.IdentitySeed == b.Device.IdentitySeed {
		t.Error("identity 密钥必须按账号隔离")
	}
	if a.Infer.PublicKey == b.Infer.PublicKey || a.Infer.Seed == b.Infer.Seed {
		t.Error("推理密钥必须按账号隔离")
	}
	now := nowMS()
	a.storeInferenceToken("token-a", now+120_000, now)
	if b.cachedInferenceToken().Token != "" {
		t.Error("推理令牌缓存必须按账号隔离（b 读到了 a 的令牌）")
	}
	if a.cachedInferenceToken().Token != "token-a" {
		t.Error("a 自己的令牌应当可读")
	}
	a.applyServerTime(time.UnixMilli(now+5000).UTC().Format(time.RFC3339Nano), now)
	if b.skew() != 0 {
		t.Error("时钟偏移必须按账号隔离")
	}
	if a.skew() != -5000 {
		t.Errorf("a 的偏移应为 -5000，得到 %d", a.skew())
	}
	// 闸的键按账号隔离
	if a.refreshKey() == b.refreshKey() {
		t.Error("不同账号必须有不同的闸键（否则 A 的刷新会挡住 B）")
	}
}

// TestTokenExpiryIsUnixSeconds 对外一定给 Unix **秒**（本仓踩过秒/毫秒读错）。
func TestTokenExpiryIsUnixSeconds(t *testing.T) {
	a := newTestAuth("u-1", "rt")
	sec, ok := a.ExpiresAt()
	if !ok {
		t.Fatal("应当能给出到期时刻")
	}
	if sec < 1_700_000_000 || sec > 2_000_000_000 {
		t.Fatalf("到期时刻必须是 Unix 秒，得到 %d（按毫秒读会把 2027 年的号判成已过期）", sec)
	}
	p := newFakeProvider(t, httptest.NewServer(http.NotFoundHandler()))
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}
	got, ok := p.TokenExpiry(cred)
	if !ok || got != sec {
		t.Errorf("Provider.TokenExpiry = (%d,%v), want (%d,true)", got, ok, sec)
	}
	if !p.HasToken(cred) {
		t.Error("HasToken 应当为真（凭证里有 access_token）")
	}
	if p.HasToken(gateway.Credential{Provider: providerID, UID: "x"}) {
		t.Error("无 Secret 时 HasToken 必须为假（且不得 panic）")
	}
	if p.HasToken(gateway.Credential{Provider: providerID, UID: "y", Secret: (*Auth)(nil)}) {
		t.Error("nil *Auth 断言会成功 ⇒ 实现必须自己判 nil")
	}
}

// TestClassifyNeverPenalizesSigningProblems 签名/时钟类 403 不许罚号。
//
// ⚠ 守的是 gateway.ErrorClassifier 文件头记的两个危害：把"客户端拼装错"
// 判成 ErrKindSessionDead 会让一个健康账号被**永久禁用**。
func TestClassifyNeverPenalizesSigningProblems(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{403, `{"error":{"code":"request_not_authorized"}}`, gateway.ErrKindClient},
		{403, `{"error":{"code":"desktop_binding_invalid"}}`, gateway.ErrKindClient},
		{400, `{"error":{"code":"desktop_account_scope_invalid"}}`, gateway.ErrKindClient},
		{403, `{"error":{"code":"model_not_allowed"}}`, gateway.ErrKindClient},
		// ⚠ 2026-11-13 改正：这台设备**还没注册** ⇒ 属"初始化未完成"，不是凭据废了；
		//   原先判 auth ⇒ 核心"账号层失效·一次即摘" ⇒ 用户报障（oauth 健康的号被禁）。
		{401, `{"error":{"code":"desktop_installation_required"}}`, gateway.ErrKindClient},
		{401, `{"error":{"code":"invalid_grant"}}`, gateway.ErrKindAuth}, // 真·凭据废了仍算
		{429, `{"error":{"code":"rate_limit_exceeded"}}`, gateway.ErrKindSoftRate},
		{402, `{"error":{"code":"insufficient_credits"}}`, gateway.ErrKindHardCredit},
		{404, `{"error":{"code":"model_not_found"}}`, gateway.ErrKindNotFound},
		{500, `boom`, gateway.ErrKindServer},
		{200, `{"ok":true}`, gateway.ErrKindNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestParseCredentialRoundTrip 落盘 → 回读一致（含 JSON 标签）。
func TestParseCredentialRoundTrip(t *testing.T) {
	a := newTestAuth("u-1", "rt-1")
	a.Reg.RegisteredAtMS = 1_700_000_000_000
	a.Reg.ReasonCode = "first_device_owner"
	_, raw, err := a.MarshalAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("回读失败: %v；原文: %s", err, raw)
	}
	if back.UID() != "u-1" {
		t.Errorf("回读后 uid = %q, want u-1", back.UID())
	}
	if back.Device.IdentitySeed != a.Device.IdentitySeed {
		t.Error("identity 种子必须原样回读（变了就等于换设备）")
	}
	if back.Infer.Seed != a.Infer.Seed || back.Infer.PublicKey != a.Infer.PublicKey {
		t.Error("推理密钥必须原样回读")
	}
	if back.Reg.RegisteredAtMS != a.Reg.RegisteredAtMS || back.Reg.ReasonCode != a.Reg.ReasonCode {
		t.Error("注册状态必须原样回读（熔断靠它）")
	}
	if back.OAuth.RefreshToken != "rt-1" || back.OAuth.ExpiresAtMS != a.OAuth.ExpiresAtMS {
		t.Error("OAuth 令牌必须原样回读")
	}
	// 推理令牌缓存**不该**落盘（TTL 180 秒，写文件只会误导）
	if strings.Contains(string(raw), "inference_token") || strings.Contains(string(raw), "exp_at_ms") {
		t.Errorf("推理令牌缓存不该出现在凭证文件里: %s", raw)
	}
}

// TestParseCredentialAcceptsFlatForm 扁平形（手写凭证）也要认。
func TestParseCredentialAcceptsFlatForm(t *testing.T) {
	seed, _, _ := generateEd25519()
	inSeed, inPub, _ := generateEd25519()
	flat := `{"access_token":"` + fakeJWT("u-9", 1794446024) + `","account_scope":"u-9",` +
		`"device":{"identity_seed":"` + seed + `"},` +
		`"inference":{"seed":"` + inSeed + `","public_key":"` + inPub + `"}}`
	a, err := ParseCredential([]byte(flat))
	if err != nil {
		t.Fatalf("扁平形应被接受: %v", err)
	}
	if a.UID() != "u-9" {
		t.Errorf("uid = %q, want u-9", a.UID())
	}
	if _, err := ParseCredential([]byte(`{"account_scope":"u-9"}`)); err == nil {
		t.Error("缺 access_token 应报错（否则进池就是一个幽灵号）")
	}
}

// TestLoadDirScansOnlyOwnPrefix 只扫自己的前缀（别家的凭证不能被当自己的）。
func TestLoadDirScansOnlyOwnPrefix(t *testing.T) {
	dir := t.TempDir()
	a := newTestAuth("u-1", "rt")
	_, raw, _ := a.MarshalAuthFile()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("phanthywork-u-1.json")
	write("cline-xyz.json")      // 别家
	write("phanthy-legacy.json") // 自己的历史前缀，但当前前缀是 phanthywork
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("只应读到 1 份自己的凭证，实际 %d 份", len(list))
	}
	if list[0].UID() != "u-1" {
		t.Errorf("uid = %q, want u-1", list[0].UID())
	}
	// 空目录 / 不存在的目录 / 空 dir 参数都不是错误
	empty := t.TempDir()
	if l2, err := LoadDir(empty); err != nil || len(l2) != 0 {
		t.Errorf("空目录应返回 (空, nil)，得到 (%d, %v)", len(l2), err)
	}
	if l3, err := LoadDir(filepath.Join(empty, "nope")); err != nil || len(l3) != 0 {
		t.Errorf("不存在的目录应返回 (空, nil)，得到 (%d, %v)", len(l3), err)
	}
	if l4, err := LoadDir(""); err != nil || len(l4) != 0 {
		t.Errorf("空 dir 应返回 (空, nil)，得到 (%d, %v)", len(l4), err)
	}
}

// TestCredentialLoadersAreSameSource 两个 Loader 必须同源（读同一份对象）。
//
// ⚠ 不同源的后果是"轮换式 refreshToken 被消费两次"（codearts 那个 503 的根因）。
func TestCredentialLoadersAreSameSource(t *testing.T) {
	dir := t.TempDir()
	a := newTestAuth("u-1", "rt")
	_, raw, _ := a.MarshalAuthFile()
	if err := os.WriteFile(filepath.Join(dir, "phanthywork-u-1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewProvider(Config{AuthDir: dir})
	plain, err := p.LoadCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	withSecret, err := p.LoadCredentialsWithSecrets("")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 1 || len(withSecret) != 1 {
		t.Fatalf("两条 loader 都应读到 1 份，得到 %d / %d", len(plain), len(withSecret))
	}
	if plain[0].UID != withSecret[0].UID || plain[0].FilePath != withSecret[0].FilePath {
		t.Error("两条 loader 的投影必须一致（同源）")
	}
	if plain[0].Provider != providerID {
		t.Errorf("投影的 Provider = %q, want %q", plain[0].Provider, providerID)
	}
	if !plain[0].ExpiresAt.IsZero() {
		t.Error("投影不该带 ExpiresAt（启动快照会与活 secret 分叉）")
	}
	if _, ok := withSecret[0].Secret.(*Auth); !ok {
		t.Errorf("Secret 必须是 *Auth，得到 %T", withSecret[0].Secret)
	}
	if p.AuthDir() != dir {
		t.Errorf("AuthDir = %q, want %q（读与写必须问同一个目录）", p.AuthDir(), dir)
	}
}

// TestManualLoginIsDiscoverableAndUsable 手动兜底可被核心发现、且端点真的能用。
//
// ⚠ 少了 AdminRoutes 那一条的后果是完全静默的：manifest 里 login.manual 有值、
// 前端渲染出输入框，用户粘完点提交得到 404。
func TestManualLoginIsDiscoverableAndUsable(t *testing.T) {
	p := NewProvider(Config{})
	path, instr := p.ManualLogin()
	if path != ManualLoginPath || instr == "" {
		t.Fatalf("ManualLogin = (%q,%q)", path, instr)
	}
	found := false
	for _, r := range p.AdminRoutes() {
		if r.Path == ManualLoginPath && r.Method == http.MethodPost && r.Handler != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("ManualLoginExt 报的路径必须在 AdminRoutes 上挂着（否则用户粘完点提交得到 404）")
	}
	// 端点能不 panic 地被调用（核心的契约检查会真调一次）。
	//
	// ⚠ 只调**这一条**路由的 handler。原实现是把 `AdminRoutes()` 里每一条都调一遍、
	//   然后看最后一次的状态码 —— 签到那两条路由（P1-a 修好契约工厂之后这里就有了）
	//   会写自己的 400，把 recorder 覆盖掉，于是这条断言测的变成「最后跑的那条路由」。
	//   按 path+method 选中目标路由，断言才真的落在它身上。
	var manual http.HandlerFunc
	for _, r := range p.AdminRoutes() {
		if r.Path == ManualLoginPath && r.Method == http.MethodPost {
			manual = r.Handler
		}
	}
	if manual == nil {
		t.Fatal("找不到手动兜底那条路由的 handler")
	}
	rec := httptest.NewRecorder()
	manual(rec, httptest.NewRequest(http.MethodGet, ManualLoginPath, nil)) // http.HandlerFunc：直接调用
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 打到只接受 POST 的端点应回 405，得到 %d", rec.Code)
	}
}

// TestStartReturnsAuthorizeURLWithPKCE 授权 URL 的 8 个 query 齐备。
func TestStartReturnsAuthorizeURLWithPKCE(t *testing.T) {
	p := NewProvider(Config{})
	state, authURL, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	if state == "" || authURL == "" {
		t.Fatal("Start 必须返回 state 与授权 URL")
	}
	for _, want := range []string{
		"client_id=" + OAuthClientID,
		"response_type=code",
		"code_challenge_method=S256",
		"state=" + url.QueryEscape(state),
	} {
		if !strings.Contains(authURL, want) {
			t.Errorf("授权 URL 缺 %q：%s", want, authURL)
		}
	}
	if !strings.Contains(authURL, url.QueryEscape("org:create_api_key user:profile user:inference")) {
		t.Errorf("授权 URL 的 scope 不是逐字那 6 个（空格连接）：%s", authURL)
	}
	if _, err := p.Poll(state); err != gateway.ErrLoginPending {
		t.Errorf("未完成时 Poll 应回 ErrLoginPending，得到 %v", err)
	}
	if _, err := p.Poll("nope"); err == nil {
		t.Error("未知 state 应返回可读错误（不是 panic）")
	}
	if !p.Configured() {
		t.Error("本包的登录流程是编译期常量，Configured 应当为真")
	}
}

// TestExtractPastedForms 手动兜底的四种粘贴形态。
func TestExtractPastedForms(t *testing.T) {
	cases := []struct {
		in        string
		hint      string
		wantState string
		wantCode  string
		wantErr   bool
	}{
		{"http://127.0.0.1:8080/oauth/callback?code=abc&state=xyz", "", "xyz", "abc", false},
		{"code=abc&state=xyz", "", "xyz", "abc", false},
		{"http://x/cb#code=abc&state=xyz", "", "xyz", "abc", false},
		{"abc", "hint-state", "hint-state", "abc", false},
		{"http://127.0.0.1:8080/oauth/callback?state=xyz", "", "", "", true},
		{"", "", "", "", true},
	}
	for _, c := range cases {
		s, code, err := extractPasted(c.in, c.hint)
		if c.wantErr {
			if err == nil {
				t.Errorf("extractPasted(%q) 应当报错", c.in)
			}
			continue
		}
		if err != nil || s != c.wantState || code != c.wantCode {
			t.Errorf("extractPasted(%q) = (%q,%q,%v), want (%q,%q,nil)", c.in, s, code, err, c.wantState, c.wantCode)
		}
	}
}

// TestCommonHeadersOnEveryRequest 每个上游请求都要带 UA 与 client-source（坑②）。
func TestCommonHeadersOnEveryRequest(t *testing.T) {
	srv, _ := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	req, err := http.NewRequest(http.MethodGet, srv.URL+pathProfile, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.client.applyCommonHeaders(req)
	if got := req.Header.Get("User-Agent"); got != DefaultUA {
		t.Errorf("UA = %q, want %q", got, DefaultUA)
	}
	if got := req.Header.Get("x-phanthy-client-source"); got != clientSource {
		t.Errorf("x-phanthy-client-source = %q, want %q", got, clientSource)
	}
}

// TestBuildingDoesNotSendRequests 构造 Provider 不发任何请求（契约要求）。
func TestBuildingDoesNotSendRequests(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	_ = NewWithClient(NewWithBase(srv.URL), t.TempDir(), 0)
	_ = NewProvider(Config{})
	if n := len(fake.pathsSnapshot()); n != 0 {
		t.Errorf("构造 Provider 不该发请求，实际发了 %d 次: %v", n, fake.pathsSnapshot())
	}
}

// TestFrozenContract 冻结接口（用户明确指定、不得改名）自检。
//
// 这条把"接口名字"变成可执行断言：改名是编译期就该炸的事，
// 但契约里的常量值（ProviderID / DefaultAPIBase / DefaultUA / TTL）
// 只能靠断言。
func TestFrozenContract(t *testing.T) {
	if ProviderID != "phanthywork" || providerID != ProviderID {
		t.Errorf("ProviderID = %q（必须逐字 phanthywork）", ProviderID)
	}
	if DefaultAPIBase != "https://code.phanthy.com" {
		t.Errorf("DefaultAPIBase = %q", DefaultAPIBase)
	}
	if DefaultUA != "Bun/1.3.14" {
		t.Errorf("DefaultUA = %q", DefaultUA)
	}
	if InferenceTokenTTL != 180*time.Second {
		t.Errorf("InferenceTokenTTL = %v", InferenceTokenTTL)
	}
	var p gateway.Provider = NewProvider(Config{})
	if p.ID() != "phanthywork" {
		t.Errorf("ID() = %q", p.ID())
	}
	if got := p.Caps(); !got.Has(gateway.CapChat) || !got.Has(gateway.CapModels) ||
		!got.Has(gateway.CapQuotaProbe) || !got.Has(gateway.CapCheckin) || !got.Has(gateway.CapImport) ||
		!got.Has(gateway.CapInvite) {
		t.Errorf("Caps() = %v（必须含 chat|models|quota-probe|checkin|import|invite）", got.Names())
	}
	// **已实现并验证**的四个（checkin / quota-probe / import / invite）不在下面这个清单里；
	// 下面这些是仍然**没有实现**的能力位 —— 声明它们就是给前端画一个点了必然失败的按钮。
	//
	// ⚠ 2026-11-12 变更：`CapInvite` **从这里移出**（用户要求加邀请码面板，已实现 + 有断言）。
	//   移出而不是注释掉：这条断言的价值就是"清单必须跟着实现走"。
	for _, c := range []gateway.Capability{gateway.CapTasks, gateway.CapWelfare, gateway.CapGrowth, gateway.CapTravel} {
		if p.Caps().Has(c) {
			t.Errorf("本轮不该声明 %v（声明即承诺；它是 follow-up）", gateway.String(c))
		}
	}
}

// TestDisplayNameIsChinese 界面名（gateway.DisplayNameExt）。
func TestDisplayNameIsChinese(t *testing.T) {
	p := NewProvider(Config{})
	if got := p.DisplayName(); got != "PhanthyWork" {
		t.Errorf("DisplayName = %q, want PhanthyWork", got)
	}
}

// TestRefreshSkewPercentOfLifetime access token 的提前续期窗口按寿命比例。
//
// 寿命 1800 秒 ⇒ 窗口 = 一半 = 900 秒（本仓统一的 gateway 策略）。
func TestRefreshSkewPercentOfLifetime(t *testing.T) {
	p := newFakeProvider(t, httptest.NewServer(http.NotFoundHandler()))
	a := newTestAuth("u-1", "rt")
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}
	skew, ok := p.RefreshSkew(cred)
	if !ok {
		t.Fatal("有 iat/exp 与 refreshToken 时必须给出窗口")
	}
	if skew != 900*time.Second {
		t.Errorf("窗口 = %v, want 900s（寿命 1800s 的一半）", skew)
	}
	// 没有 refreshToken ⇒ 不可续期 ⇒ ok=false（而不是"明确不需要提前刷"）
	b := newTestAuth("u-2", "")
	if _, ok := p.RefreshSkew(gateway.Credential{Provider: providerID, UID: b.UID(), Secret: b}); ok {
		t.Error("没有 refreshToken 时应回 ok=false（不可续期）")
	}
}
