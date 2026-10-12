// candidate_tier_invalidate_test.go 「用完就标脏」的守卫 —— 本次报障的修法核心。
//
// # 用户报的现象
//
//	「我有一个账号，它免费额度是 0，但其他账号有免费额度，
//	  它没有优先使用免费额度，它的额度（永久）一直在变少」
//
// # 根因（隔离实验复现过，见 candidate_tier.go 文件头）
//
// 档位缓存的"新鲜度"是相对**墙上时钟**的，而真正要防的是**我们自己的消耗**：
// 一个号服务完一次之后，它的免费额度已经少了，而缓存里那个数记的是"花之前"。
// 60 秒 TTL 之内它一直"新鲜但已经错了"，于是被反复优先选中，把永久额度烧掉。
//
// 复现数据（A 只有 2000 免费、B 有 20000，每次扣 2000）：
//
//	修复前：A 在 #3 #5 #7 #9 #11 被使用 —— 那五次的当时，B 都还有 16000+ 免费额度
//	修复后：0 次违规（#12 那次用它是对的 —— 那一刻两个号的免费额度都空了）
//
// # 两个必须同时成立的判据
//
//	① `Chat` 返回后**响应体被关闭时**标脏（不是请求之前 —— 那会和记账竞争）
//	② 标脏的结果是「不确定」档，而它排在「确认只剩永久」**前面**
//
// 下面两条各钉一个。
package loomy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// 假上游：**只**提供对话端点。积分端点故意打不通 ——
// 这样"标脏后触发的异步刷新"必然失败，缓存不会被它重新填上，
// 断言因此是**确定性**的（否则要跟刷新赛跑）。
func chatOnlyFixture(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"+
					"data: [DONE]\n\n")
			return
		}
		http.NotFound(w, r) // 包括 /api/v1/points/records
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	writeFile(t, dir, "loomy-x.json", `{"session":"`+fqSess+`","userid":"`+fqUID+`"}`)
	return NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir})
}

// ⚠ 核心判据：**响应体被关闭时**才标脏 —— 不能在请求之前。
//
// # 为什么这条要连"关闭之前还没脏"一起断言
//
// 我第一版把标脏写在**发送请求之前**，于是它和记账**竞争**：异步刷新可能
// 在上游扣费之前就读到旧余额，把刚标脏的号又标回"确认有免费额度" ——
// 修了个寂寞（隔离实验里那个号仍被多用了 1 次）。
//
// 所以判据有**两个方向**：
//
//	Chat 返回、响应体还没关 → 档位**仍在**（说明没有提前标脏）
//	Close 之后             → 档位**已被删**（下次读会落「不确定」）
//
// 只断言后一半会漏掉"提前标脏"那一版实现。
func TestChatInvalidatesTierOnBodyClose(t *testing.T) {
	p := chatOnlyFixture(t)

	// 预先造一个"已确认还有免费额度"的档位（模拟刚刷新过）。
	p.tiers.set(fqUID, tierFree)
	if e, ok := p.tiers.get(fqUID); !ok || e.tier != tierFree {
		t.Fatal("前置不成立：档位没写进去")
	}

	a := &Auth{Session: fqSess, UID: fqUID} // UID 由 LoadDir 填，生产形态
	cs, err := p.Chat(context.Background(), gateway.Credential{Provider: providerID, UID: fqUID, Secret: a},
		[]byte(`{"model":"fake-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if cs.Body == nil {
		t.Fatal("Chat 应当返回响应体")
	}

	// ① 还没关闭 ⇒ 档位应当**仍在**（没有提前标脏）。
	if _, ok := p.tiers.get(fqUID); !ok {
		t.Error("Chat 返回、响应体还没关，档位就被删掉了 —— " +
			"标脏必须发生在**请求完成之后**：提前标脏会让异步刷新和上游记账竞争，" +
			"刷新可能读到旧余额、把刚标脏的号又标回「确认有免费额度」")
	}

	// 读干净再关闭（核心就是这么做的）。
	_, _ = io.Copy(io.Discard, cs.Body)
	if err := cs.Body.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// ② 关闭之后 ⇒ 档位应当已被删（下次读会落「不确定」）。
	if e, ok := p.tiers.get(fqUID); ok {
		t.Errorf("响应体关闭后档位仍缓存着 %+v —— 那个数记的是"+
			"「这次请求花额度**之前**」的状态，而我们刚花了它的额度。\n"+
			"不删的话，它的免费额度被我们用完后仍会被优先选中，把永久额度烧掉"+
			"（用户实测报的正是这个形态）。", e)
	}
}

// Close 幂等：核心可能先读到 EOF 再 Close，也可能直接 Close。
func TestChatInvalidateIsIdempotent(t *testing.T) {
	p := chatOnlyFixture(t)
	p.tiers.set(fqUID, tierFree)

	cs, err := p.Chat(context.Background(), gateway.Credential{Provider: providerID, UID: fqUID, Secret: &Auth{Session: fqSess, UID: fqUID}},
		[]byte(`{"model":"x","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, cs.Body)
	_ = cs.Body.Close()
	// 再关一次不该 panic，也不该把缓存写回什么。
	if err := cs.Body.Close(); err != nil {
		t.Errorf("重复 Close 不该报错: %v", err)
	}
	if _, ok := p.tiers.get(fqUID); ok {
		t.Error("重复 Close 之后档位仍不该在")
	}
}

// ⚠ 判据②：过期的档位必须报「不确定」，**不能**把旧值当当前值报出去。
//
// 这条与上一条互补：上一条管"我们自己的消耗"，这条管"缓存自然过期"
// （用户在 Loomy 客户端里消耗、或自然日重置）。
func TestStaleTierReportsUnsure(t *testing.T) {
	p := NewWithConfig(Config{}) // 无上游：刷新必然失败 ⇒ 缓存不会被重填

	// 手工塞一个"已过期"的、写着"确认有免费额度"的档位。
	p.tiers.mu.Lock()
	p.tiers.m[fqUID] = tierEntry{tier: tierFree, at: time.Now().Add(-2 * tierCacheTTL)}
	p.tiers.mu.Unlock()

	got := p.CandidateTiers("", []string{fqUID})
	if got[fqUID] != tierUnsure {
		t.Errorf("过期档位应报「不确定」(%d)，实际 %d ——\n"+
			"把旧值当当前值报出去，就是拿一个已经不成立的结论当事实用："+
			"那个号可能早已耗尽免费额度，而我们仍会优先选中它、烧掉永久积分",
			tierUnsure, got[fqUID])
	}
}

// 「不确定」必须排在「确认只剩永久」**前面**（用户口径，见 candidate_tier.go 文件头）。
func TestUnsureRanksBeforePaid(t *testing.T) {
	if !(tierFree < tierUnsure && tierUnsure < tierPaid) {
		t.Fatalf("档位顺序必须满足 免费 < 不确定 < 只剩永久，实际 %d/%d/%d ——\n"+
			"「不确定」的号**可能**还有免费额度，而「确认只剩永久」的一定没有；"+
			"先在不确定的那些里找，才符合用户要的「别烧永久积分」",
			tierFree, tierUnsure, tierPaid)
	}
}

// 已确认没有免费额度 ⇒ tierPaid（与「不确定」必须分开）。
//
// 混在一起会让"查不到"被当成"确认没有"，于是"可能还有免费额度"的号
// 被排到最后 —— 那正好会烧掉永久额度。
func TestTierOfKnownStatesAreDistinct(t *testing.T) {
	if tierOf(20000, 0) != tierPaid {
		t.Error("daily=0 且已问过 ⇒ 确认没有免费额度")
	}
	if tierOf(0, 0) != tierPaid {
		t.Error("两个账本都空且已问过 ⇒ 同样是确认没有（不是不确定）")
	}
	if tierOf(26434, 1434) != tierFree {
		t.Error("daily>0 ⇒ 确认还有免费额度")
	}
}
