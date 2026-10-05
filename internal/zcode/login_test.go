// login_test.go 页内登录流程的守卫。
//
// # 这些断言守的是用户报的那个障
//
// 报障原文："zcode 该上游未声明页内登录流程" —— 界面上没有「＋ 添加账号」按钮。
// 那个按钮的判据是 `/admin/providers` 的 `login` 字段，而它只在上游实现了
// `gateway.LoginFlow`（且 Configured() 为真）时才有值。
//
// 所以最要紧的一条断言是：**LoginFlow 能被 ExtOf 认出来**。
// 它比"Start 返回了 URL"更基础 —— 认不出来就什么都免谈。
package zcode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fakeOAuthUpstream 假的上游 OAuth 端点。
//
// 它按官方契约回答（cli-oauth.ts），并记录收到的请求头 ——
// 这样"轮询有没有带 pollToken"能被断言，而不是靠读代码相信。
type fakeOAuthUpstream struct {
	srv   *httptest.Server
	calls struct {
		init int32
		poll int32
	}
	// pollStatuses 按顺序返回的轮询状态（用完后固定最后一项）。
	pollStatuses []string
	// lastInitAuth / lastPollAuth 记录 Authorization 头。
	lastInitAuth string
	lastPollAuth string
	// pollToken 上游"签发"的 poll_token（回显我们给的）。
	echoedPollToken string
	// flowID 上游签发的 flow_id。
	flowID string
}

func newFakeOAuth(t *testing.T, statuses ...string) *fakeOAuthUpstream {
	t.Helper()
	f := &fakeOAuthUpstream{flowID: "flow-abc-123", pollStatuses: statuses}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oauth/cli/init", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls.init, 1)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.lastInitAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var probe struct {
			Provider string `json:"provider"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Provider == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":100003,"msg":"provider required"}`))
			return
		}
		// 回显我们给的 poll_token（官方行为，实测确认）。
		tok := strings.TrimPrefix(f.lastInitAuth, "Bearer ")
		f.echoedPollToken = tok
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{
		  "flow_id":"` + f.flowID + `",
		  "poll_token":"` + tok + `",
		  "authorize_url":"https://chat.z.ai/api/oauth/authorize?client_id=client_TEST&redirect_uri=https%3A%2F%2Fzcode.z.ai%2Fapi%2Fv1%2Foauth%2Fcli%2Fcallback%2Fzai&state=st&response_type=code",
		  "expires_at":` + itoa(time.Now().Add(15*time.Minute).Unix()) + `,
		  "poll_interval_sec":1}}`))
	})
	mux.HandleFunc("/api/v1/oauth/cli/poll/", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&f.calls.poll, 1)
		f.lastPollAuth = r.Header.Get("Authorization")
		i := int(n) - 1
		if i >= len(f.pollStatuses) {
			i = len(f.pollStatuses) - 1
		}
		st := "pending"
		if len(f.pollStatuses) > 0 && i >= 0 {
			st = f.pollStatuses[i]
		}
		w.Header().Set("Content-Type", "application/json")
		switch st {
		case "ready":
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"ready",
			  "token":"` + fixtureJWT(time.Now().Add(2*time.Hour).Unix()) + `",
			  "user":{"user_id":"user-999","email":"a@b.c","name":"测试用户"},
			  "zai":{"access_token":"at-xyz","refresh_token":"rt-xyz"}}}`))
		case "failed":
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"failed"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"pending"}}`))
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// itoa 避免为测试引入 strconv（本文件只这一处用）。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// fixtureJWT 造一个带指定 exp 的假 JWT（只需 payload 段能被解出）。
func fixtureJWT(exp int64) string {
	payload := base64URLEncode([]byte(`{"exp":` + itoa(exp) + `}`))
	return "h." + payload + ".s"
}

func base64URLEncode(b []byte) string {
	const digits = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		out.WriteByte(digits[(v>>18)&0x3f])
		out.WriteByte(digits[(v>>12)&0x3f])
		if n > 1 {
			out.WriteByte(digits[(v>>6)&0x3f])
		}
		if n > 2 {
			out.WriteByte(digits[v&0x3f])
		}
	}
	return out.String()
}

// newLoginProvider 造一个指向假 OAuth 的 Provider。
func newLoginProvider(t *testing.T, f *fakeOAuthUpstream) *Provider {
	t.Helper()
	p := New(Config{AuthDir: t.TempDir(), HTTPClient: f.srv.Client(), OAuthBase: f.srv.URL + "/api/v1"})
	p.probeOrigin = false
	return p
}

// ---- 最要紧的一条 ----

// LoginFlow **必须能被 gateway.ExtOf 认出来**。
//
// # 这是用户报障的直接对因
//
// 认不出 → `/admin/providers` 的 login 是 null → 前端**不渲染**
// 「＋ 添加账号」按钮 → 用户界面上完全没有入口。
//
// 而"实现了 Start/Poll"与"类型断言能认出"是两件事：方法集差一点
// （比如 Configured 少写、签名不同）就认不出来，且**编译能过**。
func TestLoginFlowIsDiscoverable(t *testing.T) {
	f := newFakeOAuth(t, "pending")
	p := newLoginProvider(t, f)

	lf, ok := gateway.ExtOf[gateway.LoginFlow](p)
	if !ok {
		t.Fatal("LoginFlow 未被 ExtOf 认出 —— 前端不会渲染「＋ 添加账号」按钮" +
			"（这正是用户报的障：该上游未声明页内登录流程）")
	}
	if !lf.Configured() {
		t.Error("Configured() 应为 true —— 否则 admin 仍然不下发 login 字段")
	}
}

// Configured() 为 false 时不该被下发（假按钮的防线）。
//
// 这条守的是 gateway.LoginFlow 注释里那个判据：只看"实现了没有"
// 会渲染出点了报错的假按钮。
func TestConfiguredFalseMeansNoButton(t *testing.T) {
	// 用 nil Provider 模拟"没配置好"的情况。
	var p *Provider
	if p.Configured() {
		t.Error("nil Provider 的 Configured 应为 false")
	}
	// 正常构造的应为 true（CLI OAuth 不需要额外配置）。
	f := newFakeOAuth(t)
	p2 := newLoginProvider(t, f)
	if !p2.Configured() {
		t.Error("正常构造的 Provider 应 Configured=true（CLI OAuth 不需额外配置）")
	}
}

// ---- Start ----

// Start 必须返回 flow_id 与**指向官方授权页**的 URL。
func TestStartReturnsOfficialAuthorizeURL(t *testing.T) {
	f := newFakeOAuth(t, "pending")
	p := newLoginProvider(t, f)

	state, authURL, err := p.Start()
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if state == "" {
		t.Error("state 不能为空（Poll 按它查）")
	}
	if state != f.flowID {
		t.Errorf("state 应等于上游的 flow_id（%q），实际 %q", f.flowID, state)
	}
	if !strings.HasPrefix(authURL, "https://chat.z.ai/api/oauth/authorize") {
		t.Errorf("授权 URL 应指向官方 chat.z.ai，实际 %q", authURL)
	}
	// 必须带 client_id（否则授权页认不出我们）。
	if !strings.Contains(authURL, "client_id=client_") {
		t.Errorf("授权 URL 缺 client_id: %q", authURL)
	}
	// ⚠ 必须是 https —— 那个 URL 会交给用户打开，
	// `http:` 或 `javascript:` 的授权页是安全问题（官方 parseInitData 也校验这条）。
	if !strings.HasPrefix(authURL, "https://") {
		t.Errorf("授权 URL 必须是 https，实际 %q", authURL)
	}
}

// Start 必须**用我们自己的 pollToken** 调 init（不是上游回显的那个）。
//
// 判据：init 请求的 Authorization 非空且是 64 hex（32 字节）。
// 这个值是"取走凭证的凭证"，必须由我们生成（crypto/rand），
// 不能接受上游给的 —— 那等于把取号权交给对方指定。
func TestStartSendsOwnRandomPollToken(t *testing.T) {
	f := newFakeOAuth(t, "pending")
	p := newLoginProvider(t, f)

	if _, _, err := p.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	auth := f.lastInitAuth
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("init 应带 Bearer pollToken，实际 %q", auth)
	}
	tok := strings.TrimPrefix(auth, "Bearer ")
	if len(tok) != pollTokenBytes*2 {
		t.Errorf("pollToken 应是 %d 个 hex 字符（%d 字节），实际 %d 个: %q",
			pollTokenBytes*2, pollTokenBytes, len(tok), tok)
	}
	for _, r := range tok {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("pollToken 应是纯 hex，实际含 %q", string(r))
			break
		}
	}
	// 两次 Start 的 token 必须不同（每次登录一个）。
	if _, _, err := p.Start(); err != nil {
		t.Fatal(err)
	}
	if f.lastInitAuth == auth {
		t.Error("两次 Start 用了同一个 pollToken —— 应每次新生成")
	}
}

// init 被上游拒绝（业务码非零）要如实报错，不能当成拿到了 URL。
func TestStartReportsUpstreamBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// ⚠ HTTP 200 + 业务码非零 —— 本上游的典型失败形态。
		_, _ = w.Write([]byte(`{"code":100003,"msg":"provider not supported"}`))
	}))
	defer srv.Close()

	p := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), OAuthBase: srv.URL + "/api/v1"})
	p.probeOrigin = false

	_, _, err := p.Start()
	if err == nil {
		t.Fatal("业务码非零应报错（不能被 HTTP 200 骗过去）")
	}
	if !strings.Contains(err.Error(), "provider not supported") {
		t.Errorf("错误信息应带上游的说明，实际: %v", err)
	}
}

// init 给的 authorize_url 不是 https 必须拒绝。
func TestStartRejectsNonHTTPSAuthorizeURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f1",
		  "authorize_url":"javascript:alert(1)","expires_at":1,"poll_interval_sec":2}}`))
	}))
	defer srv.Close()

	p := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), OAuthBase: srv.URL + "/api/v1"})
	p.probeOrigin = false

	if _, _, err := p.Start(); err == nil {
		t.Error("非 https 的授权 URL 必须拒绝 —— 它会被交给用户打开")
	}
}

// ---- Poll ----

// Poll 在未授权时返回 ErrLoginPending（**不是** error）。
//
// 判据很重要：返回 error 会让前端把"等待中"渲染成"失败"，
// 用户看到红色报错就放弃授权了。
func TestPollReturnsPendingNotError(t *testing.T) {
	f := newFakeOAuth(t, "pending", "pending")
	p := newLoginProvider(t, f)

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Poll(state)
	if err != gateway.ErrLoginPending {
		t.Fatalf("未授权时应返回 ErrLoginPending，实际 %v", err)
	}
	// 再轮一次仍然 pending（会话没被消费）。
	if _, err := p.Poll(state); err != gateway.ErrLoginPending {
		t.Errorf("第二次轮询也应是 pending，实际 %v", err)
	}
	// 轮询必须带 pollToken（否则上游 401，而我们会误以为是"还没授权"）。
	if !strings.HasPrefix(f.lastPollAuth, "Bearer ") {
		t.Errorf("轮询必须带 Bearer pollToken，实际 %q", f.lastPollAuth)
	}
}

// Poll 在授权完成后返回**完整的凭证**。
func TestPollReturnsCredentialWhenReady(t *testing.T) {
	f := newFakeOAuth(t, "pending", "ready")
	p := newLoginProvider(t, f)

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Poll(state); err != gateway.ErrLoginPending {
		t.Fatalf("第一次应 pending，实际 %v", err)
	}

	cred, err := p.Poll(state)
	if err != nil {
		t.Fatalf("第二次应成功，实际 %v", err)
	}
	if cred.Provider != ProviderID {
		t.Errorf("Provider = %q，期望 %q", cred.Provider, ProviderID)
	}
	if cred.UID != "user-999" {
		t.Errorf("UID 应来自上游的 user.user_id，实际 %q", cred.UID)
	}
	if cred.Nickname == "" {
		t.Error("Nickname 不能为空（账号池显示它）")
	}

	// ⚠ Secret 必须是**能 MarshalAuthFile 的包装类型**，不是裸 *Auth。
	//
	// # 这条断言本身就是对一次真实事故的修补
	//
	// 我最初的测试断言的是 `Secret.(*Auth)` —— 而生产代码的落盘判据是
	// `Secret.(authFileWriter)`（见 admin.pollViaFlow）。两者**不一致**，
	// 于是测试全绿、登录却在 501 处卡死：
	//
	//	该上游的凭证结构尚未接入落盘
	//	（LoginFlow 的 Secret 需要实现 MarshalAuthFile）
	//
	// 用户看到的是"登录不了"，而上游那边其实已经授权成功。
	// 断言写错了判据 = 守了一个**生产中不存在**的契约。
	mw, ok := cred.Secret.(interface {
		MarshalAuthFile() (string, []byte, error)
	})
	if !ok {
		t.Fatalf("Secret 必须实现 MarshalAuthFile（落盘判据），实际 %T —— "+
			"否则核心会 501「该上游的凭证结构尚未接入落盘」", cred.Secret)
	}
	name, raw, err := mw.MarshalAuthFile()
	if err != nil {
		t.Fatalf("MarshalAuthFile 失败: %v", err)
	}
	if !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, "zcode-") {
		t.Errorf("文件名应形如 zcode-<uid>.json，实际 %q", name)
	}
	// 落盘内容必须能被**本包自己**读回来（导入/重载走的是同一条解析路径）。
	back, err := parseAuth(raw)
	if err != nil {
		t.Fatalf("落盘内容读不回来: %v\n%s", err, raw)
	}
	if back.JWT == "" || back.UID == "" {
		t.Errorf("回读的凭证缺字段: %+v", back)
	}
	if back.KindOf() != CredKindJWT {
		t.Errorf("回读的通道应是 jwt，实际 %q", back.KindOf())
	}

	// 取出内层 *Auth 做字段断言。
	a := back
	// 通道必须是 jwt —— JWT 通道走 Anthropic 协议 + 验证码，
	// 判错会让请求打到错误端点（而且大概率是 401 而非明确的参数错）。
	if a.KindOf() != CredKindJWT {
		t.Errorf("Kind 应是 %q，实际 %q", CredKindJWT, a.KindOf())
	}
	if a.JWT == "" {
		t.Error("JWT 不能为空")
	}
	if a.RefreshToken != "rt-xyz" {
		t.Errorf("RefreshToken 应存下来（上游给了就留着），实际 %q", a.RefreshToken)
	}
	// ⚠ 过期时刻必须从 JWT 里解出来。
	// 解不出来的后果是"过期判定永远为未知" → 账号不会被标需重登，
	// 表现为"用过一段时间后所有请求 401，而界面显示账号正常"。
	if a.ExpiresAt <= 0 {
		t.Error("ExpiresAt 必须从 JWT 的 exp 解出来（否则过期判定永远未知）")
	}
	if a.ExpiresAt <= time.Now().Unix() {
		t.Errorf("ExpiresAt 应在未来，实际 %d", a.ExpiresAt)
	}
	// 验证码区域要随账号存下来（参照实现的约定：不能写死）。
	if a.CaptchaRegion == "" {
		t.Error("CaptchaRegion 应随凭证存下来（参照实现：「the captcha region rides with the minted token, never hardcoded」）")
	}
}

// 授权完成后会话必须被**消费**（不能重复取号）。
func TestPollConsumesSessionAfterReady(t *testing.T) {
	f := newFakeOAuth(t, "ready")
	p := newLoginProvider(t, f)

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Poll(state); err != nil {
		t.Fatalf("首次应成功: %v", err)
	}
	// 再问同一个 state：应报"未知会话"（不是再给一份凭证）。
	if _, err := p.Poll(state); err == nil {
		t.Error("会话应已被消费 —— 重复取号会产出多份同 uid 的凭证")
	} else if err == gateway.ErrLoginPending {
		t.Error("消费后不该还是 pending")
	}
}

// 上游报 failed 是终态：会话消费掉，并给**明确**的错误。
func TestPollFailedIsTerminal(t *testing.T) {
	f := newFakeOAuth(t, "failed")
	p := newLoginProvider(t, f)

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Poll(state)
	if err == nil {
		t.Fatal("failed 应报错")
	}
	if err == gateway.ErrLoginPending {
		t.Fatal("failed 不该是 pending")
	}
	// 会话应被消费（重新申请才对）。
	if _, err := p.Poll(state); err == nil || err == gateway.ErrLoginPending {
		t.Errorf("failed 后会话应被消费，实际 %v", err)
	}
}

// 未知 state 要报错（不是 pending、也不是 panic）。
func TestPollUnknownStateFails(t *testing.T) {
	f := newFakeOAuth(t, "pending")
	p := newLoginProvider(t, f)

	_, err := p.Poll("never-existed")
	if err == nil {
		t.Fatal("未知 state 应报错")
	}
	if err == gateway.ErrLoginPending {
		t.Error("未知 state 不该是 pending —— 那会让前端一直等一个不存在的会话")
	}
}

// 网络错误**不消费**会话（用户可能只是抖了一下）。
func TestPollNetworkErrorKeepsSession(t *testing.T) {
	f := newFakeOAuth(t, "pending")
	p := newLoginProvider(t, f)

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	// 关掉假上游 → 轮询会网络失败。
	f.srv.Close()

	_, err = p.Poll(state)
	if err == nil || err == gateway.ErrLoginPending {
		t.Fatalf("网络失败应报 error，实际 %v", err)
	}
	// 会话还在（能再次轮询并拿到同样的错误，而不是"未知会话"）。
	if p.getLogin(state) == nil {
		t.Error("网络错误不该消费会话 —— 那会让一次抖动毁掉整次登录")
	}
}

// 上游报「非 pending 的业务错误」时**必须报错**，不能当成 pending。
//
// # 为什么这条要单独写（变异验证发现的测试缺口）
//
// 我把"非零业务码 → 报错"这条分支改写成"返回 pending"后，**测试仍然全绿** ——
// 因为之前的用例只覆盖了 pending / ready / failed 三种**正常**状态，
// 没覆盖"上游回业务错误码"。
//
// 那条误判的后果很具体：用户会一直等一个**永远不会完成**的会话，
// 直到 5 分钟超时。期间界面显示"正在等待授权"，而上游早就拒绝了。
func TestPollReportsBusinessCodeErrorNotPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/oauth/cli/init" {
			w.Header().Set("Content-Type", "application/json")
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f-biz",
			  "poll_token":"` + tok + `",
			  "authorize_url":"https://chat.z.ai/api/oauth/authorize?x=1",
			  "expires_at":9999999999,"poll_interval_sec":1}}`))
			return
		}
		// 轮询时回一个**非零业务码**（是信封的 code，不是 data.status）。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":5003,"msg":"session storage unavailable"}`))
	}))
	defer srv.Close()

	p := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), OAuthBase: srv.URL + "/api/v1"})
	p.probeOrigin = false

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Poll(state)
	if err == nil {
		t.Fatal("上游回业务错误码时应报错")
	}
	if err == gateway.ErrLoginPending {
		t.Fatal("业务错误**不能**被当成 pending —— " +
			"那会让用户等一个永远不会完成的会话，直到 5 分钟超时")
	}
	if !strings.Contains(err.Error(), "5003") && !strings.Contains(err.Error(), "session storage") {
		t.Errorf("错误信息应带上游的业务码或说明，实际: %v", err)
	}
	// 会话不该被消费（这是上游侧故障，用户重试可能就好了）。
	if p.getLogin(state) == nil {
		t.Error("上游业务错误不该消费会话")
	}
}

// `invalid_flow`（3004）是**终态**：要报错，不是 pending。
//
// 判错成 pending 的症状与上一条一样（永远等下去），
// 但原因不同：这次是**我们的**会话已失效（过期 / 服务重启）。
func TestPollInvalidFlowIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/oauth/cli/init" {
			w.Header().Set("Content-Type", "application/json")
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f-inv",
			  "poll_token":"` + tok + `",
			  "authorize_url":"https://chat.z.ai/api/oauth/authorize?x=1",
			  "expires_at":9999999999,"poll_interval_sec":1}}`))
			return
		}
		// 实测形态：HTTP 400 + {"code":3004,"msg":"invalid_flow"}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3004,"msg":"invalid_flow"}`))
	}))
	defer srv.Close()

	p := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), OAuthBase: srv.URL + "/api/v1"})
	p.probeOrigin = false

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Poll(state)
	if err == nil {
		t.Fatal("invalid_flow 应报错（终态）")
	}
	if err == gateway.ErrLoginPending {
		t.Fatal("invalid_flow 是终态，不能当 pending")
	}
}

// JWT 过期时刻的解析。
func TestJWTExpiryParsing(t *testing.T) {
	now := time.Now().Add(time.Hour).Unix()
	tok := fixtureJWT(now)
	if got := jwtExpiry(tok); got != now {
		t.Errorf("jwtExpiry = %d，期望 %d", got, now)
	}
	// 解不出时返回 0（语义是"不知道"，而不是"永不过期"）。
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.!!!.c"} {
		if got := jwtExpiry(bad); got != 0 {
			t.Errorf("jwtExpiry(%q) = %d，期望 0", bad, got)
		}
	}
}

// loginNickname 在三个字段都空时也要给出**能区分**的名字。
func TestLoginNicknameFallback(t *testing.T) {
	if got := loginNickname("张三", "a@b.c", "u1"); got != "张三" {
		t.Errorf("有 name 时应优先用它，实际 %q", got)
	}
	if got := loginNickname("", "a@b.c", "u1"); got != "a@b.c" {
		t.Errorf("没有 name 时应用 email，实际 %q", got)
	}
	// 都没有 → 用 uid 尾部。不能返回空串（账号池里显示空白没法区分）。
	got := loginNickname("", "", "1234567890")
	if got == "" {
		t.Error("三个字段都空时不能返回空串")
	}
	if !strings.Contains(got, "567890") {
		t.Errorf("应含 uid 尾部以便区分，实际 %q", got)
	}
}
