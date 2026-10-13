// candidate_tier_warmup_test.go 后台档位预热的守卫。
//
// # 用户实测到的那个 -30 永久积分的根因
//
// 档位缓存是**纯内存**的，重启后是空的。空缓存里每个账号都是「不确定」档，
// 而"不确定"之间**没有优先级** —— 选号退化成加权抽签。
//
// 用户 10-13 14:22 重启过实例，之后 22 小时里唯一一次计费记在了那唯一一个
// 免费额度为 0 的号上（-30 永久积分），而另外 21 个号都还有免费额度。
//
// 与"第一次选号时顺带异步刷新"的区别是关键：
//
//	那个是**请求驱动**的：第一次抽签已经发生了才去刷新 ⇒ 第一次必然没有偏好
//	预热是**启动驱动**的：第一个请求到达之前就把档位准备好
//
// 所以这几条测的是"预热能不能在没有请求的情况下把档位准备好"。
package loomy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// warmupFixture 造一个 loomy provider + 假上游，返回 (provider, 查询计数)。
//
// 假上游按 session 给不同余额：
//
//	free  → 还有今日赠送（0 档）
//	paid  → 只剩永久额度（2 档）
//	dead  → 401（模拟 session 失效）
func warmupFixture(t *testing.T) (*Provider, *int64) {
	t.Helper()
	var queries int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/points/records" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&queries, 1)
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("token") {
		case sessFree:
			io.WriteString(w, `{"code":"000000","desc":"ok","data":{"balance":1000,"dailyBalance":5000,"availableBalance":6000}}`)
		case sessPaid:
			io.WriteString(w, `{"code":"000000","desc":"ok","data":{"balance":20000,"dailyBalance":0,"availableBalance":20000}}`)
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	writeFile(t, dir, "loomy-free.json", `{"session":"`+sessFree+`","userid":"`+uidFree+`"}`)
	writeFile(t, dir, "loomy-paid.json", `{"session":"`+sessPaid+`","userid":"`+uidPaid+`"}`)
	writeFile(t, dir, "loomy-dead.json", `{"session":"dead0000000000000000000000000000","userid":"`+uidDead+`"}`)

	return NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir}), &queries
}

const (
	sessFree = "aaaa0000000000000000000000000000"
	sessPaid = "bbbb0000000000000000000000000000"
	uidFree  = "260101010101010101"
	uidPaid  = "260202020202020202"
	uidDead  = "260303030303030303"
)

// ⚠ 核心判据：**没有任何请求**的时候，预热也要把档位准备好。
//
// 这条直接对应用户那个 -30：重启后缓存是空的，第一个请求到来时
// `CandidateTiers` 只能报「不确定」，于是那次抽签退化成随机。
// 预热必须在第一个请求之前就把 `tierFree` / `tierPaid` 写进缓存。
func TestWarmupFillsTiersWithoutAnyRequest(t *testing.T) {
	p, _ := warmupFixture(t)

	// 预热前的真实状态：缓存空 ⇒ 一切「不确定」。
	// 断言它，是为了说明"不预热就会发生什么"，而不是假设。
	if got := p.CandidateTiers("", []string{uidFree, uidPaid})[uidFree]; got != tierUnsure {
		t.Fatalf("前置不成立：预热前应报不确定，实际 %d", got)
	}

	p.warmupPass()

	free, okF := p.tiers.get(uidFree)
	paid, okP := p.tiers.get(uidPaid)
	if !okF || free.tier != tierFree {
		t.Errorf("预热后「还有免费额度」的号应落 0 档，实际 ok=%v tier=%d ——\n"+
			"没落上的话，重启后的第一次抽签就没有偏好，那个只剩永久额度的号"+
			"仍会被 1/N 的概率抽中、白烧积分（用户实测报了 -30）", okF, free.tier)
	}
	if !okP || paid.tier != tierPaid {
		t.Errorf("预热后「只剩永久额度」的号应落 2 档，实际 ok=%v tier=%d", okP, paid.tier)
	}

	// 预热之后，第一个请求就能拿到**有偏好**的档位。
	got := p.CandidateTiers("", []string{uidFree, uidPaid})
	if got[uidFree] != tierFree || got[uidPaid] != tierPaid {
		t.Errorf("预热后应直接报出真实档位，实际 %v —— 预热的意义就是让第一次选号"+
			"也有偏好；报成不确定等于没预热", got)
	}
}

// 查不到的账号（session 失效）必须**保持不确定**，不能被写成一个假档位。
//
// 写成 tierPaid 会让它排到"可能还有免费额度"的号后面（尚可接受），
// 但写成 tierFree 就会让它被**优先**选中 —— 而它其实已经用不了了。
func TestWarmupLeavesUnreachableAccountsUnsure(t *testing.T) {
	p, _ := warmupFixture(t)
	p.warmupPass()

	if e, ok := p.tiers.get(uidDead); ok {
		t.Errorf("查不到的账号不该被写进缓存（会变成一个假档位），实际 %+v", e)
	}
	if got := p.CandidateTiers("", []string{uidDead})[uidDead]; got != tierUnsure {
		t.Errorf("查不到的账号应报不确定，实际 %d", got)
	}
}

// 新鲜的条目**不重查**；上一遍失败的**要重试**。
//
// 这两件事是一体两面，也是这个测试真正要钉的：
//
//	不重查新鲜的 → 空闲部署的周期成本只该是一次凭证目录读取，
//	                不该在没人用的时候白白打上游（容易被限流）
//	要重试失败的 → 这是它自愈的唯一途径：某个号 session 失效时查不到，
//	                而它被修好（重新登录）之后没有任何请求会"恰好"碰上它，
//	                只有周期重试能把它补回来
//
// ⚠ 我第一版把这条写成"第二遍一次都不发" —— 那是错的，它会把
// "重试失败条目"这个**必要**行为判成 bug。（夹具里那个 dead 号就是为此存在的。）
func TestWarmupSkipsFreshButRetriesFailed(t *testing.T) {
	p, queries := warmupFixture(t)

	p.warmupPass() // 第一遍：3 个账号各查一次（free / paid / dead）
	first := atomic.LoadInt64(queries)
	if first != 3 {
		t.Fatalf("第一遍应当把 3 个账号都查一遍，实际 %d 次", first)
	}

	p.warmupPass() // 第二遍：只有 dead 那个没落缓存 ⇒ 只该重试它
	second := atomic.LoadInt64(queries)
	if second-first != 1 {
		t.Errorf("第二遍只该重试「上一遍没查到」的那 1 个账号，实际多发 %d 次 ——\n"+
			"多发说明新鲜的条目被重查了（空闲部署白打上游）；\n"+
			"少发（0 次）则说明失败的条目不再重试，那个号修好之后永远回不来",
			second-first)
	}
}

// 过期的档位会被下一遍补齐（覆盖"用户在 Loomy 客户端里消耗 / 自然日重置"）。
func TestWarmupRefillsStaleEntries(t *testing.T) {
	p, queries := warmupFixture(t)
	p.warmupPass()
	before := atomic.LoadInt64(queries)

	// 把两个号的档位改成"已过期"。
	p.tiers.mu.Lock()
	for _, uid := range []string{uidFree, uidPaid} {
		e := p.tiers.m[uid]
		e.at = time.Now().Add(-2 * tierCacheTTL)
		p.tiers.m[uid] = e
	}
	p.tiers.mu.Unlock()

	p.warmupPass()
	if after := atomic.LoadInt64(queries); after <= before {
		t.Error("过期档位应当被下一遍补齐 —— 不补的话，缓存会一直停在一个" +
			"已经不成立的值上（那正是本次报障的形态）")
	}
}

// StartTierWarmup 幂等：重复调用只启动一个预热器。
//
// 不幂等的话，装配层一次误调就会变成两个 ticker 同时打上游。
func TestStartTierWarmupIsIdempotent(t *testing.T) {
	p, _ := warmupFixture(t)
	p.StartTierWarmup()
	p.StartTierWarmup()
	p.StartTierWarmup()

	// 等到第一次 pass 落地（轮询而不是固定 sleep：CI 上慢机器也不 flaky）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := p.tiers.get(uidFree); ok && e.tier == tierFree {
			return // 预热生效了
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("StartTierWarmup 之后 5 秒内没看到档位被填上 —— " +
		"重复调用不该破坏预热，但一次都没生效说明它根本没跑起来")
}

// ⚠ 周期补齐**真的会发生**（变异验证抓出来的第二个缺口）。
//
// # 为什么这条不能省
//
// 我第一版的预热测试全部直接调 `warmupPass()` —— 于是把 ticker 循环体
// 改成空的（"只跑启动那一遍，不周期补齐"）**全绿**。
//
// 而周期补齐是这条机制**自愈能力**的全部来源：
//
//	某个号 session 失效 ⇒ 查不到 ⇒ 保持不确定；
//	它被修好（重新登录）之后**没有任何请求会恰好碰上它**，
//	只有周期重试能把它补回来。
//
// 所以要把间隔调小到几十毫秒，真跑几轮。
func TestWarmupKeepsRefillingOnTick(t *testing.T) {
	old := tierWarmupInterval
	tierWarmupInterval = 40 * time.Millisecond
	defer func() { tierWarmupInterval = old }()

	p, _ := warmupFixture(t)
	p.StartTierWarmup()

	// 等第一遍把档位落上。
	if !waitFor(t, 3*time.Second, func() bool {
		e, ok := p.tiers.get(uidFree)
		return ok && e.tier == tierFree
	}) {
		t.Fatal("第一遍预热没落地")
	}

	// 把它人为改成"已过期"，然后**不再做任何事** ——
	// 只靠 ticker 应该把它补回来。
	p.tiers.mu.Lock()
	e := p.tiers.m[uidFree]
	e.at = time.Now().Add(-2 * tierCacheTTL)
	p.tiers.m[uidFree] = e
	p.tiers.mu.Unlock()

	if !waitFor(t, 3*time.Second, func() bool {
		e, ok := p.tiers.get(uidFree)
		return ok && time.Since(e.at) < tierCacheTTL
	}) {
		t.Error("把档位改成过期之后，等了 3 秒（>70 个预热周期）仍没被补回来 ——\n" +
			"说明 ticker 循环没有在跑（只跑了启动那一遍）。\n" +
			"周期补齐是自愈的唯一途径：查不到的号被修好之后，" +
			"没有任何请求会恰好碰上它，只有周期重试能把它补回来。")
	}
}

// waitFor 轮询直到条件成立或超时。
func waitFor(t *testing.T, d time.Duration, ok func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ok()
}
