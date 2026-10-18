package loomy

import (
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
//   本仓已有的后台任务都是这个形态，不新造（AGENTS 第 8 条：能抄则抄）。
// ⚠ 除了等第一个 tick，**启动时先立刻跑一轮**：冷启动后用户往往马上打开页面，
//   等 60 秒才补第一轮太晚。
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
//   只判其中一个会让另一半永远空着（那正是"新手任务好了、邀请码还是正在读取"的形态）。
// ⚠ 返回值与日志都为**排障**而留 —— 没有它们，"预热有没有在动、为什么没补上"
//   就只能靠猜（今天已经猜了很久）。
func (p *Provider) sweepPanels() int {
	accts := p.accounts()
	if len(accts) == 0 {
		log.Printf("loomy: 面板预热一轮 —— **凭证目录里读不到账号**（authDir=%q）⇒ 什么都没做", p.authDir)
		return 0
	}

	// ① 先挑出这一轮要补谁（顺序扫，只读缓存）。
	type job struct {
		a     accountRef
		which string // "tasks" | "invite"
	}
	var jobs []job
	todoT, todoI := 0, 0
	for _, a := range accts {
		if _, ok := p.tasksPanel().get(a.UID); !ok {
			todoT++
			if len(jobs) < panelWarmupBatch {
				jobs = append(jobs, job{a, "tasks"})
			}
		} else if _, ok := p.invitePanel().get(a.UID); !ok {
			todoI++
			if len(jobs) < panelWarmupBatch {
				jobs = append(jobs, job{a, "invite"})
			}
		}
	}
	if len(jobs) == 0 {
		return 0
	}

	// ② 限并发跑（⚠ 顺序跑太慢：单号约 10 秒 ⇒ 12 个要 2 分钟，而 4 并发只要 30 秒）。
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr string
	sem := make(chan struct{}, panelWarmupConcurrency)
	done := 0
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { _ = recover() }() // ⚠ 一个号 panic 不该带走整轮
			sem <- struct{}{}
			defer func() { <-sem }()
			var errMsg string
			// ⚠⚠ **顺手把额度也刷回池里**（用户 2026-10-18 的核心问题）。
			//
			//	"我们的要求不是说我们调用一次调用一个账号一次请求，它会返回出额度吗？
			//	 然后直接写回本地的磁盘文件缓存……但为什么会出现前端显示是过时的数据？"
			//
			// ⚠ 事实：**chat 回执里根本没有余额字段**（`client.go` 的响应结构里没有
			//   Balance/remaining/quota 任何一个）⇒ "每次调用写回真实余额"做不到。
			// ⚠ 而 `gateway/quota_consumed.go` 又明确禁止"拿过期值做减法"：
			//   那样算出来的余额会让**选号权重**多扣，把一个还能用的号提前降权。
			// ⇒ 唯一正确做法：**让刷新自动、持续地发生**（而不是只在用户点按钮时）。
			//   ⚠ `RefreshQuota` 会写回池子（并置 dirty ⇒ 5 秒后落进 state.json）
			//     以及档位缓存 —— 于是盘上的额度**跟着这个循环变新**。
			// ⚠ 它同时是这个循环"本来就该做的一件事"：`tasksViewFor` 已经查过一次
			//   积分接口，再问一次余额是重复劳动，但两者走的是不同端点
			//   （/points/records vs 余额），合并的收益小于"两条独立失效"的风险。
			// ⚠ 关键：**自己写回池子** —— 核心平时是靠"调 RefreshQuota 之后执行
			//   SetQuota"来落这笔账的（见 gateway/quota_consumed.go 的说明），
			//   而我们这里是 loomy 内部直接调，所以必须自己补上这一步。
			//   ⚠ `SetQuota` 会 `dirty.Store(true)` ⇒ **5 秒后落进 state.json**
			//     （`pool.flushInterval`）⇒ 前端读到的就是**这个循环刷新的值**。
			// ⚠ `!HasData` 时**一律不写**：那是"没查到"，写成 0 会把一个还能用的号
			//   显示成额度耗尽（本仓"未知 ≠ 零"的纪律，见 quota_ext.go 的注释）。
			if view, ok := p.RefreshQuota(j.a.UID); ok && view.HasData {
				p.pool().SetQuota(j.a.UID, view)
			}
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

	log.Printf("loomy: 面板预热一轮 —— 待补 tasks=%d invite=%d（共 %d 个号）· 本轮发起 %d 个 · 首个错误=%q",
		todoT, todoI, len(accts), done, firstErr)
	// ⚠ 返回**剩余待补数**（不是本轮发起数）—— 调用方据此决定"连着补"还是"歇一会儿"。
	// ⚠ 用 todo（本轮开始时的缺口）减掉本轮的 done 是最稳的口径：
	//   本轮成功后那些号就进缓存了，所以下一次 sweep 的 todo 自然会变小。
	left := todoT + todoI - done
	if left < 0 {
		left = 0
	}
	return left
}
