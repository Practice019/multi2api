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
// # ⚠⚠ 第二次报障：真正的问题是"新鲜度是相对墙上时钟的"
//
// 用户报：某个号免费额度已经是 0，别的号还有免费额度，它却仍被使用、
// 永久额度一直在减。
//
// 我第一版用 60 秒 TTL 判断新鲜度，而这**抓不住真正的问题**：
//
//	某账号的免费额度是**被我们自己的请求消耗掉的**。它服务完一次之后，
//	缓存里仍然写着"0 档（还有 2000 免费）" —— 那个值在写入时是对的，
//	而现在**因为它刚被用过**已经不成立了。60 秒 TTL 之内它一直"新鲜但已经错了"，
//	于是整个突发期内它继续被优先选中，把永久额度烧掉。
//
// 隔离实验复现（两个号：A 只有 2000 免费、B 有 20000，每次扣 2000）：
//
//	#1 A 服务（正确 —— 它当时还有 2000）
//	#2 B 服务
//	#3 A 服务 ← ✗ B 还有 18000 免费，却用了 A 的永久额度
//	#5 #7 #9 #11 同上；最终 A 烧掉 9000 永久，而 B 还剩 8000 免费
//
// 修法两条，都要有：
//
//	① **用完就标脏**：`Chat` 一返回就把该账号的档位缓存删掉（见 invalidateTier）。
//	   它下次被看到时落到「不确定」档 —— 而在"还有确认有免费额度的号"时，
//	   不确定的号不会被优先选中。
//	② 档位顺序把「不确定」放在「确认只剩永久」前面（见上）。
//
// 两条合起来，复现用例里 #3 那一步会去用 B（唯一"确认还有免费额度"的号）。
package loomy

import (
	"context"
	"log"
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
//
// ⚠ 它**不**适用于"已确认只剩永久"那一档，见 tierPaidRecheck。
const tierCacheTTL = 60 * time.Second

// tierPaidRecheck 「已确认只剩永久」在**同一个自然日内**的复查周期。
//
// # 为什么不能沿用 60 秒的 tierCacheTTL（用户报障）
//
// `tierCacheTTL` 是为"免费额度可能被别处消耗掉"设计的 —— 那种情形下
// 结论会**变坏**，所以要勤查。而"只剩永久"这个结论在同一个自然日内
// **不会变好**：赠送额度只按自然日重置，所以它今天不可能变回"有"。
//
// 每 60 秒复查一次只会换来一串注定相同的答案，而上游那份接口一抖
// （实测 502 / 非 JSON 回执），或者我们自己刚用过它一次，就会把它从
// tierPaid(2) 降级成 tierUnsure(1) —— 而 1 排在 2 **前面**：
//
//	结果：一个已知没有免费额度的号，**因为缓存过期或刚被用过而变得更优先**，
//	于是在"所有号的免费额度都耗尽"之后反复抢走选号机会。
//
// 30 分钟是个折中：既能在"我们当初读错了"时当天自愈，又不会因为上游
// 一抖就反复降级。自然日一变（见 CandidateTiers 的 day 判据）立刻重新评估。
const tierPaidRecheck = 30 * time.Minute

// naturalDay 返回某时刻所属的自然日（本地时区，"2006-01-02"）。
//
// Loomy 的赠送额度**按自然日重置**为 5000，所以"档位"这个结论天然
// 带一个自然日的作用域：
//
//	"还剩免费额度"  同一自然日内怎么查都还是"有"
//	"只剩永久额度"  同一自然日内**不会**变回"有"
//
// 于是"只剩永久"是**当天有效**的结论，不该因为缓存过了 60 秒就被降级。
//
// ⚠ 用本地时区而不是 UTC：上游按它的服务端时区重置，而我们看不到它。
// 差几小时的唯一后果是自然日边界附近那一小段会多查几次（保守方向），
// 不会漏判；而"该重查时不重查"那种错由 tierPaidRecheck 兜住。
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
type tierCache struct {
	mu   sync.RWMutex
	m    map[string]tierEntry
	busy map[string]bool // 正在刷新的 uid（去重）
}

func newTierCache() *tierCache {
	return &tierCache{m: map[string]tierEntry{}, busy: map[string]bool{}}
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

// claim 尝试占住一个 uid 的刷新权（并发去重）。
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
// **纯内存**：不查网络、不读磁盘。调用方在持有账号池锁时调用它。
//
// 缓存未命中或已过期的 uid ⇒ 报 tierUnsure（**不是**把过期值当当前值报出去），
// 同时排一次异步刷新。这是第三次报障的修法之一 —— 见文件头。
func (p *Provider) CandidateTiers(model string, uids []string) map[string]int {
	_ = model // 两个账本与模型无关；保留参数是为了契约一致
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
			out[uid] = tierUnsure // 从没查到
			stale = append(stale, uid)
			continue
		}
		// ⚠⚠「已确认只剩永久」在**同一个自然日内**是个稳定结论。
		//
		// 赠送额度只按自然日重置，所以它这一天里**不会**变回"有免费额度"。
		// 因此**不能**因为缓存过了 TTL 就把它降级成"不确定" ——
		// tierUnsure(1) 排在 tierPaid(2) **前面**，降级等于让一个已知
		// 没有免费额度的号**因为缓存过期而变得更优先**。
		//
		// 但仍按 tierPaidRecheck 复查一次：万一我们当初**读错了**
		//（例如把 dailyBalance 的缺失读成了 0），当天也要能自愈，
		// 而不是一错一整天。
		if e.tier == tierPaid && e.day == naturalDay(now) {
			if now.Sub(e.at) >= tierPaidRecheck {
				stale = append(stale, uid)
			}
			out[uid] = tierPaid
			continue
		}
		if now.Sub(e.at) >= tierCacheTTL || e.day != naturalDay(now) {
			// ⚠ **不能**报 e.tier：它描述的是"上一次问的时候"。
			// 报出去就等于拿一个旧结论当新事实用 —— 那正是烧永久额度的形态。
			out[uid] = tierUnsure
			stale = append(stale, uid)
			continue
		}
		out[uid] = e.tier
	}
	if len(stale) > 0 {
		p.refreshTiersAsync(stale)
	}
	return out
}

// invalidateTier 把一个账号的档位缓存**删掉**：它的余额刚刚变过。
//
// # 谁调用它（这条是本次修复的核心）
//
// `Provider.Chat` —— 每次对话返回后（成功或失败都算）都调一次。
// 理由：那个账号的余额**一定**变了（我们刚花了它的额度），
// 而缓存里那个数记的是"花之前"的状态。
//
// 不这么做的话，一个号的免费额度被我们用完后，缓存仍说它"确认还有免费额度"，
// 于是整个 TTL 窗口内它继续被优先选中，把永久额度烧掉 ——
// 用户实测报的就是这个（隔离实验也复现了：见文件头那段序列）。
//
// 删掉之后它落到 tierUnsure：只要还有**别的**账号是"确认有免费额度"，
// 这个号就不会被优先选中。等异步刷新回来，它的新状态（tierPaid）落定。
func (p *Provider) invalidateTier(uid string) {
	if p == nil || uid == "" {
		return
	}
	// ⚠⚠「已确认只剩永久」**不能**因为"刚被用过"就标脏（用户报障的一半）。
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
	if e, ok := p.tiers.get(uid); ok && e.tier == tierPaid && e.day == naturalDay(time.Now()) {
		return
	}
	p.tiers.drop(uid)
	p.refreshTiersAsync([]string{uid})
}

// refreshTiersAsync 后台刷新若干 uid 的档位（**不阻塞调用方**）。
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
			if r := recover(); r != nil {
				log.Printf("loomy: 档位刷新 panic（已忽略）: %v", r)
			}
			for _, uid := range todo {
				p.tiers.release(uid)
			}
		}()
		for _, uid := range todo {
			p.refreshTierOne(uid, false)
		}
	}()
}

// refreshTierOne 查一个账号的余额并把档位写进缓存。
//
// 查不到时**不写缓存**（保持"不确定"）—— 写一个档位进去会让它在 TTL 内
// 被当成"已确认"，而那个号可能已经回血（或者刚刚才失败过）。
//
// quiet=true 时不逐条打日志（后台预热用；它改成打一行汇总）。
// 理由：后台预热会**周期性**重试——某个号 session 死了就会每轮失败一次，
// 逐条打日志会变成每分钟两条噪音，把真正的信号淹掉。
func (p *Provider) refreshTierOne(uid string, quiet bool) {
	a := p.authByUID(uid)
	if a == nil {
		return // 该 uid 不属于本上游（或凭证目录里没有）—— 不是错误
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// ⚠ 用 PointsSnapshot 而**不是** Balance（本次修的根因之一）。
	//
	// `Balance` 是 `PointsSnapshot` 的薄封装，它把这面旗帜丢掉了：
	//
	//	return pts.Available, pts.Daily, nil     // ← DailyKnown 丢了
	//
	// 而 `Points.DailyKnown` 的存在理由就是这条（见 points.go 那段注释：
	// "只回 int64 的话字段缺失会被读成 0 …… 这是把未知说成确定的零"）。
	pts, err := p.PointsSnapshot(ctx, a.Session)
	if err != nil {
		if !quiet {
			log.Printf("loomy: 档位刷新失败 uid=%s: %v（保持上次结论）", shortUID(uid), err)
		}
		return
	}
	p.tiers.set(uid, tierOfPoints(pts))
}

// tierOfPoints 由**三态**余额算档位。
//
// # ⚠ 为什么必须看 DailyKnown（本次修的根因）
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

// tierWarmupInterval 后台预热的间隔。
//
// 它只补齐"未知或过期"的账号，**全部新鲜时一轮查询都不发**（只读一次凭证
// 目录）。所以这个间隔决定的是"一个号从变成未知到被补上"的上限，
// 而不是上游压力 —— 30 秒是个够快又不浪费的值。
// ⚠ 它是 var 而不是 const，**只为测试**：测试要把它调到几十毫秒，
// 才可能在不睡 30 秒的前提下验证「周期补齐真的会发生」。生产代码不要改它。
var tierWarmupInterval = 30 * time.Second

// StartTierWarmup 启动后台档位预热（装配层在启动期调用一次；幂等）。
//
// # 为什么需要它（这是用户实测到的那个 -30 永久积分的根因）
//
// 档位缓存是**纯内存**的，重启后是空的。空缓存里每个账号都是「不确定」档，
// 而"不确定"之间**没有优先级** —— 选号退化成加权抽签。
//
// 用户实测的形态：他 10-13 14:22 重启过实例，之后 22 小时里唯一一次计费，
// 记在了那唯一一个免费额度为 0 的号上（永久积分 -30），而另外 21 个号都还有
// 免费额度。**那一次是重启后缓存空导致的，本来可以避免。**
//
// 与"第一次选号时顺带异步刷新"的区别：
//
//	那个是**请求驱动**的 —— 第一次抽签已经发生了才去刷新，
//	所以第一次必然没有偏好；
//	这个是**启动驱动**的 —— 在第一个请求到达之前就把档位准备好。
//
// 三个来源都靠它自愈：
//
//	① 进程重启（缓存空）
//	② 新导入的账号（首次出现在候选里之前就被补上）
//	③ 之前刷新失败的账号（周期重试）
//
// ⚠ 不阻塞启动：全部查询在一个后台 goroutine 里**串行**做完。
// 串行是刻意的：34 个号并发打上游容易被限流，而那会把"预热"变成"批量失败"。
func (p *Provider) StartTierWarmup() {
	if p == nil {
		return
	}
	p.warmupOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("loomy: 档位预热 panic（已忽略）: %v", r)
				}
			}()
			p.warmupPass() // 立即一遍：第一个请求到达之前就把档位准备好
			t := time.NewTicker(tierWarmupInterval)
			defer t.Stop()
			for range t.C {
				p.warmupPass()
			}
		}()
	})
}

// warmupPass 把"未知或过期"的账号补齐，并打一行汇总。
//
// 全程新鲜时**一次上游查询都不发** —— 所以空闲部署的周期成本只是一次
// 目录读取。这条很重要：预热器不该在没人用的时候白白打上游。
func (p *Provider) warmupPass() {
	refs := p.accounts()
	if len(refs) == 0 {
		return
	}
	now := time.Now()
	var todo []string
	for _, r := range refs {
		e, ok := p.tiers.get(r.UID)
		if !ok || now.Sub(e.at) >= tierCacheTTL {
			todo = append(todo, r.UID)
		}
	}
	if len(todo) == 0 {
		return // 全都新鲜：不发查询、不打日志
	}
	okN := 0
	for _, uid := range todo {
		before, _ := p.tiers.get(uid)
		p.refreshTierOne(uid, true)
		if e, ok := p.tiers.get(uid); ok && (!before.at.Equal(e.at)) {
			okN++
		}
	}
	// 一行汇总（而不是逐条）：失败的那几个已经在上面被 quiet 掉了，
	// 但"有几个没补上"必须可见 —— 否则预热的失效是静默的。
	if okN < len(todo) {
		log.Printf("loomy: 档位预热 %d/%d 个账号（其余查不到，保持不确定）", okN, len(todo))
	} else {
		log.Printf("loomy: 档位预热完成 %d 个账号", okN)
	}
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
