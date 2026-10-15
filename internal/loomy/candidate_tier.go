// candidate_tier.go Loomy 实现 gateway.CandidateTierExt —— 「候选账号的优先级分档」。
//
// # 用户的需求（原话）
//
//	「Loomy 每一个账号每天都会送 5000 的免费额度，但是如果这个免费额度
//	  没有用完，明天还是重置 5000 的免费额度，也就是不会保留。所以我希望
//	  调用 Loomy 账号的时候，优先使用免费额度，轮询所有账号，把所有账号的
//	  免费额度用完之后，它才能用永久的额度。」
//
// # 为什么必须"在选号时"分档，不能等上游报错
//
// Loomy 额度耗尽时**不报错**：服务端静默降级 —— 继续扣永久积分、照常返回 200。
// 所以本仓既有的"按限流错误换号"机制对它**完全无效**。
//
// # 两个账本
//
// 实测三个字段的关系（见 points.go 的文件头）：
//
//	balance           25000  永久额度（邀请/充值累积，不随自然日重置）
//	dailyBalance       1434  今日赠送剩余（服务端按自然日重置为 5000）
//	availableBalance  26434  = balance + dailyBalance
//
// # 档位：**三档**，而"不确定"排在"确认只剩永久"**前面**
//
//	0  确认还有免费额度      → 优先用它
//	1  **不确定**            → 刚被用过 / 缓存过期 / 从没查到
//	2  确认只剩永久额度      → 只有在没有任何"可能还有免费额度"的账号时才用
//
// ⚠⚠ 这条顺序与开源参照实现**相反**（它把"查询失败"放最后一档，理由是
// "宁可先用能确认余额的账号"）—— 那是**稳健性**口径。而本用户的诉求是
// **别烧永久积分**：一个"不确定"的账号**可能**还有免费额度，而一个
// "确认只剩永久"的账号**一定**没有。先在不确定的那些里找，才符合用户要的顺序。
//
// ⚠ 代价如实标注：不确定的账号里也可能恰好是个坏号（session 失效）。
// 那种情况下这次请求失败 → 核心换号重试（**不扣永久积分**）。
// 而顺序反过来时的代价是**直接烧掉用户明确不想烧的永久额度**。两者不对等。
//
// # ⚠⚠ 第三次报障（2026-10-15）：查询量必须回到"按需"
//
// 用户原话：*「不要预热。为什么要预热呢？我们现在查询太多、太频繁了。
// 把这个弄成之前的逻辑，之前的逻辑挺好的，就是按需查，也不要频繁查。」*
//
// 前两次修的是"档位判定对不对"（新鲜度、三态），这两条都保留。第三次是
// **查询量本身**：分档把 `/points/records` 的调用从"按需"改成了"高频"。
//
// 数一下打那个端点的调用方，就能看清这件事：
//
//	候选分档之前  只有 `RefreshQuota` 一个 —— 它**按需**：
//	             用户打开额度/管理面板才查一次
//	分档之后又加了两个：
//	             ① CandidateTiers 里每次抽号都触发一轮异步刷新
//	                （本函数的契约是"纯内存、不查网络"，调用方还**持着池子锁**）
//	             ② 启动期 + 每 30 秒把所有账号刷一遍（预热）
//
// 上游实测很慢（单次 1.8~23.7 秒，并发 12 条就出 502），于是新查询量
// 把它压成了"全是查询失败"：两次报障现场那几十行 `档位刷新失败` 就是这么来的。
//
// 所以现在**只有一条查询路径**：
//
//	RefreshQuota —— 按需（本来就查一次余额，顺手回填档位缓存，见 quota_ext.go）
//
// `invalidateTier` 仍然保留，但它只**标脏**、**不发请求** —— "刚用过"这件事
// 只需要让旧档位不再可信，不需要马上去问一遍。
package loomy

import (
	"sync"
	"time"
)

// 档位号（**数字越小越优先**，见 gateway.CandidateTierExt 的契约）。
const (
	// tierFree 已确认还有今日赠送额度 —— 用它不消耗永久积分。只有它会被优先。
	tierFree = 0
	// tierUnsure 不确定：刚被用过（余额一定变了）、缓存过期、或从没查到。
	//
	// 排在"确认只剩永久"之前 —— 理由见文件头（用户要的是"别烧永久积分"，
	// 而这个档里的账号**可能**还有免费额度）。
	tierUnsure = 1
	// tierPaid 已确认没有免费额度（只剩永久，或两个账本都空）。
	tierPaid = 2
)

// tierCacheTTL 档位缓存的存活期。
//
// 它只兜底"我们没用过这个号、但上游状态被别处改变了"的情形（例如用户
// 自己在 Loomy 客户端里消耗、或自然日重置）。**被我们自己用掉**那一种
// 由 invalidateTier 精确处理，不依赖这个值 —— 所以它不需要很短。
const tierCacheTTL = 60 * time.Second

// naturalDay 返回某时刻所属的自然日（本地时区，"2006-01-02"）。
//
// Loomy 的赠送额度**按自然日重置**为 5000，所以"档位"这个结论天然
// 带一个自然日的作用域：
//
//	"还剩免费额度"  同一自然日内怎么查都还是"有"
//	"只剩永久额度"  同一自然日内**不会**变回"有"
//
// ⚠ 用本地时区而不是 UTC：上游按它的服务端时区重置，而我们看不到它。
// 差几小时的唯一后果是自然日边界附近那一小段会判错方向，而那种偏差只
// 影响"先用哪个号"这个偏好，不阻断转发 —— 可以接受。
func naturalDay(t time.Time) string { return t.Format("2006-01-02") }

// tierEntry 一个账号的档位、取到的时刻、以及它属于哪个自然日。
type tierEntry struct {
	tier int
	at   time.Time
	// day 得出这个结论时的自然日（见 naturalDay）。
	//
	// ⚠ 它只对 tierPaid 有意义（那是唯一"当天不会反悔"的结论）。
	// 加这个字段而不是靠 at 推算：at 是"取到的时刻"，而 day 是
	// "这个结论属于哪一天" —— 两者在自然日边界附近会不同（23:59 取的
	// paid 结论，00:01 就作废了）。
	day string
}

// tierCache 档位缓存。
//
// # ⚠ 它刻意**只有读缓存**（第三次报障的结论）
//
// 它曾经还有一个 `busy`（在途去重）与一个 `fail`（失败负缓存），那是为
// "从抽号路径发起网络刷新"配套的。那条路已经拆掉了（见文件头），所以
// 这两个计数器不再需要 —— 留着只会让人以为"这里还会发请求"。
//
// 现在唯一往这里**写**的是两处：
//
//	RefreshQuota  按需（管理端「刷新额度」）：本来就查一次余额，顺手回填
//	invalidateTier 只用过之后**标脏**（删缓存，不发请求）
type tierCache struct {
	mu sync.RWMutex
	m  map[string]tierEntry
}

func newTierCache() *tierCache {
	return &tierCache{m: map[string]tierEntry{}}
}

func (c *tierCache) get(uid string) (tierEntry, bool) {
	if c == nil {
		return tierEntry{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[uid]
	return e, ok
}

func (c *tierCache) set(uid string, tier int) {
	if c == nil || uid == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.m[uid] = tierEntry{tier: tier, at: now, day: naturalDay(now)}
	c.mu.Unlock()
}

// drop 删掉一个账号的档位（见 invalidateTier）。
func (c *tierCache) drop(uid string) {
	if c == nil || uid == "" {
		return
	}
	c.mu.Lock()
	delete(c.m, uid)
	c.mu.Unlock()
}

// CandidateTiers 返回候选 uid 的档位（gateway.CandidateTierExt）。
//
// **纯内存**：不查网络、不读磁盘。调用方在持有账号池锁时调用它。
//
// # ⚠⚠ 缓存未命中/过期/刚被用过 ⇒ 报 tierUnsure，**并且就此为止**
//
// 改造前这里在报 tierUnsure 之后会顺手发起一轮异步刷新
// （`if len(stale) > 0 { p.refreshTiersAsync(stale) }`）。那让"每次抽号"
// 等于"一轮上游查询"，而上游实测很慢（1.8~23.7 秒）—— 用户实测的
// *「查询太多、太频繁」* 就是这个。现在只读缓存，见文件头。
//
// 缓存没命中**不是**错误：它只说明"我们这次不知道"，而档位顺序把
// "不确定"排在"确认只剩永久"前面，所以这个号仍然会被用到（只是不优先）。
// 真正的补齐靠**按需**的 `RefreshQuota`。
func (p *Provider) CandidateTiers(model string, uids []string) map[string]int {
	_ = model // 两个账本与模型无关；保留参数是为了契约一致
	if p == nil || len(uids) == 0 {
		return nil
	}
	now := time.Now()
	have, zero := p.pool().snapshot(now)

	out := make(map[string]int, len(uids))

	// 池子空了 = **所有已知账号**的免费额度都用完了 ⇒ 允许动永久额度。
	//（用户原话：「当所有的这个轮询池都没有账号了之后，那就说明所有的免费
	//  额度都没了，那再把那个所有的账号再重新添加回去」。）
	//
	// ⚠ 判据是「len(zero) > 0」而不是只看「len(have) == 0」：
	// 一个账号都还没查过时 have 也是空的，但那时**不能**说"免费额度用完了"
	// —— 那会正好烧在用户最不想烧的地方（启动后第一波请求）。
	// 那种情况落 tierUnsure（不确定），不构成偏好，但也不"确认可以烧"。
	empty := len(have) == 0 && len(zero) > 0

	for _, uid := range uids {
		if uid == "" {
			continue
		}
		switch {
		case have[uid]:
			out[uid] = tierFree
		case empty:
			out[uid] = tierFree // 池子空了：全体放开（含已知为 0 的那些）
		case zero[uid]:
			out[uid] = tierPaid
		default:
			out[uid] = tierUnsure // 还没查过
		}
	}
	return out
}

// invalidateTier 把一个账号的档位缓存**删掉**：它的余额刚刚变过。
//
// # 谁调用它
//
// `Provider.Chat` —— 每次对话返回后（成功或失败都算）都调一次。
// 理由：那个账号的余额**一定**变了（我们刚花了它的额度），
// 而缓存里那个数记的是"花之前"的状态。
//
// 不这么做的话，一个号的免费额度被我们用完后，缓存仍说它"确认还有免费额度"，
// 于是整个 TTL 窗口内它继续被优先选中，把永久额度烧掉 ——
// 用户实测报的就是这个（隔离实验也复现了：见文件头那段序列）。
//
// # ⚠ 它**只标脏，不发请求**（第三次报障的修法）
//
// 改造后它后面还跟着一句 `p.refreshTiersAsync([]string{uid})` ——
// 那让"每次对话"都多一次上游查询。而"刚用过"这件事只需要让旧档位
// 不再可信：删掉之后它落到 tierUnsure，只要还有**别的**账号是"确认有
// 免费额度"，这个号就不会被优先选中。下次真正需要它的档位时，
// 按需的 `RefreshQuota` 会把它问清楚。
func (p *Provider) invalidateTier(uid string) {
	if p == nil || uid == "" {
		return
	}
	// ⚠⚠「已确认只剩永久」**不能**因为"刚被用过"就标脏。
	//
	// 旧实现无条件 drop，于是"用过一次"就把它降级成 tierUnsure(1) ——
	// 而 tierUnsure 排在 tierPaid(2) **前面**：一个已知没有免费额度的号
	// **因为刚被用过而变得更优先**。
	//
	// 在"所有号的免费额度都耗尽"之后（那正是用户规则里允许烧永久的时刻），
	// 它们全在 unsure/paid 之间反复，本已不构成偏好；而这个 drop 让
	// "刚用过的那个"额外获得了优先权 —— 于是它反复被选中、永久额度一路减。
	//
	// ⚠ 为什么"用它"不会改变这个结论：用它只会消耗**赠送**额度（用完了
	// 就落到永久），而赠送额度**只按自然日重置**。所以"只剩永久"这件事
	// 在今天之内不会因为被用而变。这正是它与其他档位的关键区别 ——
	// tierFree 用完就**必须**标脏（它可能刚刚变成 paid）。
	// ⚠ 改成挂"需要重查"的标记，而**不再删缓存、也不在这里发请求**。
	//
	// 旧实现在这里 drop + 立刻异步查一次 —— 那等于"每次对话一次上游查询"，
	// 正是用户报的"查询太多太频繁"。现在只挂标记，查询由池子按
	// freeRecheckEvery 节流、并带失败退避（见 freepool.go 的 noteUsed）。
	p.pool().noteUsed(uid, time.Now())
	go p.drainRecheck()
}

// tierOfPoints 由**三态**余额算档位。
//
// # ⚠ 为什么必须看 DailyKnown（这是修过的根因之一，保留）
//
// `dailyBalance` 字段**缺失**时 `Daily` 是 0，而 0 与"确实是 0"是两件事。
// 只回 int64 的实现会把"没读到"读成"免费额度已用完" —— 于是这个号被判成
// **tierPaid（确认只剩永久）**，与**真正**用完免费额度的号**同档**。
//
// 硬分档（pickForTier 只留最优那一档）因此选不出任何差别：一个**还有**
// 免费额度的号，会被与"该烧永久"的号一起抽中 —— 这正是用户报的
// *「赠送积分没有了还是会调用，优先免费额度的功能没起作用」*。
//
// 落 tierUnsure 才是如实的：它**可能**还有免费额度，而"不确定"档在
// "已确认还有免费额度"的号面前不会胜出（档位顺序 0 < 1 < 2，见文件头）。
func tierOfPoints(pts Points) int {
	if !pts.DailyKnown {
		// 没读到"今日赠送剩余" ⇒ 不能说它"只剩永久"。
		return tierUnsure
	}
	return tierOf(pts.Available, pts.Daily)
}

// tierOf 由 (可用总额度, 今日剩余) 算出一个**已知**余额的档位。
//
//	available = 永久 + 今日（实测关系，见 points.go）
//	所以 daily == 0 时 available 就是永久额度。
//
// 只有两种结果：还有免费额度 / 已经没有。第三个档（不确定）不是由余额算出来的，
// 而是"不知道余额"时由 CandidateTiers 直接给的 —— 两者刻意分开：
// 混在一起会让"查不到"被当成"确认没有"，而那正好会烧掉永久额度。
func tierOf(available, daily int64) int {
	if daily > 0 {
		return tierFree
	}
	return tierPaid
}

// 编译期断言：Provider 实现候选分档扩展点。
//
// 缺了它不会编译失败，而是**静默退化**：核心拿不到档位 ⇒ 保留全部候选
// ⇒ 用户的规则完全不生效，而界面上看不出任何异常。
var _ interface {
	CandidateTiers(model string, uids []string) map[string]int
} = (*Provider)(nil)
