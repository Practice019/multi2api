// freepool_sweep_test.go 定期清扫的守卫。
//
// # 这里守的是用户提的第二个设计（原话）
//
//	「或者你可以添加一个任务，那个任务，然后30秒或者1分钟循环一次。或者说，
//	  就是定期检测轮询池里面有没有额度为0的，然后移出去就行了。」
//
// ⚠ 而真正需要被钉住的**不是**"清扫会发生"，是"**它不会变成查询洪水**"：
//
//	旧预热：所有账号 × 每 30 秒 × 失败无退避 ⇒ 用户实测"查询太多、太频繁"
//	本清扫：只查池子成员（会随额度用尽单调缩小到零）+ 节流 + 退避
//
// 所以下面每条用例的第二半都在断言"哪些账号**不该**被查"。
package loomy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sweepFixture 造一个可控的假上游：每个账号的"今日赠送剩余"由 daily 决定。
//
// 返回 (provider, 每个账号被查了几次的计数器)。
func sweepFixture(t *testing.T, daily map[string]int64) (*Provider, *sync.Map) {
	t.Helper()
	var mu sync.Mutex
	hits := &sync.Map{} // uid -> *int64
	dailyOf := func(uid string) int64 {
		mu.Lock()
		defer mu.Unlock()
		return daily[uid]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/points/records" {
			http.NotFound(w, r)
			return
		}
		// 会话 → uid 的映射由夹具的固定表给出（见下）。
		uid := sessToUID[r.Header.Get("token")]
		if uid == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if c, ok := hits.Load(uid); ok {
			atomic.AddInt64(c.(*int64), 1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":"000000","desc":"ok","data":{"balance":25000,"dailyBalance":%d,"availableBalance":%d}}`,
			dailyOf(uid), 25000+dailyOf(uid))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	for i, uid := range sweepUIDs {
		writeFile(t, dir, fmt.Sprintf("loomy-%d.json", i),
			fmt.Sprintf(`{"session":"%s","userid":"%s"}`, sweepSessions[i], uid))
		c := new(int64)
		hits.Store(uid, c)
	}
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir})
	return p, hits
}

const (
	sweepUIDHave1 = "260101010101010101"
	sweepUIDHave2 = "260202020202020202"
	sweepUIDZero  = "260303030303030303"
)

var (
	sweepUIDs     = []string{sweepUIDHave1, sweepUIDHave2, sweepUIDZero}
	sweepSessions = []string{"aaaa0000000000000000000000000001", "bbbb0000000000000000000000000002", "cccc0000000000000000000000000003"}
	sessToUID     = map[string]string{}
)

func init() {
	for i, uid := range sweepUIDs {
		sessToUID[sweepSessions[i]] = uid
	}
}

func hitsOf(t *testing.T, hits *sync.Map, uid string) int64 {
	t.Helper()
	c, _ := hits.Load(uid)
	return atomic.LoadInt64(c.(*int64))
}

// ⚠ 最重要的那条：**池子空了就一个请求都不发**。
//
// 这条是"本清扫 ≠ 旧预热"的分界：旧预热会永远按"账号总数"打上游，
// 本清扫在没有免费额度可盯的时候应该完全静默。
func TestSweepSendsNothingWhenPoolEmpty(t *testing.T) {
	p, hits := sweepFixture(t, map[string]int64{
		sweepUIDHave1: 0, sweepUIDHave2: 0, sweepUIDZero: 0,
	})
	// 先把池子填成"全部已知为 0"（池子空）。
	now := time.Now()
	for _, uid := range sweepUIDs {
		p.pool().noteQuota(uid, 0, true, now)
	}
	before := hitsOf(t, hits, sweepUIDHave1)

	if n := p.sweepFreePool(); n != 0 {
		t.Errorf("池子空时清扫不该查任何账号（实际查了 %d 个）", n)
	}
	if after := hitsOf(t, hits, sweepUIDHave1); after != before {
		t.Errorf("★ 池子空时仍然向这个账号发了请求（%d → %d）——\n"+
			"\n"+
			"那正是旧预热的形态：不管有没有免费额度可盯，都按账号总数打上游。\n"+
			"正确的行为是：池子空了（都在用永久额度）就完全静默，等自然日重置。",
			before, after)
	}
}

// ⚠ 只查**池子成员**；已知为 0 的账号不查（它今天不会变回"有"）。
func TestSweepOnlyQueriesPoolMembers(t *testing.T) {
	p, hits := sweepFixture(t, map[string]int64{
		sweepUIDHave1: 1434, sweepUIDHave2: 800, sweepUIDZero: 0,
	})
	now := time.Now()
	p.pool().noteQuota(sweepUIDHave1, 1434, true, now)
	p.pool().noteQuota(sweepUIDHave2, 800, true, now)
	p.pool().noteQuota(sweepUIDZero, 0, true, now) // 已知为 0：不在池子里

	// 把 checkedAt 推远，让两个成员都"该查"。
	p.pool().mu.Lock()
	p.pool().checkedAt = map[string]time.Time{}
	p.pool().mu.Unlock()

	beforeZero := hitsOf(t, hits, sweepUIDZero)
	if n := p.sweepFreePool(); n != 2 {
		t.Errorf("应当只查 2 个池子成员，实际 %d", n)
	}
	if after := hitsOf(t, hits, sweepUIDZero); after != beforeZero {
		t.Errorf("★ 已知为 0（不在池子里）的账号被查了（%d → %d）——\n"+
			"它今天不会变回「有」，查它是纯浪费，而且会让清扫的查询量\n"+
			"等于账号总数（就退回旧预热了）。", beforeZero, after)
	}
}

// ★ 核心效果：清扫之后，额度变成 0 的账号**离开池子**、不再被优先选用。
func TestSweepRemovesExhaustedFromPool(t *testing.T) {
	p, _ := sweepFixture(t, map[string]int64{
		sweepUIDHave1: 1434, sweepUIDHave2: 0, sweepUIDZero: 0,
	})
	now := time.Now()
	// 两个都在池子里（第二条的额度随后被上游改成 0 —— 模拟"用完了"）。
	p.pool().noteQuota(sweepUIDHave1, 1434, true, now)
	p.pool().noteQuota(sweepUIDHave2, 111, true, now)
	p.pool().mu.Lock()
	p.pool().checkedAt = map[string]time.Time{}
	p.pool().mu.Unlock()

	before := p.CandidateTiers("", []string{sweepUIDHave2})
	if before[sweepUIDHave2] != tierFree {
		t.Fatalf("前置不成立：HAVE2 应在池子里（实际 %d）", before[sweepUIDHave2])
	}

	p.sweepFreePool() // 这一轮会查到 HAVE2 的 daily 已经是 0

	after := p.CandidateTiers("", []string{sweepUIDHave2, sweepUIDZero})
	if after[sweepUIDHave2] != tierPaid {
		t.Errorf("★ 清扫之后，免费额度已用完的账号应当落 tierPaid(%d)（实际 %d）——\n"+
			"\n"+
			"用户要的正是这条：\"定期检测轮询池里面有没有额度为0的，然后移出去\"。\n"+
			"不移出去它就会继续被优先选中，也就是继续烧永久额度。",
			tierPaid, after[sweepUIDHave2])
	}
	// ⚠ Zero 在这个用例里**从没被查过** ⇒ 它是"未知"（档 1），不是"已知为 0"（档 2）。
	// 这两者必须分开：一个是"还没查"，一个是"查过确实是 0"。
	//（我第一版这条断言写成了 tierPaid —— 正是守卫把我自己纠正过来的。）
	if after[sweepUIDZero] != tierUnsure {
		t.Errorf("从没查过的账号应当是 tierUnsure(%d)（实际 %d）——\n"+
			"「还没查」与「查过是 0」是两件事，混起来会让一个\n"+
			"**可能还有免费额度**的号被当成'已用完'跳过。",
			tierUnsure, after[sweepUIDZero])
	}
}

// ⚠ 同一账号在节流窗口内不重复查（避免"每分钟都把一个号查一遍"）。
func TestSweepRespectsThrottle(t *testing.T) {
	p, hits := sweepFixture(t, map[string]int64{sweepUIDHave1: 1434})
	now := time.Now()
	p.pool().noteQuota(sweepUIDHave1, 1434, true, now) // checkedAt = now

	if n := p.sweepFreePool(); n != 0 {
		t.Errorf("刚查过的成员不该被再查（实际查了 %d 个）", n)
	}
	if got := hitsOf(t, hits, sweepUIDHave1); got != 0 {
		t.Errorf("★ 节流窗口内仍然发了请求（%d 次）——\n"+
			"没有节流的话，60 秒一轮 × 池子成员数就是常态负载；\n"+
			"有了它，一个账号至少 90 秒才被复查一次。", got)
	}
}

// ⚠ ticker 真的会周期触发（用可调间隔证明，不睡一分钟）。
func TestSweepTickerFires(t *testing.T) {
	old := freePoolSweepInterval
	freePoolSweepInterval = 40 * time.Millisecond
	defer func() { freePoolSweepInterval = old }()

	p, hits := sweepFixture(t, map[string]int64{sweepUIDHave1: 1434})
	p.pool().noteQuota(sweepUIDHave1, 1434, true, time.Now().Add(-time.Hour))
	p.StartFreePoolSweep()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hitsOf(t, hits, sweepUIDHave1) > 0 {
			return // 清扫跑起来了
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("StartFreePoolSweep 之后 2 秒内一次清扫都没发生 —— " +
		"定期检测没跑起来，池子里的「0」就永远没人移出去")
}
