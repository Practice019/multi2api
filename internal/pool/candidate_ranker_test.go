// candidate_ranker_test.go 候选分档的守卫（gateway.CandidateTierExt 在池侧的行为）。
//
// # 这个文件守的是用户的一条明确规则
//
// 用户原话：
//
//	「优先使用免费额度，轮询所有账号，把所有账号的免费额度用完之后，
//	  它才能用永久的额度。如果说有一个账号的免费额度用完了，那就轮询下一个账号，
//	  直到所有的账号免费额度都没有了，才能用永久额度。」
//
// 那条规则之所以**必须**在选号时实现（而不是等上游报错换号）：
// Loomy 额度耗尽时**不报错** —— 服务端静默降级、继续扣永久积分、照常返回 200。
// 所以"等限流错误再换号"对它完全无效，只能按余额硬分档。
//
// 下面每条用例都对应那条规则的一个面。
package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// rankerStub 注入一个固定分档，并记录它收到的候选。
func rankerStub(tiers map[string]int, seen *[]string) func(string, string, []string) map[string]int {
	return func(provider, model string, uids []string) map[string]int {
		if seen != nil {
			*seen = uids
		}
		if provider != "loomy" {
			return nil // 别的上游没有档位信息
		}
		return tiers
	}
}

// 有"还有免费额度"的号时，**必须**只用它 —— 不许碰只剩永久额度的号。
//
// 这条是用户规则的正身。用 50 次抽签来断言"从不选中"，是因为池子的选号带
// 加权随机：只抽一次的话，"选错"与"随机没抽到"无法区分。
func TestCandidateRankerPrefersFreeTierAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("loomy")
	p.SyncToDirFor("loomy", []*auth.Auth{{UID: "free"}, {UID: "paid"}})

	// free 还有每日赠送（0 档）；paid 只剩永久额度（1 档）。
	p.SetCandidateRanker(rankerStub(map[string]int{"free": 0, "paid": 1}, nil))

	for i := 0; i < 50; i++ {
		a := p.PickFor("loomy", "", nil)
		if a == nil {
			t.Fatal("有可用账号时不该返回 nil")
		}
		if a.UID != "free" {
			t.Fatalf("第 %d 次抽到 %q —— 应**只用** 0 档（还有免费额度）的号。\n"+
				"抽到 paid 意味着永久积分被消耗掉了，而 free 的免费额度还没用完。",
				i, a.UID)
		}
	}
}

// 免费额度的号已经被试过（tried）⇒ 才轮到永久额度的号。
//
// 这条实现的是"一个号用完了就轮询下一个"的下一半：
// 当免费档全部试完，永久档才成为最优档。
func TestCandidateRankerFallsBackToPaidTierWhenFreeTried(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("loomy")
	p.SyncToDirFor("loomy", []*auth.Auth{{UID: "free"}, {UID: "paid"}})
	p.SetCandidateRanker(rankerStub(map[string]int{"free": 0, "paid": 1}, nil))

	// 模拟"free 这个号本次已经试过并失败/额度用尽"。
	tried := map[string]bool{"free": true}
	a := p.PickFor("loomy", "", tried)
	if a == nil {
		t.Fatal("免费档已被排除时应回落到永久档，而不是返回 nil")
	}
	if a.UID != "paid" {
		t.Errorf("应回落到 1 档（只剩永久额度）的号，实际 %q", a.UID)
	}
}

// 同档内**不做筛选**：档内全部候选都留在抽签池里（既有加权选号不被破坏）。
//
// # ⚠ 这条用例的第一版写错了，而变异验证把它抓了出来（教训值得留下）
//
// 第一版断言的是「120 次抽签里出现了 ≥2 个不同的 uid」。那个断言**恒真**，
// 抓不住"同档只留一个候选"的缺陷 ——
//
//	收窄后留下的那一个是 **map 迭代顺序**的第一个，
//	而 map 迭代顺序每次都可能不同 ⇒ 120 次里照样出现 3 个 uid。
//
// 实测：注入"同档只留一个"的变异后，第一版**仍然是绿的**，
// 而它本该红。**恒真的断言比没有断言更糟** —— 它让人以为覆盖了。
//
// 正解是让**权重可判别**：给同档三个号截然不同的额度，再断言高额度的那个
// 占明显多数。候选被收窄成一个时，它只会占约 1/3
// （等于"恰好在 map 里排第一"的概率）。
func TestCandidateRankerKeepsAllWithinSameTier(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("loomy")
	p.SyncToDirFor("loomy", []*auth.Auth{{UID: "u1"}, {UID: "u2"}, {UID: "u3"}})

	// 三者同档（都 0 档），但额度差 6 个数量级 ⇒ 加权选号应明显偏向 u3。
	p.SetQuota("u1", FromCredits(1))
	p.SetQuota("u2", FromCredits(1))
	p.SetQuota("u3", FromCredits(1_000_000))

	p.SetCandidateRanker(rankerStub(map[string]int{"u1": 0, "u2": 0, "u3": 0}, nil))

	const n = 300
	picks := map[string]int{}
	for i := 0; i < n; i++ {
		a := p.PickFor("loomy", "", nil)
		if a == nil {
			t.Fatal("同档时不该返回 nil")
		}
		picks[a.UID]++
	}
	if picks["u3"] < n*2/3 {
		t.Errorf("高额度账号应占明显多数（至少 %d/%d），实际分布 %v。\n"+
			"同档内**不得**被收窄候选集 —— 那会把既有的加权选号退化成"+
			"「随机挑一个 map 里排第一的」，既有的负载均衡随之失效。",
			n*2/3, n, picks)
	}
}

// ranker 传 nil（本部署没有分档能力）⇒ 行为与改造前**完全一致**。
func TestCandidateRankerNilChangesNothing(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("loomy")
	p.SyncToDirFor("loomy", []*auth.Auth{{UID: "u1"}, {UID: "u2"}})
	// 不调用 SetCandidateRanker。

	got := map[string]bool{}
	for i := 0; i < 120; i++ {
		a := p.PickFor("loomy", "", nil)
		if a == nil {
			t.Fatal("不该返回 nil")
		}
		got[a.UID] = true
	}
	if len(got) != 2 {
		t.Errorf("没有分档能力时应保持原有行为（两个号都能被选中），实际 %v", got)
	}
}

// ranker 返回空 map（整批候选都拿不到档位）⇒ 保持原候选集。
//
// 这是"冷缓存"的形态：Loomy 的档位来自异步刷新的缓存，首次调用时是空的。
// 此时必须优雅降级到既有行为，而不是把候选筛没（那会让请求 503）。
func TestCandidateRankerEmptyMapKeepsCandidates(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("loomy")
	p.SyncToDirFor("loomy", []*auth.Auth{{UID: "u1"}, {UID: "u2"}})
	p.SetCandidateRanker(rankerStub(map[string]int{}, nil))

	got := map[string]bool{}
	for i := 0; i < 120; i++ {
		a := p.PickFor("loomy", "", nil)
		if a == nil {
			t.Fatal("空档位时不该把候选筛没（那会让冷启动后的请求直接 503）")
		}
		got[a.UID] = true
	}
	if len(got) != 2 {
		t.Errorf("空档位应保持原行为，实际 %v", got)
	}
}

// 只回了一部分 uid 的档位 ⇒ 未回的那些按**最低优先级**处理。
//
// 判据来自用户要求：宁可先用能确认余额的账号，也不拿查不准的号去烧额度。
func TestCandidateRankerUnknownUIDsGoLast(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("loomy")
	p.SyncToDirFor("loomy", []*auth.Auth{{UID: "known"}, {UID: "unknown"}})

	// 只给 known 的档位：unknown 不在 map 里 = 最低优先级。
	p.SetCandidateRanker(rankerStub(map[string]int{"known": 1}, nil))

	for i := 0; i < 50; i++ {
		a := p.PickFor("loomy", "", nil)
		if a == nil {
			t.Fatal("不该返回 nil")
		}
		if a.UID != "known" {
			t.Fatalf("未上报档位的账号应排在最后（宁可先用能确认余额的号），实际选中 %q", a.UID)
		}
	}
}

// 分档必须**按上游隔离**：别的上游没有档位信息时不受影响。
func TestCandidateRankerOnlyAppliesToItsProvider(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("alpha")
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})
	p.SyncToDirFor("loomy", []*auth.Auth{{UID: "free"}, {UID: "paid"}})
	p.SetCandidateRanker(rankerStub(map[string]int{"free": 0, "paid": 1}, nil))

	// alpha 的请求：ranker 对它回 nil ⇒ 必须照常选到 a1。
	for i := 0; i < 20; i++ {
		if a := p.PickFor("alpha", "", nil); a == nil || a.UID != "a1" {
			t.Fatalf("alpha 不受 loomy 的分档影响，实际 %v", a)
		}
	}
}
