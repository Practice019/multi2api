// autodisable.go 阈值化自动禁用：连续 N 次失败 → **禁用**（不是冷却）；任意一次成功 → 恢复。
//
// # 与既有三条线的分工（关键：四者正交，不要混用）
//
//	冷却 until        按**错误类别**给固定时长（429 / 额度耗尽）—— 时间到就回来
//	熔断 breakerUntil 对"反复失败"逐次加长封禁（30m→1h→2h→6h）—— 时间到就回来
//	自动禁用 autodisable 连续失败达阈值 → **永久摘号**，只能靠探测成功或人工恢复
//	禁用 disabled     账号不可用的**总闸**（人工禁用 / 凭证死亡也会落到这里）
//
// 为什么熔断之外还要一条"禁用"：熔断的语义是"这个号现在不好，歇一会儿"，
// 到期必然回来；而一个已经不行的号（凭证死了、账号被风控、上游持续拒绝）
// 会在每个冷却周期结束后**再被选中一次**，白跑一次往返、再冷一轮，
// 如此循环 —— 用户看到的是"这个号永远在池子里偶尔失败一次"。
// new-api 用一个独立的渠道状态（ChannelStatusAutoDisabled）把这类号摘出轮询，
// 本文件做同一件事，只是判据从"单次错误码/关键词"换成本仓的"连续失败计数"。
//
// # ⚠ 为什么不复用 e.fails（这是本文件诞生的第一个坑）
//
// e.fails 看起来就是"连续失败计数"，但它是**熔断器独占**的：recordBreakerFailureLocked
// 在达到 breakerThreshold 时会 `e.fails = 0`（重置后重新累计，见 pool.go:1854）。
// 而 breakerThreshold 默认 3、自动禁用阈值默认 5 —— 拿 e.fails 做判据的话，
// 计数在 3 就归零，**永远到不了 5**：这个功能会安静地永不触发。
// 所以必须自己维护 streak（autoDisableState.streak），并且**与熔断解耦**：
// 熔断期间（冷却中）不失败也不成功，streak 原地保留；熔断到期后再失败一次就继续累加。
// 换句话说："熔断三次 + 冷却结束后继续失败"才是真正该被摘掉的号。
//
// # ⚠ 为什么"禁用"直接写 entry.disabled + 一份 owned 标记
//
// 本机制要真的把号摘出选号（pick 只看 entry.healthy → disabled），所以必须
// 落到 entry.disabled。但 entry 里只有一个 bool 和一个 reason，**无法区分**
// "我们因为连败禁的"与"人工禁的 / 凭证死了禁的"。
//
// 区分是必须的：Enable（人工解禁）与凭证续期成功都可能把 e.disabled 清掉，
// 而我们的判定状态还留在表里；反过来，若不分青红皂白地"成功即解禁"，
// 一次探测成功就会**静默推翻操作员按下的禁用**。
//
// new-api 对这件事的答案就是状态值本身：ShouldEnableChannel 里写着
// `if status != common.ChannelStatusAutoDisabled { return false }` ——
// 只有"自动禁用"这一态才允许自动启用。本仓没有渠道状态列（entry 是池内运行态），
// 所以用 autoDisableState.owned 承载同一语义：**这个禁用是不是我们置的**。
//
// # ⚠ 状态表为什么挂在包级注册表上（本文件唯一的架构妥协）
//
// 硬约束：本次不改 pool.go（不给 Pool/entry 加字段）。而自动禁用的运行态
// （streak / 由谁置位 / 禁用时刻）必须与选号状态**在同一把锁下**读写，
// 否则"streak 已到阈值"与"disabled 已置位"会出现撕裂窗口。
//
// 所以：表的**内容**由 p.mu 保护（加锁风格与 pool.go 逐字一致：套 Locked 后缀的
// 辅助函数、调用方持锁），包级 sync.Map 只做 *Pool → 表 的一次性查找。
// 锁序永远是 p.mu → sync.Map（sync.Map 内部锁），没有反向路径，不会死锁。
//
// 代价：Pool 对象被 GC 后注册表里仍留着一个空表（惰性创建，只记录过失败的池才有），
// 进程内通常只有 1 个池，因此生产无影响；测试里用 sweepAutoDisableBook 清理。
//
// # ⚠ 禁用之后就不会再有"成功"了（必须配探测入口，否则永不恢复）
//
// 被禁用的号不在候选集里（healthy=false），于是它**永远拿不到下一次成功** ——
// "一次成功即恢复"在只有请求路径的情况下是死条款。
// new-api 的解法是 channel test 定时任务（AutomaticEnableChannelEnabled）；
// 本仓对应的是 health.go 的主动探测，但 RecoverableCandidates **明确跳过 disabled**
// （那边的理由是"禁用是明确的决定，探测成功也不该自动翻案"）。
//
// 两条判据并不矛盾，差别正是 owned：人工/凭证禁用不该被探测翻案；
// 自动禁用**必须**能被探测翻案。所以本文件提供 AutoDisabledCandidates()
// 作为探测候选集（刻意不含人工禁用的号），探测成功后调用 RecordAutoDisableSuccess
// 即可恢复。不修改 health.go —— 由调用方决定探哪一组。
package pool

import (
	"sort"
	"sync"
	"time"
)

// DefaultAutoDisableThreshold 连续失败多少次触发自动禁用。
//
// 取 5 的依据与取舍：
//
//  1. 必须**明显大于**熔断阈值（默认 3）。熔断已经是"三次连续失败就歇 30 分钟"，
//     自动禁用的判据若也在 3 附近，两者会同时触发 —— 那就退化成"熔断即摘号"，
//     失去了"先歇一会儿，再不行才摘"的两级渐进。
//  2. 不能太小：一次上游抖动 + 一次重试就可能凑满 2 次，阈值 2/3 会误杀。
//     5 次的全失败概率在上游成功率 0.9 时约 1e-5，可以接受；
//     而上游成功率 0.5 时约 3%，本来就该被摘掉。
//  3. 与计划文件（PLAN-newapi-alignment.md T5）以及 new-api 的
//     ChannelDisableThreshold 默认值 5 对齐 —— 数值一致便于两套系统对账。
//
// ⚠ 留痕（避免下一个人按错依据调它）：new-api 的 ChannelDisableThreshold **不是**
// 连续失败次数，是**响应时间**阈值（单位秒，见 controller/channel-test.go:1072
// 的 `disableThreshold := int64(common.ChannelDisableThreshold * 1000)` 与随后的
// `milliseconds > disableThreshold`）。new-api 的自动禁用本身是
// service/channel.go 的 ShouldDisableChannel：**单次**错误按状态码/关键词判定，
// 没有计数门控。也就是说"连续 N 次"是本仓自有的设计（与熔断器同族），
// 只有默认值 5 是从 new-api 借来的。- 别照着 new-api 去找一个不存在的计数逻辑。
const DefaultAutoDisableThreshold = 5

// DefaultAutoDisableWindow 连续失败的默认衰减窗口：0 = 不衰减（严格连续）。
//
// # 为什么默认关掉，而不是照抄熔断器的 60s 窗口
//
// NoteError 给 fails 套了 breakerFailureWindow（60s）的理由是"只有突发才算数"，
// 那是因为**熔断**的代价只是"歇一会儿"，误熔断一次的成本很低。
//
// 自动禁用的代价完全不同：摘号之后不会再被选中，只能等探测或人工 ——
// 误禁一个健康号会白白丢掉一份产能，而且管理台上看起来像"这个号坏了"。
// 两种错误代价不对称时，默认值应当选**最保守**的那个（宁可晚禁，不可误禁）。
//
// 需要"跨天累计的病态号也要被摘掉"时，把它设成 10*time.Minute 之类：
// 超过窗口的旧失败不参与累计（见 RecordAutoDisableFailure）。
const DefaultAutoDisableWindow = time.Duration(0)

// autoDisableCauseMaxRunes 写进 reason 的"最后一次失败原因"最多保留多少字符。
//
// 为什么要截断：reason 会进 state.json（每 5s 一次落盘）与管理台表格，
// 而上游的失败原因**经常是整页 HTML 或数 KB 的 JSON**（本仓在 errmask 里
// 已经为出站错误踩过同一个坑）。截断只在**记录**这一侧做，不外发。
const autoDisableCauseMaxRunes = 200

// autoDisableState 单个账号的自动禁用运行态。
//
// 生命期：随账号存在于池中；uid 从池里消失时被自动清理（见 RecordAutoDisableFailure）。
type autoDisableState struct {
	// owner 这份状态属于哪一个 entry（账号实例）。
	//
	// # 为什么必须钉住实例而不是只用 uid（一次真实的测试失败）
	//
	// "删号 → 重新导入同名 uid"是**正常运维操作**（凭证文件被替换、账号被
	// 重新登录）。此时旧 entry 连同它的禁用一起消失，但本表以 uid 为键，
	// 旧进度会原样留下来 —— 表现是"刚重新导入的号带着 4/5 的进度上台，
	// 下一次失败立刻被秒禁"。测试 TestAutoDisableUnknownUID 正是这样红的。
	//
	// 所以状态在读取时校验实例身份：不一致即视为"另一个账号"，旧状态作废。
	// 这比在 Remove 时清理更稳 —— 删除路径可能来自 SyncToDir、快照恢复等多处，
	// 而"实例变了"是所有路径的共同终点。
	owner *entry

	// streak 连续失败次数（**本机制独占**，不是 e.fails —— 见文件头的坑）。
	streak int
	// lastFail 最近一次失败时刻（衰减窗口的判据）。
	lastFail time.Time
	// disabled 本机制是否已判定该账号应被禁用。
	//
	// ⚠ 它与 entry.disabled **不一定相等**：人工 Enable 会清掉 entry.disabled，
	// 但这里要等下一次失败/成功才自愈（见 RecordAutoDisableFailure 的状态自愈段）。
	disabled bool
	// owned 该禁用是否是**我们置的**。
	//
	// true  → 允许 RecordAutoDisableSuccess 自动解禁（等价 new-api 的 AutoDisabled 态）
	// false → 账号是被人工/凭证失效禁用的，我们只计数，**不动**那个禁用
	owned bool
	// disabledAt 首次被本机制禁用的时刻（重复失败不刷新，保留"什么时候坏掉的"）。
	disabledAt time.Time
	// reason 本机制给出的禁用原因（含第几次、阈值、最后一次上游原因）。
	reason string
}

// reset 清空计数与判定（保留 owned/reason 的清零也一并做）。
func (s *autoDisableState) reset() {
	owner := s.owner
	*s = autoDisableState{owner: owner}
}

// ownsDisableLocked 报告"账号此刻的禁用仍然是**我们置的那个**"。调用方必须持 p.mu。
//
// # 判据为什么是比较 reason 而不是只用 owned（一个真实的语义洞）
//
// 场景：账号被自动禁用（owned=true, entry.reason=我们的文案）后，操作员在管理台
// 又按了一次「禁用」并填了自己的原因。此时 entry.reason 被改写，但 owned 仍为 true
// —— 若只看 owned，一次探测成功就会把**操作员刚刚按下的禁用**一并撤销。
//
// 这正是文件头讲的"自动探测不该推翻人的决定"的第二种形态：不是从来没被禁过，
// 而是**人重新表过态**。entry.reason 是那次表态的唯一痕迹（pool 只有一个 string，
// 无法区分"谁写的"），所以用它做对账：我们的文案与它不一致 ⇒ 归属已转移。
//
// 代价：若另一个机制碰巧写出一模一样的字符串，这里会误判为我们置的。
// 那句文案里带"连续失败 N 次（阈值 M）"与具体上游原因，实际不可能撞上。
func ownsDisableLocked(st *autoDisableState, e *entry) bool {
	if st == nil || e == nil {
		return false
	}
	return st.disabled && st.owned && e.disabled && e.reason == st.reason
}

// autoDisableBook 单个 Pool 的自动禁用配置 + 全量状态。
//
// 内容由该 Pool 的 mu 保护（加锁纪律与 pool.go 一致：调用方持锁，
// 方法名带 Locked 后缀）。
type autoDisableBook struct {
	threshold int
	window    time.Duration
	states    map[string]*autoDisableState
}

// stateForLocked 取（或惰性建）某账号的状态。调用方必须持 p.mu。
//
// 实例身份不一致（uid 被删掉后重新导入）时**丢弃旧状态重建**（见 owner 字段）。
func (b *autoDisableBook) stateForLocked(uid string, e *entry) *autoDisableState {
	if s, ok := b.existingLocked(uid, e); ok {
		return s
	}
	s := &autoDisableState{owner: e}
	b.states[uid] = s
	return s
}

// existingLocked 取**属于当前实例**的既有状态；没有则返回 false（并顺手清掉陈旧条目）。
//
// 只读语义：调用方不打算建状态时用它（成功路径不应该给健康号留空记录）。
func (b *autoDisableBook) existingLocked(uid string, e *entry) (*autoDisableState, bool) {
	s, ok := b.states[uid]
	if !ok {
		return nil, false
	}
	if s.owner != e {
		// 属于上一个 entry 的残留：直接删除再报"没有"。
		delete(b.states, uid)
		return nil, false
	}
	return s, true
}

// autoDisableBooks *Pool → *autoDisableBook 的包级注册表。
//
// 只做查找，内容由各自 Pool 的 mu 保护（理由见文件头的"架构妥协"）。
// 键用 *Pool 而不是 uid：两个池里同名 uid 是**两个不同的账号**
// （测试与多实例部署都会出现），混在一起会串状态。
var autoDisableBooks sync.Map

// autoBookLocked 取（或创建）本池的自动禁用状态表。调用方必须持 p.mu。
//
// 惰性创建而不是在 New() 里建：New() 在 pool.go 里，本次不改它。
// 用 LoadOrStore 而不是 Load+Store 是防御：即便将来有人在持锁之外调用，
// 也不会丢掉一个已存在的表（那会静默清空所有计数）。
func (p *Pool) autoBookLocked() *autoDisableBook {
	if v, ok := autoDisableBooks.Load(p); ok {
		return v.(*autoDisableBook)
	}
	b := &autoDisableBook{
		threshold: DefaultAutoDisableThreshold,
		window:    DefaultAutoDisableWindow,
		states:    map[string]*autoDisableState{},
	}
	v, _ := autoDisableBooks.LoadOrStore(p, b)
	return v.(*autoDisableBook)
}

// sweepAutoDisableBook 丢弃本池的状态表（测试清理用；生产进程内只有一个池，无需调用）。
//
// 为什么允许它存在：注册表是包级强引用，测试会创建成百上千个池，
// 不清理就是稳定的内存泄漏 —— 那会让 -race/-count=N 跑出"越来越慢"的假象。
func sweepAutoDisableBook(p *Pool) {
	autoDisableBooks.Delete(p)
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// SetAutoDisable 注入自动禁用参数（装配层从 config 解析后调用）。
//
// 语义与 SetBreaker 逐字对齐：非正值保留原值（用默认），
// 于是"配置里没写"与"配置写了 0"都安全地落到默认，不需要额外的 hasXxx 判据。
//
// window 是**例外**：0 是合法且默认的取值（不衰减），所以判据是 >= 0。
// 传负数与不传等价。
func (p *Pool) SetAutoDisable(threshold int, window time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.autoBookLocked()
	if threshold > 0 {
		b.threshold = threshold
	}
	if window >= 0 {
		b.window = window
	}
	// ⚠ 不重算已禁用账号的状态：阈值调小不会把历史账号"补禁"，
	// 阈值调大也不会把已禁用的号放出来（它需要一次探测成功或人工恢复）。
	// 理由：禁用是一次**决定**，改配置不应回溯性地改写历史决定 ——
	// 否则运维调一次阈值，池子里会有一批号在不同时刻莫名其妙地被摘掉/放回。
	p.dirty.Store(true)
}

// AutoDisableThreshold 返回当前生效的自动禁用阈值（管理台展示 / 配置回读用）。
func (p *Pool) AutoDisableThreshold() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.autoBookLocked().threshold
}

// AutoDisableWindow 返回当前生效的衰减窗口（0 = 严格连续）。
func (p *Pool) AutoDisableWindow() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.autoBookLocked().window
}

// ---------------------------------------------------------------------------
// 记录
// ---------------------------------------------------------------------------

// RecordAutoDisableFailure 记录一次**请求级失败**，返回"这一次失败是否造成了禁用"。
//
// 调用时机与 NoteError 互补而不重叠：
//
//	NoteError                  喂熔断器（冷却）+ 累计 errTotal + 上游级熔断
//	RecordAutoDisableFailure   喂阈值化禁用（本文件）
//
// 两者应当**同时**喂（出口层一次失败 = 一次 NoteError + 一次本方法）。
// 刻意不合并成一个方法：熔断与禁用是两条代价不同的线，
// 合并会让"只想熔断、不想摘号"的入口（如 429 软限流）无处表达。
//
// reason 是**这一次**失败的上游原因（会被截断后拼进禁用原因）。
// 传空串合法（有些失败没有可读原因）。
//
// 返回值语义（重要）：true 表示"这一刻账号从可用变成了被本机制禁用"。
// 已经处于禁用中的账号再次失败返回 false —— 它不是"新的禁用事件"，
// 调用方据此可以只在 true 时打日志/发通知，避免每次失败都刷一条。
func (p *Pool) RecordAutoDisableFailure(uid, reason string) (disabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.byUID[uid]
	if !ok || e == nil {
		// 号已经不在池里（被 Remove）：顺手清掉残留状态，
		// 否则"删号 → 重新导入同名 uid"会带着旧的连败进度上台。
		if b, ok := autoDisableBooks.Load(p); ok {
			delete(b.(*autoDisableBook).states, uid)
		}
		return false
	}

	b := p.autoBookLocked()
	st := b.stateForLocked(uid, e)
	now := time.Now()

	// ① 状态自愈：表说"已禁用"，但账号实际没被禁 —— 说明那次禁用已被人工撤销
	//   （Enable）或账号被重新导入。此时应重新计数，而不是带着 4/5 的旧进度
	//   在下一次失败时**立刻**被秒禁（那会让操作员的解禁看起来完全无效）。
	if st.disabled && !e.disabled {
		st.reset()
	}

	// ② 衰减窗口：窗口外的旧失败不参与本次累计（只有窗口内的**连续**失败才算数）。
	//   窗口 0 = 关闭衰减 = 严格连续（默认，理由见 DefaultAutoDisableWindow）。
	if b.window > 0 && !st.lastFail.IsZero() && now.Sub(st.lastFail) > b.window {
		st.streak = 0
	}

	st.streak++
	st.lastFail = now

	if st.streak < b.threshold {
		// 未达阈值：只计数，**不碰**任何禁用/冷却状态。
		// 账号此刻该不该歇一会儿，是熔断器的事，不是我们的。
		p.dirty.Store(true)
		return false
	}

	// ③ 达阈值 → 禁用。
	//
	// ⚠ 已经因**我们**被禁用的号再次失败（探测失败、或禁用前的在途请求刚回来）：
	//   streak 继续累加（它反映真实情况），但禁用时刻与 reason 保留**首次**的 ——
	//   "什么时候坏掉的"比"最后一次失败说了什么"对排查更有价值。
	//   返回 false：没有发生新的状态跃迁。
	if ownsDisableLocked(st, e) {
		p.dirty.Store(true)
		return false
	}

	why := autoDisableReason(st.streak, b.threshold, reason)
	st.disabled = true
	if st.disabledAt.IsZero() {
		st.disabledAt = now
	}
	st.reason = why

	// ④ 写 entry。这是"禁用"与"冷却"的分界：
	//
	//   Cooldown 只设 until，号到期自动回来；
	//   disabled 让号**不进候选集**，只能由 RecordAutoDisableSuccess /
	//   ResetAutoDisable / Enable 解除。
	//
	// ⚠ 若账号此前已被人工/凭证禁用（e.disabled 已 true），**不改它的 reason**
	//   —— 人工给的原因比我们的"连续失败 N 次"更能说明问题，覆盖它等于
	//   把操作员的判断抹掉。但仍记 owned=false：那个禁用不是我们的，
	//   成功时不许自动解除。
	if e.disabled {
		st.owned = false
		p.dirty.Store(true)
		return false
	}
	e.disabled = true
	e.reason = why
	st.owned = true
	p.dirty.Store(true)
	return true
}

// RecordAutoDisableSuccess 记录一次**请求级成功**并尝试恢复，返回"是否解除了本机制造成的禁用"。
//
// 清零语义（与 NoteSuccess 的失败线清零同族）：
//   - 未禁用时：只把 streak 归零（这就是"任意一次成功 → 清除计数"）。
//     不清零的话，跨小时的零散失败会慢慢攒到阈值，误禁一个健康号 ——
//     这正是 sessionDeadStreak 那条线已经踩过并写进注释的坑。
//   - 已禁用且由**我们**置位：解除禁用、清空原因、清空禁用时刻。
//   - 已禁用但**不是**我们置位的：只清计数，**不动**那个禁用。
//     依据是 new-api 的 ShouldEnableChannel（只对 ChannelStatusAutoDisabled 放行）——
//     自动探测成功不该静默推翻"凭证死了要重登"或操作员按下的禁用。
//
// 调用时机：请求成功路径（与 NoteSuccess 并列），以及**健康探测成功**时
// （后者是被禁用的号唯一的复活通道，见文件头）。
func (p *Pool) RecordAutoDisableSuccess(uid string) (recovered bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.byUID[uid]
	if !ok || e == nil {
		if b, ok := autoDisableBooks.Load(p); ok {
			delete(b.(*autoDisableBook).states, uid)
		}
		return false
	}

	b := p.autoBookLocked()
	st, exists := b.existingLocked(uid, e)
	if !exists {
		// 从没失败过：无事可做（连表都不建，避免给每个成功过的号留一条空记录）。
		return false
	}

	st.streak = 0
	st.lastFail = time.Time{}

	if !st.disabled {
		return false
	}

	// 清掉本机制的判定状态。无论能否解禁，判定都结束（下次失败从 1 开始）。
	wasOwned := ownsDisableLocked(st, e)
	st.disabled = false
	st.owned = false
	st.disabledAt = time.Time{}
	st.reason = ""

	if !wasOwned || !e.disabled {
		// 禁用不是我们置的（或已被人工解除）：不碰 entry 的禁用与原因。
		p.dirty.Store(true)
		return false
	}
	e.disabled = false
	e.reason = ""
	p.dirty.Store(true)
	return true
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// AutoDisableStatus 单个账号的自动禁用视图（导出，供管理台/诊断）。
//
// ⚠ Streak 与 Disabled 回答的是**两个不同的问题**，不要互相替代：
//
//	Streak   我们已经连续观察到几次失败（本机制自己的账）
//	Disabled 账号此刻**实际上**是不是不可用（含人工禁用/凭证禁用）
type AutoDisableStatus struct {
	UID      string `json:"uid"`
	Provider string `json:"provider,omitempty"`

	// Streak 当前连续失败次数（任意一次成功清零）。
	Streak int `json:"streak"`
	// Threshold 生效中的阈值（前端可以直接画 "3/5"）。
	Threshold int `json:"threshold"`
	// Window 生效中的衰减窗口（0 = 严格连续）。
	Window time.Duration `json:"window,omitempty"`

	// Disabled 账号当前是否不可用（entry.disabled 的真相，含所有来源）。
	Disabled bool `json:"disabled"`
	// AutoOwned 该禁用是否由本机制置位。
	//
	// 只有它为 true 时，一次成功（或探测成功）才会自动解禁；
	// 为 false 且 Disabled 为 true 表示"它被别的机制/人禁掉了"，需要人工处理。
	AutoOwned bool `json:"auto_owned,omitempty"`

	// DisabledAt 首次被本机制禁用的时刻（零值 = 从未）。
	DisabledAt time.Time `json:"disabled_at,omitempty"`
	// Reason 禁用原因（含第几次、阈值、最后一次上游原因）。
	Reason string `json:"reason,omitempty"`
	// LastFail 最近一次失败时刻。
	LastFail time.Time `json:"last_fail,omitempty"`
}

// AutoDisableStatus 查询单个账号的自动禁用状态。第二返回值为 false 表示 uid 不在池里。
//
// 为"从没失败过"的账号也返回零值状态（Streak=0、Threshold 为生效值）：
// 管理台要能在同一列里把"0/5"与"3/5"并排显示，
// 缺记录等同于 0 —— 这与 Status.SuccessCount 刻意不带 omitempty 是同一条经验。
func (p *Pool) AutoDisableStatus(uid string) (AutoDisableStatus, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	e, ok := p.byUID[uid]
	if !ok || e == nil {
		return AutoDisableStatus{}, false
	}

	b := p.autoBookLocked()
	out := AutoDisableStatus{
		UID:       uid,
		Provider:  p.providerOf(e),
		Threshold: b.threshold,
		Window:    b.window,
		Disabled:  e.disabled,
		Reason:    "",
	}
	if st, ok := b.existingLocked(uid, e); ok {
		out.Streak = st.streak
		out.LastFail = st.lastFail
		// AutoOwned 要求"我们还认这次禁用"（disabled）且账号确实还禁着：
		// 人工 Enable 之后表示已被撤销，不该继续声称是我们禁的。
		out.AutoOwned = ownsDisableLocked(st, e)
		out.DisabledAt = st.disabledAt
		if out.AutoOwned {
			// 只有由本机制置位的禁用才把我们的 reason 暴露为"禁用原因"，
			// 否则 e.reason（人工填的）更有信息量 —— 见下方回填。
			out.Reason = st.reason
		}
	}
	if out.Reason == "" {
		// 人工禁用 / 凭证禁用的原因（entry.reason 是管理台展示的权威文案）。
		out.Reason = e.reason
	}
	return out, true
}

// AutoDisableStreak 返回当前连续失败次数（便捷读取；不在池里返回 0）。
func (p *Pool) AutoDisableStreak(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok || e == nil {
		return 0
	}
	if b, ok := autoDisableBooks.Load(p); ok {
		if st, ok := b.(*autoDisableBook).existingLocked(uid, e); ok {
			return st.streak
		}
	}
	return 0
}

// IsAutoDisabled 报告账号是否**被本机制**禁用（仍在生效）。
//
// ⚠ 与 Status(uid).Disabled 的区别：后者含人工禁用与凭证失效禁用。
// 判"要不要走自动恢复探测"用本方法；判"能不能给它派活"用后者。
func (p *Pool) IsAutoDisabled(uid string) bool {
	st, ok := p.AutoDisableStatus(uid)
	if !ok {
		return false
	}
	return st.AutoOwned
}

// AutoDisabledAccount 一个被自动禁用、值得主动探测恢复的账号。
type AutoDisabledAccount struct {
	UID        string    `json:"uid"`
	Provider   string    `json:"provider,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	DisabledAt time.Time `json:"disabled_at,omitempty"`
	Streak     int       `json:"streak"`
}

// AutoDisabledCandidates 返回值得主动探测的**自动禁用**账号。
//
// 判据：被本机制禁用（AutoOwned）且当前仍禁用。
//
// # 为什么不复用 health.go 的 RecoverableCandidates
//
// 那边**明确排除** disabled（health.go:57 的 `if e.disabled { continue }`），
// 理由是"禁用是用户/会话死亡的明确决定，探测成功也不该自动翻案"。
// 那条判据对**人工/凭证**禁用是对的，对**自动**禁用恰恰相反：
// 自动禁用是一个统计结论（"它最近连续不行"），本就应该允许被新证据推翻 ——
// 否则这个功能只有"摘号"没有"回池"，池子会随时间单调收缩。
//
// 两个集合因此是互补的：人工禁用 → 人工处理；自动禁用 → 探测恢复。
//
// 排序（provider, uid）与 RecoverableCandidates 一致：探测逐号串行时
// 稳定顺序便于日志对照，也让测试无需依赖 map 迭代顺序。
func (p *Pool) AutoDisabledCandidates() []AutoDisabledAccount {
	p.mu.RLock()
	defer p.mu.RUnlock()

	b := p.autoBookLocked()
	out := make([]AutoDisabledAccount, 0, len(b.states))
	for uid, st := range b.states {
		e, ok := p.byUID[uid]
		if !ok || e == nil || st.owner != e || !ownsDisableLocked(st, e) {
			// 表里有、池里没有或已被人放宽：不是探测对象（它们会在下一次
			// 记录时自愈，不必在这里顺手改状态 —— 本方法只读）。
			continue
		}
		out = append(out, AutoDisabledAccount{
			UID:        uid,
			Provider:   p.providerOf(e),
			Reason:     st.reason,
			DisabledAt: st.disabledAt,
			Streak:     st.streak,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// ---------------------------------------------------------------------------
// 人工干预
// ---------------------------------------------------------------------------

// ResetAutoDisable 人工清除本机制的判定：连败计数归零；若禁用是我们置的，一并解除。
//
// 返回 true 表示**确实清掉了一个自动禁用**（不只是清了计数）。
//
// # 与 Enable 的分工
//
//	Enable            无条件解除 entry.disabled，且**不**清我们的计数
//	ResetAutoDisable  只解除"我们置的"那一个禁用，并清计数
//
// 管理台的「恢复」按钮应当**两个都调**：先 ResetAutoDisable（清账），
// 再 Enable（兜底解除任何来源的禁用）。反过来只调 Enable 会留下 5/5 的旧进度，
// 下一次失败立刻再次禁用 —— 操作员会觉得"点了恢复没用"，那是本仓
// ClearCooldown 注释里已经记录过的同类体验坑（"点了清冷却，一分钟后冷两小时"）。
func (p *Pool) ResetAutoDisable(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.byUID[uid]
	if !ok || e == nil {
		if b, ok := autoDisableBooks.Load(p); ok {
			delete(b.(*autoDisableBook).states, uid)
		}
		return false
	}

	b := p.autoBookLocked()
	st, exists := b.existingLocked(uid, e)
	clearedDisable := exists && ownsDisableLocked(st, e)

	if exists {
		st.reset()
	}
	if clearedDisable {
		e.disabled = false
		e.reason = ""
	}
	p.dirty.Store(true)
	return clearedDisable
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// autoDisableReason 组装写进 entry.reason 的禁用文案。
//
// 为什么要写这么长：这行字会原样出现在管理台的"原因"列，而
// "已禁用"三个字（改造前 NoteSessionDead 的写法）无法区分
// "人工禁的 / 凭证死了 / 连败太多"，排查时只能靠翻日志。
// 本仓在 Status.Reason 上已经有同样的教训。
func autoDisableReason(streak, threshold int, cause string) string {
	s := "自动禁用：连续失败 " + itoa(streak) + " 次（阈值 " + itoa(threshold) + "）"
	if cause != "" {
		s += "，最后一次原因：" + truncateRunes(cause, autoDisableCauseMaxRunes)
	}
	return s
}

// truncateRunes 按**字符**（而非字节）截断，避免把多字节 UTF-8 切成乱码。
//
// 按字节截断的后果不是"少几个字"而是"出现 U+FFFD 替换符"，
// 而这段字符串会被写进 state.json 并被 JSON 编码器转义 —— 一页上游 HTML
// 的错误原因能把它撑成几十 KB。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// 快速路径：字节数不超 n 就一定不超 n 个字符。
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos] + "…"
		}
		i++
	}
	return s
}
