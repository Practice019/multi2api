// renew_test.go MonkeyCode「密码续期」（引擎侧）的契约测试 —— 全程 hermetic（httptest 假上游，不碰网络）。
//
// # 这是什么（2026-11-01 实测跑通的链路）
//
// session 是 30 天硬过期的，**没有** refresh 接口。但上游有一条「自救」路径：
//
//	① POST /api/v1/public/captcha/challenge  → {challenge:{c,s,d}, token}   公开端点，无需登录态
//	   解 c 个子挑战（PoW cap.js）→
//	② POST /api/v1/public/captcha/redeem     → {success, token}             captcha_token
//	③ POST /api/v1/users/password-login {email, password, captcha_token}
//	   → 200 {"code":0,…} + Set-Cookie: monkeycode_ai_session=<36 位 uuid>; …; Max-Age=2592000
//
// 三条实测事实（本文件的断言由它们推出）：
//
//	· Max-Age=2592000 = 30 天（上游亲口给的真值，比 expiry.go 的「锚点+30 天」推断值可信）；
//	· **新旧 session 并存**（旧的不会被吊销）⇒ 续期安全，不是「换一个可能失败的新号」；
//	· challenge 与 password-login 都**不要求登录态** ⇒ session 死透也能救回来。
//
// 字段名必须**蛇形** `captcha_token`：写成 camelCase 会被上游 403，且报错不指出字段名。
//
// # 契约面（引擎 renew.go 尚未发布；下面哪些「有据」、哪些是假定）
//
// 有据（来自同仓并行任务的 `adminrenew.go` seam 示例，不是我猜的）：
//
//	入口形状   _, res, err := p.RenewSession(ctx, uid)      // 三个返回值，第二个带两个事实
//	结果字段   res.ExpiresInSec / res.TTLSource             // 取值 "max-age" / "fallback"
//	凭证字段   Auth.Email / Auth.Password（string）、Auth.SessionRenewedAt（**time.Time**）
//	取凭证     p.cred(uid)（同包测试可直接写 p.creds[uid]）
//
// 注入点（有据）：`renewHostOverride` —— renew.go 自己留的测试钩子（生产恒为空），
// 它的注释里就点名了本文件。刻意不去动 Auth.Host()：那条路径被 egress 自检 /
// 模型目录共用，改它风险更大。
//
// ⚠ 本文件**禁止 `t.Parallel()`**：`renewHostOverride` 是**包级变量**，并行会串台
// （多个测试互相改 host → 随机红、且红的位置与被测代码无关，极难查）。
// 若将来确需并行，必须先把注入点从包级变量改成**按测试实例传递**（例如挂到 Client 上）。
//
// ⚠ 诚实记录：本文件第一版假定过「`Client.Host` 字段」（那时 renew.go 还没落地），
// 落地后按真实实现改成上面这个钩子（`go vet` 当时只报了这一处未定义，其余假定全都命中）。
//
// 本文件与 `adminrenew_test.go`（批量端点那侧）互不重叠：那边测「跳不跳/remaining/并发」，
// 这边测「PoW 真解出、字段名对、TTL 来源、重试边界、写回保全、锚点优先级」。
package monkeycode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 假上游 ───────────────────────────────────────────────────────────────────

const (
	renewFakeSession = "00000000-0000-4000-8000-000000000000" // 36 位 uuid 形态
	renewFakeOldSess = "00000000-0000-4000-8000-000000000000"
	renewFakeCapTok  = "cap-token-0001"     // challenge 下发的 token（PoW 的种）
	renewFakeCapOK   = "cap-token-redeemed" // redeem 换回的 captcha_token
	renewFakeMaxAge  = 2592000              // 30 天，实测真值
)

// renewFakeOpts 假上游的可调行为。
type renewFakeOpts struct {
	// cookieMode password-login 成功时怎么下发 cookie：
	//	"max-age"        标准形态（Max-Age=2592000）
	//	"no-max-age"     只有 Path=/（⇒ 回退 SessionLifetime）
	//	"zero-max-age"   Max-Age=0（边界：按回退处理，否则刚续期的号会被判"立刻过期"）
	//	"second-cookie"  先下另一条 cookie，再下 monkeycode_ai_session（多条里要挑对）
	cookieMode string
	// loginStatus password-login 的 HTTP 状态（0 = 200）。
	loginStatus int
	// loginBody 非 2xx 时的响应体。
	loginBody string
	// failFirstLogin 让**第一次** password-login 在传输层断开（模拟代理抖动）。
	failFirstLogin bool
	// powD 目标长度（hex 字符数）。4 ⇒ 16 位目标 ⇒ 期望 6.5 万次哈希，毫秒级。
	powD int
}

type renewFake struct {
	srv  *httptest.Server
	opts renewFakeOpts

	challengeCalls int32
	redeemCalls    int32
	loginCalls     int32
	statusCalls    int32

	mu sync.Mutex
	// loginBodies 每次 password-login 的请求体（解成 map ⇒ 断言字段名）。
	loginBodies []map[string]any
	// 凭据泄漏检查：两步都不该带 cookie / Authorization。
	challengeCookie, challengeAuth bool
	loginCookie, loginAuth         bool
	redeemCookie                   bool
	// badSolutions redeem 收到的 PoW 解没通过独立复核（说明求解与目标派生口径不一致）。
	badSolutions bool
}

func newRenewFake(t *testing.T, opts renewFakeOpts) *renewFake {
	t.Helper()
	if opts.cookieMode == "" {
		opts.cookieMode = "max-age"
	}
	if opts.powD == 0 {
		opts.powD = 4
	}
	f := &renewFake{opts: opts}
	mux := http.NewServeMux()

	// ① challenge：公开端点。
	mux.HandleFunc(pathCaptchaChallenge, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.challengeCalls, 1)
		f.mu.Lock()
		f.challengeCookie = r.Header.Get("Cookie") != ""
		f.challengeAuth = r.Header.Get("Authorization") != ""
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"challenge": map[string]int{"c": 1, "s": 8, "d": f.opts.powD},
			"token":     renewFakeCapTok,
		})
	})

	// ② redeem：**独立复核** PoW 解是否真满足派生目标；不满足就回 success:false（与上游一致）。
	mux.HandleFunc(pathCaptchaRedeem, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.redeemCalls, 1)
		f.mu.Lock()
		f.redeemCookie = r.Header.Get("Cookie") != ""
		f.mu.Unlock()

		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Token     string  `json:"token"`
			Solutions []int64 `json:"solutions"`
		}
		_ = json.Unmarshal(raw, &req)

		ok := req.Token == renewFakeCapTok && len(req.Solutions) == 1
		if ok {
			for b := 1; b <= len(req.Solutions); b++ {
				idx := itoa(b)
				// 目标派生口径照上游：salt=capI(token+b, s)、tgt=capI(token+b+"d", d)。
				salt := capI(req.Token+idx, 8)
				tgt := capI(req.Token+idx+"d", f.opts.powD)
				if !powSatisfies(salt, tgt, req.Solutions[b-1]) {
					ok = false
					break
				}
			}
		}
		if !ok {
			f.mu.Lock()
			f.badSolutions = true
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			_, _ = io.WriteString(w, `{"success":false,"message":"invalid solutions"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "token": renewFakeCapOK})
	})

	// ③ password-login：公开端点（不要求登录态）。
	mux.HandleFunc("/api/v1/users/password-login", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&f.loginCalls, 1)
		if f.opts.failFirstLogin && n == 1 {
			// 传输层断开（与 netretry_test.go 的 flakyServer 同款手法）：
			// 客户端看到 EOF/连接重置 ⇒ 属"可重试的瞬时错误"。
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			return
		}

		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.loginBodies = append(f.loginBodies, body)
		f.loginCookie = r.Header.Get("Cookie") != ""
		f.loginAuth = r.Header.Get("Authorization") != ""
		f.mu.Unlock()

		if f.opts.loginStatus != 0 && f.opts.loginStatus != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.opts.loginStatus)
			out := f.opts.loginBody
			if out == "" {
				out = `{"code":403}`
			}
			_, _ = io.WriteString(w, out)
			return
		}

		switch f.opts.cookieMode {
		case "no-max-age":
			w.Header().Add("Set-Cookie", "monkeycode_ai_session="+renewFakeSession+"; Path=/")
		case "zero-max-age":
			w.Header().Add("Set-Cookie", "monkeycode_ai_session="+renewFakeSession+"; Path=/; Max-Age=0; HttpOnly")
		case "second-cookie":
			w.Header().Add("Set-Cookie", "other_cookie=zzz; Path=/; Max-Age=600")
			w.Header().Add("Set-Cookie",
				"monkeycode_ai_session="+renewFakeSession+"; Path=/; Max-Age="+itoa(renewFakeMaxAge)+"; HttpOnly; SameSite=Lax")
		default:
			w.Header().Add("Set-Cookie",
				"monkeycode_ai_session="+renewFakeSession+"; Path=/; Max-Age="+itoa(renewFakeMaxAge)+"; HttpOnly; SameSite=Lax")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"message":"success","data":{"user":{"id":"u-renew"}}}`)
	})

	// ④ status：契约点到它；这里只记账（续期链路不该依赖它在先）。
	mux.HandleFunc("/api/v1/users/status", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.statusCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"user":{"id":"u-renew"}}}`)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// renewPointAt 把续期用的站点基址指向假上游。
//
// 注入点就是 renew.go 自己留的测试钩子 `renewHostOverride`（生产恒为空）——
// 刻意不去动 Auth.Host()：那条路径被 egress 自检 / 模型目录共用。
//
// ⚠ 维护提示：`renewHostOverride` 是**包级变量**，在 `t.Parallel()` 下会串台
// （本文件目前串行跑，所以安全）。若它迁成 `Client` 字段（或在 Client 上加同等
// 能力的覆盖点），本文件的注入方式需同步改 —— 只需改 renewPointAt 这一处。
func renewPointAt(t *testing.T, f *renewFake) {
	t.Helper()
	prev := renewHostOverride
	renewHostOverride = strings.TrimSuffix(f.srv.URL, "/")
	t.Cleanup(func() { renewHostOverride = prev })
}

// renewProvider 造一个「凭证放在实例内 + 落盘到临时目录」的 Provider。
//
// ⚠ AuthDir 必须给临时目录：RenewSession 成功后会 `SaveFile(p.authDir, …)`，
// 而空目录会让 `filepath.Join("", name)` 落到**当前工作目录** ——
// 测试跑完会在仓库根留下 monkeycode-*.json（真凭证形态的垃圾文件）。
func renewProvider(t *testing.T, f *renewFake, a *Auth) (*Provider, string) {
	t.Helper()
	renewPointAt(t, f)
	p := New(Config{Client: NewClientWithProxy(""), AuthDir: t.TempDir()})
	uid := a.UID()
	p.creds[uid] = a
	return p, uid
}

// renewAuth 造一份"待续期"的凭证：session 旧、带 email/password。
func renewAuth() *Auth {
	return &Auth{
		Session:  renewFakeOldSess,
		Email:    "renew@example.com",
		Password: "pw-secret",
		Region:   RegionCN,
	}
}

// powSatisfies 独立复核一个 PoW 解：salt+nonce 的 sha256 前 4*d 位必须等于目标。
//
// 与 captcha.go 的 solveOne 是**同一判据的独立实现**（那边求解、这边校验），
// 所以它能抓住"求解写错但仍把结果送出去"这类缺陷（redeem 会回 success:false）。
func powSatisfies(salt, target string, nonce int64) bool {
	bits := 4 * len(target)
	a := bits / 8
	l := bits % 8
	pad := target
	if len(target)%2 != 0 {
		pad = target + "0"
	}
	want, err := hex.DecodeString(pad)
	if err != nil {
		return false
	}
	h := sha256.Sum256([]byte(salt + itoa64(nonce)))
	for i := 0; i < a; i++ {
		if h[i] != want[i] {
			return false
		}
	}
	if l > 0 {
		mask := byte(255 << (8 - l))
		if h[a]&mask != want[a]&mask {
			return false
		}
	}
	return true
}

// ── 契约 1：happy path ───────────────────────────────────────────────────────

// 契约 1：challenge → solve → redeem → password-login 走通；TTL 来自 Max-Age。
func TestRenewHappyPathMaxAge(t *testing.T) {
	f := newRenewFake(t, renewFakeOpts{cookieMode: "max-age"})
	a := renewAuth()
	p, uid := renewProvider(t, f, a)

	_, res, err := p.RenewSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("续期应成功，实际 err=%v", err)
	}

	// ① 解析出的 session 必须是 36 位 uuid 形态，且与假上游下发的一致。
	if len(res.Session) != 36 {
		t.Errorf("session 应是 36 位 uuid，实际 %d 位：%q", len(res.Session), res.Session)
	}
	if res.Session != renewFakeSession {
		t.Errorf("session 取值错：got %q want %q", res.Session, renewFakeSession)
	}
	// ② Max-Age=2592000 ⇒ ExpiresInSec=2592000，来源标成 max-age。
	if res.ExpiresInSec != renewFakeMaxAge {
		t.Errorf("ExpiresInSec = %d，期望 %d（Max-Age 真值）", res.ExpiresInSec, renewFakeMaxAge)
	}
	if res.TTLSource != "max-age" {
		t.Errorf("TTLSource = %q，期望 \"max-age\"（有 Max-Age 时不该走回退）", res.TTLSource)
	}
	// ③ 三步各一次，且 PoW 解通过独立复核。
	if got := atomic.LoadInt32(&f.challengeCalls); got != 1 {
		t.Errorf("challenge 应恰好 1 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(&f.redeemCalls); got != 1 {
		t.Errorf("redeem 应恰好 1 次，实际 %d", got)
	}
	if got := atomic.LoadInt32(&f.loginCalls); got != 1 {
		t.Errorf("password-login 应恰好 1 次，实际 %d", got)
	}
	f.mu.Lock()
	bad := f.badSolutions
	f.mu.Unlock()
	if bad {
		t.Error("★ 送出去的 PoW 解没通过假上游的独立复核（求解实现与目标派生口径不一致）")
	}
}

// ── 契约 2：请求体字段名 ─────────────────────────────────────────────────────

// 契约 2：password-login 的 body JSON 键**恰好**是 email / password / captcha_token。
func TestRenewLoginBodyFieldNames(t *testing.T) {
	f := newRenewFake(t, renewFakeOpts{})
	a := renewAuth()
	p, uid := renewProvider(t, f, a)

	if _, _, err := p.RenewSession(context.Background(), uid); err != nil {
		t.Fatalf("续期应成功，实际 err=%v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.loginBodies) != 1 {
		t.Fatalf("应恰好抓到 1 次 password-login 请求体，实际 %d", len(f.loginBodies))
	}
	body := f.loginBodies[0]

	want := map[string]bool{"email": true, "password": true, "captcha_token": true}
	for k := range body {
		if !want[k] {
			t.Errorf("★ 请求体多出字段 %q（上游只认 email/password/captcha_token；camelCase 的 captchaToken 会 403）", k)
		}
	}
	for k := range want {
		if _, ok := body[k]; !ok {
			t.Errorf("★ 请求体缺少字段 %q", k)
		}
	}
	if len(body) != 3 {
		t.Errorf("请求体应恰好 3 个键，实际 %d 个：%v", len(body), body)
	}
	if v, _ := body["captcha_token"].(string); v != renewFakeCapOK {
		t.Errorf("captcha_token 应是 redeem 换回来的那个（%q），实际 %q", renewFakeCapOK, v)
	}
	if v, _ := body["email"].(string); v != "renew@example.com" {
		t.Errorf("email 传错：%q", v)
	}
	if v, _ := body["password"].(string); v != "pw-secret" {
		t.Errorf("password 传错：%q", v)
	}
}

// ── 契约 3：两步都不带凭据 ───────────────────────────────────────────────────

// 契约 3：challenge 与 password-login 都不带 Cookie、不带 Authorization
// （上游事实：两个端点都不要求登录态 ⇒ 这正是"session 死透也能救"的依据）。
func TestRenewStepsCarryNoCredentials(t *testing.T) {
	f := newRenewFake(t, renewFakeOpts{})
	a := renewAuth()
	p, uid := renewProvider(t, f, a)

	if _, _, err := p.RenewSession(context.Background(), uid); err != nil {
		t.Fatalf("续期应成功，实际 err=%v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.challengeCookie {
		t.Error("★ challenge 请求带了 Cookie —— 它是公开端点，而续期的前提正是「session 已经死了」")
	}
	if f.challengeAuth {
		t.Error("★ challenge 请求带了 Authorization")
	}
	if f.loginCookie {
		t.Error("★ password-login 请求带了 Cookie —— 同上，公开端点")
	}
	if f.loginAuth {
		t.Error("★ password-login 请求带了 Authorization")
	}
	if f.redeemCookie {
		t.Logf("提示：redeem 带了 Cookie（契约只约束 challenge 与 password-login，不影响本用例）")
	}
}

// ── 契约 4/5：Max-Age 缺失回退与边界 ────────────────────────────────────────

// 契约 4：没有 Max-Age ⇒ TTLSource="fallback"、ExpiresInSec=SessionLifetime 秒数。
func TestRenewTTLFallbackWithoutMaxAge(t *testing.T) {
	f := newRenewFake(t, renewFakeOpts{cookieMode: "no-max-age"})
	a := renewAuth()
	p, uid := renewProvider(t, f, a)

	_, res, err := p.RenewSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("续期应成功，实际 err=%v", err)
	}
	if res.TTLSource != "fallback" {
		t.Errorf("TTLSource = %q，期望 \"fallback\"（响应没给 Max-Age）", res.TTLSource)
	}
	if want := int64(SessionLifetime.Seconds()); res.ExpiresInSec != want {
		t.Errorf("ExpiresInSec = %d，期望 SessionLifetime 秒数 %d", res.ExpiresInSec, want)
	}
}

// 契约 5：Max-Age=0 按回退处理；多条 cookie 里要挑出 monkeycode_ai_session。
func TestRenewTTLBoundaries(t *testing.T) {
	t.Run("Max-Age=0 视为回退", func(t *testing.T) {
		f := newRenewFake(t, renewFakeOpts{cookieMode: "zero-max-age"})
		a := renewAuth()
		p, uid := renewProvider(t, f, a)
		_, res, err := p.RenewSession(context.Background(), uid)
		if err != nil {
			t.Fatalf("续期应成功，实际 err=%v", err)
		}
		// Max-Age=0 在 HTTP 语义里是"立刻删除"，但这里它是**新下发**的会话：
		// 当成 0 秒到期会让刚续期的账号立刻被判定过期 ⇒ 按回退处理。
		if res.TTLSource != "fallback" {
			t.Errorf("Max-Age=0 时 TTLSource = %q，期望 \"fallback\"", res.TTLSource)
		}
		if want := int64(SessionLifetime.Seconds()); res.ExpiresInSec != want {
			t.Errorf("Max-Age=0 时 ExpiresInSec = %d，期望 %d", res.ExpiresInSec, want)
		}
		if res.Session != renewFakeSession {
			t.Errorf("session 仍应按 UUID 解析出来，实际 %q", res.Session)
		}
	})

	t.Run("多条 cookie 里挑 monkeycode_ai_session", func(t *testing.T) {
		f := newRenewFake(t, renewFakeOpts{cookieMode: "second-cookie"})
		a := renewAuth()
		p, uid := renewProvider(t, f, a)
		_, res, err := p.RenewSession(context.Background(), uid)
		if err != nil {
			t.Fatalf("续期应成功，实际 err=%v", err)
		}
		if res.Session != renewFakeSession {
			t.Errorf("★ 挑错了 cookie：got %q want %q（应挑 monkeycode_ai_session，不是第一条）",
				res.Session, renewFakeSession)
		}
		if res.TTLSource != "max-age" {
			t.Errorf("TTLSource = %q，期望 \"max-age\"", res.TTLSource)
		}
	})
}

// ── 契约 6：403 业务错不重试 ────────────────────────────────────────────────

// 契约 6：password-login 恒回 403 ⇒ **只被调用 1 次**（业务错重试无意义）。
func TestRenew403NotRetried(t *testing.T) {
	f := newRenewFake(t, renewFakeOpts{loginStatus: http.StatusForbidden, loginBody: `{"code":403}`})
	a := renewAuth()
	p, uid := renewProvider(t, f, a)

	_, _, err := p.RenewSession(context.Background(), uid)
	if err == nil {
		t.Fatal("403 必须报错（不许假装续期成功）")
	}
	if got := atomic.LoadInt32(&f.loginCalls); got != 1 {
		t.Errorf("★ 403 是业务错，必须只发 1 次，实际 %d 次", got)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误里应能看出是 403（用户要能分辨「账号密码错」与「网络抖动」），实际 %q", err.Error())
	}
}

// ── 契约 7：网络偶发会重试 ──────────────────────────────────────────────────

// 契约 7：第一次 password-login 在传输层断开 → 第二次成功 ⇒ 命中 2 次。
//
// 这里的重试只能由续期路径自己提供：doJSON 对 POST 是"一发一次"（见 netretry.go 的设计）。
// 安全性依据：password-login 只是换一个新 session，新旧并存、不吊销旧的（实测），
// 所以重复提交不会破坏任何东西。
func TestRenewRetriesTransientNetworkError(t *testing.T) {
	f := newRenewFake(t, renewFakeOpts{failFirstLogin: true})
	a := renewAuth()
	p, uid := renewProvider(t, f, a)

	_, res, err := p.RenewSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("第一次的传输层抖动应被重试吃掉，实际 err=%v", err)
	}
	if res.Session != renewFakeSession {
		t.Errorf("重试成功后应拿到新 session，实际 %q", res.Session)
	}
	if got := atomic.LoadInt32(&f.loginCalls); got != 2 {
		t.Errorf("★ password-login 应恰好 2 次（1 次抖动 + 1 次成功），实际 %d 次", got)
	}
}

// ── 契约 8：缺 email/password 不发网络请求 ──────────────────────────────────

// 契约 8：email 或 password 缺失 ⇒ 直接报错，**零请求**（别先把 PoW 算完再失败）。
func TestRenewMissingCredentialsMakesNoRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(a *Auth)
	}{
		{"缺 email", func(a *Auth) { a.Email = "" }},
		{"缺 password", func(a *Auth) { a.Password = "" }},
		{"email 是空白", func(a *Auth) { a.Email = "   " }},
		{"password 是空白", func(a *Auth) { a.Password = "\t" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRenewFake(t, renewFakeOpts{})
			a := renewAuth()
			tc.mut(a)
			p, uid := renewProvider(t, f, a)

			_, _, err := p.RenewSession(context.Background(), uid)
			if err == nil {
				t.Fatal("缺凭证必须报错")
			}
			total := atomic.LoadInt32(&f.challengeCalls) + atomic.LoadInt32(&f.redeemCalls) +
				atomic.LoadInt32(&f.loginCalls) + atomic.LoadInt32(&f.statusCalls)
			if total != 0 {
				t.Errorf("★ 缺凭证时应零请求，实际打了 %d 次（challenge=%d redeem=%d login=%d status=%d）",
					total, f.challengeCalls, f.redeemCalls, f.loginCalls, f.statusCalls)
			}
		})
	}
}

// ── 契约 9：写回保全 ────────────────────────────────────────────────────────

// 契约 9：续期写回后，除 Session 与续期时刻外**其它字段一字不变**。
//
// 写回的真实语义（renew.go）：返回**一份拷贝**（`out := *a`）、自己 SaveFile 到 `p.authDir`、
// 并把新对象写回池内缓存 —— 刻意**不**就地改 `a`（它可能是池里的共享对象，就地改会让并发
// 请求读到半写状态）。所以下面核三处：返回值、磁盘文件、池内缓存。
func TestRenewWriteBackPreservesOtherFields(t *testing.T) {
	f := newRenewFake(t, renewFakeOpts{})
	a := renewAuth()
	a.APIKey = "oma_keep_me"
	a.SigningSecret = "omas_keep_me_too"
	a.UserID = "u-renew"
	a.Nickname = "保留我"
	a.Plan = "pro"
	a.Balance = "12.5"
	a.AddedAt = "2026-10-01T00:00:00Z"
	p, uid := renewProvider(t, f, a)

	got, _, err := p.RenewSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("续期应成功，实际 err=%v", err)
	}
	if got == nil {
		t.Fatal("成功时必须返回更新后的 Auth")
	}

	// ① Session 换新、续期时刻盖章。
	if got.Session != renewFakeSession {
		t.Errorf("返回对象的 Session 应是新的，实际 %q（旧值 %q）", got.Session, renewFakeOldSess)
	}
	if got.SessionRenewedAt.IsZero() {
		t.Fatal("续期必须给 SessionRenewedAt 盖章（SessionAnchor 的优先级靠它）")
	}
	if d := time.Since(got.SessionRenewedAt); d < 0 || d > 5*time.Minute {
		t.Errorf("SessionRenewedAt 应是「刚刚」，实际 %s（差 %s）",
			got.SessionRenewedAt.Format(time.RFC3339), d)
	}

	// ② 磁盘上那份 —— 重启后真正决定状态的是它。
	path := filepath.Join(p.authDir, FileName(got))
	back, err := LoadFile(path)
	if err != nil {
		t.Fatalf("续期没写回磁盘（%s 读不回来）：%v", path, err)
	}
	if back.Session != renewFakeSession {
		t.Errorf("落盘后的 Session 应是新的，实际 %q", back.Session)
	}
	if back.SessionRenewedAt.IsZero() {
		t.Error("★ 落盘后的 SessionRenewedAt 丢了（没进 JSON）—— 重启后续期时刻归零")
	}
	if back.Session == renewFakeOldSess {
		t.Error("落盘后还是旧 session（续期没写回磁盘）")
	}

	// ③ 一字不变的字段：返回值与磁盘两处都核。
	for _, tc := range []struct {
		name string
		get  func(x *Auth) string
		want string
	}{
		{"APIKey", func(x *Auth) string { return x.APIKey }, "oma_keep_me"},
		{"SigningSecret", func(x *Auth) string { return x.SigningSecret }, "omas_keep_me_too"},
		{"Email", func(x *Auth) string { return x.Email }, "renew@example.com"},
		{"Password", func(x *Auth) string { return x.Password }, "pw-secret"},
		{"Region", func(x *Auth) string { return x.Region }, RegionCN},
		{"UserID", func(x *Auth) string { return x.UserID }, "u-renew"},
		{"Nickname", func(x *Auth) string { return x.Nickname }, "保留我"},
		{"Plan", func(x *Auth) string { return x.Plan }, "pro"},
		{"Balance", func(x *Auth) string { return x.Balance }, "12.5"},
		{"AddedAt", func(x *Auth) string { return x.AddedAt }, "2026-10-01T00:00:00Z"},
	} {
		if v := tc.get(got); v != tc.want {
			t.Errorf("★ [返回值] %s 被续期改动了：got %q want %q", tc.name, v, tc.want)
		}
		if v := tc.get(back); v != tc.want {
			t.Errorf("★ [磁盘] %s 被续期改动了：got %q want %q", tc.name, v, tc.want)
		}
	}

	// ④ 池内缓存也要换成新 session（下次请求直接拿到新的）。
	p.mu.RLock()
	inPool := p.creds[uid]
	p.mu.RUnlock()
	if inPool == nil || inPool.Session != renewFakeSession {
		t.Errorf("池内缓存没换成新 session：%+v", inPool)
	}

	// ⑤ 文件名稳定：UID 用 UserID，续期不该改变主键（否则每次续期多一个账号记录）。
	if base := FileName(got); filepath.Base(path) != base {
		t.Errorf("★ 续期后凭证文件名变了：磁盘是 %s，按当前凭证应叫 %s", filepath.Base(path), base)
	}

	// 记录（不硬断言）：入参对象是否被就地改 —— 实现当前是拷贝语义。
	if a.Session != renewFakeOldSess {
		t.Logf("提示：入参 Auth 被就地改了（a.Session=%q）；实现目前是拷贝语义", a.Session)
	}
}

// ── 契约 10：SessionAnchor 优先级 ───────────────────────────────────────────

// 契约 10：SessionRenewedAt > AddedAt > 文件 mtime。
func TestSessionAnchorPriority(t *testing.T) {
	t.Run("SessionRenewedAt 优先于 AddedAt", func(t *testing.T) {
		added := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		renewed := added.Add(20 * 24 * time.Hour)
		a := &Auth{
			Session:          renewFakeSession,
			AddedAt:          added.Format(time.RFC3339),
			SessionRenewedAt: renewed,
		}
		at, ok := SessionAnchor(a)
		if !ok {
			t.Fatal("两个时刻都有，必须能取到锚点")
		}
		if !at.Equal(renewed) {
			t.Errorf("★ 锚点取了 AddedAt（%s）而不是续期时刻（%s）—— 续期后到期列会显示成「早就过期」",
				at.Format(time.RFC3339), renewed.Format(time.RFC3339))
		}
	})

	t.Run("凭证里的 session_renewed_at 三种形态都能当锚点", func(t *testing.T) {
		// 依据 expiry.go 的注释：字符串容错（数字毫秒 / RFC3339 / 手写日期）做在 Auth.UnmarshalJSON 里，
		// SessionAnchor 只看零值。这条钉住"老形态的凭证文件不会把续期时刻丢掉"。
		want := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
		cases := map[string]string{
			"RFC3339": want.Format(time.RFC3339),
			"数字毫秒":    strconv.FormatInt(want.UnixMilli(), 10),
			"手写日期时间":  want.Format("2006-01-02 15:04:05"),
		}
		for name, raw := range cases {
			doc := `{"auth":{"session":"` + renewFakeSession + `","session_renewed_at":"` + raw + `"}}`
			a, err := parseAuth([]byte(doc))
			if err != nil {
				t.Errorf("[%s] 解析凭证失败：%v", name, err)
				continue
			}
			at, ok := SessionAnchor(a)
			if !ok {
				t.Errorf("[%s] session_renewed_at=%q 被丢掉了（锚点回落到未知）", name, raw)
				continue
			}
			if d := at.Sub(want); d < -time.Minute || d > time.Minute {
				t.Errorf("[%s] 锚点解析错：got %s want %s", name,
					at.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		}
	})

	t.Run("没有续期时刻时回落 AddedAt", func(t *testing.T) {
		added := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		a := &Auth{Session: renewFakeSession, AddedAt: added.Format(time.RFC3339)}
		at, ok := SessionAnchor(a)
		if !ok || !at.Equal(added) {
			t.Errorf("没有 SessionRenewedAt 时应回落 AddedAt=%s，实际 ok=%v at=%s",
				added.Format(time.RFC3339), ok, at.Format(time.RFC3339))
		}
	})

	t.Run("两个时刻都没有时回落文件 mtime", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "monkeycode-x.json")
		if err := os.WriteFile(path, []byte(`{"auth":{"session":"`+renewFakeSession+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatal(err)
		}
		a := &Auth{Session: renewFakeSession, FilePath: path}
		at, ok := SessionAnchor(a)
		if !ok {
			t.Fatal("有文件时应能取到 mtime 锚点")
		}
		if d := at.Sub(mt); d < -2*time.Second || d > 2*time.Second {
			t.Errorf("锚点应约等于文件 mtime=%s，实际 %s", mt.Format(time.RFC3339), at.Format(time.RFC3339))
		}
	})

	t.Run("三样都没有时报告未知（不是 1970）", func(t *testing.T) {
		if _, ok := SessionAnchor(&Auth{Session: renewFakeSession}); ok {
			t.Error("既无时刻又无文件时必须报告未知（ok=false），不能给一个会显示成「已过期」的零值")
		}
		if _, ok := SessionAnchor(nil); ok {
			t.Error("nil 必须报告未知")
		}
	})
}
