package loomy

import (
	"log"
	"sort"
	"sync"
	"time"
)

// freepool.go 免费额度轮询池 —— 用户设计的实现。
//
// # 用户的要求（原话）
//
//	「我们不是要求优先使用免费的额度吗？免费额度使用，所有账号的免费额度使用
//	  完之后才能使用永久的额度，现在好像还是做不到… 因为我们是可以查询每个
//	  账号它的免费额度是多少的，当那个免费额度是0的时候，那就轮询的时候就不
//	  要使用这个账号，就应该有一个池吧，轮询池，然后它就从池子里出去，
//	  然后当所有的这个轮询池都没有账号了之后，那就说明所有的免费额度都没了，
//	  那再把那个所有的账号再重新添加回去。」
//
// # 为什么之前做不到（真实缺陷，2026-10-15）
//
// 改造前档位缓存的**唯一**写入方是 `RefreshQuota`（按需），而 `CandidateTiers`
// 只读不查。于是：
//
//	重启后 / 没人打开过账号池之前 ⇒ 缓存是空的
//	⇒ 35 个账号全是 tierUnsure ⇒ **档位完全相同**
//	⇒ pickForTier 保留唯一那档（= 全部账号）⇒ 完全没有偏好
//	⇒ 永久额度被随机烧掉
//
// 根因是我把"启动时那一次填充"和"每 30 秒周期重刷"**一起**拆掉了。该拆的是
// 后者（那是查询洪水的来源），前者是这条规则成立的前提。
//
// # 池子的语义（与用户描述一一对应）
//
//	成员          = **已知**免费额度 > 0 的账号  ← 只有它们会被优先选取
//	已知为 0      = 从池子里出去
//	池子空了      = 所有已知账号的免费额度都用完了 ⇒ 允许用永久额度
//	                （用户："再把那个所有的账号再重新添加回去"）
//	还没查过      = 不算成员，但也**不算"已用完"** —— 排在中档
//	                （把它当成"已用完"会让一个可能还有免费额度的号被跳过）
//
// # 池子怎么被填上（三条，都不是定时器）
//
//	① 启动时**一次**填充（`StartFreePoolFill`）—— 只跑一次，不挂 ticker
//	② `RefreshQuota`（管理端「刷新额度」/ 账号池页面）—— 用户驱动
//	③ 用过某个账号之后**只重查它一个**（`noteUsed`）—— 用量驱动
//
// ⚠ 这里**刻意没有周期 ticker**：用户明确要求过「不要频繁查」。
// 上面的 ① 是常数级（每次重启 35 个请求一次），③ 与真实用量成正比而不是与
// 时间成正比 —— 空闲部署一个请求都不发。
//
// ⚠ 自然日一变，池子**整体失效**：赠送额度按自然日重置，所以昨天的"已用完"
// 今天不成立。`resetIfNewDay` 在每次读池子前判一次。

// freePool 免费额度轮询池。
type freePool struct {
	mu sync.Mutex
	// have 已知免费额度 > 0 的 uid（池子成员）。
	have map[string]bool
	// zero 已知免费额度 == 0 的 uid。
	//
	// ⚠ 必须与 have 分开存：只看 have 无法区分"已知为 0"与"还没查过"，
	// 而这两者在前端/选号上的含义完全不同。
	zero map[string]bool
	// free 每个 uid **上一次已知的**今日赠送剩余（绝对值）。
	//
	// 它不是为了展示 —— 它是"用对话回执里的 points_consumed 做减法"的基准：
	// 没有绝对值就无法扣减（扣了就是凭空造数）。见 noteConsumed。
	free map[string]int64
	// day 池子所属的自然日；变了就整体清空（见 resetIfNewDay）。
	day string
	// checkedAt 每个 uid 上次**成功查到**余额的时刻（用于重查节流）。
	checkedAt map[string]time.Time
	// failAt 每个 uid 上次查失败的时刻（失败退避，避免"越失败越查"）。
	failAt map[string]time.Time
	// needRecheck 用过、需要重新确认免费额度的 uid。
	needRecheck map[string]bool
	// filling 启动填充是否已经在跑（幂等）。
	filling bool
	// sweepOnce 定期清扫只启动一次（幂等）。
	sweepOnce sync.Once
}

func newFreePool() *freePool {
	return &freePool{
		have:        map[string]bool{},
		zero:        map[string]bool{},
		free:        map[string]int64{},
		day:         naturalDay(time.Now()),
		checkedAt:   map[string]time.Time{},
		failAt:      map[string]time.Time{},
		needRecheck: map[string]bool{},
	}
}

// 重查节流与失败退避（两个都必须有）：
//
//	freeRecheckEvery  同一个账号两次重查之间至少隔这么久 ——
//	                  用它替掉"每次对话都查一次"，把查询量压到按时间摊开
//	freeFailBackoff   查失败之后不再碰它的时长 —— 没有它就会"越失败越查"
const (
	freeRecheckEvery = 90 * time.Second
	freeFailBackoff  = 5 * time.Minute
)

// resetIfNewDay 跨自然日时清空池子（调用方不必持锁）。
//
// 为什么必须清：赠送额度按自然日重置，昨天的"已用完"今天不成立。
// 不清的话，第二天所有账号都会停在"已知为 0"那一档 —— 也就是**永久额度
// 从早用到晚**，正好与用户的要求相反。
func (f *freePool) resetIfNewDay(now time.Time) {
	d := naturalDay(now)
	if f.day == d {
		return
	}
	f.day = d
	f.have = map[string]bool{}
	f.zero = map[string]bool{}
	// free 也要清：它是"今日赠送剩余"的绝对值，跨天必须重新取
	//（昨天的余额今天不成立），不能凭一个过期的基准做减法。
	f.free = map[string]int64{}
	f.checkedAt = map[string]time.Time{}
	f.failAt = map[string]time.Time{}
	f.needRecheck = map[string]bool{}
}

// noteQuota 记录一次**成功的**余额查询结果（daily = 今日赠送剩余）。
//
// ⚠ dailyKnown=false 时**什么都不做**：字段缺失与"确实是 0"是两件事
// （见 points.go 的 Points.DailyKnown）。把"没读到"记成"已用完"，
// 会让一个可能还有免费额度的号被永久跳过。
func (f *freePool) noteQuota(uid string, daily int64, dailyKnown bool, now time.Time) {
	if uid == "" || !dailyKnown {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetIfNewDay(now)
	f.checkedAt[uid] = now
	delete(f.failAt, uid)
	delete(f.needRecheck, uid)
	if daily > 0 {
		f.have[uid] = true
		f.free[uid] = daily // 绝对值基准：之后靠 points_consumed 递减
		delete(f.zero, uid)
		return
	}
	delete(f.have, uid)
	delete(f.free, uid)
	f.zero[uid] = true
}

// noteConsumed 用**对话回执里**的 points_consumed 扣减本地估计值。
//
// # 用户的思路（原话）
//
//	「如果发一次请求就有返回额度的话，那就直接每次发请求之后，我们就把那个
//	  额度给更新掉就行了，把那个缓存给更新掉。那我们就不需要去管那个缓存了，
//	  也就等于我们会自动去更新我们网页上前端那个数据，那不是更快吗？」
//
// 这是"用完一个号就从池子里出去"最便宜的实现：**零额外请求**，而且用的是
// 上游自己报的数（实测与账本吻合：1870 → 1868，两次请求 points_consumed 各 1）。
//
// ⚠ 只减**免费（今日赠送）**那一份：赠送额度按自然日重置；永久额度没有
// "用完"的语义，也不该让账号离开池子。扣除后 ≤ 0 ⇒ 移出池子（用户要的那条）。
//
// ⚠ **不知道绝对值就什么都不做**（见 free 字段）：凭空减等于造数，
// 而造出来的"已用完"会让一个还有免费额度的号被跳过。
//
// ⚠ 漂移如实标注：网关之外的消耗（用户自己在 Loomy 桌面客户端里用）不经过
// 这里，所以这个估计会**偏高**（以为还有免费额度）。定期清扫那条对账路径
// 不能删 —— 它就是兜这个漂移的。
func (f *freePool) noteConsumed(uid string, points int64, now time.Time) {
	if uid == "" || points <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetIfNewDay(now)
	// 只有"已知还有免费额度"的号才扣。未知的不猜 —— 不猜就不会猜错。
	if !f.have[uid] {
		return
	}
	left, ok := f.free[uid]
	if !ok {
		return // 没有绝对值基准 ⇒ 不能减
	}
	left -= points
	if left > 0 {
		f.free[uid] = left
		return
	}
	// 免费额度用完 ⇒ 从池子里出去（用户原话："它就从池子里出去"）。
	delete(f.have, uid)
	delete(f.free, uid)
	f.zero[uid] = true
}

// noteFail 记一次查询失败（失败退避）。
//
// ⚠ **不删 needRecheck 标记** —— 那会让这个号**永远不再被重查**，
// 于是它一直停在"不确定"档。正确的语义是"**过一会儿再试**"，而不是"放弃"：
// 退避由 freeFailBackoff 控制（dueForRecheck 里判），标记要留着。
// 这条是测试抓出来的：第一版就是在这里把标记删了，于是"退避窗口过后应当
// 挑出该重查的账号"永远为空。
func (f *freePool) noteFail(uid string, now time.Time) {
	if uid == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetIfNewDay(now)
	f.failAt[uid] = now
	// 保留 f.needRecheck[uid]：退避窗口一过就要重试。
	f.needRecheck[uid] = true
}

// noteUsed 一个账号刚被用过 ⇒ 它的免费额度可能已经变成 0，需要重新确认。
//
// ⚠ **不立刻查**：只挂个标记，由后台 worker 按 freeRecheckEvery 节流地去查。
// 用法这里只回答"这个号刚花过额度"，查询频率由池子统一控制 ——
// 否则就回到"每次对话一次查询"那条路上去了（用户报过）。
func (f *freePool) noteUsed(uid string, now time.Time) {
	if uid == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetIfNewDay(now)
	// 已知为 0 的号被用到（说明池子空了、正在用永久额度）—— 不必重查。
	if f.zero[uid] {
		return
	}
	f.needRecheck[uid] = true
}

// snapshot 取当前池子状态（选号路径读它，**纯内存**）。
//
// 返回 (have, zero)：两者都是"已知"的集合；两个集合都没提到的 uid = 还没查过。
func (f *freePool) snapshot(now time.Time) (have, zero map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetIfNewDay(now)
	have = make(map[string]bool, len(f.have))
	for k := range f.have {
		have[k] = true
	}
	zero = make(map[string]bool, len(f.zero))
	for k := range f.zero {
		zero[k] = true
	}
	return have, zero
}

// dueForRecheck 取出一批"该重查"的 uid（节流 + 失败退避之后）。
func (f *freePool) dueForRecheck(limit int, now time.Time) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetIfNewDay(now)
	out := make([]string, 0, limit)
	for uid := range f.needRecheck {
		if len(out) >= limit {
			break
		}
		if at, ok := f.failAt[uid]; ok && now.Sub(at) < freeFailBackoff {
			continue // 刚失败过：别再碰它（没有这条就会"越失败越查"）
		}
		if at, ok := f.checkedAt[uid]; ok && now.Sub(at) < freeRecheckEvery {
			continue // 刚查过：一个账号 90 秒内不重复查
		}
		out = append(out, uid)
	}
	return out
}

// ── Provider 侧 ────────────────────────────────────────────────────────

func (p *Provider) pool() *freePool {
	p.poolMu.Lock()
	defer p.poolMu.Unlock()
	if p.freePool == nil {
		p.freePool = newFreePool()
	}
	return p.freePool
}

// StartFreePoolFill 启动时**一次性**把免费额度池填满（幂等，只有一次）。
//
// # ⚠ 只跑一次，**不挂 ticker** —— 这条区分很重要
//
// 改造前这里既有"立即一遍"又有"每 30 秒重刷一遍"。用户报的"查询太多、
// 太频繁"来自**后者**；而我上次修复时把两者一起拆了，于是池子永远空着、
// 规则完全失效（见文件头）。
//
// 立即这一遍是常数级成本（每次重启 N 个请求），且它是"第一个请求到达之前
// 规则就成立"的前提。周期重刷则与时间成正比、且空闲部署也在打上游 ——
// 那个不要。
//
// ⚠ 串行 + 每个账号之间有间隔：35 个号一次性并发打上游会被限流
// （实测 12 条并发就开始 502），而"填充"不是延迟敏感路径。
func (p *Provider) StartFreePoolFill() {
	if p == nil {
		return
	}
	f := p.pool()
	f.mu.Lock()
	if f.filling {
		f.mu.Unlock()
		return
	}
	f.filling = true
	f.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("loomy: 免费额度池填充 panic（已忽略）: %v", r)
			}
		}()
		refs := p.accounts()
		okN := 0
		for _, a := range refs {
			// ⚠ 每个账号一份独立预算（见 withPointsTimeout 的事故记录）。
			// ⚠ 复用 RefreshQuota：它本来就查一次余额并把结果写进档位缓存，
			//   这里不再另写一条查询路径（两条路径迟早会不一致）。
			if _, ok := p.RefreshQuota(a.UID); ok {
				okN++
			}
			time.Sleep(200 * time.Millisecond) // 串行 + 间隔：不冲上游
		}
		log.Printf("loomy: 免费额度池已填充 %d/%d 个账号"+
			"（池子里的账号会被优先选用；池子空了才动永久额度）", okN, len(refs))
		p.drainRecheck()
	}()
}

// drainRecheck 后台把"用过、需要重查"的账号按节流查掉。
//
// ⚠ 由 `noteUsed` 挂标记驱动，**没有定时器** —— 空闲部署一个请求都不发。
func (p *Provider) drainRecheck() {
	f := p.pool()
	for {
		todo := f.dueForRecheck(4, time.Now())
		if len(todo) == 0 {
			return
		}
		for _, uid := range todo {
			p.RefreshQuota(uid)
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// ── 定期清扫（用户提的第二个设计）────────────────────────────────────

// freePoolSweepBatch 一轮清扫**最多**查几个账号。
//
// # ⚠ 为什么必须有这个上限（用户的问题问得对，这条是它的答案）
//
// 用户问：「定期扫描它不也是读缓存吗？读缓存怎么会那样呢？怎么会变成查询
// 洪水呢？除非那个缓存没更新，对不对？」
//
// **后半句就是答案**：清扫**不是**读缓存，它必须**向上游发请求** ——
// 因为"缓存没更新"正是它要解决的问题。纯读缓存是没用的：缓存里已经知道的
// 那些（已知为 0 的）本来就不在池子里了；池子成员之所以要清扫，正是因为
// **我们不确定它们现在的值**，而要知道就得问上游。
//
// 于是"每轮查哪些"就成了成本本身。两条限制：
//
//	dueForSweep()      同一账号 90 秒内不重复查（freeRecheckEvery）+ 失败退避
//	本常量             一轮最多查 N 个（轮转推进，谁久没查谁先来）
//
// 合起来是**严格有界**的：每分钟最多 N 个请求，与池子大小无关。
// 池子大的时候每个账号的复查周期自动变长 —— 这比"每轮把整个池子刷一遍"
// 稳得多（后者在 100 个号时会变成每分钟 100 个请求）。
const freePoolSweepBatch = 4

// freePoolSweepInterval 定期清扫的间隔。
//
// ⚠ var 而不是 const，**只为测试**：测试要把它压到几十毫秒，才可能在不睡
// 一分钟的前提下验证"周期清扫真的会发生"。生产代码不要改它。
var freePoolSweepInterval = 60 * time.Second

// StartFreePoolSweep 定期把"免费额度已经用完"的账号移出轮询池（幂等）。
//
// # 用户提的两个设计，这是其中之一（原话）
//
//	「或者你可以添加一个任务，那个任务，然后30秒或者1分钟循环一次。或者说，
//	  就是定期检测轮询池里面有没有额度为0的，然后移出去就行了。」
//
// 另一个（"发请求的时候读缓存看它是不是0，是0就移出去、找下一个"）
// **本来就已经是现在的做法**：`CandidateTiers` 在选号时被调用，它只读内存
// （那条"纯内存、不查网络"的性能契约有守卫钉着），报出来的档位直接决定
// "这一轮只用哪些账号" —— 已知为 0 的会被排除掉。
//
// 所以本清扫补的是**另一段空窗**：缓存里还不知道"它已经变成 0 了"的时候。
//
// # ⚠ 为什么它不会变成查询洪水（与旧预热的关键区别）
//
//	旧预热：**所有账号**，每 30 秒一轮 —— 池子大小不随消耗下降，
//	        空闲部署也在打上游，而且（当时的）失败没有退避 ⇒ 越失败越查。
//	本清扫：**只查池子成员** —— 我们相信"还有免费额度"的那些。
//
//	账号额度被用完 → 移出池子 → **下一轮就不再查它**
//	池子空了       → 一轮一个请求都不发（直接 return）
//
// 于是查询量随"还有免费额度的账号数"**单调下降**，而不是永远等于账号总数。
// 再加两条限制：`freeRecheckEvery`（同一账号 90 秒内不重复查）与
// `freeFailBackoff`（刚失败过的先放着）。
//
// 换句话说：这套的稳态负载是"池子里还有多少号要盯" × 每分钟一次，
// 而池子只会越来越小、直到归零。
func (p *Provider) StartFreePoolSweep() {
	if p == nil {
		return
	}
	p.pool().sweepOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("loomy: 免费额度池清扫 panic（已忽略）: %v", r)
				}
			}()
			t := time.NewTicker(freePoolSweepInterval)
			defer t.Stop()
			for range t.C {
				p.sweepFreePool()
			}
		}()
	})
}

// sweepFreePool 一轮清扫：只查池子成员，把变成 0 的移出去。
//
// ⚠ 返回值只为测试 —— 生产代码不关心"这轮查了几个"。
func (p *Provider) sweepFreePool() int {
	return p.sweepFreePoolAt(time.Now())
}

// sweepFreePoolAt 与 sweepFreePool 相同，但**时刻由调用方给**。
//
// ⚠⚠ 为什么必须能注入（2026-10-18 实测，这是本文件第三次踩同一类坑）：
//
// `TestSweepTickerFires` 原来用"给 `noteQuota` 传 `time.Now().Add(-time.Hour)`"
// 来造"该重查"的状态，而清扫内部用**自己的** `time.Now()` 判节流。于是：
//
//	在 23:00–24:00 之间跑：noteQuota 的 now = 昨天 ⇒ 跨日 ⇒ 池子被清空 ⇒ 扫不到成员
//	把 noteQuota 改成"当天中午"跑：checkedAt = 11:00（**未来**）
//	                            ⇒ swept 的 now.Sub(checkedAt) = **负数** ⇒ 被节流跳过
//
// 两条路都红，而**被测逻辑本身完全正确** —— 根因是"两个不同的 now"。
// 注入时刻之后，夹具与清扫看到的是同一个 `now`，测试才有确定性。
//
// ⚠ 这不是"为测试改产品代码"：`FreePoolSweep` 那条路径本来就有 `now` 参数
// （见 `dueForSweep(members, now)`、`members(now)`），这里只是把最外层的
// `time.Now()` 提到函数签名上，让它和里面那几个**读的是同一个时刻** ——
// 原来最外层读一次、里层各读一次，本身就是个（无害但没必要的）不一致。
func (p *Provider) sweepFreePoolAt(now time.Time) int {
	f := p.pool()
	members := f.members(now)
	if len(members) == 0 {
		// 池子空了 = 所有已知账号的免费额度都用完了，我们正在用永久额度。
		// 此时**一个请求都不发**：没有什么可"用尽"了，等到自然日重置即可。
		return 0
	}
	todo := f.dueForSweep(members, now)
	// ⚠ **严格限批量**：一轮最多查 freePoolSweepBatch 个。
	//
	// 不限制的话，"每分钟把池子里所有成员都查一遍"在账号多的时候就是
	// 每分钟 N 个请求 —— 那正是用户担心的事。限批量之后每分钟最多
	// freePoolSweepBatch 个，与池子大小**无关**；池子大时每个账号的
	// 复查周期自动拉长（这是我们要的方向：先保证不烧永久额度，再谈新鲜度）。
	//
	// 选谁先查：谁最久没查过谁先来（dueForSweep 已按节流与退避筛过）。
	if len(todo) > freePoolSweepBatch {
		todo = todo[:freePoolSweepBatch]
	}
	if len(todo) == 0 {
		return 0
	}
	before := len(members)
	for _, uid := range todo {
		// 复用 RefreshQuota：它本来就查一次余额、写档位缓存，并喂给池子。
		// 不为清扫另写一条查询路径（两条路径迟早会不一致）。
		p.RefreshQuota(uid)
		time.Sleep(150 * time.Millisecond) // 串行 + 间隔：不冲上游
	}
	// ⚠ 只在**池子大小变了**的时候打日志（= 有账号被移出去），
	// 否则每分钟一行"查了 N 个"会变成噪音、把真正的信号淹掉。
	if after := len(f.members(now)); after != before {
		log.Printf("loomy: 免费额度池清扫 —— %d → %d 个账号（用完的已移出，不再参与轮询）",
			before, after)
	}
	return len(todo)
}

// members 池子当前成员（已知免费额度 > 0 的 uid）。
func (f *freePool) members(now time.Time) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetIfNewDay(now)
	out := make([]string, 0, len(f.have))
	for uid := range f.have {
		out = append(out, uid)
	}
	return out
}

// dueForSweep 从池子成员里挑出"该查"的那些（节流 + 失败退避之后）。
//
// ⚠ 只对**成员**做节流判断，不对已知为 0 的做 —— 后者已经不在池子里了，
// 不需要也不应该再查（它今天不会变回"有"）。
func (f *freePool) dueForSweep(members []string, now time.Time) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetIfNewDay(now)
	out := make([]string, 0, len(members))
	for _, uid := range members {
		if at, ok := f.failAt[uid]; ok && now.Sub(at) < freeFailBackoff {
			continue // 刚失败过：先放着（没有这条就会"越失败越查"）
		}
		if at, ok := f.checkedAt[uid]; ok && now.Sub(at) < freeRecheckEvery {
			continue // 刚查过：一个账号 90 秒内不重复查
		}
		out = append(out, uid)
	}
	// ⚠ 按"谁最久没查过"升序 —— 因为调用方会**截断成一小批**（见
	// freePoolSweepBatch）。不排序的话，截断永远取到同一批账号（map 遍历顺序
	// 虽然随机，但"没查过的 at 是零值"会让它们长期霸占名额），
	// 于是后面的账号永远轮不到 —— 那是"限批量"引入的新缺陷。
	sort.Slice(out, func(i, j int) bool {
		return f.checkedAt[out[i]].Before(f.checkedAt[out[j]])
	})
	return out
}
