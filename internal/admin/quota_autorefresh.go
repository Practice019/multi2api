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
			for {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("loomy: 额度自动刷新**这一轮** panic 已被隔离：%v", r)
						}
					}()
					h.RefreshAllQuotasNow()
				}()
				time.Sleep(quotaAutoRefreshEvery)
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
