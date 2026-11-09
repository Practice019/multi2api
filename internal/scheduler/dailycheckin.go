// dailycheckin.go 统一签到：**一处注册，覆盖所有实现了 DailyCheckinExt 的上游**（T-427）。
//
// # 为什么有这个文件（用户报障）
//
//	「就是我们现在不是有很多上游都是有这个功能的吗？就是自动签到的功能，
//	  我们全部统一一下，全部统一到一个时间，统一签到。」
//	「但是你说这个有定时自动签到，但是它自动签到好像没有用，
//	  因为这两个账号都是我自己手动签的。」
//
// 改造前：8 个上游都实现了 `CheckinAll`，但**只有 2 个**（trae / workbuddy）
// 注册了签到 Job ⇒ 另外 6 个的自动签到**静默失效**
// （实测：checkin-log.json 里 trigger=schedule 的 codar 记录 0 条）。
//
// # 为什么是一处注册，而不是每个上游各写一个 Job
//
// 逐个补 = 6 份几乎相同的 Job + Due + 已签判断 ⇒ 迟早漂移。
// 本仓已有先例：`shortUID` 在 8 个包里复制了 8 份，最后要专门花一轮合并。
// ⇒ 一处注册 + 在 Run 里遍历 Registry 发现所有 DailyCheckinExt
//
//	⇒ 未来新增上游**只要实现接口就自动被管，零注册代码**。
//
// 设计见 `docs/design/design-notes.md`（Q1/Q2/Q3/Q6 的结论都落在本文件）。
//
// # ★ 幂等为什么必须由**调度侧**保证（本文件最关键的取舍）
//
// 各上游的 `CheckinAll` **内部并不跳过已签账号**（codar 的会 doSign 每一个）。
// 若只靠 1 小时间隔、指望上游自己幂等 ⇒ 同一账号一天被 POST **24 次**，
// 而 codar 面板明确写着「上游已有风控记录，请低频使用」⇒ 那会把账号打坏。
// ⇒ 所以这里有一道**每上游每天一趟**的闸门：今天已成功扫过的上游，
//
//	**连 CheckinAll 都不调**（性能梯级 L1「别做这件事」，不只是 L2「少做几次」）。
//
// # 性能梯级（见 design-notes §3）
//
//	L1：闸门放行才调 CheckinAll —— 今天已扫过的上游调用数是 **0**（不是"调了内部跳过"）
//	L2：每上游**每天 1 趟**（对比按 30 分钟裸扫 = 每上游每天 48 趟）
//	Due：**只读内存**，零网络零磁盘 —— 它每个 tick（30 秒）都会被问一次，
//	     8 个上游 × 2880 tick = 23040 次/天，绝不能在里面发请求。
//	     ⚠ 这条由 cmd/server 的源码级断言钉住（Due 体里不许出现 CheckinAll / http.*）
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// DailyCheckinJobName 任务名。被管理台的任务列表按名字索引（见 admin/jobLabels）。
const DailyCheckinJobName = "core-daily-checkin"

// dailyCheckinNow 取当前时刻。**测试可替换**（生产永远是 time.Now）。
//
// # 为什么需要它
//
// `lastOKDay` 的语义是「哪个**本地自然日**已经成功扫过」。它错了的后果是
// **两种相反的灾难**：
//
//	· 永远不重置 ⇒ 第二天再也不签到（用户每天丢额度，而日志上看不出异常）
//	· 重置错了   ⇒ 一天签多次（codar 有风控，会打坏账号）
//
// 而这恰恰是**只有跨天才能暴露**的 bug ⇒ 不给测试一个可控时钟，
// 就等于不测这个设计的核心（见 TestDailyCheckinJobResetsAcrossDays）。
//
// ⚠ 用包级变量而不是给构造函数加参数：Run 的闭包形状由 gateway.Job 规定
// （`func(ctx) error`，拿不到额外参数）⇒ 换它就要动契约层，为一个测试不值得。
// 测试里替换、Cleanup 还原即可。
var dailyCheckinNow = time.Now

// retryFloor 两次尝试之间的**最小间隔**（★ T-435）。
//
// # 为什么需要它（实测事故，2026-11-09）
//
// `jobDue` 的既定语义是「**有 Due 就完全交给 Due**」（见 job.go 的注释：
// Interval 被有意忽略，避免给错峰任务配的 Interval 变成隐式的额外门槛）。
// 而本 Job 的 Due 对「上一轮失败的上游」永远返回 true（设计本意：失败要重试）
// ⇒ 变成每个调度 tick（30s）都重试一次。
//
// 实测：部署后 75 秒内写了 59 条扫描记录（**约 40 秒一次**），
// 3 个失效号每天约 4300 次请求 —— 而 codar 面板写着「上游已有风控记录，请低频使用」。
// ⇒ 这正是 L1/L2 闸门要避免的事。教训：**有 Due 的任务必须自己管时间下限。**
//
// # 为什么取 30 分钟
//
// 用户要「每小时一趟」，但失败要重试：更短（5 分钟）对风控不友好；
// 更长（1 小时）等于没有重试 ⇒ 30 分钟 = 「最多两次/小时」的折中。
//
// ⚠ 它必须是**包级变量**而不是常量：测试要能读到它（断言"下限之内不重试"）；
//
//	也不放在测试文件里 —— 那样产品代码就引用不到（实测编译失败过一次）。
var retryFloor = 30 * time.Minute

// NewDailyCheckinJob 造「统一签到」这条核心自带的任务。
//
// reg 为 nil（或没有任何上游实现 DailyCheckinExt）时：Due 恒为 false、Run 是空操作
// —— 与 `Config.Registry` 为 nil 时调度器的既有退化行为一致。
//
// ⚠ 由装配层（cmd/server）用 `Scheduler.AddJob("core", …)` 注册：
//
//	它是**跨上游**的机制，不属于任何单个上游（与账号池健康检查同一条）。
//
// rec（T-433）：账号禁用/恢复能力，由装配层注入（cmd/server 传 pool）。
// ⚠ 传 nil 合法（退化成"只签到、不管禁用"）—— 与 Registry 为 nil 的既有退化一致。
func NewDailyCheckinJob(reg *gateway.Registry, lg *checkinlog.Log, rec DisableRecorder) gateway.Job {
	var mu sync.Mutex
	// day 下面 donePids 所属的本地自然日（YYYY-MM-DD）。跨天 ⇒ 整张表重置。
	day := ""
	// donePids **今天已成功扫过**的上游 ⇒ 这些连 CheckinAll 都不调（L1 闸门）。
	//
	// ⚠ 必须**按上游**记，不能只记一个全局的「今天扫过了」：
	//   那样只要有一个上游失败、退了全局标记，就会把**所有**上游再打一遍。
	//   —— TestDailyCheckinJobRetriesFailedUpstream 实测抓到的第一个缺陷。
	donePids := map[string]bool{}
	// lastTry 每个上游**上次尝试**的时刻（不论成败）★ T-435。
	//
	// # 为什么必须有它（实测事故，2026-11-09）
	//
	// `jobDue` 的既定语义是「**有 Due 就完全交给 Due**」（见 job.go 的注释：
	// Interval 被有意忽略，避免给错峰任务配的 Interval 变成隐式的额外门槛）。
	// 而本 Job 的 Due 对「上一轮失败的上游」永远返回 true（设计本意：失败要重试）
	// ⇒ 变成每个调度 tick 都重试一次。
	//
	// 实测：部署后 75 秒内写了 59 条扫描记录（**约 40 秒一次**），
	// 3 个失效号每天约 4300 次请求 —— 而 codar 面板写着「上游已有风控记录，请低频使用」。
	// ⇒ 这正是 L1/L2 闸门要避免的事。**Due 必须自己管时间下限。**
	lastTry := map[string]time.Time{}

	// localDay 本地自然日。**与 checkinlog 的「今天」同一基准**（都是本地 0 点起的
	// 那个自然日）—— 两边基准不同会出现"上午签过、次日又签一次"或漏签一天（notes Q5③）。
	//
	// ⚠ 用传入的 t，**不要**用 checkinlog.TodayStart()：后者读的是进程的真实此刻，
	//   在注入时钟的测试里会让"跨天"永远测不出来
	//   —— TestDailyCheckinJobResetsAcrossDays 实测抓到的第二个缺陷
	//（那个缺陷的真实后果：第二天再也不签到，用户每天丢额度且日志上看不出异常）。
	localDay := func(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

	// needSweep 该上游此刻是否还需要扫（**只读内存**）。两道闸门：
	//
	//	① 跨天，或今天还没成功扫过          （原有）
	//	② 距**上次尝试** ≥ retryFloor       （★ T-435 新增，见 retryFloor 的注释）
	needSweep := func(pid string, now time.Time) bool {
		if localDay(now) != day {
			return true // 跨天 ⇒ 全都要重扫（Run 里会重置 donePids）
		}
		if donePids[pid] {
			return false // 今天已成功 ⇒ 当天剩余时间连 CheckinAll 都不调（L1）
		}
		if last, ok := lastTry[pid]; ok && now.Sub(last) < retryFloor {
			return false // ★ 第二道闸门：上次尝试还太近 ⇒ 不重试
		}
		return true
	}

	return gateway.Job{
		Name:     DailyCheckinJobName,
		Interval: time.Hour,
		// Due 必须**零外部调用**：它每 30 秒被问一次（见文件头）。
		Due: func(now time.Time) bool {
			if reg == nil {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			for _, p := range reg.All() {
				if _, ok := gateway.ExtOf[gateway.DailyCheckinExt](p); !ok {
					continue
				}
				if needSweep(p.ID(), now) {
					return true
				}
			}
			return false
		},
		Run: func(ctx context.Context) error {
			if reg == nil {
				return nil
			}
			now := dailyCheckinNow()
			today := localDay(now)
			// 本轮实际调用 CheckinAll 的上游（用于日志 —— 它是 L1 闸门生效的证据）
			called, skipped, succeeded := []string{}, []string{}, []string{}
			failed := 0
			seen := 0
			for _, p := range reg.All() {
				if err := ctx.Err(); err != nil {
					return err
				}
				ext, ok := gateway.ExtOf[gateway.DailyCheckinExt](p)
				if !ok {
					continue
				}
				seen++
				pid := p.ID()
				mu.Lock()
				ok2 := needSweep(pid, now)
				mu.Unlock()
				if !ok2 {
					// ★ L1：今天已成功扫过 ⇒ **连 CheckinAll 都不调**
					skipped = append(skipped, pid)
					continue
				}
				called = append(called, pid)
				// ★ T-435：先记「尝试时刻」（**不论成败**）—— 否则失败的上游
				//   会被每个调度 tick 重试（实测事故：约 40 秒一次，见 lastTry 的注释）。
				mu.Lock()
				lastTry[pid] = now
				mu.Unlock()
				rep := ext.CheckinAll(ctx)
				// ★ T-433：在 **Job 层**做「凭据失效 ⇒ 禁用 / 成功 ⇒ 恢复」。
				//
				// 为什么在这里而不是各上游：10 个上游里只有 lobsterai 走 dailycheckin.Driver，
				// 另外 7 个**手写** CheckinAll ⇒ 在各上游做要改 7 条手写路径 + 10 处注入点；
				// 而 DailyCheckinResult 已经带齐 UID/Status/Detail ⇒ 这里判就够，
				// **上游一行都不用改**。详见 dailycheckin_disable.go 的文件头。
				applyDisablePolicy(rec, rep.Results)
				if upstreamHadFailure(rep) {
					// ⚠ 失败**不标记完成** ⇒ 下一轮只重试它（其余上游不再被打扰）。
					//   —— 这里曾经是"记一个全局标记、全成功才置位"，于是
					//   TestDailyCheckinJobRetriesFailedUpstream 抓到"成功的上游也被扫了两次"。
					failed++
					continue
				}
				succeeded = append(succeeded, pid)
			}
			mu.Lock()
			if today != day { // 跨天 ⇒ 整张表重置（否则第二天再也不签）
				day = today
				donePids = map[string]bool{}
			}
			for _, pid := range succeeded {
				donePids[pid] = true
			}
			mu.Unlock()
			if lg != nil {
				lg.Append(checkinlog.Record{
					At: now, UID: "", Kind: checkinlog.KindActivity,
					Status: checkinlog.StatusOK, Trigger: "schedule",
					Detail: "统一签到扫描：扫 " + itoa(len(called)) + " 个上游（跳过 " +
						itoa(len(skipped)) + " 个今天已扫过的）；本轮失败 " +
						itoa(failed) + " 个",
				})
			}
			log.Printf("统一签到：本轮扫 %d 个上游（跳过今天已扫过的 %d 个，覆盖 %d 个可签到上游）；失败 %d 个",
				len(called), len(skipped), seen, failed)
			return nil
		},
	}
}

// upstreamHadFailure 该上游这一趟算不算失败（失败要下一小时重试）。
//
// 判据两条：整趟跑不动（Report.Error 非空），或**有任一账号失败**。
// ⚠ `already` **不算失败** —— 上游说"今天已领过"是正常状态
// （若把 already 算失败，每天都会重试一次、日志上永远一条红）。
func upstreamHadFailure(rep gateway.DailyCheckinReport) bool {
	if rep.Error != "" {
		return true
	}
	for _, r := range rep.Results {
		if r.Status == checkinlog.StatusFail {
			return true
		}
	}
	return false
}
