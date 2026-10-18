package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// ⚠ 钉住「哪家上游快」这件事（2026-10-18 加，用户第三条要求"保证请求速度"）。
//
// # 实测背景（为什么需要它）
//
// 同一个模型 `deepseekv41flash` 在 loomy 与 workbuddy 都有：
//
//	loomy      TTFB 中位 6093ms · p90 34232ms · 失败率 15.22%
//	workbuddy  TTFB 中位 3374ms · p90  4800ms · 失败率  0.13%
//
// ⚠ 而请求被模型前缀 `loomy/` 钉死在慢的那家上 ⇒ 没有这个数，出口层就没有依据换。
//
// ⚠ 这条测试同时钉住两个"宁可不动、也不乱切"的约定：
//   · 样本不足时不许拿来比较（否则一次冷启动 30 秒就会把流量全切走）
//   · 倍数不到阈值（2 倍）不算"明显更慢"（否则正常抖动会把流量来回甩）

func TestProviderLatencyNeedsEnoughSamples(t *testing.T) {
	p := New("")
	p.upsertLockedFor("loomy", &auth.Auth{UID: "u1"})

	p.NoteProviderLatency("loomy", 30000)
	if _, ok := p.ProviderLatency("loomy"); ok {
		t.Fatal("只有 1 个样本时不该说「我了解这家有多快」—— 一次冷启动就足以误导切换")
	}

	for i := 0; i < provTTFBMinN; i++ {
		p.NoteProviderLatency("loomy", 30000)
	}
	ms, ok := p.ProviderLatency("loomy")
	if !ok {
		t.Fatalf("喂满 %d 个样本后应该可用", provTTFBMinN)
	}
	if ms < 20000 {
		t.Errorf("EMA = %.0f，明显偏离喂进去的 30000 —— 初值处理有问题", ms)
	}
}

func TestSlowerThanOnlyWhenDramaticallySlower(t *testing.T) {
	p := New("")
	p.upsertLockedFor("loomy", &auth.Auth{UID: "slow"})
	p.upsertLockedFor("workbuddy", &auth.Auth{UID: "fast"})
	fill := func(prov, uid string, ms int64) {
		for i := 0; i < provTTFBMinN; i++ {
			p.NoteProviderLatency(prov, ms)
		}
		_ = uid
	}
	fill("loomy", "slow", 6000)
	fill("workbuddy", "fast", 3400)

	if !p.SlowerThan("loomy", "workbuddy") {
		t.Errorf("6000ms vs 3400ms（近 2 倍）应判为明显更慢（阈值 %.1f 倍）", provTTFBSlowRas)
	}
	if p.SlowerThan("workbuddy", "loomy") {
		t.Error("快的不该被判为慢的那一方")
	}

	// ⚠ 接近的两家不许判成"更慢" —— 否则正常抖动会把流量在两家之间来回甩
	p2 := New("")
	p2.upsertLockedFor("a", &auth.Auth{UID: "a1"})
	p2.upsertLockedFor("b", &auth.Auth{UID: "b1"})
	for i := 0; i < provTTFBMinN; i++ {
		p2.NoteProviderLatency("a", 3300)
		p2.NoteProviderLatency("b", 3400)
	}
	if p2.SlowerThan("a", "b") {
		t.Error("3300 vs 3400 是抖动范围，不该判为明显更慢")
	}

	// ⚠ 样本不足的一侧 ⇒ 一律 false（宁可不动）
	p3 := New("")
	p3.upsertLockedFor("x", &auth.Auth{UID: "x1"})
	p3.upsertLockedFor("y", &auth.Auth{UID: "y1"})
	for i := 0; i < provTTFBMinN; i++ {
		p3.NoteProviderLatency("x", 30000)
	}
	p3.NoteProviderLatency("y", 100) // 只有 1 个样本
	if p3.SlowerThan("x", "y") {
		t.Error("另一侧样本不足时不许判「明显更慢」—— 那是拿噪声当信号")
	}
}

// TestNoteProviderLatencyIgnoresZero ⚠ 0 表示"没测到首字节"，不该污染 EMA。
//
// 来源：非流式响应、或上游没给首字节时刻时，出口层手里就是 0。
// 若把它喂进去，一个健康上游的 EMA 会被一路拉到 0，看起来"飞快" —— 比没有数据更坏。
func TestNoteProviderLatencyIgnoresZero(t *testing.T) {
	p := New("")
	p.upsertLockedFor("loomy", &auth.Auth{UID: "u1"})
	for i := 0; i < provTTFBMinN; i++ {
		p.NoteProviderLatency("loomy", 5000)
	}
	before, _ := p.ProviderLatency("loomy")
	for i := 0; i < 100; i++ {
		p.NoteProviderLatency("loomy", 0)
	}
	after, _ := p.ProviderLatency("loomy")
	if after < before*0.9 {
		t.Errorf("喂了 100 个 0 之后 EMA 从 %.0f 掉到 %.0f —— 0 该被忽略", before, after)
	}
}
