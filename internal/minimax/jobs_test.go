// jobs_test.go 后台续期扫描的行为守卫。
//
// # 为什么必须有行为断言（而不只是"实现了 JobExt"）
//
// `cmd/server/jobs_pairing_test.go` 守的是**结构**：能续期的上游必须实现
// JobExt、生产形态下 Jobs() 非空。那两条能挡住"完全没写"，但挡不住
// **扫描逻辑本身写错** —— 而扫描写错有好几种都"看起来在工作"：
//
//	每轮无脑全量刷    → 上游压力白增，且失败累计把健康账号禁用
//	判据比出站更严    → 后台永远不刷，等于没加这个任务（报障原样复发）
//	刷了磁盘副本      → 落盘已续期、界面仍显示已过期（本仓栽过多次）
//
// 所以这里用**假上游 + 调用计数**测行为：新鲜的不刷、到期的要刷、
// 刷不了的绝不发请求。
package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// refreshJobFixture 后台续期的测试夹具。
//
// 计数假上游收到的**续期请求次数** —— 这是"到底刷没刷"的唯一硬证据。
// 另外捕获标准日志，用于守"不刷屏"那一条 ——
// 那是"是否过滤掉刷不了的凭证"的**唯一**有判别力的观测面
// （请求数区分不出来，见 TestRefreshJobDoesNotLogSpamForUnrefreshable）。
type refreshJobFixture struct {
	p     *Provider
	calls *int64
	creds map[string]*Auth
	logs  *bytes.Buffer
}

// failureLogCount 本轮产生的日志里，"续期失败"相关的行数。
func (f *refreshJobFixture) failureLogCount() int {
	if f.logs == nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(line, "续期失败") || strings.Contains(line, "无法自动续期") {
			n++
		}
	}
	return n
}

// newRefreshJobFixture 造一个 provider + 假令牌端点。
//
// ⚠ 关键：`uidEnumerator` 与 `credSrc` 都要接线 —— 生产就是这么接的
// （见 cmd/server/main.go 的 SetCredentialSource / SetUIDEnumerator）。
// 只接一个的话，扫描要么列不出账号、要么拿不到池里的活对象，
// 而单测会"绿着通过"（这正是本仓反复出现的假绿形态）。
func newRefreshJobFixture(t *testing.T) *refreshJobFixture {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathToken {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&calls, 1)
		// ⚠ 字段必须齐全：parseTokenGrant 会校验
		// token_type=bearer、expires_in>0、scope 含 agent.default ——
		// 少一个就让"续期成功"变成解析失败，而症状（token 没写回）
		// 看起来像是"写回逻辑坏了"。我第一版就漏了这三个字段。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "mmoat_new",
			"refresh_token": "mmort_new",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         oauthScope,
		})
	}))
	t.Cleanup(srv.Close)

	// 捕获 log 输出（后台任务的失败日志走标准 log 包）。
	var logs bytes.Buffer
	prevWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevWriter) })

	f := &refreshJobFixture{calls: &calls, creds: map[string]*Auth{}, logs: &logs}
	f.p = New(Config{
		// 令牌端点指向假上游（与 refresh_test.go 同一手法）。
		AccountBase: srv.URL,
		APIBase:     srv.URL,
		AuthDir:     t.TempDir(),
	})
	f.p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		a, ok := f.creds[uid]
		if !ok {
			return gateway.Credential{}, false
		}
		return gateway.Credential{Provider: providerID, UID: uid, Secret: a}, true
	})
	f.p.SetUIDEnumerator(func() []string {
		out := make([]string, 0, len(f.creds))
		for uid := range f.creds {
			out = append(out, uid)
		}
		return out
	})
	return f
}

// addAccount 加一个"寿命 1 小时、还剩 remainSec 秒"的可续期账号。
func (f *refreshJobFixture) addAccount(uid string, remainSec int) {
	now := time.Now()
	f.creds[uid] = &Auth{
		AccessToken:  "mmoat_" + uid,
		RefreshToken: "mmort_" + uid,
		// 已发时刻 = 现在 - (1h - 剩余)，于是寿命恰好 1 小时。
		IssuedAt:  millisAt(now.Add(-time.Hour + time.Duration(remainSec)*time.Second)),
		ExpiresAt: millisAt(now.Add(time.Duration(remainSec) * time.Second)),
	}
}

// addAccountNoRefreshToken 加一个刷不了的账号（没有 refresh_token）。
func (f *refreshJobFixture) addAccountNoRefreshToken(uid string) {
	now := time.Now()
	f.creds[uid] = &Auth{
		AccessToken: "mmoat_" + uid,
		// RefreshToken 故意为空。
		IssuedAt:  millisAt(now.Add(-59 * time.Minute)),
		ExpiresAt: millisAt(now.Add(time.Minute)),
	}
}

// runOnce 跑一轮后台扫描，返回假上游收到的续期请求数。
func (f *refreshJobFixture) runOnce() int {
	before := atomic.LoadInt64(f.calls)
	if err := f.p.runRefresh(context.Background()); err != nil {
		panic("runRefresh 失败: " + err.Error())
	}
	n := int(atomic.LoadInt64(f.calls) - before)
	return n
}

// millisAt 把时刻编成**纯数字毫秒串**。
//
// ⚠ 必须是纯数字：`parseExpiryMillis` 照抄参照项目的 `/^\d+$/` 判据，
// 只认数字串（"带小数/带单位"会让换算结果错得离谱，所以直接判"读不到"）。
//
// 我第一版写成了 ISO 8601（`2026-10-14T…Z`）—— 结果是**夹具**把时间戳
// 解析成 0，于是判据走"过期时刻未知 ⇒ 不刷"的分支，测试红在"该刷却没刷"上。
// 那不是产品缺陷，是我造假数据时没对齐格式。记在这里因为这类错很容易
// 被误读成"实现有 bug"而改错地方。
func millisAt(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// ── 行为 ────────────────────────────────────────────────────────────────

// 新鲜的凭证**不该**被刷。
//
// 无脑每轮全量刷的代价：账号多时每 10 分钟一轮全量上游请求，
// 而且那些请求一旦失败会被记成"续期失败"、累计禁用**本来健康**的账号。
func TestRefreshJobSkipsFreshCredentials(t *testing.T) {
	f := newRefreshJobFixture(t)
	f.addAccount("u-fresh", 59*60) // 还剩 59 分钟，远没到"寿命一半"

	if n := f.runOnce(); n != 0 {
		t.Errorf("对一份刚发的凭证（还剩 59 分钟 / 寿命 1 小时），后台续期不该打上游（期望 0 次，实际 %d 次）",
			n)
	}
}

// ⚠ 核心：**临近过期**的凭证必须被刷。
//
// 这条直接对应本次报障（token 一直过期着没人管，而 refresh_token 还能用）。
func TestRefreshJobRefreshesDueCredentials(t *testing.T) {
	f := newRefreshJobFixture(t)
	// 寿命 1 小时、剩 1 分钟 ⇒ 早已进入"寿命一半"（=30m）的窗口。
	f.addAccount("u-due", 60)

	if n := f.runOnce(); n != 1 {
		t.Fatalf("对一份即将过期的凭证（剩 1 分钟 / 寿命 1 小时），后台续期**必须**刷（期望 1 次，实际 %d 次）——\n"+
			"不刷就回到本次报障的形态：token 过期几小时没人管，"+
			"而 refresh_token 明明还能用", n)
	}
	// 续期必须**原地写回池里那个对象**（否则界面读到的还是旧 token）。
	got := f.creds["u-due"]
	if got.AccessToken != "mmoat_new" {
		t.Errorf("续期后池里那个 *Auth 的 AccessToken = %q，期望 mmoat_new ——\n"+
			"没写回的话就是本仓栽过多次的形态：落盘已续期、界面仍显示已过期", got.AccessToken)
	}
	if got.RefreshToken != "mmort_new" {
		t.Errorf("refresh_token 轮换后没写回: %q", got.RefreshToken)
	}
}

// 刷不了的凭证（没有 refresh_token）**一次上游请求都不该发**。
//
// # ⚠ 这条的实际收益比我一开始写的小（已查证，写准）
//
// 我原先的理由是"不过滤会反复打上游 + 失败累计禁用账号"。查证后两条都不对：
//
//	· RefreshCredential 在 !Refreshable() 时提前返回，本就不发 HTTP
//	· 禁用计数只在请求路径记（handler.go），后台任务不碰池子
//
// 所以这条过滤的真实收益是"不让日志被刷屏"（每个刷不了的号每 10 分钟
// 一条"续期失败"，永久如此）以及"把「刷不了」和「刷失败」在代码里分开"。
//
// 也正因为真实差异很小，**这条测试对"去掉过滤"是等价变异** ——
// 去掉后 requests 仍然是 0（上游压根没被调），它照样绿。
// 我在变异验证里见过这个现象，当时的判断是"测试判别力不够"；
// 实际是**变异不改变可观测行为**。这两者要分清，否则会去改一个没问题的测试。
func TestRefreshJobSkipsUnrefreshableCredentials(t *testing.T) {
	f := newRefreshJobFixture(t)
	f.addAccountNoRefreshToken("u-norefresh")

	if n := f.runOnce(); n != 0 {
		t.Errorf("没有 refresh_token 的凭证不该被刷（期望 0 次上游请求，实际 %d 次）", n)
	}
}

// ⚠ 上一条守不住的那个性质，用**真正有判别力**的判据守：**日志不刷屏**。
//
// # 为什么换判据（这是变异验证逼出来的）
//
// 去掉 `if !a.Refreshable()` 那个 continue 之后，上一条测试**仍然绿** ——
// 因为上游压根不会被调用（requests 恒 0）。所以用"请求数"根本区分不出
// 有没有那条过滤。
//
// 而那条过滤真实作用在**日志**上：没有它，每个刷不了的号每轮都会走进
// 失败分支打一条日志。所以判据要换成"**失败日志的条数**"：
//
//	有过滤 → 刷不了的号不打失败日志
//	没过滤 → 每个刷不了的号各打一条
//
// 这才是有判别力的判据。（"永远会失败的重试"最消耗排查者的注意力，
// 所以这条日志量本身是值得守的。）
func TestRefreshJobDoesNotLogSpamForUnrefreshable(t *testing.T) {
	f := newRefreshJobFixture(t)
	// 多个刷不了的号：没过滤的话每个都会打一条失败日志。
	f.addAccountNoRefreshToken("u-x1")
	f.addAccountNoRefreshToken("u-x2")
	f.addAccountNoRefreshToken("u-x3")

	f.runOnce() // 忽略请求数（上一条已断言为 0）

	if n := f.failureLogCount(); n != 0 {
		t.Errorf("刷不了的凭证不该产生失败日志（期望 0 条，实际 %d 条）——\n"+
			"它们每 10 分钟一轮、永久如此，而永远不会成功；"+
			"这种日志会把真正需要看的失败淹掉", n)
	}
}

// 混合场景：一轮里该刷的刷、不该刷的不碰。
//
// 分开的两条用例各自只覆盖一种输入，而真实的账号表里两种混在一起 ——
// 这条防的是"判据写成了'只要有一个该刷就全刷'或反之"。
func TestRefreshJobHandlesMixedSet(t *testing.T) {
	f := newRefreshJobFixture(t)
	f.addAccount("u-due", 60)         // 该刷
	f.addAccount("u-fresh", 59*60)    // 不该刷
	f.addAccountNoRefreshToken("u-x") // 刷不了

	if n := f.runOnce(); n != 1 {
		t.Errorf("混合场景下只该刷那 1 个到期的（期望 1 次，实际 %d 次）——\n"+
			"多了说明判据太松（新鲜的和刷不了的都被刷了）；"+
			"少了说明判据太严（到期的没被刷，报障原样复发）", n)
	}
}

// 不知道过期时刻的凭证：**不刷**（而不是每轮都刷）。
//
// 判据要是写成"过期时刻缺失 ⇒ 视为该刷"，那这份凭证会**每一轮**都刷一次
// （因为"未知"永远成立），且每次都可能失败。
func TestRefreshJobSkipsUnknownExpiry(t *testing.T) {
	f := newRefreshJobFixture(t)
	f.creds["u-noexp"] = &Auth{
		AccessToken:  "mmoat_u-noexp",
		RefreshToken: "mmort_u-noexp",
		// ExpiresAt 故意为空。
	}
	if n := f.runOnce(); n != 0 {
		t.Errorf("过期时刻未知的凭证不该每轮都刷（期望 0 次，实际 %d 次）——\n"+
			"「未知」是恒成立的，用它当判据会让这份凭证每轮刷一次、且每次都失败", n)
	}
}

// 目录为空 ⇒ 不注册任务（不起空转的定时器）。
func TestJobsSkippedWithoutAuthDir(t *testing.T) {
	p := New(Config{})
	if jobs := p.Jobs(); len(jobs) != 0 {
		t.Errorf("没有凭证目录时不该注册后台任务，实际 %v", jobs)
	}
}

// 生产形态（有凭证目录）⇒ 注册任务，且形状正确。
func TestJobsShapeInProductionForm(t *testing.T) {
	p := New(Config{AuthDir: t.TempDir()})
	jobs := p.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("应当注册 1 个后台续期任务，实际 %d 个", len(jobs))
	}
	j := jobs[0]
	if j.Name != JobRefresh {
		t.Errorf("任务名 = %q，期望 %q（控制台的定时任务列表按它展示）", j.Name, JobRefresh)
	}
	if j.Interval <= 0 {
		t.Errorf("Interval = %v，必须为正", j.Interval)
	}
	if j.Run == nil {
		t.Error("Run 为 nil —— 任务会被注册但什么也不做")
	}
}
