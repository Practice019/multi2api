package admin

import (
	"log"
	"time"
)

// 本文件：**额度自动刷新**。设计见 docs/cache-refresh-design.md 第 2 节（定时兜底）。
//
// # 它解决什么（用户 2026-10-18 的原话）
//
//	"我们的要求不是说我们调用一次调用一个账号一次请求，它会返回出额度吗？
//	 然后直接写回本地的磁盘文件缓存，前端就读取这个缓存就行了呀。
//	 但为什么会出现前端显示是过时的文件、过时的数据？"
//
// 查证结论（三层，都记在 panelwarmup.go 的同一处注释里）：
//  1. **chat 回执里没有余额字段** ⇒ "每次调用写回真实余额"做不到。
//  2. **也不能拿过期值做减法**（`gateway/quota_consumed.go` 记着那次"多扣"的教训，
//     而那个数会喂给**选号权重**）。
//  3. 所以真实余额只有两条来源：用户点「刷新额度」／`StartFreePoolSweep`（只查池子成员）
//     ⇒ ⚠ **界面那个值 = "上次刷新的值" = 过时**。
//
// ⇒ 唯一的正解就是**让刷新自动发生**：本文件那个循环。
// ⚠ 它必须在这里（admin 层）而不是 loomy 层：余额的落账口是核心的
//   `pool.Pool.SetQuota`，而 loomy 拿不到它（依赖方向不对 —— 试过了，编译期就断）。

// quotaAutoRefreshEvery 自动刷新的间隔。
//
// ⚠ 它取 loomy 的 `TTLQuota`（3 分钟）同一个值，理由是那个值的**消费者**：
// 额度是**选号权重**的输入，选号又决定"下一个请求发给谁"。3 分钟足够跟上
// "按分钟级使用"的节奏，而它把请求量压在 `44 号 / 3 分钟 ≈ 15 次/分钟`。
//
// ⚠ 不要调成"每次调用都刷"：那会把请求量变成跟调用量线性相关（实测口径
//
//	见设计文档第 4 节：100 调用/分 ⇒ 100 请求/分，是这里的 5 倍以上），
//	而收益只是"提前 3 分钟知道"。
const quotaAutoRefreshEvery = 3 * time.Minute

// 首轮**延迟**：错开启动瞬间的其它预热（面板预热、free pool 填充）。
//
// ⚠ 不 await、不抢跑：启动流程一毫秒都不该为它多等（与 admin.go 里那条
// "不 await" 的预热同一个约束）。
const quotaAutoRefreshFirstDelay = 20 * time.Second

// 自适应降速的**上限**。⚠ 上游不稳时最长让路 30 分钟，而不是无限退让 ——
// 无限退让的后果是"上游恢复了、额度却再也不更新"，而这件事**没有别的信号**
// （这条循环是唯一在刷额度的地方）。
const quotaAutoRefreshMaxEvery = 30 * time.Minute

// 连续多少轮"失败率过半"就退让一档。
//
// ⚠ 判据用**比例**而不是绝对数：实测 76 个号里失败 1 个（2026-10-18 的真实数据）
// 是正常抖动；76 个里失败 40 个才说明上游/网络出问题了 —— 两者必须区分得开，
// 否则一次偶发抖动就会让额度刷新**主动变慢**（那是在制造用户报的那个现象）。
const quotaAutoRefreshFailRun = 2

// quotaBackoffStep 自适应降速的**决策本身**，抽成纯函数。
//
// ⚠ 为什么不把它留在循环里：循环体带 `time.Sleep`，测它要真的等上一分钟；
//
//	而"退让的判据"是这条循环里**最容易错、错了又最不容易发现**的一段 ——
//	判错的方向有两个（该退不退 ⇒ 猛撞上游；不该退却退 ⇒ 额度悄悄变慢），
//	两个都不会报错。抽出来才能把每个方向都钉住。
//
// 语义：
//   - 这一轮正常（bad=false）⇒ 连续计数归零、间隔**立刻**回到基准
//     （退让是为了**让路**，不是为了惩罚上游；上游一好就该全速刷）
//   - 连续 `quotaAutoRefreshFailRun` 轮不正常 ⇒ 间隔翻倍，封顶 `quotaAutoRefreshMaxEvery`
//     ⚠ 封顶的意义：无限退让会让"上游恢复了、额度却再也不更新"变成一个**没有任何信号**
//     的状态（这条循环是唯一在刷额度的地方）。
func quotaBackoffStep(bad bool, badRuns int, every time.Duration) (int, time.Duration) {
	if !bad {
		return 0, quotaAutoRefreshEvery
	}
	badRuns++
	if badRuns >= quotaAutoRefreshFailRun && every < quotaAutoRefreshMaxEvery {
		every *= 2
		if every > quotaAutoRefreshMaxEvery {
			every = quotaAutoRefreshMaxEvery
		}
	}
	return badRuns, every
}

// StartQuotaAutoRefresh 起后台循环，按 `quotaAutoRefreshEvery` 刷**全部账号**的额度。
//
// ⚠ 幂等（`sync.Once`）：重复调用不会起第二个循环。
// ⚠ 不 await：立刻返回。
// ⚠ 每一轮都自带 `recover` ⇒ 一轮崩了不影响下一轮，更不影响进程。
func (h *Handler) StartQuotaAutoRefresh() {
	if h == nil || h.cfg.Pool == nil {
		return
	}
	h.quotaAutoRefreshOnce.Do(func() {
		log.Printf("loomy: 额度自动刷新循环已启动 —— 每 %v 一轮，首轮 %v 后",
			quotaAutoRefreshEvery, quotaAutoRefreshFirstDelay)
		go func() {
			// ⚠ 这一层的 recover 是整个 goroutine 的兜底：一旦漏出去，
			//   循环就**永久停摆**了，而症状是"额度又变回旧的" —— 靠日志才能发现。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("loomy: ⚠ 额度自动刷新循环**退出**了（panic=%v）—— 额度会停止自动更新", r)
				}
			}()
			time.Sleep(quotaAutoRefreshFirstDelay)
			// ⚠ **自适应降速**（设计第 5 节"全局预算"的最后一道闸门）。
			//
			// 判据是**失败比例**而不是绝对数：实测 76 个号里失败 1 个是正常抖动
			//（那一天的上游就是这样），而 76 个里失败 40 个才是"上游/网络出问题了"。
			// ⚠ 连续 `quotaAutoRefreshFailRun` 轮高失败 ⇒ 间隔翻倍，封顶
			//   `quotaAutoRefreshMaxEvery`；任一**轮正常** ⇒ 立刻恢复原间隔。
			//
			// ⚠ 为什么"立刻恢复"而不是"慢慢恢复"：退让是为了**让路**，不是为了惩罚上游。
			//   上游一好就该全速刷（额度是选号权重，慢了直接影响选号）。
			// ⚠ 为什么封顶 30 分钟：无限退让会让"上游恢复了、额度却再也不更新"
			//   变成一个**没有任何信号**的状态（这条循环是唯一在刷它的东西）。
			every := quotaAutoRefreshEvery
			badRuns := 0
			for {
				res := func() (r quotaRefresher) {
					defer func() {
						if rec := recover(); rec != nil {
							log.Printf("loomy: 额度自动刷新**这一轮** panic 已被隔离：%v", rec)
						}
					}()
					return h.RefreshAllQuotasNow()
				}()

				// 失败比例 > 一半视为"这一轮不正常"（分母含跳过/未知：
				// 它们也是"没刷到"的账号，只是原因不同）。
				considered := res.Updated + res.Unknown + res.Failed
				bad := considered > 0 && res.Failed*2 > considered

				prev := every
				badRuns, every = quotaBackoffStep(bad, badRuns, every)
				if every != prev {
					if every > prev {
						log.Printf("loomy: ⚠ 额度刷新连续 %d 轮失败偏多（本轮 %d/%d）⇒ 间隔退让到 %v",
							badRuns, res.Failed, considered, every)
					} else {
						log.Printf("loomy: 额度刷新恢复正常 ⇒ 间隔回到 %v", every)
					}
				}
				time.Sleep(every)
			}
		}()
	})
}

// RefreshAllQuotasNow 立刻刷一轮全部账号的额度，并把结果打出来。
//
// ⚠ 分开导出是为了让"定时"这一层薄到不需要测试：循环只负责节拍，
//
//	真正的动作是这个函数，它可以直接被单测与排障调用。
//
// ⚠ 全量传 uid 是**安全**的：`refreshQuotas` 内部按**账号自己**的 provider 找
//
//	额度扩展点，没实现 `QuotaExt` 的上游走"跳过"分支（计入 `Skipped`，
//	不是错误）—— 所以这里不需要先筛出"哪个上游支持额度"。
func (h *Handler) RefreshAllQuotasNow() quotaRefresher {
	if h == nil || h.cfg.Pool == nil {
		return quotaRefresher{}
	}
	list := h.cfg.Pool.List()
	uids := make([]string, 0, len(list))
	for _, st := range list {
		if st.UID != "" {
			uids = append(uids, st.UID)
		}
	}
	if len(uids) == 0 {
		return quotaRefresher{}
	}

	res := h.refreshQuotas(uids)
	// ⚠⚠ 这四个数是**分开**计的（`refreshQuotas` 的注释里有理由），所以日志也分开打：
	//   把它们合成一个"成功数"会让"某个上游整片被跳过"（Skipped）彻底不可见 ——
	//   而那正是本仓最需要人介入的状态。
	log.Printf("loomy: 额度自动刷新一轮 —— 账号 %d · 更新 %d · 未知 %d · 失败 %d · 跳过 %d · 上游 %v",
		len(uids), res.Updated, res.Unknown, res.Failed, res.Skipped, res.Providers)
	return res
}
