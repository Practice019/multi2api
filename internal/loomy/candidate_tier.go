// candidate_tier.go Loomy 实现 gateway.CandidateTierExt —— 「候选账号的优先级分档」。
//
// # 用户的需求（原话）
//
//	「Loomy 每一个账号每天都会送 5000 的免费额度，但是如果这个免费额度
//	  没有用完，明天还是重置 5000 的免费额度，也就是不会保留。所以我希望
//	  调用 Loomy 账号的时候，优先使用免费额度，轮询所有账号，把所有账号的
//	  免费额度用完之后，它才能用永久的额度。如果说有一个账号的免费额度用完了，
//	  那就轮询下一个账号，直到所有的账号免费额度都没有了，才能用永久额度。」
//
// # 为什么必须"在选号时"分档，而不能等上游报错
//
// Loomy 额度耗尽时**不报错**：服务端静默降级 —— 继续扣永久积分、照常返回 200。
// 所以本仓既有的"按限流错误换号"机制对它**完全无效**：它会一直烧同一个号的
// 永久积分，而核心永远不会知道该换号。
//
// 要在这种上游上实现用户的规则，唯一的位置就是**选号时按余额把候选分开**。
//
// # 两个账本与档位的对应（这是本文件的核心）
//
// 实测三个字段的关系（见 points.go 的文件头）：
//
//	balance           25000  永久额度（邀请/充值累积，不随自然日重置）
//	dailyBalance       1434  今日赠送剩余（服务端按自然日重置为 5000）
//	availableBalance  26434  = balance + dailyBalance
//
// 因为 available = 永久 + 今日，所以**只看 (available, daily) 就能分档**：
//
//	daily > 0            → 0 档：还有免费额度，用它不花永久积分
//	daily == 0 且 available > 0 → 1 档：只剩永久额度，用它是真的在花钱
//	其余（两个都为 0 / 查不到）  → 2 档：与"未知"同档
//
// 第 3 条刻意把"余额为 0"与"查不到"放**同一档**（用户明确要求）：
// 宁可先用能确认余额的账号。把"查不到"当最优会让网关拿不准的号去烧额度。
//
// # ⚠ 为什么不能在这里发网络请求（这条决定了整个实现形状）
//
// 核心在**持有账号池锁**的状态下调用 CandidateTiers（见 pool.pickFor）。
// 若这里逐个账号查余额（N 次 HTTP × 每次超时），会把整个池子的选号
// 串行阻塞住 —— 等于网关停摆。
//
// 所以：
//
//	本方法**只读缓存**（纯内存 map，微秒级）
//	缓存由**异步刷新**填充（fire-and-forget，不阻塞调用方）
//	同时 RefreshQuota（管理端「刷新额度」）也顺带回填 —— 那条路径本来就要查余额
//
// 冷缓存时的行为是**优雅降级**：所有 uid 都回 2 档 ⇒ 核心保留全部候选
// ⇒ 与改造前**逐字节相同**。等异步刷新回来（秒级）就自动开始分档。
// 这个"第一拍不分档"是刻意的取舍：宁可晚一拍生效，也不阻塞选号。
package loomy

import (
	"context"
	"log"
	"sync"
	"time"
)

// 档位号（**数字越小越优先**，见 gateway.CandidateTierExt 的契约）。
const (
	// tierDaily 还有今日赠送额度 —— 用它不消耗永久积分。
	tierDaily = 0
	// tierPermanent 今日赠送已用尽、只剩永久额度 —— 用它是真的在花钱。
	tierPermanent = 1
	// tierUnknown 两个账本都空，或**查不到**（同一档，见文件头）。
	tierUnknown = 2
)

// tierCacheTTL 档位缓存的存活期。
//
// 60 秒：与参照实现同量级。它要平衡两件事 ——
//
//	太短 → 每次选号都可能触发一遍全账号查询（N 次网络往返）
//	太长 → 免费额度用完到"换号"之间有明显滞后（用户会看到"明明还有额度却换了号"
//	       或"用完了还在用同一个号"）
//
// 60 秒的滞后是可接受的：用户的诉求是"别烧永久积分"，晚一分钟换号不影响它。
const tierCacheTTL = 60 * time.Second

// tierEntry 一个账号的档位与取到的时刻。
type tierEntry struct {
	tier int
	at   time.Time
}

// tierCache 档位缓存。
//
// 两个 map 各自被 mu 保护：
//
//	m     uid → 档位（读多写少）
//	busy  uid → 正在刷新中（去重，避免同一账号被反复并发查询）
type tierCache struct {
	mu   sync.RWMutex
	m    map[string]tierEntry
	busy map[string]bool
}

func newTierCache() *tierCache {
	return &tierCache{m: map[string]tierEntry{}, busy: map[string]bool{}}
}

// get 读一个 uid 的档位（**纯内存**，调用方可能持有池锁）。
func (c *tierCache) get(uid string) (tierEntry, bool) {
	if c == nil {
		return tierEntry{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[uid]
	return e, ok
}

// set 写入档位。
func (c *tierCache) set(uid string, tier int) {
	if c == nil || uid == "" {
		return
	}
	c.mu.Lock()
	c.m[uid] = tierEntry{tier: tier, at: time.Now()}
	c.mu.Unlock()
}

// claim 尝试占住一个 uid 的刷新权；已被别人占住则返回 false。
//
// 这一个方法就是"异步刷新的去重"：并发调用 CandidateTiers 时，
// 同一个 uid 只会有一个刷新在飞。
func (c *tierCache) claim(uid string) bool {
	if c == nil || uid == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[uid] {
		return false
	}
	c.busy[uid] = true
	return true
}

// release 释放刷新权。
func (c *tierCache) release(uid string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.busy, uid)
	c.mu.Unlock()
}

// CandidateTiers 返回候选 uid 的档位（gateway.CandidateTierExt）。
//
// # 三条契约（见 gateway/candidate_tier.go）
//
//	数字越小越优先；未出现在返回 map 里的 uid 视为最低优先级
//	**档内不排序** —— 本方法只回答"谁属于哪档"
//	查不到归**最后的档**
//
// # 性能契约（这是本方法存在的形状）
//
// **纯内存**：不查网络、不读磁盘。调用方在持有池锁时调用它。
// 缓存未命中的 uid 会被排进一次**异步**刷新，本拍先如实回答"未知"（2 档）。
//
// model 参数本上游用不上（两个账本与模型无关），保留是为了契约一致。
func (p *Provider) CandidateTiers(model string, uids []string) map[string]int {
	_ = model
	if p == nil || len(uids) == 0 {
		return nil
	}
	out := make(map[string]int, len(uids))
	now := time.Now()
	var stale []string
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		e, ok := p.tiers.get(uid)
		if !ok {
			// 从未查过：如实回答"未知"（2 档），并排一次刷新。
			out[uid] = tierUnknown
			stale = append(stale, uid)
			continue
		}
		out[uid] = e.tier
		if now.Sub(e.at) >= tierCacheTTL {
			stale = append(stale, uid) // 过期：先用旧值，同时后台刷新
		}
	}
	if len(stale) > 0 {
		p.refreshTiersAsync(stale)
	}
	return out
}

// refreshTiersAsync 后台刷新若干 uid 的档位（**不阻塞调用方**）。
//
// 用 claim/release 去重：同一个 uid 同时只会有一个刷新在飞，
// 所以高频选号不会把上游打爆。
func (p *Provider) refreshTiersAsync(uids []string) {
	todo := make([]string, 0, len(uids))
	for _, uid := range uids {
		if p.tiers.claim(uid) {
			todo = append(todo, uid)
		}
	}
	if len(todo) == 0 {
		return
	}
	go func() {
		defer func() {
			// 兜住上游实现的 panic：刷新是后台活，崩了不该带走进程。
			if r := recover(); r != nil {
				log.Printf("loomy: 档位刷新 panic（已忽略）: %v", r)
			}
			for _, uid := range todo {
				p.tiers.release(uid)
			}
		}()
		for _, uid := range todo {
			p.refreshTierOne(uid)
		}
	}()
}

// refreshTierOne 查一个账号的余额并把档位写进缓存。
//
// 查不到时**不写缓存**（保持"未知"）—— 写一个 2 档进去会让它
// 在 TTL 内被当成"确认过是最后档"，而下次刷新前它可能已经回血。
// 不写就等于"下次还会再试"，代价只是每拍多一次 claim。
func (p *Provider) refreshTierOne(uid string) {
	a := p.authByUID(uid)
	if a == nil {
		return // 该 uid 不属于本上游（或凭证目录里没有）—— 不是错误
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bal, daily, err := p.Balance(ctx, a.Session)
	if err != nil {
		log.Printf("loomy: 档位刷新失败 uid=%s: %v（保持未知）", shortUID(uid), err)
		return
	}
	p.tiers.set(uid, tierOf(bal, daily))
}

// tierOf 由 (可用总额度, 今日剩余) 算出档位。
//
// 纯函数，单独拿出来是为了能被直接断言 —— 分档规则是这个需求的全部内容，
// 不该只能通过"发一个假请求再看选号结果"来验证。
//
//	available = 永久 + 今日（实测关系，见 points.go）
//	所以 daily == 0 时 available 就是永久额度。
func tierOf(available, daily int64) int {
	switch {
	case daily > 0:
		return tierDaily
	case available > 0:
		return tierPermanent
	default:
		return tierUnknown
	}
}

// 编译期断言：Provider 实现候选分档扩展点。
//
// 缺了它不会编译失败，而是**静默退化**：核心拿不到档位 ⇒ 保留全部候选
// ⇒ 用户的规则完全不生效，而界面上看不出任何异常。
// 与本包其它扩展点同一条理由（见 provider.go 顶部那份清单）。
var _ interface {
	CandidateTiers(model string, uids []string) map[string]int
} = (*Provider)(nil)
