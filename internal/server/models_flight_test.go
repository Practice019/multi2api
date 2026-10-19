// models_flight_test.go —— modelsForProviderCached 的守卫测试。
//
// 守的是三件被实测暴露的事（2026-10-20，用户报「可用模型加载很慢」）：
//
//	① 连打两次**只打一次上游**（正缓存 1h 命中）
//	② 并发 8 个冷调用**只打一次上游**（单飞；没有它时并发越多越慢）
//	③ 有过期目录时**立刻回旧目录**（不阻塞），后台把新目录刷进来（stale-while-revalidate）
//
// 三条都是"没有它就会退化成原来那样"的判据：删掉缓存 / 删掉锁 / 改成同步刷新，
// 对应条目会立刻变红。
package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// countingFetch 造一个"数调用次数"的取数桩。
func countingFetch(calls *int32, delay time.Duration, info gateway.ModelInfo) func() ([]gateway.ModelInfo, bool) {
	return func() ([]gateway.ModelInfo, bool) {
		atomic.AddInt32(calls, 1)
		if delay > 0 {
			time.Sleep(delay)
		}
		return []gateway.ModelInfo{info}, true
	}
}

// ① 正缓存命中：第二次不该再打上游。
func TestModelsCacheHitsWithinTTL(t *testing.T) {
	t.Cleanup(resetProviderModelsCache)
	const id = "prov-ttl"
	var calls int32
	fetch := countingFetch(&calls, 0, gateway.ModelInfo{ID: id + "/m1"})

	for i := 0; i < 3; i++ {
		infos, ok := modelsForProviderCached(id, fetch)
		if !ok || len(infos) != 1 {
			t.Fatalf("第 %d 次取目录失败: ok=%v len=%d", i+1, ok, len(infos))
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("三次调用应只打一次上游，实际打了 %d 次（正缓存没生效）", n)
	}
}

// ② 单飞：并发冷调用只允许一个去取，其余等在锁上，取完各自命中缓存。
func TestModelsColdConcurrentSingleFlight(t *testing.T) {
	t.Cleanup(resetProviderModelsCache)
	const id = "prov-flight"
	var calls int32
	// 故意给 200ms 延迟：让后来者**一定**落在锁上等，而不是抢在第一次写完缓存之前跑完。
	fetch := countingFetch(&calls, 200*time.Millisecond, gateway.ModelInfo{ID: id + "/m1"})

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			infos, ok := modelsForProviderCached(id, fetch)
			if !ok || len(infos) != 1 {
				errs <- "并发取目录返回不完整"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("8 个并发冷调用应只打一次上游，实际打了 %d 次（单飞没生效）", n)
	}
}

// ③ stale-while-revalidate：有过期目录时立刻回旧的，刷新在后台完成。
func TestModelsStaleServedImmediatelyThenRefreshed(t *testing.T) {
	t.Cleanup(resetProviderModelsCache)
	const id = "prov-stale"

	// 种一份两小时前的旧目录（TTL 1h ⇒ 已过期）。
	storeProviderModels(id, []gateway.ModelInfo{{ID: id + "/old"}})
	providerModelsCache.Lock()
	providerModelsCache.entries[id].fetched = time.Now().Add(-2 * time.Hour)
	providerModelsCache.Unlock()

	var calls int32
	fetch := countingFetch(&calls, 300*time.Millisecond, gateway.ModelInfo{ID: id + "/new"})

	start := time.Now()
	infos, ok := modelsForProviderCached(id, fetch)
	elapsed := time.Since(start)
	if !ok || len(infos) != 1 {
		t.Fatalf("过期时也应立即回旧目录，实际 ok=%v len=%d", ok, len(infos))
	}
	if infos[0].ID != id+"/old" {
		t.Fatalf("首次应回旧目录，实际回了 %q", infos[0].ID)
	}
	// 上游桩要 300ms；同步等它就说明"没有先给旧的"。
	if elapsed > 150*time.Millisecond {
		t.Fatalf("应立刻返回旧目录（不等上游），实际等了 %v", elapsed)
	}

	// 后台刷新应把新目录写进缓存（最多等 2s）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		providerModelsCache.RLock()
		e := providerModelsCache.entries[id]
		providerModelsCache.RUnlock()
		if e != nil && len(e.infos) == 1 && e.infos[0].ID == id+"/new" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("后台刷新未把新目录写进缓存（上游被调用 %d 次）", atomic.LoadInt32(&calls))
}

// 取不到上游时语义不变：返回 (nil,false)，且**不写缓存**（下次还会再试）。
func TestModelsFailureNotCached(t *testing.T) {
	t.Cleanup(resetProviderModelsCache)
	const id = "prov-fail"
	var calls int32
	fail := func() ([]gateway.ModelInfo, bool) {
		atomic.AddInt32(&calls, 1)
		return nil, false
	}
	for i := 0; i < 2; i++ {
		if infos, ok := modelsForProviderCached(id, fail); ok || infos != nil {
			t.Fatalf("失败应返回 (nil,false)，实际 ok=%v infos=%v", ok, infos)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("失败不该被负缓存，两次调用都该打上游，实际 %d 次", n)
	}
}

var _ = context.Background // 保持与产品同名的 import 习惯，避免将来加超时参数时漏改
