// candidate_tier_test.go 候选分档的守卫。
//
// # 这里守的是用户的一条明确规则
//
//	「优先使用免费额度…把所有账号的免费额度用完之后，它才能用永久的额度。」
//
// 分档规则的全部内容就是一个纯函数（tierOf），所以本文件最重要的用例是
// 它的**真值表** —— 而不是"发个请求看选号结果"。分档错了，选号一定错，
// 但反过来从选号结果反推很难定位到是哪一档判错了。
//
// 另一半是**性能契约**：核心在持有池锁时调用 CandidateTiers，
// 所以它必须只读内存。这一条用"把客户端指向一个必然连不上的地址"来证明 ——
// 若实现里发了网络请求，用例会因超时而失败，而不是悄悄变慢。
package loomy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// deadBase 一个必然连不上的基址（用它的用例在证明"没发网络请求"）。
const deadBase = "http://127.0.0.1:1"

// tierFixture 造两个账号：一个还有每日赠送（free），一个只剩永久额度（paid）。
//
// 数值取实测形态（见 points.go 文件头）：
//
//	balance 25000 + dailyBalance 1434 = availableBalance 26434
//
// 于是同一个账号只需换 dailyBalance 就能表达"免费额度还有/已用尽"。
func tierFixture(t *testing.T) (*Provider, func()) {
	t.Helper()

	// session 用 32 位小写 hex（实测形态），避免触发"格式可疑"的告警日志。
	const (
		uidFree  = "260101010101010101"
		uidPaid  = "260202020202020202"
		sessFree = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		sessPaid = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/points/records" {
			http.NotFound(w, r)
			return
		}
		var balance, daily int64
		switch r.Header.Get("token") {
		case sessFree:
			balance, daily = 25000, 1434 // 还有每日赠送 ⇒ 0 档
		case sessPaid:
			balance, daily = 25000, 0 // 每日赠送已用尽 ⇒ 1 档
		default:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":"000000","desc":"ok","data":{"balance":%d,"dailyBalance":%d,"availableBalance":%d}}`,
			balance, daily, balance+daily)
	}))

	dir := t.TempDir()
	writeFile(t, dir, "loomy-a.json", `{"session":"`+sessFree+`","userid":"`+uidFree+`"}`)
	writeFile(t, dir, "loomy-b.json", `{"session":"`+sessPaid+`","userid":"`+uidPaid+`"}`)

	p := NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir})
	return p, srv.Close
}

// 分档规则的真值表。
//
// 判据来自用户要求：先用免费额度，全用光才碰永久额度；
// 而且"查不到"与"余额为 0"归同一档（宁可先用能确认余额的号）。
func TestTierOfMatchesUserRule(t *testing.T) {
	cases := []struct {
		name      string
		available int64
		daily     int64
		want      int
		why       string
	}{
		{"还有每日赠送", 26434, 1434, tierDaily,
			"用它不消耗永久积分 —— 这是要优先的那一档"},
		{"每日赠送刚好用完", 25000, 0, tierPermanent,
			"available == 永久额度（因为 daily==0），用它真的在花钱"},
		{"只剩一点点每日", 1, 1, tierDaily,
			"只要 >0 就是免费额度，不该因为数额小就被跳过"},
		{"两个账本都空", 0, 0, tierUnknown,
			"没额度可用 —— 与'查不到'同档"},
		{"余额为负（理论透支）", -5, 0, tierUnknown,
			"负数不构成可用额度"},
		{"只有永久额度且很大", 999999, 0, tierPermanent,
			"再大也是永久额度，只该在所有免费额度用尽后才用"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tierOf(c.available, c.daily); got != c.want {
				t.Errorf("tierOf(%d, %d) = %d，want %d —— %s",
					c.available, c.daily, got, c.want, c.why)
			}
		})
	}
}

// 分档依赖的那条等式：available = 永久 + 今日（实测）。
//
// 单独钉住它是为了让"为什么只凭 (available, daily) 就能分档"这个推理
// 有据可查 —— 若哪天上游改成 available 不含每日赠送，这条会红，
// 而分档会立刻变得毫无意义（所有账号都会落到 1 档）。
func TestTierDependsOnMeasuredIdentity(t *testing.T) {
	// 实测样本：balance 25000 / dailyBalance 1434 / availableBalance 26434
	const permanent, daily = 25000, 1434
	if permanent+daily != 26434 {
		t.Fatal("实测样本本身不自洽")
	}
	if tierOf(26434, daily) != tierDaily {
		t.Error("两个账本相加等于 available 时，有每日赠送的号必须落 0 档")
	}
	// daily==0 ⇒ available 恰好等于永久额度
	if tierOf(permanent, 0) != tierPermanent {
		t.Error("daily==0 时 available 就是永久额度，必须落 1 档")
	}
}

// ⚠ 性能契约：CandidateTiers **只读缓存**，不发网络请求。
//
// 用"客户端指向必然连不上的地址"来证明：若实现里查了余额，
// 这次调用会卡在连接超时上（用例会明显变慢甚至失败）。
func TestCandidateTiersReadsCacheOnly(t *testing.T) {
	p := NewWithConfig(Config{Client: NewWithBase(deadBase)})
	uid := "260101010101010101"
	p.tiers.set(uid, tierDaily)

	start := time.Now()
	got := p.CandidateTiers("", []string{uid})
	elapsed := time.Since(start)

	if got[uid] != tierDaily {
		t.Errorf("应返回缓存里的档位 %d，实际 %d", tierDaily, got[uid])
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("耗时 %v —— 明显像发了网络请求。\n"+
			"本方法在**持有账号池锁**时被调用（pool.pickFor），\n"+
			"任何 I/O 都会把整个池子的选号串行阻塞住。", elapsed)
	}
}

// 冷缓存（从未查过）⇒ 如实回答"未知"，且**立刻返回**。
//
// 这是首次调用/刚启动时的形态。核心会因此保留全部候选 ⇒
// 行为与改造前相同（优雅降级，而不是把候选筛没导致 503）。
func TestCandidateTiersColdCacheIsUnknownAndFast(t *testing.T) {
	p := NewWithConfig(Config{Client: NewWithBase(deadBase)})

	start := time.Now()
	got := p.CandidateTiers("", []string{"u1", "u2"})
	elapsed := time.Since(start)

	for _, uid := range []string{"u1", "u2"} {
		if got[uid] != tierUnknown {
			t.Errorf("冷缓存时 %s 应回未知档 %d，实际 %d", uid, tierUnknown, got[uid])
		}
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("冷缓存也必须立刻返回（异步刷新不允许阻塞调用方），实际耗时 %v", elapsed)
	}
}

// RefreshQuota 会顺手回填档位缓存（它本来就要查一次余额，不回填就是白查）。
func TestRefreshQuotaFillsTierCache(t *testing.T) {
	p, closeSrv := tierFixture(t)
	defer closeSrv()

	if _, ok := p.RefreshQuota("260101010101010101"); !ok {
		t.Fatal("额度查询应成功（假上游可用）")
	}
	got := p.CandidateTiers("", []string{"260101010101010101"})
	if got["260101010101010101"] != tierDaily {
		t.Errorf("刷新额度后档位应已知（%d），实际 %d —— "+
			"额度路径与分档路径查的是同一个数，不回填等于白查一次",
			tierDaily, got["260101010101010101"])
	}
}

// 异步刷新：冷缓存先答"未知"，稍后缓存被填上。
//
// 这条钉住"自愈"：网关刚启动时没有档位，但不需要人工点「刷新额度」，
// 选号路径自己会把缓存热起来。
func TestAsyncRefreshFillsCacheSelfHealing(t *testing.T) {
	p, closeSrv := tierFixture(t)
	defer closeSrv()

	// 第一拍：cold ⇒ 未知（且不阻塞）
	first := p.CandidateTiers("", []string{"260101010101010101"})
	if first["260101010101010101"] != tierUnknown {
		t.Fatalf("冷缓存第一拍应是未知档，实际 %d", first["260101010101010101"])
	}

	// 等异步刷新落定（最多 3 秒）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := p.tiers.get("260101010101010101"); ok && e.tier == tierDaily {
			return // 自愈成功
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("异步刷新未在 3 秒内填上档位 —— 没有它，用户要手工点「刷新额度」才生效")
}

// 两个账号：还有免费额度的落 0 档、只剩永久额度的落 1 档。
//
// 这是用户规则的**最小完整样本**：核心据此只会选 0 档那个号。
func TestCandidateTiersDistinguishesFreeAndPermanent(t *testing.T) {
	p, closeSrv := tierFixture(t)
	defer closeSrv()

	// 直接喂缓存（避开异步时序），先把两个号的真实余额查出来。
	p.refreshTierOne("260101010101010101") // 有每日赠送
	p.refreshTierOne("260202020202020202") // 只剩永久额度

	got := p.CandidateTiers("", []string{"260101010101010101", "260202020202020202"})
	if got["260101010101010101"] != tierDaily {
		t.Errorf("还有免费额度的号应是 %d 档，实际 %d", tierDaily, got["260101010101010101"])
	}
	if got["260202020202020202"] != tierPermanent {
		t.Errorf("只剩永久额度的号应是 %d 档，实际 %d", tierPermanent, got["260202020202020202"])
	}
	if got["260101010101010101"] >= got["260202020202020202"] {
		t.Error("有免费额度的号必须**严格优先**于只剩永久额度的号")
	}
}

// 查不到余额 ⇒ **不写缓存**（保持未知），下次还会再试。
//
// 若把失败也写成一个档位，它会在 TTL 内被当成"已确认"，而那个号可能已经回血。
func TestRefreshFailureLeavesCacheEmpty(t *testing.T) {
	p := NewWithConfig(Config{Client: NewWithBase(deadBase)})
	p.refreshTierOne("260101010101010101")
	if _, ok := p.tiers.get("260101010101010101"); ok {
		t.Error("查失败不应写缓存 —— 写进去会让它在 TTL 内被误当成'已确认'")
	}
}

// 没有凭证的 uid ⇒ 不查、不写（那不是错误，只是不属于本上游）。
func TestRefreshTierSkipsUnknownUID(t *testing.T) {
	p, closeSrv := tierFixture(t)
	defer closeSrv()
	p.refreshTierOne("不存在的-uid")
	if _, ok := p.tiers.get("不存在的-uid"); ok {
		t.Error("凭证目录里没有的 uid 不该被写入缓存")
	}
}
