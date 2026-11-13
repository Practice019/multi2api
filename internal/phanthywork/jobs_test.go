// jobs_test.go 后台续期任务的断言（gateway.JobExt）。
//
// 本仓这条不变量来自三次用户报障（cline → raccoon → minimax）：
// **能续期 ⇒ 必须有后台续期任务**。所以这里断言的不只是"任务注册了"，
// 而是它**真的会刷、且只在该刷的时候刷**。
//
// 三条硬要求（用户明确指定，逐条对应下面的用例）：
//
//	不过 RefreshSkew ⇒ **零请求**（"能只读就只读"的落点）
//	过了 ⇒ **恰好一次**（不多不少）
//	同账号并发 ⇒ **恰好一次**（复用包内刷新闸，坑⑤）
package phanthywork

import (
	"context"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// authWithRemaining 造一份"剩余寿命 = remainingSec"的凭证。
//
// 人生成 liveness 1800 秒（= 报告实测的 exp-iat），于是 RefreshSkew 的窗口
// 恰好是 900 秒 —— 下面用"剩余 >900s ⇒ 不该刷 / ≤900s ⇒ 该刷"来分档。
func authWithRemaining(uid, refreshToken string, remainingSec int64) *Auth {
	now := nowMS() / 1000
	exp := now + remainingSec
	iat := exp - 1800
	a := newTestAuth(uid, refreshToken)
	a.OAuth.AccessToken = jwtWithTimes(uid, iat, exp)
	a.OAuth.ExpiresAtMS = exp * 1000
	return a
}

// jwtWithTimes 造一个带指定 iat/exp 的假 JWT（只需本地可解析）。
func jwtWithTimes(sub string, iat, exp int64) string {
	hdr := b64url([]byte(`{"alg":"RS256","kid":"configured-key"}`))
	pl := b64url([]byte(`{"sub":"` + sub + `","iat":` + msToString(iat) + `,"exp":` + msToString(exp) + `}`))
	return hdr + "." + pl + ".sig"
}

// jobsFixture 起一个"生产形态"的 Provider（有 authDir）+ 假上游。
func jobsFixture(t *testing.T) (*Provider, *fakeUpstream) {
	t.Helper()
	srv, fake := startFakeUpstream(t)
	p := NewWithClient(NewWithBase(srv.URL), t.TempDir(), 0)
	return p, fake
}

// wireAccounts 把账号接进后台任务的扫描范围（**两条注入都要**，与生产一致）。
//
// ⚠ 注入源必须读**它自己维护的那张表**，不能回调 p.cred —— 后者会再问一次注入源，
// 于是无限递归（我第一版就是这么写的，栈直接爆掉；这条注释就是那次事故的记录）。
// 生产里也是一样的形状：注入源问的是**池子**，而池子与 Provider 是两条独立的持有链。
func wireAccounts(p *Provider, accounts ...*Auth) {
	m := make(map[string]*Auth, len(accounts))
	uids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		m[a.UID()] = a
		uids = append(uids, a.UID())
	}
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		a := m[uid]
		if a == nil {
			return gateway.Credential{}, false
		}
		return gateway.Credential{Provider: providerID, UID: uid, Secret: a}, true
	})
	p.SetUIDEnumerator(func() []string { return uids })
}

// TestJobsRequiresAuthDir 两个方向都守：没凭证目录不起任务、有则必须起。
//
// ⚠ 只守"必须有"会让人把判据写成恒真，那样"未配置的上游也在空转打上游"
// 就没人发现了 —— 这条守卫的两个方向在本仓是成对的。
func TestJobsRequiresAuthDir(t *testing.T) {
	empty := NewProvider(Config{})
	if jobs := empty.Jobs(); len(jobs) != 0 {
		t.Errorf("没有凭证目录时不该注册后台任务（没有账号要刷），实际 %v", jobNamesOf(jobs))
	}
	p := NewProvider(Config{AuthDir: t.TempDir()})
	jobs := p.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("生产形态（authDir 非空）下应注册 1 个后台续期任务，实际 %v", jobNamesOf(jobs))
	}
	if jobs[0].Name != JobRefresh {
		t.Errorf("任务名 = %q, want %q", jobs[0].Name, JobRefresh)
	}
	if jobs[0].Interval <= 0 {
		t.Errorf("Interval = %v，必须为正", jobs[0].Interval)
	}
	if jobs[0].Run == nil {
		t.Error("Run 为 nil —— 任务会被注册但点了什么也不做")
	}
}

// TestJobExtIsDiscoverableAndCapsUnchanged 扩展点可发现 + 能力位**没有**被顺手加。
func TestJobExtIsDiscoverableAndCapsUnchanged(t *testing.T) {
	p := NewProvider(Config{AuthDir: t.TempDir()})
	if _, ok := gateway.ExtOf[gateway.JobExt](p); !ok {
		t.Fatal("Provider 必须能被 ExtOf[JobExt] 发现（访问器形态发现不了）")
	}
	// JobExt **不是**能力位：声明它不该改变 Caps。
	//
	// ⚠ 能力位是**声明即承诺**：这里的期望值是"恰好这几个"而不是"包含这几个"——
	// 多报一个会被契约测试当场抓住，而"少报"只是界面少个入口（安全的那个方向）。
	want := gateway.CapChat | gateway.CapModels | gateway.CapQuotaProbe |
		gateway.CapCheckin | gateway.CapImport | gateway.CapInvite
	if got := p.Caps(); got != want {
		t.Errorf("Caps = %v，want %v（job 不是能力位；新增能力位各自有实现与断言）", got.Names(), want.Names())
	}
}

// TestJobIntervalIsShorterThanRefreshPoint 扫描间隔必须**明显短于**该刷点。
//
// ⚠ 这是"后台任务看起来在跑、实际永远观察到已过期"的唯一防线：
// access token 寿命 1800 秒 ⇒ 该刷点在剩 900 秒；间隔若 ≥ 900 秒，
// 一个在两次扫描之间变"该刷"的账号会直接过期。
func TestJobIntervalIsShorterThanRefreshPoint(t *testing.T) {
	p := NewProvider(Config{AuthDir: t.TempDir()})
	iv := p.Jobs()[0].Interval
	refreshPoint := AccessTokenTTL / 2 // 900s：gateway 的统一策略「剩余 ≤ 寿命一半」
	if iv >= refreshPoint {
		t.Fatalf("扫描间隔 %v ≥ 该刷点 %v ⇒ 会观察到「账号已过期」而任务一切正常\n"+
			"（access token 寿命 %v，该刷点是它的寿命一半）", iv, refreshPoint, AccessTokenTTL)
	}
	// 余量：至少要在"剩下不到该刷点时"还能赶上一轮（这里要求 ≥ 三分之一余量）
	if iv > refreshPoint*2/3 {
		t.Errorf("扫描间隔 %v 吃掉了太多余量（该刷点 %v）—— 失败后没有重试机会", iv, refreshPoint)
	}
	if iv < time.Minute {
		t.Errorf("扫描间隔 %v 过密：上游每刷一次就轮换一次 refreshToken（坑⑤）", iv)
	}
}

// TestRunRefreshZeroRequestsWhenSkewNotMet 不过 skew ⇒ **零请求**。
//
// 这是「能只读就只读」（报告 §7.1 第 2 条 / 坑⑤ 的对策）在后台任务上的落点：
// 后台任务存在的意义是"迟早会刷"，不是"每轮都刷"。
func TestRunRefreshZeroRequestsWhenSkewNotMet(t *testing.T) {
	defer swapSpacing(time.Duration(0))()
	p, fake := jobsFixture(t)
	// 剩余 1700 秒 > 该刷点 900 秒 ⇒ 不该刷
	a := authWithRemaining("u-fresh", "refresh-old", 1700)
	wireAccounts(p, a)

	if err := p.Jobs()[0].Run(context.Background()); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if n := fake.pathCount(pathOAuthToken); n != 0 {
		t.Errorf("不过 skew 时必须**零请求**（能只读就只读），实际打了 %d 次续期", n)
	}
	if a.OAuth.RefreshToken != "refresh-old" {
		t.Errorf("不该动 refreshToken，得到 %q", a.OAuth.RefreshToken)
	}
}

// TestRunRefreshExactlyOnceWhenSkewMet 过了 skew ⇒ **恰好一次**，且刷的是池里那个对象。
func TestRunRefreshExactlyOnceWhenSkewMet(t *testing.T) {
	defer swapSpacing(time.Duration(0))()
	p, fake := jobsFixture(t)
	// 剩余 800 秒 ≤ 900 秒 ⇒ 该刷（且还没过期）
	a := authWithRemaining("u-due", "refresh-old", 800)
	wireAccounts(p, a)

	if err := p.Jobs()[0].Run(context.Background()); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if n := fake.pathCount(pathOAuthToken); n != 1 {
		t.Errorf("过 skew 后应**恰好**刷 1 次，实际 %d 次", n)
	}
	// ★ 必须刷的是池里那个 *Auth（不是磁盘副本）：断言这个**同一个指针**被更新了
	if a.OAuth.RefreshToken != "refresh-rotated" {
		t.Errorf("池里那个 *Auth 的 refreshToken 未被更新（得到 %q）——\n"+
			"说明刷的是另一个对象（磁盘副本），界面会继续显示已过期", a.OAuth.RefreshToken)
	}
	// 刷完就不再该刷：第二轮应当零请求
	if err := p.Jobs()[0].Run(context.Background()); err != nil {
		t.Fatalf("第二轮 Run 报错: %v", err)
	}
	if n := fake.pathCount(pathOAuthToken); n != 1 {
		t.Errorf("刷完就新鲜了，第二轮不该再刷（共 %d 次）", n)
	}
}

// TestRunRefreshConcurrentSameAccountExactlyOnce 同账号并发 ⇒ **恰好一次**。
//
// 这条守的是坑⑤ 的实质风险：refreshToken 轮换 ⇒ 第二路刷新拿着已作废的旧 token
// 去换 ⇒ "登录莫名失效"。并发去重由包内 refreshGate 提供（本文件不再加锁）。
//
// 假上游刻意把 token 端点**拖慢**（tokenDelay）：不拖慢的话 8 个 goroutine
// 会天然串行，用例就变成了"永远绿"的样子。
func TestRunRefreshConcurrentSameAccountExactlyOnce(t *testing.T) {
	defer swapSpacing(time.Duration(0))()
	p, fake := jobsFixture(t)
	fake.tokenDelay = 200 * time.Millisecond
	var accounts []*Auth
	for i := 0; i < 8; i++ {
		// 8 份**同一个 uid** 的凭证对象？不 —— 那样测的不是"同账号并发"。
		// 这里用同 uid 的**同一个对象**被 8 个 Run 同时看到（生产里就是池子里
		// 那一个 *Auth），所以只登记一份。
		accounts = append(accounts, authWithRemaining("u-race", "refresh-old", 800))
		break
	}
	wireAccounts(p, accounts...)
	run := p.Jobs()[0].Run

	start := make(chan struct{})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			errs <- run(context.Background())
		}()
	}
	close(start)
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Errorf("并发 Run 报错: %v", err)
		}
	}
	if n := fake.pathCount(pathOAuthToken); n != 1 {
		t.Errorf("同账号并发续期必须只打 1 次 token 端点（坑⑤：轮换会互踢），实际 %d 次", n)
	}
}

// TestRunRefreshFirstRunDoesNotStampede 首轮**不许**一次性刷完所有账号。
//
// refreshToken 轮换 ⇒ 启动即刷全池等于"启动即把所有桌面端踢下线"，
// 而且服务端看到的是同一秒里的 N 个 token 请求（那正是风控的形状）。
func TestRunRefreshFirstRunDoesNotStampede(t *testing.T) {
	defer swapSpacing(time.Duration(0))()
	p, fake := jobsFixture(t)
	// 三个都该刷（剩 700/600/500 秒），首轮只准刷 1 个
	a1 := authWithRemaining("u-1", "rt-1", 700)
	a2 := authWithRemaining("u-2", "rt-2", 600)
	a3 := authWithRemaining("u-3", "rt-3", 500)
	wireAccounts(p, a1, a2, a3)
	run := p.Jobs()[0].Run

	if err := run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fake.pathCount(pathOAuthToken); n != firstRunRefreshLimit {
		t.Fatalf("首轮应只刷 %d 个账号（避免刷新风暴），实际 %d 次", firstRunRefreshLimit, n)
	}
	// 首轮该刷的是**最急的那个**（剩 500 秒 = u-3）—— 这条断言把
	// "限额取的是排序后的第一个"钉住（否则限额可能随机刷掉一个不急的）。
	if a3.OAuth.RefreshToken == "rt-3" {
		t.Error("首轮刷的应当是最急的那个（剩 500 秒 = u-3），但它没被刷")
	}
	// 另外两个必须**原样不动**（首轮限额 = 1）
	if a1.OAuth.RefreshToken != "rt-1" || a2.OAuth.RefreshToken != "rt-2" {
		t.Errorf("首轮不该动其余账号：a1=%q a2=%q", a1.OAuth.RefreshToken, a2.OAuth.RefreshToken)
	}
	// 第二轮放开限额：剩下的两个应当被刷掉
	if err := run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fake.pathCount(pathOAuthToken); n != 3 {
		t.Errorf("第二轮应把剩下的也刷掉（共 3 次），实际 %d 次", n)
	}
	for _, a := range []*Auth{a1, a2, a3} {
		if a.OAuth.RefreshToken != "refresh-rotated" {
			t.Errorf("uid=%s 的 refreshToken 未被更新：%q", a.UID(), a.OAuth.RefreshToken)
		}
	}
}

// TestRunRefreshSkipsAccountsWithoutRefreshToken 没有 refreshToken 的号跳过且**零请求**。
func TestRunRefreshSkipsAccountsWithoutRefreshToken(t *testing.T) {
	defer swapSpacing(time.Duration(0))()
	p, fake := jobsFixture(t)
	a := authWithRemaining("u-no-rt", "", 100) // 已到期也刷不了：没有 refreshToken
	wireAccounts(p, a)
	if err := p.Jobs()[0].Run(context.Background()); err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if n := fake.pathCount(pathOAuthToken); n != 0 {
		t.Errorf("没有 refreshToken 的号不该打续期端点，实际 %d 次", n)
	}
}

// TestRunRefreshRespectsContextCancel 取消后立刻返回、且不发请求。
func TestRunRefreshRespectsContextCancel(t *testing.T) {
	defer swapSpacing(time.Duration(0))()
	p, fake := jobsFixture(t)
	wireAccounts(p, authWithRemaining("u-1", "rt-1", 700))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Jobs()[0].Run(ctx); err == nil {
		t.Error("已取消的 ctx 上 Run 应返回错误")
	}
	if n := fake.pathCount(pathOAuthToken); n != 0 {
		t.Errorf("取消后不该发任何请求，实际 %d 次", n)
	}
}

// TestRunRefreshNoAccountsIsQuiet 没有账号时静默返回（不报错、不打请求）。
func TestRunRefreshNoAccountsIsQuiet(t *testing.T) {
	p, fake := jobsFixture(t)
	if err := p.Jobs()[0].Run(context.Background()); err != nil {
		t.Fatalf("没有账号时 Run 不该报错: %v", err)
	}
	if n := len(fake.pathsSnapshot()); n != 0 {
		t.Errorf("没有账号时不该发任何请求，实际 %d 次: %v", n, fake.pathsSnapshot())
	}
}

// TestRunRefreshUsesInjectedPoolObject 注入的凭证源优先于本地表（生产路径）。
//
// ⚠ 反过来（先读本地表）会让生产永远读到空 map —— 那是 zcode 上踩过的坑：
// 端点返回 200 + 空账号列表，看起来像"没有账号"。
func TestRunRefreshUsesInjectedPoolObject(t *testing.T) {
	defer swapSpacing(time.Duration(0))()
	p, fake := jobsFixture(t)
	poolObj := authWithRemaining("u-pool", "pool-rt", 700)
	localObj := authWithRemaining("u-pool", "local-rt", 700)
	p.SetLocalCredential("u-pool", localObj)
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		if uid != "u-pool" {
			return gateway.Credential{}, false
		}
		return gateway.Credential{Provider: providerID, UID: uid, Secret: poolObj}, true
	})
	p.SetUIDEnumerator(func() []string { return []string{"u-pool"} })

	if err := p.Jobs()[0].Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.pathCount(pathOAuthToken) != 1 {
		t.Fatalf("该刷的号应当被刷一次，实际 %d 次", fake.pathCount(pathOAuthToken))
	}
	if poolObj.OAuth.RefreshToken != "refresh-rotated" {
		t.Error("必须刷**注入源给的**那个对象（池里的活 secret）")
	}
	if localObj.OAuth.RefreshToken != "local-rt" {
		t.Error("本地表那份不该被动（它不是池里持有的对象）")
	}
}

// TestNeedsRefreshFollowsRefreshSkew needsRefresh 与出站路径**同源**。
//
// 判据必须是 RefreshSkew（"剩余 ≤ 寿命一半"），不是另写的固定窗口：
// 比出站更严 ⇒ 后台永远不刷（报障复发）；更松 ⇒ 白刷（多一次互踢机会）。
func TestNeedsRefreshFollowsRefreshSkew(t *testing.T) {
	p := NewProvider(Config{AuthDir: t.TempDir()})
	now := time.Now()
	// 剩余 1700s（寿命 1800s ⇒ 该刷点 900s）⇒ 不该刷
	if p.needsRefresh(authWithRemaining("u-1", "rt", 1700), now) {
		t.Error("剩余 1700 秒（> 900 秒该刷点）不该判为需要续期")
	}
	// 剩余 800s ⇒ 该刷
	if !p.needsRefresh(authWithRemaining("u-2", "rt", 800), now) {
		t.Error("剩余 800 秒（≤ 900 秒该刷点）必须判为需要续期")
	}
	// 拿不到过期时刻 ⇒ 不刷（否则"未知"永远成立 ⇒ 每轮都刷）
	noExp := newTestAuth("u-3", "rt")
	noExp.OAuth.ExpiresAtMS = 0
	noExp.OAuth.AccessToken = "not-a-jwt"
	if p.needsRefresh(noExp, now) {
		t.Error("读不到过期时刻时不该刷（那会变成每轮都刷）")
	}
}

// authWithRemainingLifetime 造"寿命 = lifetimeSec、剩余 = remainingSec"的凭证。
//
// ⚠ 为什么需要它（2026-11-13 T-913）：`authWithRemaining` 固定寿命 1800 秒，
//
//	于是"提前量 = 寿命一半 = 900 秒"总是**大于**扫描周期 —— 那条路径一直是对的。
//	而用户报障里 4 个号掉到 1 分钟，说明真机上有**寿命更短**的令牌：
//	寿命 ≤ 2×扫描周期时，"一半"就短于"扫描一圈"⇒ 漏一轮就过期。
func authWithRemainingLifetime(uid, refreshToken string, remainingSec, lifetimeSec int64) *Auth {
	now := nowMS() / 1000
	exp := now + remainingSec
	iat := exp - lifetimeSec
	a := newTestAuth(uid, refreshToken)
	a.OAuth.AccessToken = jwtWithTimes(uid, iat, exp)
	a.OAuth.ExpiresAtMS = exp * 1000
	return a
}

// outboundWouldRefresh 出站路径的判据（`remaining ≤ RefreshSkew`，**不带**扫描地板）。
//
// ⚠ 它是核心那条路的忠实替身：核心拿 RefreshSkewExt 的返回值比剩余寿命。
func outboundWouldRefresh(t *testing.T, p *Provider, a *Auth, now time.Time) bool {
	t.Helper()
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}
	skew, ok := p.RefreshSkew(cred)
	if !ok || skew <= 0 {
		return false
	}
	exp, has := a.ExpiresAt()
	if !has || exp <= 0 {
		return false
	}
	return exp*1000-now.UnixMilli() <= skew.Milliseconds()
}

// TestNeedsRefreshLeadCoversScanInterval ★ ① 续期提前量必须**大于扫描周期**。
//
// 用户报障（截图）：4 个号掉到 1 分钟 / 13 分钟 / 1 分钟 / 22 分钟。
// 根因：判据是"剩余 ≤ 寿命一半"，而扫描**每 10 分钟才一次** ⇒
//
//	寿命短于 2×扫描周期的令牌，一半的提前量**小于**扫描一圈 ⇒ 跨过阈值后要等下一轮，
//	漏一轮就掉到 1 分钟 ⇒ 在下次扫描前已经过期。
//
// 判据：提前量 = max(寿命一半, 扫描周期 + 缓冲)。
func TestNeedsRefreshLeadCoversScanInterval(t *testing.T) {
	p := NewProvider(Config{AuthDir: t.TempDir()})
	now := time.Now()

	// 组①：长寿命（1800 秒 ⇒ 一半 900 秒 > 地板）⇒ 行为**不变**：剩余 900+30 不该刷。
	if p.needsRefresh(authWithRemaining("u-long", "rt", 930), now) {
		t.Error("长寿命令牌剩余 930 秒（> 一半 900）不该刷 —— 长寿命那档行为不许变")
	}
	// 组②：**寿命较短**（1200 秒 ⇒ 一半只有 600 秒 < 地板 720 秒）⇒ 剩余 700 秒**必须**刷。
	//    ⚠ 物理约束：剩余**不可能**超过寿命 ⇒ 用例必须取 剩余 ≤ 寿命
	//      （我第一版取 700/600 是假的，它把"地板"误判成了 bug）。
	//    （旧判据：700 > 600（一半）⇒ 不刷 ⇒ 跨过阈值后要等下一圈 ⇒ 掉到 1 分钟就是这么来的）
	if !p.needsRefresh(authWithRemainingLifetime("u-short", "rt", 700, 1200), now) {
		t.Error("★ 剩余 700 秒已在「扫描一圈 + 缓冲」（720 秒）之内，必须判为需要续期" +
			"（旧判据按寿命一半 600 秒判 ⇒ 不刷 ⇒ 漏一轮就过期）")
	}
	// 组③：寿命虽短但剩余已宽于地板 ⇒ 不刷（不许把提前量放大成"永远刷"）。
	if p.needsRefresh(authWithRemainingLifetime("u-short2", "rt", 900, 1200), now) {
		t.Error("剩余 900 秒（> 地板 720 秒）时不该刷")
	}
	// 组④：**寿命短于地板**（600 秒 < 720 秒）⇒ 出站与后台**都**会判要刷，这是**结构必然**：
	//    令牌活不过一圈 ⇒ 每圈都得续。⚠ 这一条是**如实记录**，不是断言它"优雅"。
	if !p.needsRefresh(authWithRemainingLifetime("u-tiny", "rt", 590, 600), now) {
		t.Error("寿命 600 秒（< 地板 720 秒）的令牌：剩余 590 秒必然要刷（它活不过下一圈）")
	}
}

// TestNeedsRefreshNeverLagsOutbound ★ ② 同源性：后台**绝不比出站更晚**。
//
// 两处判据打架的两种后果（见 needsRefresh 的注释）：更严 ⇒ 后台永不刷；更松 ⇒ 白刷。
// 本轮刻意让后台**可以更早**（要容忍漏轮），但**绝不能更晚** —— 这条把它钉住。
func TestNeedsRefreshNeverLagsOutbound(t *testing.T) {
	p := NewProvider(Config{AuthDir: t.TempDir()})
	now := time.Now()
	for _, c := range []struct {
		name      string
		remaining int64
		lifetime  int64
	}{
		{"长寿命·很近", 100, 1800},
		{"长寿命·临界内", 800, 1800},
		{"长寿命·临境外", 1000, 1800},
		{"长寿命·满", 1700, 1800},
		{"短寿命·很近", 100, 600},
		{"短寿命·临界内", 280, 600},
		{"短寿命·地板内", 700, 600},
		{"短寿命·地板外", 900, 600},
	} {
		a := authWithRemainingLifetime("u-"+c.name, "rt", c.remaining, c.lifetime)
		out := outboundWouldRefresh(t, p, a, now)
		scan := p.needsRefresh(a, now)
		if out && !scan {
			t.Errorf("%s（剩余 %ds / 寿命 %ds）：出站会刷而后台不刷 ⇒ 后台**落后于**出站（两处判据打架）",
				c.name, c.remaining, c.lifetime)
		}
		if out != scan && !out && scan {
			// 后台更早是**刻意**的（要容忍漏轮），但只在"短于扫描地板"的那档允许。
			d := time.Duration(c.remaining) * time.Second
			if c.lifetime/2 >= int64((defaultRefreshScanInterval+refreshScanBuffer)/time.Second) {
				t.Errorf("%s：长寿命令牌不该出现「后台更早」（提前量已由一半覆盖）", c.name)
			}
			_ = d
		}
	}
}

// TestNeedsRefreshDoesNotFireNearFullLifetime ★ ③ 接近满寿命 ⇒ 不刷（防提前量放大成永远续期）。
func TestNeedsRefreshDoesNotFireNearFullLifetime(t *testing.T) {
	p := NewProvider(Config{AuthDir: t.TempDir()})
	now := time.Now()
	// ⚠ 只对**寿命 ≥ 地板（720 秒）**的令牌断言"刚发下来不刷" —— 唯一自洽的口径：
	//   寿命 < 地板 的令牌活不过一圈 ⇒ 它必须每圈刷（见上一条组④）。
	// ⚠ 取 lt ≥ 900："刚发下来 10 秒"的剩余是 lt−10，必须**严格大于**地板 720 秒
	//   才算"接近满寿命"（lt=720 时剩余 710 < 720 ⇒ 那时判"要刷"是**对的**，不是 bug）。
	for _, lt := range []int64{900, 1200, 1800, 3600} {
		a := authWithRemainingLifetime("u-full", "rt", lt-10, lt) // 刚发下来 10 秒
		if p.needsRefresh(a, now) {
			t.Errorf("寿命 %ds 的令牌刚发下来（剩余 %ds）就判要刷 ⇒ 会变成每轮都刷（打爆上游 + 与桌面端互踢）",
				lt, lt-10)
		}
	}
}

// TestRefreshSkewWindowIsHalfOfLifetime 顺带把 900 秒这个前提钉住。
//
// 上面几条用例都建立在「寿命 1800 秒 ⇒ 该刷点 900 秒」之上；
// 若哪天 RefreshSkew 的策略变了，这里先红，而不是让那几条静默失去意义。
func TestRefreshSkewWindowIsHalfOfLifetime(t *testing.T) {
	p := NewProvider(Config{AuthDir: t.TempDir()})
	a := authWithRemaining("u-1", "rt", 1700)
	skew, ok := p.RefreshSkew(gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a})
	if !ok {
		t.Fatal("有 iat/exp 且有 refreshToken 时必须给出窗口")
	}
	if want := AccessTokenTTL / 2; skew != want {
		t.Fatalf("窗口 = %v, want %v（寿命 %v 的一半）—— 上面几条用例的前提失效", skew, want, AccessTokenTTL)
	}
}

// jobNamesOf 取任务名列表（失败信息里用）。
func jobNamesOf(jobs []gateway.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

// swapSpacing 临时改刷新间隔（返回还原函数）。
//
// 抽成显式的一对调用而不是 defer 匿名函数，是为了让"改了什么、什么时候还"
// 在用例里一眼可见 —— 全局可变状态需要这种可见性。
func swapSpacing(d time.Duration) func() {
	old := refreshSpacing
	refreshSpacing = d
	return func() { refreshSpacing = old }
}

var _ = strings.TrimSpace // 保留 strings（错误文案里可能用到）
