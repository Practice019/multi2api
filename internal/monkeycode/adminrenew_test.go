// adminrenew_test.go 批量续期端点的契约测试。
//
// 全程**不碰网络**（httptest.NewRecorder 直调 handler）：续期引擎由接缝注入一个
// 记账用的假实现，所以这里能精确构造那几条"真机才踩得到"的边界
// （缺凭据 / 24h 窗口 / force / remaining 递减 / 单号失败不中断），
// 而这些正是 Node 版真机出过故障的地方。
package monkeycode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fakeRenew 可编程的续期引擎假实现。
type fakeRenew struct {
	mu       sync.Mutex
	facts    map[string]renewAccountFacts
	calls    []string
	errs     map[string]error
	ttl      map[string]int64
	src      string
	inFlight int32 // 当前并发数
	maxSeen  int32 // 观测到的最大并发
	hold     time.Duration
}

func newFakeRenew() *fakeRenew {
	return &fakeRenew{
		facts: map[string]renewAccountFacts{},
		errs:  map[string]error{},
		ttl:   map[string]int64{},
		src:   "measured",
	}
}

// install 把假引擎装上去，并注册测试结束时的清理。
//
// ⚠ 清理必须**还原成装之前的那份**（生产路径由 adminrenew.go 的 init 接上真引擎），
// 不能简单置 nil —— 那会把同包里后续测试的引擎悄悄摘掉，属于"测试互相污染"。
func (f *fakeRenew) install(t *testing.T) *fakeRenew {
	t.Helper()
	prev := renewEngine
	t.Cleanup(func() { renewEngine = prev })
	SetRenewEngine(
		func(p *Provider, uid string) renewAccountFacts {
			f.mu.Lock()
			defer f.mu.Unlock()
			if fact, ok := f.facts[uid]; ok {
				return fact
			}
			// 缺省：有凭据、没续过（多数用例只关心这一种）。
			return renewAccountFacts{HasCredentials: true}
		},
		func(ctx context.Context, p *Provider, uid string) (int64, string, error) {
			n := atomic.AddInt32(&f.inFlight, 1)
			for {
				seen := atomic.LoadInt32(&f.maxSeen)
				if n <= seen || atomic.CompareAndSwapInt32(&f.maxSeen, seen, n) {
					break
				}
			}
			f.mu.Lock()
			f.calls = append(f.calls, uid)
			err := f.errs[uid]
			ttl := f.ttl[uid]
			f.mu.Unlock()
			if f.hold > 0 {
				time.Sleep(f.hold)
			}
			atomic.AddInt32(&f.inFlight, -1)
			if err != nil {
				return 0, "", err
			}
			// ★ 续期成功后**必须**把"刚续过"这个事实记下来（真引擎是写
			//   Auth.SessionRenewedAt + 落盘/写池）。这里显式模拟它，因为
			//   「remaining 能收敛到 0」这件事**依赖**它：
			//   不记的话每个号每轮都算候选，前端循环永远退不出来 ——
			//   这正是 Node 版那个 remaining 恒为 15 的故障。
			f.mu.Lock()
			fact := f.facts[uid]
			fact.HasCredentials = true
			fact.RenewedAt = time.Now()
			f.facts[uid] = fact
			f.mu.Unlock()
			if ttl == 0 {
				ttl = int64(SessionLifetime / time.Second)
			}
			return ttl, f.src, nil
		},
	)
	return f
}

func (f *fakeRenew) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRenew) called(uid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == uid {
			return true
		}
	}
	return false
}

// renewResp 与端点响应对应的解码结构。
type renewResp struct {
	OK        bool       `json:"ok"`
	Results   []renewRow `json:"results"`
	Renewed   int        `json:"renewed"`
	Failed    int        `json:"failed"`
	Skipped   int        `json:"skipped"`
	Remaining int        `json:"remaining"`
	Error     string     `json:"error"`
	Raw       string     `json:"-"`
}

// postRenew 直调 handler（不起真实 server、不发真实请求）。
func postRenew(t *testing.T, p *Provider, body string) (*httptest.ResponseRecorder, renewResp) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("{}")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, adminRenewPath, reader)
	rec := httptest.NewRecorder()
	p.handleRenew(rec, req)
	var parsed renewResp
	raw := rec.Body.String()
	parsed.Raw = raw
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("响应不是合法 JSON：%v\n%s", err, raw)
	}
	return rec, parsed
}

// newRenewProvider 造一个带 uid 枚举的 Provider（生产路径靠注入的枚举，不靠 p.creds）。
func newRenewProvider(uids ...string) *Provider {
	p := New(Config{AuthDir: "testdata-unused"})
	p.SetUIDEnumerator(func() []string { return uids })
	return p
}

// idsOf 取结果里的 uid 列表（顺序无关断言用）。
func idsOf(rows []renewRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UID)
	}
	return out
}

func rowOf(rows []renewRow, uid string) *renewRow {
	for i := range rows {
		if rows[i].UID == uid {
			return &rows[i]
		}
	}
	return nil
}

// ── ① 未接线：回 501，而不是"200 + 0 成功" ─────────────────────────────

func TestRenewEndpointNotWiredReturns501(t *testing.T) {
	prev := renewEngine
	SetRenewEngine(nil, nil)
	t.Cleanup(func() { renewEngine = prev }) // 见 fakeRenew.install 的注释：要还原而不是置 nil
	p := newRenewProvider("mk-1")
	rec, resp := postRenew(t, p, `{}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("未接线时应当是 501，实得 %d：%s", rec.Code, resp.Raw)
	}
	if resp.OK {
		t.Fatalf("未接线时 ok 必须为 false：%s", resp.Raw)
	}
	if !strings.Contains(resp.Error, "续期引擎未接线") {
		t.Fatalf("错误文案要说清是引擎没接线，实得：%q", resp.Error)
	}
}

// ── ② uids 为空 = 全部（走 uidEnumerator）────────────────────────────

func TestRenewEmptyUIDsMeansAll(t *testing.T) {
	f := newFakeRenew().install(t)
	p := newRenewProvider("mk-a", "mk-b", "mk-c")

	_, resp := postRenew(t, p, `{}`)

	if resp.Renewed != 3 || resp.Failed != 0 || resp.Skipped != 0 {
		t.Fatalf("空 uids 应当续全部 3 个，实得 renewed=%d failed=%d skipped=%d（%s）",
			resp.Renewed, resp.Failed, resp.Skipped, resp.Raw)
	}
	if resp.Remaining != 0 {
		t.Fatalf("一次做完时 remaining 必须是 0，实得 %d", resp.Remaining)
	}
	for _, uid := range []string{"mk-a", "mk-b", "mk-c"} {
		if !f.called(uid) {
			t.Fatalf("uid %s 没有被续期（调用记录：%v）", uid, f.calls)
		}
	}
}

// 显式给 uids 时**只**跑给的这些（不能顺手把全部也跑了）。
func TestRenewGivenUIDsOnly(t *testing.T) {
	f := newFakeRenew().install(t)
	p := newRenewProvider("mk-a", "mk-b", "mk-c")

	_, resp := postRenew(t, p, `{"uids":["mk-b"]}`)

	if f.callCount() != 1 || !f.called("mk-b") {
		t.Fatalf("只该续 mk-b，实得调用 %v", f.calls)
	}
	if resp.Renewed != 1 || len(resp.Results) != 1 {
		t.Fatalf("结果应当只有 1 条：%s", resp.Raw)
	}
}

// uids 去重（前端把"选中 + 全部"两个来源拼一起时会出现重复）。
func TestRenewDedupesUIDs(t *testing.T) {
	f := newFakeRenew().install(t)
	p := newRenewProvider("mk-a")

	_, resp := postRenew(t, p, `{"uids":["mk-a","mk-a"," mk-a "]}`)

	if f.callCount() != 1 {
		t.Fatalf("重复 uid 只该跑一次，实得 %d 次", f.callCount())
	}
	if resp.Renewed != 1 {
		t.Fatalf("实得：%s", resp.Raw)
	}
}

// ── ③ 缺凭据 → skipped:no-credentials，且**不计入 remaining** ─────────

func TestRenewSkipsNoCredentials(t *testing.T) {
	f := newFakeRenew().install(t)
	f.facts["mk-nopass"] = renewAccountFacts{HasCredentials: false}
	p := newRenewProvider("mk-ok", "mk-nopass")

	_, resp := postRenew(t, p, `{}`)

	row := rowOf(resp.Results, "mk-nopass")
	if row == nil || !row.Skipped || row.Reason != "no-credentials" {
		t.Fatalf("缺凭据必须 skipped:no-credentials，实得 %+v（%s）", row, resp.Raw)
	}
	if row.OK {
		t.Fatalf("跳过的行 ok 必须是 false（否则前端会把它算成成功）")
	}
	if f.called("mk-nopass") {
		t.Fatalf("跳过的号**不该**被送去续期")
	}
	if resp.Skipped != 1 || resp.Renewed != 1 || resp.Remaining != 0 {
		t.Fatalf("skipped 不该计入 remaining：skipped=%d renewed=%d remaining=%d",
			resp.Skipped, resp.Renewed, resp.Remaining)
	}
}

// ── ④ 24h 内续过 → skipped:recently-renewed；force 无视它 ────────────

func TestRenewSkipsRecentlyRenewed(t *testing.T) {
	f := newFakeRenew().install(t)
	f.facts["mk-fresh"] = renewAccountFacts{HasCredentials: true, RenewedAt: time.Now().Add(-1 * time.Hour)}
	p := newRenewProvider("mk-fresh", "mk-old")

	_, resp := postRenew(t, p, `{}`)

	row := rowOf(resp.Results, "mk-fresh")
	if row == nil || !row.Skipped || row.Reason != "recently-renewed" {
		t.Fatalf("1 小时前续过的号必须最近跳过，实得 %+v", row)
	}
	if f.called("mk-fresh") {
		t.Fatalf("最近续过的号不该再续（那正是 Node 版 remaining 不收敛的成因）")
	}
	if !f.called("mk-old") {
		t.Fatalf("另一个号应当照常续期")
	}
	if resp.Skipped != 1 || resp.Renewed != 1 {
		t.Fatalf("实得：%s", resp.Raw)
	}
}

func TestRenewForceIgnoresRecentWindow(t *testing.T) {
	f := newFakeRenew().install(t)
	f.facts["mk-fresh"] = renewAccountFacts{HasCredentials: true, RenewedAt: time.Now().Add(-1 * time.Hour)}
	p := newRenewProvider("mk-fresh")

	_, resp := postRenew(t, p, `{"force":true}`)

	if !f.called("mk-fresh") {
		t.Fatalf("force:true 必须无视 24h 窗口")
	}
	if resp.Renewed != 1 || resp.Skipped != 0 {
		t.Fatalf("实得：%s", resp.Raw)
	}
}

// 窗口的边界要写在测试里（25 小时前 = 过期，照续）。
func TestRenewRecentWindowBoundary(t *testing.T) {
	f := newFakeRenew().install(t)
	f.facts["mk-25h"] = renewAccountFacts{HasCredentials: true, RenewedAt: time.Now().Add(-25 * time.Hour)}
	f.facts["mk-23h"] = renewAccountFacts{HasCredentials: true, RenewedAt: time.Now().Add(-23 * time.Hour)}
	p := newRenewProvider("mk-25h", "mk-23h")

	_, resp := postRenew(t, p, `{}`)

	if !f.called("mk-25h") {
		t.Fatalf("25 小时前续过的号应当照常续期（窗口是 24h）")
	}
	if f.called("mk-23h") {
		t.Fatalf("23 小时前续过的号应当跳过")
	}
	if resp.Renewed != 1 || resp.Skipped != 1 {
		t.Fatalf("实得：%s", resp.Raw)
	}
}

// 从未续过（零值）不能被当成"最近续过"（零值 time 的坑）。
func TestRenewNeverRenewedIsNotSkipped(t *testing.T) {
	newFakeRenew().install(t)
	p := newRenewProvider("mk-never")

	_, resp := postRenew(t, p, `{}`)

	if resp.Renewed != 1 {
		t.Fatalf("零值 RenewedAt = 从未续过，必须照续：%s", resp.Raw)
	}
}

// ── ⑤ remaining 递减到 0（Node 版那个死循环的守卫）───────────────────

func TestRenewRemainingDecreasesToZero(t *testing.T) {
	newFakeRenew().install(t)
	p := newRenewProvider("mk-1", "mk-2", "mk-3", "mk-4", "mk-5")

	// 每轮上限 2 个候选 → remaining 应当 3,1,0 递减（最后一轮只剩 1 个）。
	wantRemaining := []int{3, 1, 0}
	wantRenewed := []int{2, 2, 1}
	seen := map[string]bool{}
	for i, want := range wantRemaining {
		_, resp := postRenew(t, p, `{"limit":2}`)
		if resp.Remaining != want {
			t.Fatalf("第 %d 轮 remaining 应为 %d，实得 %d（%s）", i+1, want, resp.Remaining, resp.Raw)
		}
		if resp.Renewed != wantRenewed[i] {
			t.Fatalf("第 %d 轮应当续 %d 个，实得 %d（%s）", i+1, wantRenewed[i], resp.Renewed, resp.Raw)
		}
		for _, row := range resp.Results {
			// 只盯"真被续了"的行：跳过的行（上一轮已续）本来就会每轮出现在结果里，
			// 把它们算进来是对这件事的误读。
			if !row.OK {
				continue
			}
			if seen[row.UID] {
				t.Fatalf("第 %d 轮：uid %s 被重复续期（remaining 不收敛的成因）", i+1, row.UID)
			}
			seen[row.UID] = true
		}
	}
	// 收尾：remaining 归零之后再请求一次 —— 因为 5 个号现在都在 24h 窗口内，
	// 所以**不带 force 就是全部跳过**（这正是收敛的机制），带 force 才真再续一次。
	_, quiet := postRenew(t, p, `{"uids":["mk-1"]}`)
	if quiet.Renewed != 0 || quiet.Skipped != 1 || quiet.Remaining != 0 {
		t.Fatalf("窗口内的号再请求应当整条跳过（收敛机制）：%s", quiet.Raw)
	}
	_, forced := postRenew(t, p, `{"uids":["mk-1"],"force":true}`)
	if forced.Renewed != 1 {
		t.Fatalf("force:true 应当无视窗口真续一次：%s", forced.Raw)
	}
	if len(seen) != 5 {
		t.Fatalf("5 个候选应当各不相同地各续一次，实得 %d 个", len(seen))
	}
}

// skipped 的号永远不进 remaining（否则就是 Node 版那个"永不收敛"）。
func TestRenewSkippedNeverCountsInRemaining(t *testing.T) {
	f := newFakeRenew().install(t)
	for _, uid := range []string{"mk-s1", "mk-s2"} {
		f.facts[uid] = renewAccountFacts{HasCredentials: false}
	}
	p := newRenewProvider("mk-a", "mk-b", "mk-s1", "mk-s2")

	_, resp := postRenew(t, p, `{"limit":1}`)

	if resp.Skipped != 2 {
		t.Fatalf("应当跳过 2 个，实得 %d", resp.Skipped)
	}
	// 候选只有 2 个（mk-a / mk-b），本轮跑了 1 个 ⇒ remaining = 1，与跳过的 2 个无关。
	if resp.Remaining != 1 {
		t.Fatalf("remaining 只数候选（实得 %d，期望 1）", resp.Remaining)
	}
}

// ── ⑥ 单号失败不中断整批 ────────────────────────────────────────────

func TestRenewSingleFailureDoesNotAbortBatch(t *testing.T) {
	f := newFakeRenew().install(t)
	f.errs["mk-bad"] = errors.New("PoW 超时")
	p := newRenewProvider("mk-1", "mk-bad", "mk-3")

	_, resp := postRenew(t, p, `{}`)

	if resp.Renewed != 2 || resp.Failed != 1 {
		t.Fatalf("一个失败不该影响另外两个：%s", resp.Raw)
	}
	bad := rowOf(resp.Results, "mk-bad")
	if bad == nil || bad.OK || !strings.Contains(bad.Error, "PoW 超时") {
		t.Fatalf("失败行要带上原始错误，实得 %+v", bad)
	}
	// 失败与跳过必须能分开看：失败不是 skipped。
	if bad.Skipped {
		t.Fatalf("失败不能被伪装成 skipped")
	}
	for _, uid := range []string{"mk-1", "mk-3"} {
		if row := rowOf(resp.Results, uid); row == nil || !row.OK {
			t.Fatalf("%s 应当成功，实得 %+v", uid, row)
		}
	}
}

// 读事实就失败（凭证不存在）也算 failed，且不伪装成 skip。
func TestRenewFactsErrorCountsAsFailure(t *testing.T) {
	f := newFakeRenew().install(t)
	f.facts["mk-gone"] = renewAccountFacts{Err: "凭证不存在"}
	p := newRenewProvider("mk-gone", "mk-ok")

	_, resp := postRenew(t, p, `{}`)

	if resp.Failed != 1 || resp.Skipped != 0 {
		t.Fatalf("读事实失败算 failed，不该算 skipped：%s", resp.Raw)
	}
	if f.called("mk-gone") {
		t.Fatalf("读不到事实的号不该送去续期")
	}
	if resp.Renewed != 1 {
		t.Fatalf("另一个号应当照常成功：%s", resp.Raw)
	}
}

// ── ⑦ 响应里绝不能出现 session 值 ───────────────────────────────────

func TestRenewResponseNeverLeaksSession(t *testing.T) {
	// 用一个"看起来像 session"的哨兵值：只要它出现在任何响应里，测试就红。
	const sentinel = "8f2c1d4e-9a7b-4c3d-8e5f-0123456789ab"
	f := newFakeRenew().install(t)
	p := newRenewProvider("mk-1", "mk-nopass")
	f.facts["mk-nopass"] = renewAccountFacts{HasCredentials: false}
	// 故意把 session 塞进**引擎侧可能碰到的**地方：凭证对象。
	p.mu.Lock()
	p.creds["mk-1"] = &Auth{Session: sentinel, UserID: "mk-1"}
	p.mu.Unlock()

	_, resp := postRenew(t, p, `{"force":true}`)

	if strings.Contains(resp.Raw, sentinel) {
		t.Fatalf("响应里出现了 session 值：%s", resp.Raw)
	}
	// 连前缀也不行（"前 8 位"这种折中同样等价于泄漏）。
	if strings.Contains(resp.Raw, sentinel[:8]) {
		t.Fatalf("响应里出现了 session 前缀：%s", resp.Raw)
	}
	// 顺手确认响应里确实有内容（避免"空响应也算通过"的假绿）。
	if !strings.Contains(resp.Raw, "mk-1") || !strings.Contains(resp.Raw, "expires_in_sec") {
		t.Fatalf("响应结构不对，这条断言就没意义了：%s", resp.Raw)
	}
	// ★ 键名是**契约**：本仓惯例 snake_case（改这条要同时改前端）。
	for _, key := range []string{"expires_in_sec", "ttl_source"} {
		if !strings.Contains(resp.Raw, key) {
			t.Fatalf("响应键名必须是 snake_case 的 %q：%s", key, resp.Raw)
		}
	}
	for _, bad := range []string{"expiresInSec", "ttlSource"} {
		if strings.Contains(resp.Raw, bad) {
			t.Fatalf("响应里不该再出现 camelCase 键名 %q：%s", bad, resp.Raw)
		}
	}
}

// ── ⑧ 并发：受限、且每号只跑一次 ────────────────────────────────────

func TestRenewConcurrencyIsBounded(t *testing.T) {
	f := newFakeRenew().install(t)
	f.hold = 30 * time.Millisecond
	uids := []string{"mk-1", "mk-2", "mk-3", "mk-4", "mk-5", "mk-6", "mk-7", "mk-8", "mk-9", "mk-10",
		"mk-11", "mk-12", "mk-13", "mk-14", "mk-15", "mk-16"}
	p := newRenewProvider(uids...)

	_, resp := postRenew(t, p, `{}`)

	if resp.Renewed != len(uids) {
		t.Fatalf("应当全部成功：%s", resp.Raw)
	}
	if got := atomic.LoadInt32(&f.maxSeen); got > renewConcurrency {
		t.Fatalf("并发超过了上限 %d（实测峰值 %d）", renewConcurrency, got)
	}
	if got := atomic.LoadInt32(&f.maxSeen); got < 2 {
		t.Fatalf("并发没生效（峰值 %d）—— 16 个号串行会很慢", got)
	}
	if f.callCount() != len(uids) {
		t.Fatalf("每个号只该跑一次，实得 %d 次", f.callCount())
	}
}

// ── ⑩ 真机踩到的坑：池里那份是旧快照，24h 窗口靠本地写回那份才生效 ────────

// TestRenewFactsPreferFreshestRenewedAt 守住"续期后第二次调用必须跳过"。
//
// 生产上 `p.cred()` 会优先给**账号池**里那份（启动时的快照），而 `RenewSession`
// 的写回是【磁盘 + 本地 p.creds】。若只读池里那份，同一进程里第二次调用时
// SessionRenewedAt 还是零值 ⇒ 24h 窗口永不生效 ⇒ 用户连点两次白烧两次 PoW。
// 实测（2026-11-02，真机）：修之前两次调用都是 renewed:1/skipped:0。
func TestRenewFactsPreferFreshestRenewedAt(t *testing.T) {
	p := newRenewProvider("mk-x")
	// 池里那份：有凭据、但续期时刻为零值（模拟"续期后池快照没更新"）。
	pooled := &Auth{UserID: "mk-x", Email: "a@b.c", Password: "p" /* SessionRenewedAt 为零值 */}
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		if uid != "mk-x" {
			return gateway.Credential{}, false
		}
		return gateway.Credential{UID: uid, Secret: pooled}, true
	})
	// 本地那份：续期成功后的写回（renew.go 干的），带着新鲜时刻。
	fresh := time.Now().Add(-1 * time.Hour)
	p.mu.Lock()
	p.creds["mk-x"] = &Auth{UserID: "mk-x", Email: "a@b.c", Password: "p", SessionRenewedAt: fresh}
	p.mu.Unlock()

	f := renewFactsOf(p, "mk-x")
	if !f.HasCredentials {
		t.Fatalf("本地那份有材料时应当判为可续期：%+v", f)
	}
	if !f.RenewedAt.Equal(fresh) {
		t.Fatalf("续期时刻应当取更新的那份（本地 %v），实得 %v", fresh, f.RenewedAt)
	}

	// 端到端：**真 facts（renewFactsOf）+ 假 run** —— 第二次调用必须因窗口而跳过。
	prev := renewEngine
	ran := 0
	SetRenewEngine(renewFactsOf, func(ctx context.Context, p *Provider, uid string) (int64, string, error) {
		ran++
		return int64(SessionLifetime / time.Second), "max-age", nil
	})
	t.Cleanup(func() { renewEngine = prev })

	_, resp := postRenew(t, p, `{}`)
	if resp.Renewed != 0 || resp.Skipped != 1 {
		t.Fatalf("窗口内的号必须恰好跳过、不重复续期：%s", resp.Raw)
	}
	if ran != 0 {
		t.Fatalf("跳过的号不该被送去续期（实测踩过：白烧 PoW）")
	}
}

// 池里那份没有任何续期记录时，不能用零值把号判成"刚续过"。
func TestRenewFactsZeroRenewedAtIsNotRecent(t *testing.T) {
	p := newRenewProvider("mk-y")
	p.mu.Lock()
	p.creds["mk-y"] = &Auth{UserID: "mk-y", Email: "a@b.c", Password: "p"}
	p.mu.Unlock()

	f := renewFactsOf(p, "mk-y")
	if !f.RenewedAt.IsZero() {
		t.Fatalf("从未续过时 RenewedAt 必须是零值，实得 %v", f.RenewedAt)
	}
}

// ── ⑨ 路由登记：POST + Hidden + 无能力位 ────────────────────────────

func TestRenewRouteRegistered(t *testing.T) {
	p := newRenewProvider("mk-1")
	var found int
	for _, r := range p.AdminRoutes() {
		if r.Path != adminRenewPath {
			continue
		}
		found++
		if r.Method != http.MethodPost {
			t.Fatalf("续期是写操作，必须是 POST，实得 %s", r.Method)
		}
		if !r.Hidden {
			t.Fatalf("纯动作端点必须 Hidden（别在左侧导航占标签页）")
		}
		if r.Capability != 0 {
			t.Fatalf("gateway 没有「续期」能力位，应当留 0，实得 %v", r.Capability)
		}
		if r.Handler == nil {
			t.Fatalf("Handler 不能为空")
		}
	}
	if found != 1 {
		t.Fatalf("续期路由应当恰好登记 1 条，实得 %d", found)
	}
}
