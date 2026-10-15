// candidate_tier_tristate_test.go 档位判定的两条"三态纪律"守卫。
//
// # 用户报障（本次修的第五条）
//
// 原话：*「loomy 如果赠送积分没有了还是会调用，也就是优先免费额度的功能没起作用」*。
//
// 这条报障与前两次（见 candidate_tier.go 文件头的两次记录）**同源但不同洞**：
// 前两次修的是"新鲜度"（用完就标脏、档位顺序），这次修的是**两条把
// "已知"降级成"未知"的路径**：
//
//	① 把「没读到 dailyBalance」读成「确认只剩永久」—— 未知当成确定的零
//	② 把「已确认只剩永久」因为缓存过期/刚用过而降级成「不确定」
//
// 两条都会让一个**已知没有免费额度**的号与"可能还有免费额度"的号**同档**，
// 而硬分档（pool.pickForTier 只留最优那一档）在同档内选不出任何差别 ⇒
// 用户的规则（先用完所有号的免费额度）失效。
package loomy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ── ① 未知不能当成确定的零 ─────────────────────────────────────────────

// ⚠ 回执里**没有** dailyBalance ⇒ 必须落 tierUnsure，**不能**落 tierPaid。
//
// 这条直接对应 points.go 那段注释点名要防的事：
// "只回 int64 的话字段缺失会被读成 0 …… 这是把未知说成确定的零"。
func TestTierOfPointsDailyUnknownIsNotPaid(t *testing.T) {
	// 回执里**故意没有** dailyBalance（老接口/部分响应就是这个形态）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/points/records" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// balance 与 availableBalance 都在，**dailyBalance 缺席**。
		fmt.Fprint(w, `{"code":"000000","desc":"ok","data":{"balance":25000,"availableBalance":25000}}`)
	}))
	defer srv.Close()

	const (
		uid  = "260303030303030303"
		sess = "cccccccccccccccccccccccccccccccc"
	)
	dir := t.TempDir()
	writeFile(t, dir, "loomy-c.json", `{"session":"`+sess+`","userid":"`+uid+`"}`)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir})

	p.RefreshQuota(uid)

	e, ok := p.tiers.get(uid)
	if !ok {
		t.Fatalf("刷新应当把档位写进缓存（即使 dailyBalance 缺席）")
	}
	if e.tier == tierPaid {
		t.Errorf("★ dailyBalance **没读到**，却把该号判成 tierPaid（确认只剩永久）——\n" +
			"\n" +
			"这是" + "「把未知说成确定的零」" + "：字段缺席与「确实是 0」是两件事。\n" +
			"（points.go 的 Points.DailyKnown 就是为这条存在的，而 Balance 那个薄封装把它丢了。）\n" +
			"\n" +
			"后果：这个号与**真正**用完免费额度的号同档 ⇒ 硬分档选不出差别 ⇒\n" +
			"一个**还有免费额度**的号会被与「该烧永久」的号一起抽中。")
	}
	if e.tier != tierUnsure {
		t.Errorf("dailyBalance 缺失时应落 tierUnsure，实际 %d", e.tier)
	}
}

// 反向：dailyBalance **在**且是 0 ⇒ 才该落 tierPaid。
//
// 只加"缺席判 unsure"而不判"确实是 0"会把兜底一起修坏 ——
// 那样所有用完免费额度的号都变成"不确定"，用户的规则同样失效。
func TestTierOfPointsDailyKnownZeroIsPaid(t *testing.T) {
	cases := []struct {
		name string
		pts  Points
		want int
	}{
		{"daily 已知且为 0 ⇒ 确认只剩永久", Points{Available: 25000, Daily: 0, DailyKnown: true}, tierPaid},
		{"daily 已知且 >0 ⇒ 确认还有免费", Points{Available: 25000, Daily: 1434, DailyKnown: true}, tierFree},
		{"daily 未知且为 0 ⇒ 不确定", Points{Available: 25000, Daily: 0, DailyKnown: false}, tierUnsure},
	}
	for _, c := range cases {
		if got := tierOfPoints(c.pts); got != c.want {
			t.Errorf("%s：tierOfPoints = %d，期望 %d", c.name, got, c.want)
		}
	}
}

// ── ② 已知的结论不能因为缓存过期而降级 ─────────────────────────────────

// ⚠ 已确认"只剩永久"在**同一个自然日内**过期后仍须报 tierPaid。
//
// 这是本次修的第二条：tierUnsure(1) 排在 tierPaid(2) **前面**，
// 所以"因为缓存过期而降级"等于让一个已知没有免费额度的号**变得更优先**。
func TestPaidTierSurvivesTTLWithinSameDay(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	po := p.pool()
	// ⚠ 用**免费额度池**（见 freepool.go）—— 它是现在选号的权威。
	// 改造前这条断言的是"档位缓存过了 TTL 不降级"，那套缓存已被池子取代；
	// 而**同一条意图**必须继续成立：一个已知没有免费额度的号，在同一天里
	// 不该因为"过了多久"而变得更优先。
	//
	// 池子天然满足它：它没有 TTL，"已知为 0"只在**自然日变更**时才失效。
	po.noteQuota("HAVE", 1434, true, now) // 让池子非空（否则全员放开）
	po.noteQuota("PAID", 0, true, now)

	got := p.CandidateTiers("", []string{"PAID"})
	if got["PAID"] != tierPaid {
		t.Errorf("★ 同一天内已确认'只剩永久'的号报了 %d（期望 %d）——\n"+
			"\n"+
			"tierUnsure(%d) 排在 tierPaid(%d) **前面**，所以报不确定等于：\n"+
			"一个**已知没有免费额度**的号，变得比真正该省的号更优先。\n"+
			"赠送额度只按自然日重置 ⇒ 它这一天里不会变回'有' ⇒ 没有理由降级。",
			got["PAID"], tierPaid, tierUnsure, tierPaid)
	}
}

// ⚠ 自然日变了 ⇒ 必须重新评估（不能把昨天的"只剩永久"用到今天）。
//
// 反向守：只加"同一天内不降级"而不判自然日，会把昨天的结论用到今天 ——
// 而赠送额度是按自然日重置的，昨天用完不等于今天没有。
func TestPaidTierReevaluatedAfterDayRollover(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	const uid = "260505050505050505"
	now := time.Now()

	// 手动写一个"昨天的"paid 档位。
	p.tiers.mu.Lock()
	p.tiers.m[uid] = tierEntry{tier: tierPaid, at: now.Add(-time.Hour), day: naturalDay(now.AddDate(0, 0, -1))}
	p.tiers.mu.Unlock()

	got := p.CandidateTiers("", []string{uid})
	if got[uid] != tierUnsure {
		t.Errorf("★ 跨自然日后仍报 tierPaid = %d（期望 %d）——\n"+
			"赠送额度按自然日重置 ⇒ 昨天用完不等于今天没有 ⇒ 必须重新评估。",
			got[uid], tierUnsure)
	}
}

// ⚠ "用它"不该把已确认"只剩永久"标脏（用户报障的另一半）。
func TestInvalidateKeepsPaidTierSameDay(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	po := p.pool()
	// ⚠ 用池子建状态（它是现在选号的权威）。改造前这里手写档位缓存 ——
	// 那套缓存已被池子取代，而**同一条意图**必须继续成立：
	// 用一个"已知只剩永久"的号，不该让它变得更优先。
	po.noteQuota("HAVE", 1434, true, now) // 让池子非空
	po.noteQuota("PAID", 0, true, now)

	p.invalidateTier("PAID")

	if got := p.CandidateTiers("", []string{"PAID"}); got["PAID"] != tierPaid {
		t.Errorf("★ 用过一个'已确认只剩永久'的号之后，它被降级成 %d（期望 %d）——\n"+
			"\n"+
			"用它只会消耗**赠送**额度，而赠送额度只按自然日重置 ⇒\n"+
			"这个结论今天不会因为被用而变。旧实现无条件 drop，于是\n"+
			"'刚用过的那个'额外获得优先权（tierUnsure 排在 tierPaid 前面）——\n"+
			"那正是用户看到的「赠送额度没有了反而更常被调用」。",
			got["PAID"], tierPaid)
	}
	// 反向：已知为 0 的号被用到**不该**挂重查标记（池子空了才会用到它，
	// 那时它已经是"允许动永久额度"的状态，重查一个已知为 0 的号没有意义）。
	if po.needRecheck["PAID"] {
		t.Error("已知为 0 的号被用到不该挂重查标记")
	}
}

// 反向：**用完一个还有免费额度的号必须挂重查标记**。
//
// 那条例子是"它可能刚刚把免费额度用完"，所以不能一起改坏 ——
// 两个方向都要有，否则守卫只保护了一半。
func TestInvalidateDropsFreeTier(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	po := p.pool()
	po.noteQuota("HAVE", 1434, true, now)

	p.invalidateTier("HAVE")

	// ⚠ 判据从"缓存被删"改成"标记被挂上"：意图（这个号的余额刚变过、
	// 它的档位不再可信）不变，但代价从"每次对话一次查询"降到"按节流查"。
	if !po.needRecheck["HAVE"] {
		t.Errorf("用过一次还有免费额度的号之后，它应当被挂上「需要重查」标记 ——\n" +
			"它可能刚刚把免费额度用完了，不重查就会继续按'还有免费'优先，\n" +
			"也就是继续烧永久额度（用户报的正是这个）。")
	}
	// 而且它**仍然在池子里**（重查回来之前不该把它踢出去）——
	// 踢出去会让一个可能还有免费额度的号被跳过。
	if have, _ := po.snapshot(now); !have["HAVE"] {
		t.Error("重查回来之前不该把该号移出池子（它可能还有免费额度）")
	}
}
