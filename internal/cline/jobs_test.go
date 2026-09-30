package cline

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

// TestRefreshCandidatesPreferLiveSecret 续期必须作用在**池持有的对象**上。
//
// # 为什么这条是本轮实测踩到的分叉（必须钉住）
//
// 第一版 `runRefresh` 走 `LoadDir`（从磁盘**新建** `*Auth`），而池里持有的是
// 启动时那份**不同的对象**。结果：磁盘上的 `expire_time` 已经 +1 小时，
// 而界面（读 `Pool.SecretOf`）仍显示"已过期 -3322 秒"。
//
// 实测日志：
//
//	cline: 后台续期完成 —— 续期 1，跳过 0，失败 0   ← 磁盘确实刷了
//	/admin/accounts → token_expire_sec = -3322      ← 池里没变
//
// 这正是本仓多个 refreshskew.go 注释里反复警告的"续期写到另一个对象上"。
//
// 判据：`refreshCandidates` 在有 `creds` 时必须返回**池里那个指针**
// （`== live`，不是"内容相同"）—— 只有同一个对象，原地更新才会被界面看到。
func TestRefreshCandidatesPreferLiveSecret(t *testing.T) {
	dir := t.TempDir()
	writeClineAuth(t, dir, "u-live", "DISK-TOKEN", "REF", time.Now().Add(-time.Hour).UnixMilli())

	// 池里那份（与磁盘那份内容不同，模拟"启动快照 + 后来磁盘被改过"）
	live := &Auth{AccessToken: "LIVE-TOKEN", RefreshToken: "REF", AccountID: "acct-u-live", Email: "e"}
	p := NewWithConfig(Config{AuthDir: dir})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		if uid == live.UID() {
			return gateway.Credential{Provider: providerID, UID: uid, Secret: live}, true
		}
		return gateway.Credential{}, false
	})

	got, err := p.refreshCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应有 1 个候选，实际 %d", len(got))
	}
	if got[0] != live {
		t.Error("没有返回池里那个对象 —— 刷磁盘副本会导致" +
			"「落盘已续期、界面仍显示已过期」（实测就是这个形态）")
	}
	if got[0].AccessToken != "LIVE-TOKEN" {
		t.Errorf("拿到的是磁盘副本（token=%q）", got[0].AccessToken)
	}
}

// TestRefreshCandidatesFallsBackToDisk 没有 creds 时退回磁盘（不报错）。
//
// 反向守卫：`creds` 未接线（纯单测 / 未入池）时不能返回空 ——
// 那会让"没有任何候选"被读成"没事可做"。
func TestRefreshCandidatesFallsBackToDisk(t *testing.T) {
	dir := t.TempDir()
	writeClineAuth(t, dir, "u-disk", "DISK", "REF", time.Now().Add(time.Hour).UnixMilli())
	p := NewWithConfig(Config{AuthDir: dir}) // 刻意不调 SetCredentialSource

	got, err := p.refreshCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("未接线时应退回磁盘并给出 1 个候选，实际 %d", len(got))
	}
}

// TestProviderRegistersRefreshJob cline **必须**注册后台续期任务。
//
// # 为什么这条是本轮的核心判据（用户报「Token 已过期，怎么不会自动刷新」）
//
// cline 一直**满足** `gateway.CredentialRefresher`（有正确签名的
// RefreshCredential），但核心的续期只有两条触发路径：
//
//	① 出站请求时  handler.needsRefreshVia
//	② 后台定时任务  上游自报的 Jobs()
//
// cline 此前**一条都不占**（没有 Jobs()），于是只有"有人拿 cline 号发对话
// 请求"时才会顺手续期。没人用 cline 时 token 就静静过期，界面上「Token」
// 列永远显示"已过期" —— 而 refresh_token 明明还在。
//
// 所以判据不能是"实现了 RefreshCredential 吗"（那一直为真、抓不到这个缺陷），
// 必须是"**注册了后台任务吗**"。
func TestProviderRegistersRefreshJob(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	ext, ok := gateway.ExtOf[gateway.JobExt](p)
	if !ok {
		t.Fatal("cline 必须实现 gateway.JobExt —— 否则没人用 cline 时 token 永不续期，" +
			"界面「Token」列会一直显示已过期（用户报障的形态）")
	}
	jobs := ext.Jobs()
	if len(jobs) == 0 {
		t.Fatal("Jobs() 返回空 —— 等于没有后台续期")
	}
	var found bool
	for _, j := range jobs {
		if j.Name == "cline-refresh" {
			found = true
			if j.Interval <= 0 {
				t.Errorf("续期任务间隔 = %v，必须 > 0（<=0 核心不注册）", j.Interval)
			}
			if j.Run == nil {
				t.Error("续期任务的 Run 为 nil —— 注册了也不会做事")
			}
			// 间隔必须**显著小于** token 寿命（1 小时），否则一轮都刷不上。
			if j.Interval > 30*time.Minute {
				t.Errorf("间隔 %v 太大 —— cline 的 access_token 寿命只有 1 小时，"+
					"间隔超过半小时会吃掉失败重试的余量", j.Interval)
			}
		}
	}
	if !found {
		t.Errorf("没有名为 cline-refresh 的任务：%+v", jobs)
	}
}

// TestNoAuthDirMeansNoJob 没有凭证目录时**不注册**任务（不空转）。
//
// 反向守卫：`Jobs()` 若无条件返回任务，每个"没开 cline"的部署都会
// 每 10 分钟空跑一轮。
func TestNoAuthDirMeansNoJob(t *testing.T) {
	p := NewWithConfig(Config{})
	ext, ok := gateway.ExtOf[gateway.JobExt](p)
	if !ok {
		t.Skip("未实现 JobExt")
	}
	if jobs := ext.Jobs(); len(jobs) != 0 {
		t.Errorf("没有凭证目录时不该注册任务（会空转），实际 %+v", jobs)
	}
}

// writeClineAuth 写一份最小可用的 cline 凭证。
func writeClineAuth(t *testing.T, dir, uid, accessToken, refreshToken string, expireMS int64) string {
	t.Helper()
	body := map[string]any{
		"auth": map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"expire_time":   expireMS,
			"account_id":    "acct-" + uid,
		},
		"account": map[string]any{"uid": uid, "nickname": "测试号"},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(dir, "cline-"+uid+".json")
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestRunRefreshRefreshesExpiredToken 后台任务**真的会把过期 token 换掉**。
//
// 端到端：写一份过期凭证 → 起假上游 → 跑 runRefresh → 断言
//
//	① 上游确实被请求了（POST /refresh 或等价端点）
//	② 磁盘上的 token **变了**（不只是内存）
func TestRunRefreshRefreshesExpiredToken(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		_ = n
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"NEW-TOKEN","refreshToken":"NEW-REFRESH","expiresIn":3600}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	// 已过期 1 小时的 token
	expired := time.Now().Add(-time.Hour).UnixMilli()
	writeClineAuth(t, dir, "u-expired", "OLD-TOKEN", "OLD-REFRESH", expired)

	c := New()
	c.APIBase = srv.URL
	c.WorkOSBase = srv.URL
	p := NewWithConfig(Config{Client: c, AuthDir: dir})

	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 报错: %v", err)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("后台任务没有向上游发起续期请求")
	}

	// 磁盘必须被更新（不只是内存）
	raw, err := os.ReadFile(filepath.Join(dir, "cline-u-expired.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "NEW-TOKEN") {
		t.Errorf("磁盘上的 access_token 没被更新 —— 续期结果没落盘，"+
			"下次启动又会读到过期的那份。实际内容：%s", string(raw)[:min(len(raw), 300)])
	}
}

// TestRunRefreshSkipsNonRenewable 没有 refresh_token 的凭证**不该**被请求。
//
// # ⚠ 这条断言为什么不能只写"没发请求"（实测的教训）
//
// 我第一版只断言 `calls == 0`，于是**变异存活**：把 `runRefresh` 里的
// `if !a.Renewable() { continue }` 去掉后测试照样绿。原因是"不刷"这个结论
// 在**三层**都成立，任何一层拦下都不会发请求：
//
//	① runRefresh 的 Renewable 跳过        （本文件）
//	② Auth.needsRefresh 开头的 Renewable  （cline.go）
//	③ Client.RefreshCredential 开头的 Renewable（client.go，直接返 ErrRefreshExpired）
//
// 所以"没发请求"证明不了**任何一层**是对的 —— 它是个恒真的弱判据。
//
// 现在的判据分两半，分别钉住不同的层：
//
//   - 行为面：确实没发请求（不管哪层拦住的）
//   - **决策面**：`needsRefresh` 对不可续期凭证必须返回 false
//     —— 这条直接钉住②，且它是"该不该刷"的唯一权威
//
// ① 那层是幂等的冗余守卫（③ 兜底），删掉不会造成缺陷，故不为它单独断言 ——
// 强行断言实现细节会让"合并冗余守卫"这种正当重构变成假红。
func TestRunRefreshSkipsNonRenewable(t *testing.T) {
	// 决策面：不可续期的凭证**永远**不该被判为"该刷"。
	//
	// 即使它已经过期很久 —— 没有 refresh_token 就是刷不了，
	// 判成"该刷"只会让每轮都白发一次请求。
	expiredNoRef := &Auth{
		AccessToken:  "OLD",
		RefreshToken: "",
		ExpireTime:   time.Now().Add(-24 * time.Hour).UnixMilli(),
	}
	if expiredNoRef.needsRefresh(time.Now(), defaultRefreshScanInterval) {
		t.Error("没有 refresh_token 的凭证被判为「该刷」—— " +
			"它根本刷不了，每轮都会白发一次请求")
	}
	if expiredNoRef.needsRefresh(time.Now(), time.Hour) {
		t.Error("同上（大窗口下也必须是 false）")
	}

	// 行为面：端到端确实不发请求。
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"X"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeClineAuth(t, dir, "u-noref", "OLD", "", time.Now().Add(-time.Hour).UnixMilli())

	c := New()
	c.APIBase = srv.URL
	c.WorkOSBase = srv.URL
	p := NewWithConfig(Config{Client: c, AuthDir: dir})

	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 报错: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("没有 refresh_token 时不该发请求，实际发了 %d 次", n)
	}
}

// TestRunRefreshSkipsFreshToken 没过期的 token **不该**被刷。
//
// 刷得太勤是反方向的事故（本仓记过「每请求都续期」那次）。
// cline 的 token 寿命 1 小时、按 50% 比例触发，所以"刚签发 5 分钟"的
// 不该被刷。
func TestRunRefreshSkipsFreshToken(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"NEW"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	// 不透明 token（解不出 JWT）→ 走 expire_time 判据；1 小时后才过期
	fresh := time.Now().Add(time.Hour).UnixMilli()
	writeClineAuth(t, dir, "u-fresh", "opaque-fresh-token", "REF", fresh)

	c := New()
	c.APIBase = srv.URL
	c.WorkOSBase = srv.URL
	p := NewWithConfig(Config{Client: c, AuthDir: dir})

	if err := p.runRefresh(context.Background()); err != nil {
		t.Fatalf("runRefresh 报错: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("还有 1 小时才过期的 token 不该被刷（会退化成每轮都换 token），实际发了 %d 次", n)
	}
}

// TestIsRefreshExpiredDistinguishesTerminal 终态与可重试必须分开。
//
// 用户动作完全不同：终态只能重新登录，可重试下一轮会自动再试。
// 判据必须是 errors.Is（`ErrRefreshExpired` 会被 %w 包装）。
func TestIsRefreshExpiredDistinguishesTerminal(t *testing.T) {
	if !isRefreshExpired(ErrRefreshExpired) {
		t.Error("裸 ErrRefreshExpired 应判为终态")
	}
	if !isRefreshExpired(wrappedErr(ErrRefreshExpired)) {
		t.Error("被 %w 包装的 ErrRefreshExpired 仍应判为终态 —— " +
			"用字符串比较会在包装后失效")
	}
	if isRefreshExpired(nil) {
		t.Error("nil 不该判为终态")
	}
	if isRefreshExpired(context.DeadlineExceeded) {
		t.Error("超时是可重试的，不该判为终态（会让用户白跑一趟重新登录）")
	}
}

// wrappedErr 模拟 fmt.Errorf("...: %w", target) 的包装。
func wrappedErr(target error) error {
	return &wrapped{target}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "包装: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
