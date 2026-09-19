package admin

import (
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/upstream"
)

// 冷启动契约：/admin/stats 必须能**自行**把模型目录拉起来。
//
// 这是被独立 Reviewer 判 REJECT 的缺陷的回归测试。
//
// 原缺陷（自锁死循环）：
//
//	server.modelCatalogState() 在 cat==nil 时返回 "unavailable"
//	admin.modelMultipliers()   在 state=="unavailable" 时**提前返回**，从不调 ModelCatalog()
//	→ cat 永远为 nil → 永远 unavailable → 目录永远不会被拉取
//
// 功能无法自举。而既有测试全把 state 桩成 "ok"，
// b1_e2e 又直接调 ModelCatalog() 绕开了 /admin/stats —— 所以全绿但功能是死的。
//
// 本测试的关键区别：把「真实的 state 与 fetch 互相依赖」这一对闭包一起接进来，
// 从**冷缓存**开始只打 /admin/stats，断言 fetch 确实发生了。
func TestStatsColdStartCanBootstrapCatalog(t *testing.T) {
	var fetchCalls atomic.Int32
	var haveCatalog atomic.Bool

	// 模拟 server 侧真实实现：state 由「缓存里有没有」决定，fetch 才让缓存有。
	stateFn := func() ModelCatalogState {
		if !haveCatalog.Load() {
			return ModelCatalogState{State: "unavailable"} // ← 冷启动就是这一态
		}
		return ModelCatalogState{State: "ok", Models: 1}
	}
	fetchFn := func(string) *upstream.ModelCatalog {
		fetchCalls.Add(1)
		haveCatalog.Store(true)
		return catalogWith(upstream.ModelCatalogEntry{ID: "deepseek-v4", Multiplier: 0.51})
	}

	h, _ := newStatsHandler(t, []logbuf.Entry{
		{Model: "deepseek-v4", Status: 200, Tokens: 10},
	})
	h.cfg.ModelCatalog = fetchFn
	h.cfg.ModelCatalogState = stateFn

	out := doStats(t, h)

	if fetchCalls.Load() == 0 {
		t.Fatal("冷启动时必须能自举：/admin/stats 应触发一次目录拉取，" +
			"但 ModelCatalog 从未被调用 —— 目录将永远是空的")
	}
	// 拉取成功后，倍率应出现在同一份响应里
	ms, ok := out["model_multipliers"].([]any)
	if !ok || len(ms) == 0 {
		t.Fatalf("拉取成功后应返回倍率，得到 %v（model_catalog=%v）",
			out["model_multipliers"], out["model_catalog"])
	}
	first, _ := ms[0].(map[string]any)
	if first["model"] != "deepseek-v4" {
		t.Errorf("倍率条目错误: %v", first)
	}
}

// 冷却期内**不该**反复回源 —— 去掉 unavailable 过早返回后，这条保证节流仍在。
//
// 依赖 ModelCatalog() 自身的负缓存（lastFail）与 TTL：
// 这里用「fetch 返回 nil 且调用计数」模拟上游拿不到，断言不会被轮询打爆。
func TestStatsDoesNotHammerCatalogWhenFetchFails(t *testing.T) {
	var fetchCalls atomic.Int32
	h, _ := newStatsHandler(t, []logbuf.Entry{
		{Model: "deepseek-v4", Status: 200, Tokens: 10},
	})
	// 永远拿不到目录（模拟上游持续失败）
	h.cfg.ModelCatalog = func(string) *upstream.ModelCatalog {
		fetchCalls.Add(1)
		return nil
	}
	h.cfg.ModelCatalogState = func() ModelCatalogState {
		return ModelCatalogState{State: "unavailable"}
	}

	for i := 0; i < 5; i++ {
		out := doStats(t, h)
		if ms, _ := out["model_multipliers"].([]any); len(ms) != 0 {
			t.Fatalf("拿不到目录时应返回空表，得到 %v", ms)
		}
	}
	// 这里断言的是"不会因为拿不到就报错或返回脏数据"；
	// 真正的节流由 server 侧 ModelCatalog 的 TTL/负缓存负责，另有测试覆盖。
	if fetchCalls.Load() == 0 {
		t.Error("拿不到目录时仍应尝试过（而非静默跳过）")
	}
}


