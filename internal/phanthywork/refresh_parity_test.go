package phanthywork

// refresh_parity_test.go 续期链路「与官方客户端 1:1」的判据。
//
// # 这个文件对应哪三条偏差
//
//	D1 请求头      TestRefreshSendsOnlyContentTypeAndAccept
//	D3 刷前查共享库 TestAdoptFresherTokenFromOfficialStore
//	              TestAdoptRejectsDifferentSub / TestAdoptIgnoresExpiredCandidate
//	              TestAdoptIgnoresSameToken / TestReadOfficialStoreToleratesGarbage
//	D2 被拒≠终态   TestRejectedThenAdoptSucceeds / TestRejectedWithoutNewTokenIsRetriable
//
// 三条都来自**官方客户端源码**（sidecar `tH`/`Pd3`/`o44`/`Po`，字节偏移见
// docs/reports/pw-renewal-parity-plan-2026-11-14.md §1），不是推断。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// ── 夹具 ──────────────────────────────────────────────────────────────────

// officialStoreDir 造一个**空的**官方配置目录，并把它指给被测代码。
//
// 用 env 覆盖是官方的第一优先级（`S3()`：`process.env[q.configDirEnv] || …`），
// 所以这样指是最忠实的 —— 不需要往真实家目录里写东西。
func officialStoreDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PHANTHY_CODE_CONFIG_DIR", dir)
	return dir
}

// writeStore 往官方配置目录写一份 .credentials.json（驼峰字段，官方 t44 读的那套）。
func writeStore(t *testing.T, dir string, oauth map[string]any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"claudeAiOauth": oauth})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeEntry 造一条官方库条目（sub 指定、还有 expMS 毫秒才过期）。
func storeEntry(sub, refresh string, expMS int64) map[string]any {
	return map[string]any{
		"accessToken":  fakeJWT(sub, expMS/1000),
		"refreshToken": refresh,
		"expiresAt":    float64(expMS),
		"scopes":       []string{"user:inference"},
	}
}

// cred 把 *Auth 包成 gateway.Credential（与本包其余用例同形）。
func cred(a *Auth) gateway.Credential {
	return gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}
}

// rejectSrv 一个只回 400 invalid_grant 的假上游（记录被打了多少次）。
func rejectSrv(t *testing.T, hits *int, onHit func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOAuthToken {
			http.NotFound(w, r)
			return
		}
		*hits++
		if onHit != nil {
			onHit()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_grant",` +
			`"message":"Refresh token is invalid or expired."}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ── D1：请求头 ────────────────────────────────────────────────────────────

// TestRefreshSendsOnlyContentTypeAndAccept 续期请求**只有** Content-Type 与 Accept。
//
// # 为什么这条必须存在
//
// 官方 `Pd3`（@99945774）逐字：
//
//	headers: { "Content-Type": "application/json", Accept: "application/json" }
//
// 而 `applyCommonHeaders` 会额外挂 `User-Agent: phanthy-code-cli/<ver>` 与
// `x-phanthy-client-source: desktop` —— 这两个在官方属于**推理路径**。
// 我们每次"换令牌"都额外声明了一次"我是桌面端"，而官方在这一跳上
// **从不表明身份** ⇒ 这个组合是别人不会产生的形态，正是风控最容易记账的特征。
//
// ⚠ 同时钉住"UA 必须真的不发出去"：Go 的传输层在 UA 缺失时会自动补
// `Go-http-client/1.1`，所以只写"不设置"是不够的（这是本用例最容易漏的一处）。
func TestRefreshSendsOnlyContentTypeAndAccept(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOAuthToken {
			http.NotFound(w, r)
			return
		}
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + fakeJWT("u-hdr", nowMS()/1000+1800) +
			`","refresh_token":"rt-new","expires_in":1800}`))
	}))
	defer srv.Close()

	a := newTestAuth("u-hdr", "rt-old")
	if _, err := NewWithBase(srv.URL).RefreshTokens(context.Background(), a); err != nil {
		t.Fatalf("续期应成功: %v", err)
	}

	// ① 官方确实发的那两个
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q，want application/json", v)
	}
	if v := got.Get("Accept"); v != "application/json" {
		t.Errorf("Accept = %q，want application/json", v)
	}
	// ② ★ 官方**不发**的那两个
	if v := got.Get("User-Agent"); v != "" {
		t.Errorf("续期请求不该带 User-Agent，实际 %q ——\n"+
			"官方这一跳只发 Content-Type + Accept。带上 UA（尤其 `phanthy-code-cli/…`\n"+
			"或 Go 默认的 `Go-http-client/1.1`）都是别人不会产生的形态，风控据此记账。", v)
	}
	if v := got.Get("X-Phanthy-Client-Source"); v != "" {
		t.Errorf("续期请求不该带 x-phanthy-client-source，实际 %q ——\n"+
			"它在官方属于**推理路径**（Yg3()），续期带上等于在换令牌时额外声明「我是桌面端」。", v)
	}
}

// ── D3：刷前查共享凭证库（官方 o44）──────────────────────────────────────

// TestAdoptFresherTokenFromOfficialStore 库里已有同账号的新令牌 ⇒ 直接采用、**零请求**。
//
// 这就是官方 `o44`：`if (Y) return Y;` —— 一个请求都不发。
// 它既是省请求，更是**避免轮换竞态**的唯一手段：对方刷好后我们照抄，
// 就不会出现"两边各自持有已作废的那把"。
func TestAdoptFresherTokenFromOfficialStore(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := NewWithClient(NewWithBase(srv.URL), t.TempDir(), 0)

	dir := officialStoreDir(t)
	// 我们的号：已过期（会触发续期），sub = u-adopt
	a := withFilePath(t, authWithRemaining("u-adopt", "rt-ours", -60))
	fresh := fakeJWT("u-adopt", nowMS()/1000+1800)
	writeStore(t, dir, map[string]any{
		"accessToken":  fresh,
		"refreshToken": "rt-theirs",
		"expiresAt":    float64(nowMS() + 1500_000),
		"scopes":       []string{"user:inference"},
	})
	wireAccounts(p, a)

	if err := p.RefreshCredential(cred(a)); err != nil {
		t.Fatalf("续期应成功: %v", err)
	}
	// ★ 核心：零请求
	if n := fake.pathCount(pathOAuthToken); n != 0 {
		t.Errorf("共享库里有别端刷好的令牌时**不该发续期请求**，实际打了 %d 次\n"+
			"后果：与官方桌面端共用账号时必然互踢（各刷各的 ⇒ 对方那把立即作废）", n)
	}
	if a.OAuth.AccessToken != fresh {
		t.Error("应采用共享库里的 accessToken")
	}
	if a.OAuth.RefreshToken != "rt-theirs" {
		t.Errorf("应采用共享库里的 refreshToken（对方轮换出来的那把才有用），实际 %q",
			a.OAuth.RefreshToken)
	}
}

// TestAdoptRejectsDifferentSub 库里是**另一个账号**的令牌 ⇒ 绝不采用。
//
// 这是 adoptCandidate 里最危险的一条判据：漏了它，多账号环境下会把
// A 号的令牌装到 B 号头上 —— 那比"续期失败"严重得多（串号）。
// 官方 `o44` 原文就写着：`if (!w || w !== O) return null;`
func TestAdoptRejectsDifferentSub(t *testing.T) {
	srv, _ := startFakeUpstream(t)
	p := NewWithClient(NewWithBase(srv.URL), t.TempDir(), 0)

	dir := officialStoreDir(t)
	a := withFilePath(t, authWithRemaining("u-mine", "rt-ours", -60))
	other := fakeJWT("u-someone-else", nowMS()/1000+1800) // ← sub 不同
	writeStore(t, dir, map[string]any{
		"accessToken":  other,
		"refreshToken": "rt-someone-else",
		"expiresAt":    float64(nowMS() + 1500_000),
	})
	wireAccounts(p, a)

	if _, ok := adoptFromOfficialStore(a, nowMS()); ok {
		t.Fatal("sub 不同却采用了对方的令牌 —— 这是**串号**，把另一个账号的凭据装到了本号头上。\n" +
			"官方 o44 判据原文：if (!w || w !== O) return null;")
	}
	if err := p.RefreshCredential(cred(a)); err != nil {
		t.Fatalf("应回落到正常续期: %v", err)
	}
	if a.OAuth.AccessToken == other {
		t.Error("串号：采用了别的账号的 accessToken")
	}
}

// TestAdoptIgnoresExpiredCandidate 库里那份**已经过期** ⇒ 不采用。
//
// 采用过期令牌只会把到期时刻往回拨，下一轮又得刷 —— 白丢一次请求，
// 还把"我们这边到底什么状态"搅浑。官方 `Vd3` 的择优（`Rd3`/`C$7`）
// 也是把"未过期"排在前面。
func TestAdoptIgnoresExpiredCandidate(t *testing.T) {
	dir := officialStoreDir(t)
	a := withFilePath(t, authWithRemaining("u-exp", "rt-ours", -60))
	writeStore(t, dir, map[string]any{
		"accessToken":  fakeJWT("u-exp", nowMS()/1000+1800),
		"refreshToken": "rt-theirs",
		"expiresAt":    float64(nowMS() - 10_000), // ← 已过期
	})
	if _, ok := adoptFromOfficialStore(a, nowMS()); ok {
		t.Fatal("库里那份已过期却仍被采用 —— 会把到期时刻往回拨，下一轮又得刷")
	}
}

// TestAdoptIgnoresSameToken 库里那把与当前**完全一样** ⇒ 不算"别人刷过"。
//
// 官方 `o44`：`if (!z || z.oauth.accessToken === K) return null;`
// 少了这条，"没人刷过"会被误判成"有人刷过"，于是续期被整个跳过。
func TestAdoptIgnoresSameToken(t *testing.T) {
	dir := officialStoreDir(t)
	a := withFilePath(t, authWithRemaining("u-same", "rt-ours", -60))
	writeStore(t, dir, map[string]any{
		"accessToken":  a.OAuth.AccessToken, // ← 一模一样
		"refreshToken": "rt-ours",
		"expiresAt":    float64(nowMS() + 1500_000),
	})
	if _, ok := adoptFromOfficialStore(a, nowMS()); ok {
		t.Fatal("与当前完全相同的令牌不该被判成「别人刷过了」——否则续期会被无谓跳过")
	}
}

// TestReadOfficialStoreToleratesGarbage 读不到 / 格式不对 ⇒ 当没有，不报错。
//
// 这是**别人的**文件，格式与生命周期都不由我们管。把它的读取失败变成
// 续期路径上的错误，等于让一个可选优化决定主流程成败。
func TestReadOfficialStoreToleratesGarbage(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"不是 JSON", `not json at all`},
		{"空对象", `{}`},
		{"没有 accessToken", `{"claudeAiOauth":{"refreshToken":"rt"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := officialStoreDir(t)
			if err := os.WriteFile(filepath.Join(dir, ".credentials.json"),
				[]byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok := ReadOfficialStore(); ok {
				t.Errorf("%s：应回 ok=false（读不到就当没有）", c.name)
			}
		})
	}
}

// TestReadOfficialStoreAcceptsPhanthyCodeOauthKey 兼容 phanthyCodeOauth 那个键。
//
// 官方 `Td3` 的优先级是 claudeAiOauth → phanthyCodeOauth → 根对象本身。
// 两个都认，将来官方换键名时不会静默失效。
func TestReadOfficialStoreAcceptsPhanthyCodeOauthKey(t *testing.T) {
	dir := officialStoreDir(t)
	raw, _ := json.Marshal(map[string]any{
		"phanthyCodeOauth": storeEntry("u-x", "rt-x", nowMS()+600_000),
	})
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, ok := ReadOfficialStore()
	if !ok {
		t.Fatal("phanthyCodeOauth 这个键也该认（官方 Td3 的第二优先级）")
	}
	if c.Sub != "u-x" {
		t.Errorf("sub = %q，want u-x", c.Sub)
	}
}

// ── D2：被拒之后的动作（官方 tH 的第二句）────────────────────────────────

// TestRejectedThenAdoptSucceeds 被拒之后重读共享库拿到新令牌 ⇒ **本次续期算成功**。
//
// 对应官方 `tH`：`if (!J) return o44(await _(), q, O());`
//
// 这是 D2 真正的价值：不是"再重试一次"（重试还是拿旧的那把去换，必然还是被拒），
// 而是**换一个来源** —— 用对方轮换出来的那把继续走。
//
// 场景精确复现竞态现场：我们发请求的那一刻库里还没有新令牌，
// 上游回 400 的**同时**"对方"刚好刷完并落盘 ⇒ 重读就能拿到。
func TestRejectedThenAdoptSucceeds(t *testing.T) {
	dir := officialStoreDir(t)
	fresh := fakeJWT("u-race", nowMS()/1000+1800)

	hits := 0
	srv := rejectSrv(t, &hits, func() {
		// 上游说"不认"的同一时刻，对方把新令牌写进了共享库。
		writeStore(t, dir, map[string]any{
			"accessToken":  fresh,
			"refreshToken": "rt-theirs",
			"expiresAt":    float64(nowMS() + 1500_000),
		})
	})

	p := NewWithClient(NewWithBase(srv.URL), t.TempDir(), 0)
	a := withFilePath(t, authWithRemaining("u-race", "rt-stale", -60))
	wireAccounts(p, a)

	if err := p.RefreshCredential(cred(a)); err != nil {
		t.Fatalf("被拒之后共享库里有别端刷好的令牌 ⇒ 本次续期应判成功，实际报错：%v\n"+
			"官方 tH 原文：if (!J) return o44(await _(), q, O());", err)
	}
	if hits != 1 {
		t.Errorf("应恰好打 1 次续期（被拒），实际 %d", hits)
	}
	if a.OAuth.RefreshToken != "rt-theirs" {
		t.Errorf("应采用共享库里对方轮换出来的那把，实际 %q —— 否则下一轮还是拿已作废的旧 token 去换",
			a.OAuth.RefreshToken)
	}
}

// TestRejectedWithoutNewTokenIsRetriable 被拒且库里也没新的 ⇒
// 返回**可重试**错误，且**不得**判成终态、**不得**出现"需重新登录"字样。
func TestRejectedWithoutNewTokenIsRetriable(t *testing.T) {
	officialStoreDir(t) // 空目录：库里没有可用的新令牌

	hits := 0
	srv := rejectSrv(t, &hits, nil)

	p := NewWithClient(NewWithBase(srv.URL), t.TempDir(), 0)
	a := withFilePath(t, authWithRemaining("u-dead", "rt-stale", -60))
	wireAccounts(p, a)

	err := p.RefreshCredential(cred(a))
	if err == nil {
		t.Fatal("库里没有可用的新令牌，续期应报错")
	}
	if !IsRefreshRejected(err) {
		t.Errorf("应判为「被拒」（可重试）：%v", err)
	}
	if IsRefreshExpired(err) {
		t.Fatalf("**不得**判成终态 ErrRefreshExpired：%v\n"+
			"后果：核心计一次失败、连续 3 次永久禁用 —— 一次轮换竞态就能把健康号打成死号", err)
	}
	if got := err.Error(); strings.Contains(got, "需重新登录") {
		t.Errorf("「被拒」这一档不该告诉用户去重新登录（官方文案是「稍后重试；登录信息已保留」）：%v", got)
	}
}
