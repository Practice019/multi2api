package loomy

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// 本文件：**面板预热的调度**（把 tasks/invite 的补数从"前端触发"搬到服务端）。
//
// # 为什么单独一个文件（AGENTS 第 2 条：尽可能多写独立文件）
//
// `adminpoints.go` 已经是 900+ 行、同时装着：两个视图结构、四个 handler、
// 行缓存的读写与落盘。再往里塞调度逻辑，任何人想改一处都得先读完整份。
// ⚠ 而这个文件只回答一个问题：**什么时候、以多大并发去补哪些号**。
//
// # 用户的要求（2026-10-18，原话）
//
//	"我希望打开页面毫秒加载整个页面数据"
//	"是不是本地文件里面有这些账号的额度等信息，前端只需要直接加载缓存就行了啊？
//	 还需要等待几分钟？一毫秒就行了啊"
//
// ⇒ 落盘（见 adminpoints 的 panelRowCache.onChange / savePanelsSnapshot）解决
//   **"重启后多久能看到数据"**；本文件解决**"盘上多久能填满"** —— 两者缺一不可：
//   盘上没数据时，前端打开还是会看到「正在读取…」。
//
// # Ⅱ 本文件的第二个职责：**别把"幽灵行"算成待办**（2026-11-02 修的真 bug）
//
// 用户报的日志：
//
//	loomy: 面板预热一轮 —— 待办：未缓存(tasks=0 invite=0) · 过期(tasks=46 invite=46) · 共 44 个号 · 本轮发起 12 个 · 首个错误=""
//
// 「过期 46」> 「共 44 个号」—— 多出来的 2 个是**已删账号在缓存里的残留行**（幽灵行）：
// 缓存是**落盘**的（panels.json），删号不会清缓存，而 `expiredTasks/expiredInvites`
// 遍历的是缓存 map 的**全部 key**、从不与账号池求交集。
//
// ⚠ 这不只是"显示难看"：幽灵行**永远不会被补齐**（账号都没了）⇒ `left` 恒 > 0 ⇒
// 调用方以为"还有活"，5 秒一轮地空转（用户日志里连着 5 轮、间隔 5~8 秒就是证据）。
// ⇒ 两件事一起做：**修剪**（prunePanelOrphans，删掉缓存里的残留行）+ **计数过滤**
//   （planPanelWarmup 里 expT++ 挪进"还在池里"的判断内）。
//
// # Ⅲ 第三个职责：日志里**别打空字段**
//
// 同一行日志里的 `首个错误=""` 是噪音（firstErr 为空 = 本轮确实没出错，
// 那就**不该出现这个字段**）。见 panelWarmupLogLine。
//
// ⚠ 「怎么刷」与「刷不刷」的分工：本文件管**怎么刷**（挑号/并发/日志），
// 「谁过期了」在 panelttl.go、**修剪**也在那里（同一类判断）。
// 而"要补哪些号"这件事被抽成 `planPanelWarmup`（纯判断、不发请求）——
// 上面那个幽灵 bug 恰好就出在这个判断里，抽出来才能被单测钉住。

const (
	// panelWarmupIdleInterval **没有待补**时的间隔（稳定态）。
	//
	// ⚠ 稳定态下几乎不发请求 —— 这是本仓"不频繁查询上游"的一贯取舍
	//   （见 StartFreePoolSweep 里那段：它不是"所有账号每 30 秒一轮"的预热）。
	//   5 分钟只是"万一有号从缓存里掉出去、或上游那边状态变了"的兜底复查周期。
	panelWarmupIdleInterval = 5 * time.Minute
	// panelWarmupBusyInterval **还有待补**时的间隔（追赶态）。
	//
	// ⚠⚠ 这一个常量是"多久之后打开就是毫秒"的直接决定因素：
	//   实测一轮（12 个 / 并发 4）约 **30 秒**跑完，而固定 60 秒间隔会让
	//   一半的时间在空等 ⇒ 44 个号要 ~6 分钟。改成 5 秒之后：
	//   44 个 ≈ 4 轮 × 30 秒 ≈ **2 分钟**（且轮间几乎不空等）。
	// ⚠ 不会因此打急上游：并发仍是 panelWarmupConcurrency，每轮仍是
	//   panelWarmupBatch 个上限 —— 缩短的只是"两轮之间干等的时间"。
	panelWarmupBusyInterval = 5 * time.Second
	// panelWarmupBatch 每轮最多补几个号。
	//
	// ⚠ 取 12 而不是更小：一轮的耗时 ≈ 批量 / 并发 × 单号耗时，批量太小会让
	//   "轮与轮之间的空等"占掉大头。
	panelWarmupBatch = 12
	// panelWarmupConcurrency 补数的并发度。
	//
	// ⚠⚠ **不能提高**：上游实测 **12 条并发就打 502**（见 adminpoints 里
	//   LAZY_MAX_INFLIGHT 的注释，那是踩过的坑）。4 是"更快"与"不打急上游"的平衡点。
	// ⚠ 每个号要发 2 次上游请求（tasks + balance）⇒ 4 并发 = 8 条在途，仍在安全区。
	panelWarmupConcurrency = 4
)

// StartPanelWarmup 起一个后台循环，**持续**把没进缓存的面板行补上。
//
// ⚠ 形状**照抄** StartFreePoolSweep（`sync.Once` + recover + Ticker）——
//
//	本仓已有的后台任务都是这个形态，不新造（AGENTS 第 8 条：能抄则抄）。
//
// ⚠ 除了等第一个 tick，**启动时先立刻跑一轮**：冷启动后用户往往马上打开页面，
//
//	等 60 秒才补第一轮太晚。
func (p *Provider) StartPanelWarmup() {
	if p == nil {
		return
	}
	p.warmOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("loomy: 面板预热 panic（已忽略）: %v", r)
				}
			}()
			// ⚠⚠ **自己调度自己**，而不是固定 Ticker：
			//   `sweepPanels` 返回**还剩多少待补**，于是间隔能跟着状态走 ——
			//     · 还有待补（追赶态）⇒ 5 秒一轮 ⇒ 44 个号约 2 分钟补满
			//     · 都补完了（稳定态）⇒ 5 分钟一轮 ⇒ 几乎不发请求
			//   ⚠ 固定 60 秒的版本实测会浪费一半时间空等（一轮只要 30 秒）。
			for {
				left := p.sweepPanels()
				if left > 0 {
					time.Sleep(panelWarmupBusyInterval)
					continue
				}
				time.Sleep(panelWarmupIdleInterval)
			}
		}()
	})
}

// sweepPanels 跑一轮：把"还没进缓存"的号补几个（限并发 panelWarmupConcurrency）。
//
// ⚠ tasks 与 invite **各补各的**：一条号可能 tasks 有缓存而 invite 没有，
//
//	只判其中一个会让另一半永远空着（那正是"新手任务好了、邀请码还是正在读取"的形态）。
//
// ⚠ 返回值与日志都为**排障**而留 —— 没有它们，"预热有没有在动、为什么没补上"
//
//	就只能靠猜（今天已经猜了很久）。
func (p *Provider) sweepPanels() int {
	accts := p.accounts()
	if len(accts) == 0 {
		log.Printf("loomy: 面板预热一轮 —— **凭证目录里读不到账号**（authDir=%q）⇒ 什么都没做", p.authDir)
		return 0
	}

	// ① 挑这一轮要处理的：**优先补"从没读到过"的**，有余量再刷**过期的**。
	//
	// ⚠ 次序是**有意**的，不是随便排的：从没读到过的行在前端就是「正在读取…」（空着），
	//   而过期的行**至少还有旧值可看** ⇒ 补数比刷新更该优先。
	// ⚠⚠ 第二类（过期）就是设计里"on-read / 定时兜底"**共同的落点**：
	//   前端翻页时 `tasksCachedPage` 只读缓存、把过期行**顺手标出来**，
	//   真正的刷新动作一律由这里发生 —— 于是请求路径上永远不发上游请求。
	//
	// ⓪ 先修剪"已删账号"的残留行（幽灵行）—— 否则它们既算过期、又永远留在缓存里
	//   （见文件头 Ⅱ）。⚠ 必须在 planPanelWarmup **之前**：那里的计数依赖
	//   "缓存里只剩活号"这个前提；顺序反了会先算出一堆幽灵、再删掉它们。
	pruned := p.prunePanelOrphans(accts)
	if pruned > 0 {
		log.Printf("loomy: 面板缓存修剪 %d 个已删账号的行（不在账号池里的残留；不清掉会让'过期数'永远大于账号数，并让预热空转）", pruned)
	}

	pl := p.planPanelWarmup(accts, time.Now())
	jobs := pl.jobs
	// ⚠ 待办总数 = 没缓存的 + 过期的（调用方据此决定下一次扫描等 5 秒还是 5 分钟）。
	// ⚠⚠ 这个数**不含幽灵行**（见 panelPlan.pending 与 planPanelWarmup 的注释）。
	left := pl.pending()
	if len(jobs) == 0 {
		return left // ⚠ 返回 left 而不是 0：否则"有活但本轮配额已满"会被当成"没活了"
	}

	// ② 限并发跑（⚠ 顺序跑太慢：单号约 10 秒 ⇒ 12 个要 2 分钟，而 4 并发只要 30 秒）。
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr string
	sem := make(chan struct{}, panelWarmupConcurrency)
	done := 0
	for _, j := range jobs {
		wg.Add(1)
		go func(j panelJob) {
			defer wg.Done()
			defer func() { _ = recover() }() // ⚠ 一个号 panic 不该带走整轮
			sem <- struct{}{}
			defer func() { <-sem }()
			var errMsg string
			// ⚠⚠ 这里**故意不刷额度**。
			//
			// 用户 2026-10-18 问："调用一次账号就返回额度，直接写回本地磁盘缓存不行吗？
			// 为什么前端显示的是过时数据？" —— 答案分三层，记在这里免得下次重走弯路：
			//
			//	① **chat 回执里没有余额字段**（`client.go` 的响应结构里没有
			//	   Balance/remaining/quota 任何一个）⇒ "每次调用写回真实余额"做不到。
			//	② **也不能拿过期值做减法**：`gateway/quota_consumed.go` 记着那次教训 ——
			//	   "基于过期值做减法 ⇒ 多扣"，而那个数会喂给**选号权重**，
			//	   把一个还能用的号提前降权。
			//	③ ⚠ **loomy 这层写不了余额**：本包能拿到的只有 `freePool`
			//	   （`p.pool()` 返回 `*freePool`，它只有 `noteQuota` 那套**免费额度**账），
			//	   而余额的落账口是核心的 `pool.Pool.SetQuota` —— **不在本包的依赖方向上**。
			//	   ⚠ 硬塞会在编译期就断（试过了：`p.pool().SetQuota undefined`）。
			//
			// ⇒ 正解是**在核心侧**做定时刷新（见 internal/server 的额度刷新循环），
			//   本层继续只负责"面板行缓存 + 免费额度账"。⚠ 这正是依赖方向该有的样子。
			if j.which == "tasks" {
				if v := p.tasksViewFor(j.a); !v.OK {
					errMsg = v.Error
				}
			} else {
				if v := p.inviteViewFor(j.a); !v.OK {
					errMsg = v.Error
				}
			}
			mu.Lock()
			done++
			if errMsg != "" && firstErr == "" {
				firstErr = errMsg
			}
			mu.Unlock()
		}(j)
	}
	wg.Wait()

	// ⚠ `首个错误` 由 panelWarmupLogLine 决定**要不要出现**：本轮无错时**不打**这个字段
	//   （打 `首个错误=""` 是纯噪音，用户已经报过一次）。
	log.Print(panelWarmupLogLine(pl, len(accts), done, firstErr))
	// ⚠ 返回**剩余待办数**（不是本轮发起数）—— 调用方据此决定"连着刷"还是"歇一会儿"。
	// ⚠ 口径：本轮开始时的待办总数（未缓存 + 过期）减掉本轮真正做完的。
	//   本轮成功的号会进缓存并带上新的 FetchedAt ⇒ 下一次 sweep 的待办自然变小。
	//   ⚠ 失败的号会**留下来**（不进缓存）⇒ 待办不减 ⇒ 会继续被排到 ——
	//     这正是"偶发失败能被自动重试"的机制（配合 freeFailBackoff 式的退避）。
	left -= done
	if left < 0 {
		left = 0
	}
	return left
}

// panelJob 一轮预热里的一个待办：某个号的某个面板。
type panelJob struct {
	a     accountRef
	which string // "tasks" | "invite"
}

// panelPlan 一轮预热的"待办画像"（算出来的，不含"跑"）。
type panelPlan struct {
	jobs []panelJob
	// todoT/todoI 从没读到过的（前端显示「正在读取…」）。
	todoT, todoI int
	// expT/expI 过了 TTL 的（有旧值可看，只是该刷新）。
	//
	// ⚠⚠ 两者都**只数还在账号池里的号**：缓存里可能存在已删账号的残留行（幽灵行），
	//   把它们数进来就会让"过期数 > 账号数"，并让调用方以为"还有活"永远空转（见文件头 Ⅱ）。
	expT, expI int
}

// pending 待办总数：调用方据此决定下一次扫描等 5 秒（追赶态）还是 5 分钟（稳定态）。
//
// ⚠⚠ **不含幽灵行**（见 panelPlan.expT/expI 的注释）。
// ⚠ 抽成方法而不是在调用处写算式：这样"幽灵行算不算待办"这件事只有一个落点，
//
//	测试也能断言到**生产用的同一个算式**（否则测试里重写一遍，等于没测）。
func (pl panelPlan) pending() int { return pl.todoT + pl.todoI + pl.expT + pl.expI }

// planPanelWarmup 算出这一轮"要补哪些号、有多少待办"——**纯判断，不发任何请求**。
//
// ⚠ 与"跑"分开是刻意的：这段只在**内存缓存**上做判断，所以能被单测完整覆盖；
//
//	而"幽灵 uid 被算成待办"这个真 bug（见文件头 Ⅱ）恰好就出在这里 ——
//	抽出来之前，它只能靠"起一个真实例跑一轮 + 人肉比对账号数与日志"才看得见。
func (p *Provider) planPanelWarmup(accts []accountRef, now time.Time) panelPlan {
	var pl panelPlan
	byUID := make(map[string]accountRef, len(accts))
	for _, a := range accts {
		byUID[a.UID] = a
	}

	inJobs := make(map[string]bool, panelWarmupBatch)
	add := func(a accountRef, which string) {
		key := a.UID + "|" + which
		if inJobs[key] || len(pl.jobs) >= panelWarmupBatch {
			return
		}
		inJobs[key] = true
		pl.jobs = append(pl.jobs, panelJob{a, which})
	}

	// ① 从没读到过的在前、有过期的在后（次序理由见 sweepPanels 的注释）。
	for _, a := range accts {
		if _, ok := p.tasksPanel().get(a.UID); !ok {
			pl.todoT++
			add(a, "tasks")
		} else if _, ok := p.invitePanel().get(a.UID); !ok {
			pl.todoI++
			add(a, "invite")
		}
	}

	// ② 还有余量 ⇒ 把**过期的**补上（这是"刷新"那条路；TTL 见 panelttl.go）。
	//
	// ⚠⚠ 计数必须在"这个 uid 还在账号池里"的判断**之内**（2026-11-02 修的真 bug）：
	//   缓存是**落盘**的，删号不清缓存 ⇒ 里面会留着幽灵行，而幽灵行**永远补不上**
	//   （账号都没了）⇒ 待办恒 > 0 ⇒ 调度器 5 秒一轮地空转。
	//   实测：缓存 46 行 / 账号池 44 个 ⇒ 报「过期(tasks=46 invite=46) · 共 44 个号」。
	//   ⚠ 这里再兜一层 `byUID` 判断（sweepPanels 已在最前面修剪过一次）：
	//     修剪是"尽量清"，而计数是"绝不能错"—— 后者不能依赖前者成功。
	for _, uid := range p.expiredTasks(now, false) {
		a, ok := byUID[uid]
		if !ok {
			continue // 幽灵行：不属于任何活账号 ⇒ 不计入待办
		}
		pl.expT++
		add(a, "tasks")
	}
	for _, uid := range p.expiredInvites(now, false) {
		a, ok := byUID[uid]
		if !ok {
			continue
		}
		pl.expI++
		add(a, "invite")
	}
	return pl
}

// prunePanelOrphans 把"账号池里已经没有的 uid"从两个面板缓存里删掉，返回删掉的行数。
//
// ⚠ 为什么不"只在计数处过滤"就够（两件事必须一起做）：
//
//	① 计数过滤只治症状 —— 幽灵行仍然**永远留在 panels.json 里**；
//	② 缓存是落盘的，每删一个号就再留一行 ⇒ 缓存只增不减（无限膨胀），
//	   而且每一轮预热都要遍历它们、每次落盘都要序列化它们。
//
// ⚠ 唯一的"删数据"动作，所以边界写清楚：**只删 uid 不在当前账号池里的行**，
//
//	账号池里有的行（哪怕很久没刷）一行都不动。
func (p *Provider) prunePanelOrphans(accts []accountRef) int {
	keep := make(map[string]struct{}, len(accts))
	for _, a := range accts {
		if a.UID != "" {
			keep[a.UID] = struct{}{}
		}
	}
	// ⚠⚠ **先把两个缓存都取到手，再动手修剪**（顺序不是风格问题，是正确性问题）。
	//
	// 两个缓存都是**惰性**创建的（第一次访问时从 panels.json 读盘），而 pruneExcept
	// 删掉行之后会**同步**触发落盘（onChange → throttledSavePanels），落盘写的是
	// **两个缓存的合并快照**。
	//
	// 写成 `p.tasksPanel().pruneExcept(keep) + p.invitePanel().pruneExcept(keep)` 时，
	// Go 先算左操作数 ⇒ tasks 修剪 → 同步落盘 → 此刻 `p.panelInvite` **还是 nil**
	// ⇒ 落盘把 invite 段写成空的 ⇒ 随后才惰性创建的 invite 缓存从**这份被截断的文件**
	// 读回来 ⇒ **invite 缓存整段丢失**（界面上就是 44 个号的邀请码全变「正在读取…」）。
	//
	// 实测（2026-11-02，真机跑这一轮）：日志出现「未缓存(tasks=0 invite=44)」——
	// 44 个号的 invite 行**明明都在盘上**，却全被判成"从没读到过"；
	// 而 panels.json 的 invite 段被写成 46 → 0 → 回填 37（永远补不齐）。
	// ⚠ 这与"幽灵行"是同一段代码里的**两个**坑，所以两条回归测试都要有。
	tasks := p.tasksPanel()
	invites := p.invitePanel()
	return tasks.pruneExcept(keep) + invites.pruneExcept(keep)
}

// panelWarmupLogLine 组装一轮预热的日志行。
//
// ⚠ `首个错误` **只在真有错时出现**（2026-11-02）：firstErr == "" 表示本轮确实没出错，
// 那时打 `首个错误=""` 是纯噪音 —— 用户就是看着这行报"这些打印的东西感觉有问题"的。
// 反过来，有错时它必须出现（排障靠它）。
func panelWarmupLogLine(pl panelPlan, accts, done int, firstErr string) string {
	line := fmt.Sprintf("loomy: 面板预热一轮 —— 待办：未缓存(tasks=%d invite=%d) · 过期(tasks=%d invite=%d) · "+
		"共 %d 个号 · 本轮发起 %d 个",
		pl.todoT, pl.todoI, pl.expT, pl.expI, accts, done)
	if firstErr != "" {
		line += fmt.Sprintf(" · 首个错误=%q", firstErr)
	}
	return line
}
