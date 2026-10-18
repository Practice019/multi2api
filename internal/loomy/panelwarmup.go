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
	// panelWarmupInterval 两轮之间的间隔。
	panelWarmupInterval = 60 * time.Second
	// panelWarmupBatch 每轮最多补几个号。
	//
	// ⚠ 取 12 而不是 8：一轮的耗时 ≈ 批量 / 并发 × 单号耗时，批量太小会让
	//   "轮与轮之间的 60 秒空等"占掉大头（8 个号 × 10s / 4 并发 = 20s，剩下 40s 在等）。
	panelWarmupBatch = 12
	// panelWarmupConcurrency 补数的并发度。
	//
	// ⚠⚠ **不能提高**：上游实测 **12 条并发就打 502**（见 adminpoints 里
	//   LAZY_MAX_INFLIGHT 的注释，那是踩过的坑）。4 是"快 4 倍"与"不打急上游"的平衡点。
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
			// ⚠ 启动时连着跑两轮：第一轮把"最早的 12 个号"补上，第二轮接着往下
			//（每轮各限 panelWarmupBatch，所以两轮不等于"一次补 24 个"导致的并发失控）。
			p.sweepPanels()
			p.sweepPanels()
			t := time.NewTicker(panelWarmupInterval)
			defer t.Stop()
			for range t.C {
				p.sweepPanels()
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
	return done
}
