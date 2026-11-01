// models_prewarm.go 目录缓存的**后台预热** —— 关掉模型名校验的 fail-open 窗口。
//
// # 它解决什么（实测复现的现象）
//
// internal/server/model_check.go 的守卫只读 dynamicModelsCache。缓存**冷**时
// （刚重启 / 拉取失败进入 5min 负缓存期）裸名与未知名会放行（fail-open），于是
// 重启后的第一波请求里，写错的模型名仍会掉进账号池：逐个试 6 个账号、白烧上游配额，
// 最后回一个把锅甩给账号的 503。
//
// 实测：重启后立刻 POST model=definitely-not-a-model-xyz → 503；
//
//	先 GET 一次 /v1/models 把缓存灌热，同样的请求才变 400 unknown_model。
//
// # 为什么是"后台定时"，而不是"请求路径上兜底"（纪律）
//
// fetchDynamicModels 会**真发一次上游请求**。在每请求的校验里调它，等于
// "用一次上游调用来判断该不该调上游" —— 那会把一次白烧变成每次白烧。
//
// 所以纪律不变，且**绝不**在请求路径上调用本文件：
//
//	请求路径：只读缓存（model_check.go 的 bareNameInDefaultCatalog）
//	后台：把缓存灌热（本文件）
//
// # 为什么可以放心定时调 fetchDynamicModels
//
// 它自带两级节流，稳态下本预热只是读一次内存：
//
//	① 正缓存 1h（modelsTTL 内直接返回，不打上游）
//	② 失败负缓存 5min（冷却期内直接返回 nil，也不打上游）
package server

import (
	"context"
	"log"
	"time"
)

// ModelsPrewarmInterval 预热的默认检查间隔。
//
// 取 2 分钟的理由：稳态下它只是读一次内存（见文件头的两级节流），代价可以忽略；
// 而失败后重试要等 fetchDynamicModels 的 5min 负缓存过期 —— 间隔比 5min 短，
// 才能在那之后**尽快**把窗口关上（否则要多等一个周期）。
//
// 不要把它调成秒级：收益是零（TTL 没到就不会打上游），只是多几次锁竞争。
const ModelsPrewarmInterval = 2 * time.Minute

// prewarmFailLogEvery 拉取失败日志的最小间隔。
//
// 上游长时间不可用时，每 2 分钟刷一行日志没有信息增量（fetchDynamicModels 已经把
// lastFail 记进缓存，请求路径也不受影响）。半小时一行足够回答"预热到底在不在跑"。
const prewarmFailLogEvery = 30 * time.Minute

// StartModelsPrewarm 在**后台**把 dynamicModelsCache 灌热。
//
// 语义（三条都由测试钉住）：
//
//	① **不阻塞启动**：立刻返回，第一次刷新在 goroutine 里做（所以启动后 ~1s 内目录就热了，
//	   而不是等第一个请求把窗口撞开）
//	② **幂等**：对同一个 Handler 重复调用不会起第二个跑批（否则多份 goroutine 互相抢锁）
//	③ **可停**：ctx.Done() 退出循环（装配层传的是那个随优雅停机取消的 ctx）
//
// interval <= 0 时用 ModelsPrewarmInterval。
func (h *Handler) StartModelsPrewarm(ctx context.Context, interval time.Duration) {
	if h == nil {
		return
	}
	if interval <= 0 {
		interval = ModelsPrewarmInterval
	}
	h.prewarmMu.Lock()
	if h.prewarmOn {
		h.prewarmMu.Unlock()
		return // ② 幂等：第二次调用直接返回，不起第二份 goroutine
	}
	h.prewarmOn = true
	h.prewarmMu.Unlock()

	go func() {
		// ① 先立刻来一次：重启后的窗口靠这一下关上。
		h.prewarmModelsOnce()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return // ③ 随 ctx 退出（不泄漏 goroutine）
			case <-t.C:
				h.prewarmModelsOnce()
			}
		}
	}()
}

// prewarmModelsOnce 刷一次目录。**任何失败都不向上抛**（预热坏了不该影响进程）。
//
// 单独一个方法是为了让它可测：测试直接调它，不必等 ticker。
func (h *Handler) prewarmModelsOnce() {
	// ⚠ 兜底 recover：预热是"锦上添花"的后台活，任何 internal 错误都不该把进程带走。
	//   本仓其它后台循环（续期/签到）同款处理方式。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("模型目录预热: 本次刷新 panic 已被兜住（不影响请求路径）: %v", r)
		}
	}()

	refresh := h.cfg.ModelsRefresh
	if refresh == nil {
		// 未注入刷新函数 ⇒ 走默认实现。
		//
		// ⚠ 未装配（Pool 为 nil）时直接跳过而不是冒险调 fetchDynamicModels：
		//   后者第一件事就是 Pool.PickForExcluding，nil 池会 panic（测试与单上游裸跑都会遇到）。
		if h.cfg.Pool == nil || h.cfg.Upstream == nil {
			return
		}
		// 包一层只为对齐签名：fetchDynamicModels 返回模型列表，而预热**不关心返回值** ——
		// 它要的效果是"缓存被写入"，而守卫读的正是缓存（见 bareNameInDefaultCatalog）。
		refresh = func() { h.fetchDynamicModels() }
	}
	refresh()

	// 观测：只在"缓存仍然空"且距上次记日志够久时记一行。
	// 判据用缓存本身而不是刷新函数的返回值 —— 那才是守卫真正读的东西。
	if h.catalogCold() && h.prewarmFailLogDue() {
		log.Printf("模型目录预热: 缓存仍为空（上游不可用或正在负缓存期），%s 后重试；"+
			"在此之前，模型名校验对裸名/未知名是放行（fail-open）的",
			ModelsPrewarmInterval)
	}
}

// catalogCold 报告目录缓存是不是空的（守卫据此 fail-open）。
func (h *Handler) catalogCold() bool {
	dynamicModelsCache.RLock()
	empty := len(dynamicModelsCache.ids) == 0
	dynamicModelsCache.RUnlock()
	return empty
}

// prewarmFailLogDue 失败日志的限流（见 prewarmFailLogEvery）。
func (h *Handler) prewarmFailLogDue() bool {
	h.prewarmMu.Lock()
	defer h.prewarmMu.Unlock()
	if !h.prewarmLastFailLog.IsZero() && time.Since(h.prewarmLastFailLog) < prewarmFailLogEvery {
		return false
	}
	h.prewarmLastFailLog = time.Now()
	return true
}

// prewarmRunning 报告预热跑批是否已启动（测试用；生产不再关心这事）。
func (h *Handler) prewarmRunning() bool {
	h.prewarmMu.Lock()
	defer h.prewarmMu.Unlock()
	return h.prewarmOn
}
