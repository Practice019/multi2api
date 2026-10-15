package loomy

import (
	"log"
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
}

func newFreePool() *freePool {
	return &freePool{
		have:        map[string]bool{},
		zero:        map[string]bool{},
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
		delete(f.zero, uid)
		return
	}
	delete(f.have, uid)
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
