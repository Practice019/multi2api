package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestRefreshResponseFieldNames 续期响应的字段名**逐字锁定**。
//
// # 为什么这条是本轮的核心（用户报「没有 Token」）
//
// 实测真实响应（2026-09-30 直接打上游）：
//
//	{"device_token":"dt-9PpG1q39MXpdm8CCYa6smmXl",
//	 "refresh_token":"drt-EEpc518l3XX9AdEMCVMcvZ2y",
//	 "token_type":"Bearer",
//	 "expires_at":"2026-10-30T06:56:55Z",          ← ISO 字符串
//	 "refresh_token_expires_at":"2027-09-25T06:56:55Z",
//	 "created_at":"2026-09-30T06:56:55Z"}
//
// 而旧实现读的是 `access_token` + `expires_in`（相对秒）—— **两个都不存在**：
//
//	access_token 不存在 → at=="" → 报"续期响应缺少 access_token"（永远失败）
//	expires_in   不存在 → 过期时刻从没被存下 → 界面 Token 列恒显示 `—`
//
// 所以判据必须断言**逐个字段**，而不是"续期成功了吗"
// （后者在旧代码里对 `device_token` 这种形状也是失败的，但它失败得"看起来合理"）。
func TestRefreshResponseFieldNames(t *testing.T) {
	// 上游真实形状（值换成假的）
	const realBody = `{"device_token":"dt-NEW","refresh_token":"drt-NEW",` +
		`"token_type":"Bearer","expires_at":"2026-10-30T06:56:55Z",` +
		`"refresh_token_expires_at":"2027-09-25T06:56:55Z",` +
		`"created_at":"2026-09-30T06:56:55Z"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realBody))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	old := &Auth{
		AccessToken:  "dt-OLD",
		RefreshToken: "drt-OLD",
		MachineID:    "mid",
		ProductID:    providerID,
	}
	next, err := c.RefreshCredential(context.Background(), old)
	if err != nil {
		t.Fatalf("续期失败（字段名读错就是这种表现）：%v", err)
	}
	if next.AccessToken != "dt-NEW" {
		t.Errorf("access token = %q，want dt-NEW —— "+
			"响应里叫 `device_token`，只认 `access_token` 会永远拿不到新 token", next.AccessToken)
	}
	if next.RefreshToken != "drt-NEW" {
		t.Errorf("refresh token = %q，want drt-NEW", next.RefreshToken)
	}
	// expires_at 是 ISO 字符串 → 必须被解析成毫秒时刻
	wantExp, _ := time.Parse(time.RFC3339, "2026-10-30T06:56:55Z")
	if next.ExpiresAt != wantExp.UnixMilli() {
		t.Errorf("ExpiresAt = %d，want %d（来自 expires_at 的 ISO 字符串）—— "+
			"旧实现读 `expires_in`（不存在）→ 恒为 0 → 界面 Token 列永远是 `—`",
			next.ExpiresAt, wantExp.UnixMilli())
	}
	wantRT, _ := time.Parse(time.RFC3339, "2027-09-25T06:56:55Z")
	if next.RefreshTokenExpiresAt != wantRT.UnixMilli() {
		t.Errorf("RefreshTokenExpiresAt = %d，want %d", next.RefreshTokenExpiresAt, wantRT.UnixMilli())
	}
}

// TestParseQoderTimeMS 时间值解析兼容 ISO 字符串与秒/毫秒数字。
//
// ⚠ 解不出时必须返回 **0**（未知），不能填当前时间 ——
// 那会让界面把"不知道"显示成"已过期"，误导用户去重新登录。
func TestParseQoderTimeMS(t *testing.T) {
	want, _ := time.Parse(time.RFC3339, "2026-10-30T06:56:55Z")
	wantMS := want.UnixMilli()

	for _, tc := range []struct {
		name string
		in   string
		want int64
	}{
		{"ISO 字符串（实测形态）", "2026-10-30T06:56:55Z", wantMS},
		{"秒级数字", "1793342215", 1793342215000},
		{"毫秒级数字", "1793342215000", 1793342215000},
		{"空串 → 未知", "", 0},
		{"垃圾 → 未知", "not-a-time", 0},
		{"0 → 未知（不是 1970）", "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseQoderTimeMS(tc.in); got != tc.want {
				t.Errorf("parseQoderTimeMS(%q) = %d，want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpiresAtMSPrefersStoredField ExpiresAtMS **优先**落盘字段，JWT 只是兜底。
//
// 用户报障的根因之一：旧实现**只**走 JWT。而 qodercn 的 access_token 是
// `dt-` 前缀的不透明串（27 字符，不是 JWT），于是恒返回 0 → 界面 `—`。
func TestExpiresAtMSPrefersStoredField(t *testing.T) {
	// 不透明 token + 有落盘字段 → 用落盘字段
	a := &Auth{AccessToken: "dt-opaque-token-not-a-jwt", ExpiresAt: 1793342215000}
	if got := a.ExpiresAtMS(); got != 1793342215000 {
		t.Errorf("ExpiresAtMS = %d，want 落盘字段 1793342215000 —— "+
			"只走 JWT 会对 `dt-` 不透明 token 恒返回 0（界面永远是 `—`）", got)
	}
	// 没有落盘字段时回落 JWT（手工导入的 JWT 凭据）
	jwt := fixtureJWT(time.Now().Add(3 * time.Hour).Unix())
	b := &Auth{AccessToken: jwt}
	if b.ExpiresAtMS() <= 0 {
		t.Error("没有落盘字段时应回落 token 的 JWT exp")
	}
	// 两者都没有 → 0（未知，不是"已过期"）
	if got := (&Auth{AccessToken: "dt-opaque"}).ExpiresAtMS(); got != 0 {
		t.Errorf("都拿不到时应返回 0（未知），实际 %d", got)
	}
}

// TestTokenExpiryReportsStoredField CredentialExpiryExt 报出落盘字段。
//
// 这是界面「Token」列的直接数据源。
func TestTokenExpiryReportsStoredField(t *testing.T) {
	expMS := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	p := NewWithConfig(Config{})
	at, ok := p.TokenExpiry(gateway.Credential{
		Provider: providerID, UID: "u1",
		Secret: &Auth{AccessToken: "dt-opaque", ExpiresAt: expMS},
	})
	if !ok {
		t.Fatal("有落盘过期字段时必须报出时刻 —— 否则界面 Token 列显示 `—`")
	}
	if want := time.UnixMilli(expMS).Unix(); at != want {
		t.Errorf("TokenExpiry = %d，want %d", at, want)
	}

	// 未知时返回 ok=false（**不编造 0**）
	if _, ok := p.TokenExpiry(gateway.Credential{
		Provider: providerID, UID: "u2", Secret: &Auth{AccessToken: "dt-opaque"},
	}); ok {
		t.Error("不知道时必须 ok=false —— 返回 (0,true) 会被渲染成「1970 年已过期」")
	}
}

// TestRefreshJobIsRegistered 必须注册**续期**后台任务，且与签到任务**不同名**。
//
// # 用户报障的形态
//
// qoder 此前只有签到任务（见 checkin.go），**没有续期任务** ——
// 而过期时刻只能从续期响应拿到，于是界面 Token 列恒为空。
//
// ⚠ 名字必须不同：调度器按名字区分任务，同名会互相覆盖
// （表现为"只有一个在跑"，且没有任何报错）。
//
// ⚠⚠ 而且名字必须**带产品 id**：qoder 与 qodercn 是两个实例，
// 实测它们注册同一个 `qoder-refresh` 时调度器输出
//
//	scheduler: 任务名 qoder-refresh 重复（上游 qoder 与 qodercn），已跳过后者
//
// 于是中国版的 token 永不被续期 —— 而用户报的那个号正是中国版。
func TestRefreshJobIsRegistered(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir(), CheckinInterval: time.Hour})
	ext, ok := gateway.ExtOf[gateway.JobExt](p)
	if !ok {
		t.Fatal("qoder 必须实现 gateway.JobExt")
	}
	wantName := p.refreshJobName()
	var names []string
	var hasRefresh bool
	for _, j := range ext.Jobs() {
		names = append(names, j.Name)
		if j.Name == wantName {
			hasRefresh = true
			if j.Interval <= 0 {
				t.Errorf("续期任务间隔 = %v，必须 > 0（<=0 核心不注册）", j.Interval)
			}
			if j.Run == nil {
				t.Error("续期任务的 Run 为 nil —— 注册了也不会做事")
			}
		}
	}
	if !hasRefresh {
		t.Errorf("没有名为 %q 的续期任务：%v\n"+
			"没有它 → 从不续期 → 过期时刻永远存不进来 → 界面 Token 列恒显示 `—`",
			wantName, names)
	}
	// 签到任务也要在（本轮把 Jobs() 合并到一处，别把签到弄丢了）
	if len(names) < 2 {
		t.Errorf("应当同时有签到与续期两个任务，实际只有 %v", names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("任务名重复 %q —— 调度器会互相覆盖：%v", n, names)
		}
		seen[n] = true
	}
}

// TestTwoInstancesHaveDistinctJobNames 两个实例的任务名**必须不同**。
//
// 这是上一条的正面形式：直接比较国际版与中国版实例的名字。
// 相同的话调度器会静默跳过其中一个（实测发生在中国版上）。
func TestTwoInstancesHaveDistinctJobNames(t *testing.T) {
	intl := NewWithConfig(Config{AuthDir: t.TempDir()})
	cn := NewWithConfig(Config{Product: QoderCN, AuthDir: t.TempDir()})
	if intl.refreshJobName() == cn.refreshJobName() {
		t.Errorf("国际版与中国版的续期任务同名（%q）—— "+
			"调度器会判重并静默跳过后者，那一侧永不续期（实测症状）",
			intl.refreshJobName())
	}
	if !strings.Contains(cn.refreshJobName(), ProviderIDCN) {
		t.Errorf("任务名 %q 应含产品 id %q —— 否则两个实例无法区分",
			cn.refreshJobName(), ProviderIDCN)
	}
}

// TestRefreshCandidatesFiltersByProduct 续期候选**必须按产品过滤**。
//
// # 实测抓到的缺陷
//
// qoder 与 qodercn 共用同一个凭证目录（`auths/qoder/`），靠文件里的
// `product_id` 区分。两个实例都会跑本任务 —— 若各自读**全部**凭证：
//
//	qoder  实例拿到 CN 的号 → 用国际版端点发 CN 的 refresh_token → 401
//
// 表现是"一半的号续期必然失败"，日志写着 `refresh_token 已失效` ——
// 而那个 token 在**对的**端点上完全可用（实测 200）。
// 极易被误读成"用户的号真失效了"，让人白跑一趟重新登录。
func TestRefreshCandidatesFiltersByProduct(t *testing.T) {
	dir := t.TempDir()
	// 同一目录放两份凭证，product_id 不同（真实布局就是这样）
	writeAuthFile(t, dir, "qoder-intl.json", "u-intl", "dt-INTL", "drt-INTL", providerID)
	writeAuthFile(t, dir, "qoder-cn.json", "u-cn", "dt-CN", "drt-CN", ProviderIDCN)

	intl := NewWithConfig(Config{AuthDir: dir})
	got, err := intl.refreshCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProductID != providerID {
		t.Errorf("国际版实例的候选 = %v —— 必须只含 product_id=%q 的凭证。"+
			"混入 CN 的号会用国际版端点发它的 refresh_token → 401，"+
			"而那个 token 本来完全可用", uidsOf(got), providerID)
	}

	cn := NewWithConfig(Config{Product: QoderCN, AuthDir: dir})
	gotCN, err := cn.refreshCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotCN) != 1 || gotCN[0].ProductID != ProviderIDCN {
		t.Errorf("中国版实例的候选 = %v —— 必须只含 product_id=%q 的凭证",
			uidsOf(gotCN), ProviderIDCN)
	}
}

func uidsOf(list []*Auth) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.ProductID+"/"+a.UIDValue())
	}
	return out
}

// writeAuthFile 写一份最小可用的 qoder 凭证。
func writeAuthFile(t *testing.T, dir, name, uid, token, refresh, product string) {
	t.Helper()
	raw := `{"auth":{"access_token":"` + token + `","refresh_token":"` + refresh +
		`","machine_id":"mid","uid":"` + uid + `","product_id":"` + product + `"},` +
		`"account":{"uid":"` + uid + `","nickname":"n"}}`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestShouldRefreshWhenExpiryUnknown 「过期时刻未知」必须判为**该刷**。
//
// 这是用户报障的直接修复：旧凭证没存 `expires_at`，而它只能从续期响应拿到。
// 若判成"不刷"，界面就永远是 `—`。
func TestShouldRefreshWhenExpiryUnknown(t *testing.T) {
	p := NewWithConfig(Config{})
	// 不透明 token + 没有落盘过期字段 = 我们完全不知道它何时过期
	a := &Auth{AccessToken: "dt-opaque", RefreshToken: "drt-x"}
	if !p.shouldRefresh(a, time.Now()) {
		t.Error("过期时刻未知时必须刷一次去问清楚 —— " +
			"否则 expires_at 永远存不进来，界面 Token 列永远显示 `—`")
	}
}

// TestShouldRefreshSkipsFarFromExpiry 离过期还远时**不该**刷（不空转）。
//
// 反方向守卫：qoder 的 token 寿命约 30 天，每 30 分钟都刷一次毫无收益。
func TestShouldRefreshSkipsFarFromExpiry(t *testing.T) {
	p := NewWithConfig(Config{})
	a := &Auth{
		AccessToken:  "dt-opaque",
		RefreshToken: "drt-x",
		ExpiresAt:    time.Now().Add(29 * 24 * time.Hour).UnixMilli(), // 还有 29 天
	}
	if p.shouldRefresh(a, time.Now()) {
		t.Error("还有 29 天过期时不该刷 —— 每轮都刷只是白耗配额")
	}
	// 但真的临近时必须刷
	b := &Auth{
		AccessToken:  "dt-opaque",
		RefreshToken: "drt-x",
		ExpiresAt:    time.Now().Add(1 * time.Minute).UnixMilli(),
	}
	if !p.shouldRefresh(b, time.Now()) {
		t.Error("只剩 1 分钟时必须刷")
	}
}

// TestRunRefreshWritesExpiryToCredential 端到端：续期后**过期时刻落到凭证上**。
//
// 判据落在"凭证对象有 ExpiresAt"——那正是界面读的东西。
func TestRunRefreshWritesExpiryToCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_token":"dt-NEW","refresh_token":"drt-NEW",` +
			`"expires_at":"2026-10-30T06:56:55Z"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	// 旧形态凭证：没有 expires_at（就是线上那份的形状）
	raw := `{"auth":{"access_token":"dt-OLD","refresh_token":"drt-OLD",` +
		`"machine_id":"mid","uid":"u1","product_id":"qoder"},` +
		`"account":{"uid":"u1","nickname":"n"}}`
	fp := filepath.Join(dir, "qoder-u1.json")
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewWithBase(srv.URL)
	p := NewWithConfig(Config{Client: c, AuthDir: dir})

	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 报错: %v", err)
	}

	// 磁盘上必须出现 expires_at
	out, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("落盘内容无法解析: %v\n%s", err, out)
	}
	if _, ok := doc["auth"]["expires_at"]; !ok {
		t.Errorf("续期后落盘内容里没有 expires_at —— 界面 Token 列下次启动仍是 `—`。\n实际：%s", out)
	}
	if !strings.Contains(string(out), "dt-NEW") {
		t.Errorf("落盘的 access_token 没更新：%s", out)
	}
}

// TestRunRefreshSkipsNonRenewable 不可续期的凭证**不发请求**（不空转）。
func TestRunRefreshSkipsNonRenewable(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_token":"dt-X"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	raw := `{"auth":{"access_token":"dt-OLD","uid":"u1","product_id":"qoder"},` +
		`"account":{"uid":"u1","nickname":"n"}}` // 没有 refresh_token
	if err := os.WriteFile(filepath.Join(dir, "qoder-u1.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewWithBase(srv.URL)
	p := NewWithConfig(Config{Client: c, AuthDir: dir})
	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 报错: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("没有 refresh_token 时不该发请求，实际 %d 次", n)
	}
}

// min 供本文件内使用（Go 1.21+ 有内置，但这里显式定义避免版本歧义）。

// itoa64 十进制整数转字符串（避免为一行引入 strconv 的噪音）。
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// writeRaw 写一份原始凭证文件。
func writeRaw(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
