// models_flight.go —— 给「按上游取模型目录」加一层：**正缓存 + 单飞 + stale-while-revalidate**。
//
// # 为什么需要它（2026-10-20 实测，用户报"可用模型加载很慢，是不是被阻塞了"）
//
// 事实（打的是用户正在跑的实例）：
//
//	/healthz     7 ms      ← 进程没被阻塞，其它端点都是毫秒级
//	/ui          1 ms
//	/admin/...   1 ms (401)
//	/v1/models   第1次 36.2 s，之后 2.8 s / 5.2 s / 23.8 s（连打 4 次**不收敛**）
//
// 也就是说：慢点**只在这一条**，而且每次调用都可能重新打一遍上游
// （12 个上游逐个拉，单上游 0.2~23.7 s 的画像见 docs/known-issues.md），
// 并发进来的多个请求各拉一遍 —— 没有单飞 ⇒ 请求数越多越慢（雷群）。
//
// # 三条硬约束（都有理由，别顺手删）
//
//  1. **不引入 ticker**：只有"被问到"才可能拉上游，没人查询就一次请求都不发
//     （与 dynamicModelsCache / modelCatalogCache 同一套心智模型）。
//  2. **失败永不向上传播**：拿不到就返回 nil/false，调用方自己降级成空目录；
//     这个包以前就因为"把上游抖动打成 500"被用户报过。
//  3. **有旧目录就先给旧的**：目录过期不等于不能用 —— 用 30 秒前的目录比让用户
//     白等 23 秒好得多。刷新在**后台**做，且用 TryLock 保证"只有一个在刷"。
//
// ⚠ 与 modelCatalogCache 是两份缓存：那份是 /v3/config（成本系数，按 provider 分域），
// 这份是 /console/enterprises/personal/models（模型可用性，按上游取）。
// 两者数据源与失败模式都不同（见 handler.go 里 modelCatalogEntry 的注释），不要合并。
package server

import (
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// modelsCacheTTL 正缓存时长。与 dynamicModelsCache / modelCatalogCache 保持同一个值 ——
// "什么时候会打上游"在代码里只该有一套答案。
const modelsCacheTTL = time.Hour

// providerModelsEntry 单个上游的模型目录缓存。
type providerModelsEntry struct {
	infos   []gateway.ModelInfo
	fetched time.Time
}

var providerModelsCache struct {
	sync.RWMutex
	entries map[string]*providerModelsEntry
	// refreshing 记录"哪个上游正在后台刷新"。它只用来回答 TryLock 那一个问题：
	// 已经有一个人在刷时，后来者立刻返回旧目录，不再排第二个人去刷。
	refreshing map[string]bool
}

// modelsForProviderCached 取某个上游的模型目录。
//
// fetch 由调用方给出（而不是在这里持有 *Handler / router 接口）：
//   - 调用点只写一个闭包，不必让本文件认识上游接口的形状；
//   - 测试可以直接塞一个"数调用次数"的桩闭包 —— 守卫测试正是靠这一点
//     证明"连打两次只打一次上游"与"并发 8 个只打一次上游"。
//
// 返回值语义与原来直接调 fetch 完全一致：拿不到就是 (nil, false)。
func modelsForProviderCached(id string, fetch func() ([]gateway.ModelInfo, bool)) ([]gateway.ModelInfo, bool) {
	// ① 快路径：正缓存命中 ⇒ 毫秒返回，不碰上游。
	providerModelsCache.RLock()
	e := providerModelsCache.entries[id]
	providerModelsCache.RUnlock()
	if e != nil && len(e.infos) > 0 && time.Since(e.fetched) < modelsCacheTTL {
		return e.infos, true
	}

	// ② 有旧目录（过期但非空）⇒ **先把旧的给出去**，刷新丢到后台。
	//    这是"页面不再等 23 秒"的关键一步；也正因为这里不阻塞，用户点任何按钮都立刻有反应。
	if e != nil && len(e.infos) > 0 {
		stale := e.infos
		if tryBeginRefresh(id) {
			go func() {
				defer endRefresh(id)
				if infos, ok := fetch(); ok && len(infos) > 0 {
					storeProviderModels(id, infos)
				}
			}()
		}
		return stale, true
	}

	// ③ 冷启动（从来没有目录）⇒ 必须同步取一次，但**同一上游只允许一个**在取。
	//    后来者在锁上等到它把缓存写好，然后走 ① 命中 —— 所以并发 N 个请求只打一次上游。
	mu := providerModelsLockFor(id)
	mu.Lock()
	defer mu.Unlock()
	// double-check：等锁期间别人可能已经填好了。
	providerModelsCache.RLock()
	e = providerModelsCache.entries[id]
	providerModelsCache.RUnlock()
	if e != nil && len(e.infos) > 0 && time.Since(e.fetched) < modelsCacheTTL {
		return e.infos, true
	}
	infos, ok := fetch()
	if !ok || len(infos) == 0 {
		// 负缓存**不写**：没有目录就是没有，下次问还会再试一次（与既有语义一致）。
		return nil, false
	}
	storeProviderModels(id, infos)
	return infos, true
}

// storeProviderModels 写入缓存（拷贝一层切片，避免调用方后续改动影响缓存）。
func storeProviderModels(id string, infos []gateway.ModelInfo) {
	cp := make([]gateway.ModelInfo, len(infos))
	copy(cp, infos)
	providerModelsCache.Lock()
	if providerModelsCache.entries == nil {
		providerModelsCache.entries = map[string]*providerModelsEntry{}
	}
	providerModelsCache.entries[id] = &providerModelsEntry{infos: cp, fetched: time.Now()}
	providerModelsCache.Unlock()
}

// tryBeginRefresh 申请"由我来刷新这个上游"；已有人在刷则返回 false。
func tryBeginRefresh(id string) bool {
	providerModelsCache.Lock()
	defer providerModelsCache.Unlock()
	if providerModelsCache.refreshing == nil {
		providerModelsCache.refreshing = map[string]bool{}
	}
	if providerModelsCache.refreshing[id] {
		return false
	}
	providerModelsCache.refreshing[id] = true
	return true
}

func endRefresh(id string) {
	providerModelsCache.Lock()
	delete(providerModelsCache.refreshing, id)
	providerModelsCache.Unlock()
}

// providerModelsLocks 每个上游一把互斥锁（惰性创建）。
//
// 用"按上游分域"而不是一把全局锁：一个慢上游（实测 loomy 单次能到 23.7 s）
// 不该把别的上游的取数一起堵住。
var providerModelsLocks struct {
	sync.Mutex
	m map[string]*sync.Mutex
}

func providerModelsLockFor(id string) *sync.Mutex {
	providerModelsLocks.Lock()
	defer providerModelsLocks.Unlock()
	if providerModelsLocks.m == nil {
		providerModelsLocks.m = map[string]*sync.Mutex{}
	}
	if mu, ok := providerModelsLocks.m[id]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	providerModelsLocks.m[id] = mu
	return mu
}

// resetProviderModelsCache 清空模型目录缓存（供「强制刷新目录」与测试使用）。
func resetProviderModelsCache() {
	providerModelsCache.Lock()
	providerModelsCache.entries = nil
	providerModelsCache.refreshing = nil
	providerModelsCache.Unlock()
}
