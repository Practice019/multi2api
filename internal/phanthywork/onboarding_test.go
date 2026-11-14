// onboarding_test.go 活动面四个新端点的判据。
//
// 每条判据都对着官方前端 bundle（`_up_\dist\assets\App-dSHMvpu3.js`）的 `Po` 定义写，
// 而不是对着本实现写 —— 这样"实现改错了"才会红。
package phanthywork

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recReq 记一次出站请求的关键面。
type recReq struct {
	Method   string
	Path     string
	RawQuery string
	Body     string
	Header   http.Header
}

// recSrv 记录所有请求并回一个固定 JSON。
type recSrv struct {
	mu       sync.Mutex
	reqs     []recReq
	respBody string
	status   int
}

func newRecSrv(t *testing.T) (*httptest.Server, *recSrv) {
	t.Helper()
	r := &recSrv{respBody: `{"ok":true}`, status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, recReq{
			Method:   req.Method,
			Path:     req.URL.Path,
			RawQuery: req.URL.RawQuery,
			Body:     string(raw),
			Header:   req.Header.Clone(),
		})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(r.status)
		_, _ = w.Write([]byte(r.respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, r
}

func (r *recSrv) snapshot() []recReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recReq, len(r.reqs))
	copy(out, r.reqs)
	return out
}

// newRecClient 造一个指向记录服务器的 Client + Auth（带真密钥，能算签名）。
func newRecClient(t *testing.T, srv *httptest.Server) (*Client, *Auth) {
	t.Helper()
	c := NewWithBase(srv.URL)
	c.UA = DefaultUA
	a := newTestAuth("onb-1", "refresh-onb-1")
	a.OAuth.AccessToken = "access-onb-1"
	return c, a
}

// assertV1Signed GET 也必须带 v1 签名（官方 No() 对所有方法都算 sign_desktop_activity）。
func assertV1Signed(t *testing.T, req recReq) {
	t.Helper()
	for _, h := range []string{"x-desktop-installation-id", "x-desktop-timestamp", "x-desktop-nonce", "x-desktop-signature"} {
		if strings.TrimSpace(req.Header.Get(h)) == "" {
			t.Errorf("%s %s 缺少 %s —— 官方对 GET 同样算 v1 签名", req.Method, req.Path, h)
		}
	}
	if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("Authorization = %q，want Bearer 前缀", got)
	}
}

// TestFetchRewardsIsSignedGetOnCleanPath 奖励端点：GET + 干净路径 + v1 签名。
func TestFetchRewardsIsSignedGetOnCleanPath(t *testing.T) {
	srv, rec := newRecSrv(t)
	c, a := newRecClient(t, srv)

	if _, err := c.FetchRewards(context.Background(), a); err != nil {
		t.Fatalf("FetchRewards: %v", err)
	}
	reqs := rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("请求数 = %d, want 1", len(reqs))
	}
	got := reqs[0]
	if got.Method != http.MethodGet {
		t.Errorf("Method = %s, want GET", got.Method)
	}
	if got.Path != pathRewards {
		t.Errorf("Path = %q, want %q", got.Path, pathRewards)
	}
	// ★ 官方 localPath 带 ?type=daily_login，upstreamPath 是干净的 —— 上游不许出现 query
	if got.RawQuery != "" {
		t.Errorf("上游带上了 query %q —— 官方 upstreamPath 是 /api/oauth/rewards（不带 query）", got.RawQuery)
	}
	assertV1Signed(t, got)
}

// TestFetchReferralRelationsIsSignedGet 邀请关系：GET + v1 签名。
func TestFetchReferralRelationsIsSignedGet(t *testing.T) {
	srv, rec := newRecSrv(t)
	c, a := newRecClient(t, srv)
	if _, err := c.FetchReferralRelations(context.Background(), a); err != nil {
		t.Fatalf("FetchReferralRelations: %v", err)
	}
	got := rec.snapshot()[0]
	if got.Method != http.MethodGet || got.Path != pathReferralsRelations {
		t.Errorf("got %s %s，want GET %s", got.Method, got.Path, pathReferralsRelations)
	}
	assertV1Signed(t, got)
}

// TestStartAndCompleteOnboardingTask 两个任务动作：路径 + 字面量 {} + 稳定幂等键。
func TestStartAndCompleteOnboardingTask(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action string
		keyFn  func(string, string) string
		call   func(*Client, context.Context, *Auth, string) error
	}{
		{
			name: "start", action: "start", keyFn: OnboardingStartKey,
			call: func(c *Client, ctx context.Context, a *Auth, id string) error {
				_, err := c.StartOnboardingTask(ctx, a, id)
				return err
			},
		},
		{
			name: "complete", action: "complete", keyFn: OnboardingCompleteKey,
			call: func(c *Client, ctx context.Context, a *Auth, id string) error {
				_, err := c.CompleteOnboardingAction(ctx, a, id)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := newRecSrv(t)
			c, a := newRecClient(t, srv)
			const taskID = "first_chat"
			if err := tc.call(c, context.Background(), a, taskID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got := rec.snapshot()[0]
			wantPath := pathOnboardingTasksPrefix + taskID + "/" + tc.action
			if got.Method != http.MethodPost || got.Path != wantPath {
				t.Errorf("got %s %s，want POST %s", got.Method, got.Path, wantPath)
			}
			// ★ 官方逐字 body:`{}` —— 不是空串、不是 null
			if got.Body != "{}" {
				t.Errorf("body = %q，want 字面量空对象 {}", got.Body)
			}
			instID, err := a.installationID()
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.keyFn(instID, taskID); got.Header.Get("idempotency-key") != want {
				t.Errorf("idempotency-key = %q，want %q", got.Header.Get("idempotency-key"), want)
			}
			assertV1Signed(t, got)
		})
	}
}

// TestOnboardingTaskIDValidatedLocally 非法任务 id 必须**零请求**。
func TestOnboardingTaskIDValidatedLocally(t *testing.T) {
	srv, rec := newRecSrv(t)
	c, a := newRecClient(t, srv)
	for _, bad := range []string{"", "  ", "Upper", "has-dash", "has space", "a/b", strings.Repeat("a", 81)} {
		if _, err := c.StartOnboardingTask(context.Background(), a, bad); err == nil {
			t.Errorf("StartOnboardingTask(%q) 应当报错（官方判据 ^[a-z0-9_]{1,80}$）", bad)
		}
		if _, err := c.CompleteOnboardingAction(context.Background(), a, bad); err == nil {
			t.Errorf("CompleteOnboardingAction(%q) 应当报错", bad)
		}
	}
	if n := len(rec.snapshot()); n != 0 {
		t.Errorf("非法 id 打了 %d 个请求，want 0（本地就该挡掉）", n)
	}
	// 反向：合法的下划线 id 必须通过
	if _, err := c.StartOnboardingTask(context.Background(), a, "finish_profile_1"); err != nil {
		t.Errorf("合法 id 被挡: %v", err)
	}
	if n := len(rec.snapshot()); n != 1 {
		t.Errorf("合法 id 请求数 = %d, want 1", n)
	}
}

// TestReconcileOnboardingBodyAndKey reconcile 的 body 两态 + 键含 latest/时间戳。
func TestReconcileOnboardingBodyAndKey(t *testing.T) {
	// ① 没有 sessionId ⇒ 字面量 {}
	srv, rec := newRecSrv(t)
	c, a := newRecClient(t, srv)
	if _, err := c.ReconcileOnboarding(context.Background(), a, ""); err != nil {
		t.Fatalf("ReconcileOnboarding(\"\"): %v", err)
	}
	got := rec.snapshot()[0]
	if got.Body != "{}" {
		t.Errorf("无 session 时 body = %q，want {}", got.Body)
	}
	if !strings.Contains(got.Header.Get("idempotency-key"), ":latest:") {
		t.Errorf("无 session 时幂等键应含 :latest:，实得 %q", got.Header.Get("idempotency-key"))
	}
	assertV1Signed(t, got)

	// ② 有 sessionId ⇒ {"session_id":"..."}
	srv2, rec2 := newRecSrv(t)
	c2, a2 := newRecClient(t, srv2)
	const sid = "sess-abc"
	if _, err := c2.ReconcileOnboarding(context.Background(), a2, sid); err != nil {
		t.Fatalf("ReconcileOnboarding(%q): %v", sid, err)
	}
	got2 := rec2.snapshot()[0]
	if got2.Body != `{"session_id":"`+sid+`"}` {
		t.Errorf("body = %q，want {\"session_id\":%q}", got2.Body, sid)
	}
	if !strings.Contains(got2.Header.Get("idempotency-key"), ":"+sid+":") {
		t.Errorf("幂等键应含 sessionId，实得 %q", got2.Header.Get("idempotency-key"))
	}
}

// TestOnboardingReconcileKeyIsUniquePerCall 官方把 Date.now() 拼进键 ⇒ 两次调用必须不同。
func TestOnboardingReconcileKeyIsUniquePerCall(t *testing.T) {
	k1 := OnboardingReconcileKey("di_x", "", 1000)
	k2 := OnboardingReconcileKey("di_x", "", 2000)
	if k1 == k2 {
		t.Errorf("两次 reconcile 的键相同 %q —— 官方键含 Date.now()，必须是新操作", k1)
	}
	if strings.Contains(k1, "::") {
		t.Errorf("键里出现空段 %q", k1)
	}
}

// TestSignedGetHashesEmptyBody GET 的 canonical 第 6 段是空串的哈希。
func TestSignedGetHashesEmptyBody(t *testing.T) {
	// sha256("") 的 base64url —— GET 的 body 是 undefined，官方哈希的就是它
	const want = "47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU"
	if got := sha256B64URL(nil); got != want {
		t.Errorf("sha256B64URL(nil) = %q，want %q", got, want)
	}
}

// TestBuildProofRejectsQueryInPath inference canonical 的 path 段不许带 query。
//
// 真源：官方 sidecar 的 proof 构造器 `_O8(q, K, _, z)` 第 3 段传的是 `K.pathname`（逐字），
// 且官方本地推理桥 `gd3` 的入口要求 query 为空（唯一例外 `/v1/messages?beta=true`）。
// 两条一起说明：这条路径上 query 既不进签名，也不该出现。
//
// 这是个"加了 query 就 403 request_not_authorized、而该错误码与时钟/UA/令牌过期完全同形"
// 的陷阱 —— 用断言把它钉在本地。
func TestBuildProofRejectsQueryInPath(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1")
	a := newTestAuth("proof-q-1", "refresh-proof-q-1")
	a.OAuth.AccessToken = "access-proof-q-1"

	// ① 带 query：必须当场报错
	_, err := c.buildProof(a, http.MethodPost, pathMessages+"?beta=true",
		[]byte(`{}`), "inference-token", nowMS())
	if err == nil {
		t.Fatal("path 带 query 时必须报错 —— 否则会签出一个服务端重算不出来的串")
	}
	if !strings.Contains(err.Error(), "query") {
		t.Errorf("错误信息应点明 query，实得: %v", err)
	}

	// ② 反向：干净的 path 不该命中这条护栏
	//（可能因别的理由失败 —— 这里只断言"不是 query 那条"）
	_, err2 := c.buildProof(a, http.MethodPost, pathChatCompletions,
		[]byte(`{}`), "inference-token", nowMS())
	if err2 != nil && strings.Contains(err2.Error(), "query") {
		t.Errorf("不带 query 的 path 不该命中这条护栏，实得: %v", err2)
	}
}