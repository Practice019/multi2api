// latency_test.go 账号延迟指标的表驱动测试。
//
// 覆盖验收口径的五件事：
//
//	① P50/P95 的**数值正确性**（nearest-rank 口径，含边界与纯函数表单测）
//	② 样本上限裁剪（环形缓冲：满则丢最旧，且裁剪后 P 值只反映留下的样本）
//	③ 并发安全（-race 下的多 goroutine 读写，状态不自相矛盾）
//	④ **无样本时返回零值而不是 panic**（池里没有的 uid、从没采样过的号、
//	   样本全部过期的号，三条路径都要过一遍）
//	⑤ 按 provider 维度查询与"同档内优先低延迟"的分档/权重口径
//
// 另外钉住几条**容易回归的语义**（都是 latency.go 注释里写明的取舍）：
//   - ms <= 0 被丢弃（0 = 没测到，喂进去会让号看起来"飞快"）
//   - 窗口外样本不参与 P 值，但**不删除**（窗口恢复后数据还在）
//   - 样本不足的号按快档处理（鸡生蛋：否则新号永远拿不到第一次请求）
//   - 慢档不是 0 权重（否则慢号被永久饿死，违反"轮询所有账号"）
//   - 硬分档与权重乘子出自**同一套**计算（不许两套口径打架）
package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newLatencyPool 建一个无落盘的池，并注册包级注册表清理
// （理由见 latency.go 文件头的"架构妥协"）。
func newLatencyPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	t.Cleanup(func() { sweepLatencyBook(p) })
	return p
}

// feedLatency 给某号喂 n 个相同延迟的样本。
func feedLatency(p *Pool, uid string, ms int64, n int) {
	for i := 0; i < n; i++ {
		p.NoteLatency(uid, ms)
	}
}

// ---------------------------------------------------------------------------
// ① P50/P95 正确性
// ---------------------------------------------------------------------------

// TestLatencyPercentileTable 纯函数表驱动：nearest-rank 的每一个边界。
//
// 这是本文件唯一"算错就会静默改变选号偏好"的地方，所以它必须**独立于池**
// 被测一遍 —— 端到端测试只能告诉你"档位不对"，不能告诉你"是 P95 的第几项错了"。
func TestLatencyPercentileTable(t *testing.T) {
	cases := []struct {
		name string
		in   []int64 // 已升序
		pct  float64
		want int64
	}{
		{"空切片返回 0（没有数据不是 0ms）", nil, 50, 0},
		{"单样本 P50 = 它自己", []int64{7}, 50, 7},
		{"单样本 P95 = 它自己（插值法在这里会出错）", []int64{7}, 95, 7},
		{"两个样本 P50 取下秩（rank=1）", []int64{10, 20}, 50, 10},
		{"两个样本 P95 取上秩（rank=2）", []int64{10, 20}, 95, 20},
		{"十样本 P50 rank=5", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 50, 5},
		{"十样本 P95 rank=10", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 95, 10},
		{"二十样本 P95 rank=19", makeRange(20), 95, 19},
		{"三十二样本 P95 rank=31（默认容量）", makeRange(32), 95, 31},
		{"pct<=0 取最小", []int64{5, 6, 7}, 0, 5},
		{"pct>=100 取最大", []int64{5, 6, 7}, 100, 7},
		{"负数 pct 取最小（防御）", []int64{5, 6, 7}, -5, 5},
		{"超大 pct 取最大（防御）", []int64{5, 6, 7}, 1000, 7},
		// 尾部离群值：P50 不动，P95 明确指出"坏日子有多坏"——
		// 这正是均值给不出、而我们必须用 P 值的原因。
		{"离群值不污染 P50", append(makeRange(9), 60000), 50, 5},
		{"离群值被 P95 抓住", append(makeRange(9), 60000), 95, 60000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := latencyPercentileSorted(c.in, c.pct); got != c.want {
				t.Errorf("latencyPercentileSorted(len=%d, pct=%v) = %d, want %d",
					len(c.in), c.pct, got, c.want)
			}
		})
	}
}

// makeRange 返回 1..n 的升序切片（表里省点字）。
func makeRange(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}

// TestLatencyStatP50P95 端到端：喂已知分布，断言 P50/P95/样本数。
func TestLatencyStatP50P95(t *testing.T) {
	p := newLatencyPool(t)
	p.SetDefaultProvider("prov")
	p.Add(&auth.Auth{UID: "u1"})

	// 20 个样本：1..19 与一个 5000ms 的尾部离群值。
	// 升序后 P50 rank=10 ⇒ 10；P95 rank=19 ⇒ 19（不是离群值 —— 这正是
	// "P95 不被单个坏样本完全劫持"的意义）。
	for i := 1; i <= 19; i++ {
		p.NoteLatency("u1", int64(i))
	}
	p.NoteLatency("u1", 5000)

	st, ok := p.LatencyOf("u1")
	if !ok {
		t.Fatal("账号在池里，ok 应为 true")
	}
	if st.UID != "u1" || st.Provider != "prov" {
		t.Errorf("视图字段错：%+v", st)
	}
	if st.Samples != 20 {
		t.Errorf("Samples=%d want 20", st.Samples)
	}
	if st.P50Ms != 10 {
		t.Errorf("P50Ms=%d want 10", st.P50Ms)
	}
	if st.P95Ms != 19 {
		t.Errorf("P95Ms=%d want 19（第 20 个是离群值，P95 不该被它独占）", st.P95Ms)
	}
	if st.LastAt.IsZero() {
		t.Error("有样本时 LastAt 不该是零值")
	}
}

// TestLatencyIgnoresNonPositive ms<=0 一律丢弃。
//
// 与 pool.go 的 TestNoteProviderLatencyIgnoresZero 同一条理由：
// 0 表示"没测到"，喂进来会把中位延迟拉到 0，让一个慢号看起来最快。
func TestLatencyIgnoresNonPositive(t *testing.T) {
	p := newLatencyPool(t)
	p.Add(&auth.Auth{UID: "u1"})

	feedLatency(p, "u1", 500, 5)
	before, _ := p.LatencyOf("u1")

	for i := 0; i < 100; i++ {
		p.NoteLatency("u1", 0)
		p.NoteLatency("u1", -1-int64(i))
	}
	after, _ := p.LatencyOf("u1")

	if before != after {
		t.Errorf("喂入 200 个非正样本后视图变了：before=%+v after=%+v", before, after)
	}
	if after.Samples != 5 || after.P50Ms != 500 {
		t.Errorf("非正样本必须被完全忽略，实得 %+v", after)
	}
}

// ---------------------------------------------------------------------------
// ② 样本上限裁剪
// ---------------------------------------------------------------------------

// TestLatencySampleCapTrimsOldest 环形缓冲满则丢**最旧**的，P 值只反映留下的样本。
//
// ⚠ 这一条同时是"不能钉住最早 N 个"的回归测试：若实现只填满一次就停止写入，
// 下面"喂 1000 后 P50 仍应是 5"的断言会红（它会一直停在 1..4 的中位数上）。
func TestLatencySampleCapTrimsOldest(t *testing.T) {
	cases := []struct {
		name    string
		capN    int
		feed    []int64
		wantN   int
		wantP50 int64
		wantP95 int64
	}{
		{
			name:  "容量 4 未填满",
			capN:  4,
			feed:  []int64{10, 20, 30},
			wantN: 3, wantP50: 20, wantP95: 30,
		},
		{
			name:  "容量 4 刚好填满",
			capN:  4,
			feed:  []int64{10, 20, 30, 40},
			wantN: 4, wantP50: 20, wantP95: 40,
		},
		{
			// 1..8 写进容量 4 的环 ⇒ 只剩 5,6,7,8 ⇒ P50 rank=2 = 6，P95 rank=4 = 8。
			name:  "容量 4 溢出后只留最近 4 个",
			capN:  4,
			feed:  []int64{1, 2, 3, 4, 5, 6, 7, 8},
			wantN: 4, wantP50: 6, wantP95: 8,
		},
		{
			// 慢→快：裁剪后必须看得见"变快了"，否则这个功能只工作一次
			// （正是"钉住最早 N 个"会造成的症状）。
			name:  "先慢后快应能反映新速度",
			capN:  4,
			feed:  []int64{9000, 9000, 9000, 9000, 100, 100, 100, 100},
			wantN: 4, wantP50: 100, wantP95: 100,
		},
		{
			name:  "容量 1 只剩最后一个",
			capN:  1,
			feed:  []int64{5, 6, 7, 8},
			wantN: 1, wantP50: 8, wantP95: 8,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newLatencyPool(t)
			p.Add(&auth.Auth{UID: "u1"})
			p.SetLatency(c.capN, 0) // window=0：本表只测裁剪，不测时间衰减
			if got := p.LatencySamples(); got != c.capN {
				t.Fatalf("LatencySamples()=%d want %d", got, c.capN)
			}
			for _, ms := range c.feed {
				p.NoteLatency("u1", ms)
			}
			st, _ := p.LatencyOf("u1")
			if st.Samples != c.wantN {
				t.Errorf("Samples=%d want %d（上限必须被遵守）", st.Samples, c.wantN)
			}
			if st.P50Ms != c.wantP50 || st.P95Ms != c.wantP95 {
				t.Errorf("P50/P95 = %d/%d want %d/%d", st.P50Ms, st.P95Ms, c.wantP50, c.wantP95)
			}
		})
	}
}

// TestLatencySetSamplesBoundaries 配置边界：非正保留原值、超大钳到上限、
// 调小后**立即**生效（保留最近的样本）。
func TestLatencySetSamplesBoundaries(t *testing.T) {
	p := newLatencyPool(t)
	p.Add(&auth.Auth{UID: "u1"})

	if got := p.LatencySamples(); got != DefaultLatencySamples {
		t.Fatalf("默认样本上限应为 %d，实得 %d", DefaultLatencySamples, got)
	}
	// 非正值不改变配置（与 SetBreaker / SetAutoDisable 同一约定）。
	p.SetLatency(0, DefaultLatencyWindow)
	p.SetLatency(-3, DefaultLatencyWindow)
	if got := p.LatencySamples(); got != DefaultLatencySamples {
		t.Errorf("非正的 samples 应保留默认，实得 %d", got)
	}
	// 超大值钳制：防手滑配置把内存吃满。
	p.SetLatency(MaxLatencySamples*10, DefaultLatencyWindow)
	if got := p.LatencySamples(); got != MaxLatencySamples {
		t.Errorf("超过上限应钳到 %d，实得 %d", MaxLatencySamples, got)
	}
	// window 的判据是 >= 0（0 合法 = 不衰减）；负数与不传等价。
	p.SetLatency(8, 0)
	if got := p.LatencyWindow(); got != 0 {
		t.Errorf("window 0 应被接受（不衰减），实得 %v", got)
	}
	p.SetLatency(8, -time.Minute)
	if got := p.LatencyWindow(); got != 0 {
		t.Errorf("负 window 应保留原值（0），实得 %v", got)
	}

	// 调小容量要**立刻**生效，并且留下的是最近的样本。
	p.SetLatency(8, 0)
	for i := 1; i <= 8; i++ {
		p.NoteLatency("u1", int64(i*100))
	}
	p.SetLatency(3, 0)
	st, _ := p.LatencyOf("u1")
	if st.Samples != 3 {
		t.Fatalf("调小容量后应立即只剩 3 个样本，实得 %d", st.Samples)
	}
	// 留下的必须是最新的三个（600/700/800），不是最旧的三个。
	if st.P50Ms != 700 || st.P95Ms != 800 {
		t.Errorf("应保留最近样本（P50=700 P95=800），实得 P50=%d P95=%d", st.P50Ms, st.P95Ms)
	}
}

// ---------------------------------------------------------------------------
// ③ 并发安全
// ---------------------------------------------------------------------------

// TestLatencyConcurrentSafe 多写多读并发（靠 -race 抓数据竞争）。
//
// 断言"样本数恒为上限、P50 落在喂进去的取值集合里"：
// 一旦出现撕裂读（读到半个环）或竞态写坏 head，这两个断言都会失败。
func TestLatencyConcurrentSafe(t *testing.T) {
	p := newLatencyPool(t)
	const capN = 16
	p.SetLatency(capN, 0)
	uids := []string{"u1", "u2", "u3"}
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid})
	}

	const writers, readers, iters = 8, 4, 200
	allowed := map[int64]bool{100: true, 200: true, 300: true}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				p.NoteLatency(uids[(w+i)%len(uids)], int64(100*(1+(i+w)%3)))
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				for _, uid := range uids {
					st, ok := p.LatencyOf(uid)
					if !ok {
						continue
					}
					if st.Samples > capN {
						t.Errorf("样本数 %d 超过上限 %d", st.Samples, capN)
					}
					if st.Samples > 0 && !allowed[st.P50Ms] {
						t.Errorf("P50=%d 不在喂入的取值集合里（读到撕裂数据？）", st.P50Ms)
					}
				}
				_ = p.LatencyStats()
				_ = p.LatencyStatsFor("")
				_, _, _, _ = p.ProviderLatencyPercentiles("")
				_ = p.LatencyTierMap("", nil)
			}
		}(r)
	}
	wg.Wait()

	// 每个号都被写过很多次 ⇒ 必须填满，且 P50/P95 都来自合法取值。
	for _, uid := range uids {
		st, _ := p.LatencyOf(uid)
		if st.Samples != capN {
			t.Errorf("%s 样本数=%d want %d（写入了 %d 次）", uid, st.Samples, capN, writers*iters/len(uids))
		}
	}
}

// ---------------------------------------------------------------------------
// ④ 无样本 → 零值而不是 panic
// ---------------------------------------------------------------------------

// TestLatencyEmptyCasesAreZeroNotPanic 三条"没有数据"的路径都必须返回零值。
func TestLatencyEmptyCasesAreZeroNotPanic(t *testing.T) {
	p := newLatencyPool(t)

	// ① 池里没有这个 uid ⇒ ok=false、零值（不能靠调用方记得判空）。
	if st, ok := p.LatencyOf("ghost"); ok || st != (LatencyStat{}) {
		t.Errorf("不存在的 uid 应返回 (零值, false)，实得 (%+v, %v)", st, ok)
	}
	// ② 号在池里但从没成功过 ⇒ ok=true、零值（管理台要显示 0 而不是空白）。
	p.Add(&auth.Auth{UID: "fresh"})
	st, ok := p.LatencyOf("fresh")
	if !ok {
		t.Fatal("账号在池里，ok 应为 true")
	}
	if st.Samples != 0 || st.P50Ms != 0 || st.P95Ms != 0 || !st.LastAt.IsZero() {
		t.Errorf("无样本应是零值，实得 %+v", st)
	}
	// ③ 号在池里、样本全过期 ⇒ 同 ② 的零值（不是上一轮的旧值）。
	p.SetLatency(8, time.Nanosecond)
	for i := 0; i < 5; i++ {
		p.NoteLatency("fresh", 1000)
	}
	time.Sleep(2 * time.Millisecond) // 让 1ns 窗口确定过期
	st, _ = p.LatencyOf("fresh")
	if st.Samples != 0 || st.P50Ms != 0 || st.P95Ms != 0 {
		t.Errorf("样本全部过期后应返回零值，实得 %+v", st)
	}

	// ④ 聚合/分档在无数据时同样不 panic、不返回垃圾。
	if stats := p.LatencyStatsFor("nobody"); len(stats) != 0 {
		t.Errorf("没有任何账号的上游应返回空切片，实得 %v", stats)
	}
	if _, _, n, ok := p.ProviderLatencyPercentiles("nobody"); ok || n != 0 {
		t.Errorf("无样本的上游应返回 ok=false n=0，实得 n=%d ok=%v", n, ok)
	}
	if m := p.LatencyTierMap("", nil); m != nil {
		t.Errorf("无数据时不应给出分档（调用方要保持原行为），实得 %v", m)
	}
	if f := p.LatencyWeightFactors("", nil); f != nil {
		t.Errorf("无数据时不应给出权重因子，实得 %v", f)
	}
	// 空池上的全量查询同样安全（用一个真正的空池，避免与上面的 fresh 混淆）。
	emptyPool := newLatencyPool(t)
	if stats := emptyPool.LatencyStats(); stats == nil || len(stats) != 0 {
		t.Errorf("空池应返回空切片（不是 nil），实得 %#v", stats)
	}
}

// TestLatencyWindowRestoresData 窗口外样本不参与统计，但**不删除**：
// 把窗口调回来，数据应当还在（这是"过期即丢"实现不会有的行为，
// 也是本机制能被人工调窗复盘的前提）。
func TestLatencyWindowRestoresData(t *testing.T) {
	p := newLatencyPool(t)
	p.Add(&auth.Auth{UID: "u1"})
	feedLatency(p, "u1", 900, 5)

	if st, _ := p.LatencyOf("u1"); st.Samples != 5 {
		t.Fatalf("默认窗口内应有 5 个样本，实得 %d", st.Samples)
	}
	p.SetLatency(0, time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	if st, _ := p.LatencyOf("u1"); st.Samples != 0 {
		t.Errorf("窗口过期后应视为无样本，实得 %d", st.Samples)
	}
	p.SetLatency(0, DefaultLatencyWindow)
	st, _ := p.LatencyOf("u1")
	if st.Samples != 5 || st.P50Ms != 900 {
		t.Errorf("窗口恢复后样本应仍可用（过期不删），实得 %+v", st)
	}
}

// TestLatencyUnknownUIDCleanup 号被移除后再喂样本，会顺手清掉残留（防串号）。
func TestLatencyUnknownUIDCleanup(t *testing.T) {
	p := newLatencyPool(t)
	p.Add(&auth.Auth{UID: "u1"})
	feedLatency(p, "u1", 500, 5)

	if !p.Remove("u1") {
		t.Fatal("Remove 应返回 true")
	}
	p.NoteLatency("u1", 500) // 号已不在池里：应清掉残留而不是新建
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.LatencyOf("u1")
	if st.Samples != 0 {
		t.Errorf("重新导入的号不应带着旧样本上台，实得 %d 个", st.Samples)
	}
}

// TestLatencyReset 人工清样本。
func TestLatencyReset(t *testing.T) {
	p := newLatencyPool(t)
	p.Add(&auth.Auth{UID: "u1"})
	feedLatency(p, "u1", 500, 5)

	if !p.ResetLatency("u1") {
		t.Fatal("有样本时应返回 true")
	}
	if p.ResetLatency("u1") {
		t.Error("没有样本时应返回 false（调用方据此避免重复打日志）")
	}
	if p.ResetLatency("ghost") {
		t.Error("不在池里的 uid 不该报告清掉了东西")
	}
	if st, _ := p.LatencyOf("u1"); st.Samples != 0 {
		t.Errorf("Reset 后应无样本，实得 %d", st.Samples)
	}
}

// ---------------------------------------------------------------------------
// ⑤ 按 provider 维度 + 同档内优先低延迟
// ---------------------------------------------------------------------------

// TestLatencyProviderDimension 按上游过滤与跨号合并。
func TestLatencyProviderDimension(t *testing.T) {
	p := newLatencyPool(t)
	p.SetDefaultProvider("alpha")
	p.Add(&auth.Auth{UID: "a1"}) // 默认上游 = alpha
	p.AddFor("beta", &auth.Auth{UID: "b1"}, nil)
	p.AddFor("beta", &auth.Auth{UID: "b2"}, nil)
	p.SetLatency(8, 0)

	feedLatency(p, "a1", 100, 4)
	feedLatency(p, "b1", 300, 4)
	feedLatency(p, "b2", 500, 4)

	// ① 按 provider 过滤：各自只看到自己的号，且 Provider 字段是**生效**归属。
	gotAlpha := p.LatencyStatsFor("alpha")
	if len(gotAlpha) != 1 || gotAlpha[0].UID != "a1" || gotAlpha[0].Provider != "alpha" {
		t.Fatalf("alpha 的延迟视图错：%+v", gotAlpha)
	}
	gotBeta := p.LatencyStatsFor("beta")
	if len(gotBeta) != 2 {
		t.Fatalf("beta 应有 2 个号，实得 %+v", gotBeta)
	}
	// 排序：P50 升序 ⇒ b1(300) 在 b2(500) 前。
	if gotBeta[0].UID != "b1" || gotBeta[1].UID != "b2" {
		t.Errorf("同 provider 内应按 P50 升序，实得 %+v", gotBeta)
	}
	// ② 空 provider 归一成默认上游（与 ListFor 同一条约定）。
	if got := p.LatencyStatsFor(""); len(got) != 1 || got[0].UID != "a1" {
		t.Errorf("空 provider 应走默认上游，实得 %+v", got)
	}
	// ③ 全量视图按 provider 再按 P50 排序。
	all := p.LatencyStats()
	if len(all) != 3 || all[0].UID != "a1" || all[1].UID != "b1" || all[2].UID != "b2" {
		t.Errorf("全量视图排序错：%+v", all)
	}

	// ④ 跨号合并的 P 值：beta 的 8 个样本是 300×4 + 500×4。
	p50, p95, n, ok := p.ProviderLatencyPercentiles("beta")
	if !ok || n != 8 {
		t.Fatalf("beta 合并样本应为 8 个，实得 n=%d ok=%v", n, ok)
	}
	if p50 != 300 || p95 != 500 {
		t.Errorf("beta 合并 P50/P95 = %d/%d want 300/500", p50, p95)
	}
	// ⑤ 合并**不含**别的上游的样本：alpha 只有一个 100ms 的号。
	if p50, _, n, ok := p.ProviderLatencyPercentiles("alpha"); !ok || n != 4 || p50 != 100 {
		t.Errorf("alpha 合并口径错：p50=%d n=%d ok=%v", p50, n, ok)
	}
}

// TestLatencyGradingTable 分档判据（相对基线，1.5 倍门槛）的表驱动测试。
//
// 每个用例给 4 个号，基线取它们 P50 的**上中位数**，于是三种档位都能被覆盖。
func TestLatencyGradingTable(t *testing.T) {
	cases := []struct {
		name    string
		p50s    map[string]int64 // uid → 每号喂入的 P50（均喂满 minSamples 次）
		want    map[string]int   // uid → 期望档位（未列出的按"快"断言）
		wantNil bool             // 是否期望"数据不足、保持原行为"
	}{
		{
			name: "典型：快/普通/慢三分",
			// 上中位数 = 300（升序 [100,300,300,3000] 的第 3 项）
			// 快 ≤ 200；慢 ≥ 450；其余普通。
			p50s: map[string]int64{"fast": 100, "n1": 300, "n2": 300, "slow": 3000},
			want: map[string]int{"fast": LatencyTierFast, "n1": LatencyTierNormal, "n2": LatencyTierNormal, "slow": LatencyTierSlow},
		},
		{
			name: "差距不到 1.5 倍都算普通（防抖）",
			// 升序 [300,310,320,330]，上中位数 = 320。300 > 320/1.5≈213 ⇒ 普通。
			p50s: map[string]int64{"a": 300, "b": 310, "c": 320, "d": 330},
			want: map[string]int{"a": LatencyTierNormal, "b": LatencyTierNormal, "c": LatencyTierNormal, "d": LatencyTierNormal},
		},
		{
			// n=2 的固有性质：base 取较快者 ⇒ 较快者只能落到普通档
			// （没有比它更快的参照物），较慢者只要差 1.5 倍就是慢档。
			name: "只有两个号时较快者是普通档、较慢者可判慢",
			p50s: map[string]int64{"a": 200, "b": 800},
			want: map[string]int{"a": LatencyTierNormal, "b": LatencyTierSlow},
		},
		{
			name: "刚过门槛算慢（1.5 倍是闭区间）",
			// 升序 [100,100,100,150] ⇒ 上中位数 = 100；150 ≥ 150 ⇒ 慢。
			p50s: map[string]int64{"a": 100, "b": 100, "c": 100, "d": 150},
			want: map[string]int{"a": LatencyTierNormal, "b": LatencyTierNormal, "c": LatencyTierNormal, "d": LatencyTierSlow},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newLatencyPool(t)
			p.SetLatency(16, 0)
			for uid := range c.p50s {
				p.Add(&auth.Auth{UID: uid})
			}
			for uid, ms := range c.p50s {
				feedLatency(p, uid, ms, DefaultLatencyMinSamples)
			}
			grades := p.LatencyTierMap("", nil)
			if grades == nil {
				t.Fatalf("数据充足时应给出分档，实得 nil")
			}
			for uid := range c.p50s {
				want := LatencyTierFast
				if w, ok := c.want[uid]; ok {
					want = w
				}
				got, ok := grades[uid]
				if !ok {
					t.Errorf("%s 应在分档结果里", uid)
					continue
				}
				if got != want {
					t.Errorf("%s 档位=%d want %d（P50=%d，全部 %+v）", uid, got, want, c.p50s[uid], grades)
				}
			}
			// 分档与权重乘子必须出自同一套口径（不许两套打架）。
			factors := p.LatencyWeightFactors("", nil)
			for uid, g := range grades {
				if got, want := factors[uid], LatencyWeightForGrade(g); got != want {
					t.Errorf("%s 的权重因子 %v 与档位 %d 的换算值 %v 不一致", uid, got, g, want)
				}
			}
		})
	}
}

// TestLatencyGradingNeedsSamples 样本不足 ⇒ 不做判断 / 按快档处理。
//
// 两个方向都要钉住：
//   - 可比对象不足 2 个 ⇒ 返回 nil（调用方保持原行为）
//   - 单个号样本不足 3 个 ⇒ 参与返回值为快档（鸡生蛋：新号必须能被试）
func TestLatencyGradingNeedsSamples(t *testing.T) {
	p := newLatencyPool(t)
	p.SetLatency(16, 0)
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})

	// 两个号都只有 2 个样本（< DefaultLatencyMinSamples）⇒ 无从判档。
	feedLatency(p, "a", 100, DefaultLatencyMinSamples-1)
	feedLatency(p, "b", 9000, DefaultLatencyMinSamples-1)
	if m := p.LatencyTierMap("", nil); m != nil {
		t.Fatalf("样本不足时不应给出分档，实得 %v", m)
	}

	// 只让 a 有足够样本 ⇒ 可比对象只有 1 个 ⇒ 仍然 nil。
	p.NoteLatency("a", 100)
	if m := p.LatencyTierMap("", nil); m != nil {
		t.Fatalf("只有一个号有足够样本时不应分档（没有可比对象），实得 %v", m)
	}

	// b 补齐后可以分档；此时 c 是新号（0 样本）⇒ 必须落在快档而不是慢档。
	p.Add(&auth.Auth{UID: "c"})
	feedLatency(p, "b", 9000, DefaultLatencyMinSamples)
	m := p.LatencyTierMap("", nil)
	if m == nil {
		t.Fatal("两个号都有足够样本后应能分档")
	}
	if m["c"] != LatencyTierFast {
		t.Errorf("无样本的新号必须是快档（否则它永远拿不到第一次请求），实得 %d", m["c"])
	}
	if m["b"] != LatencyTierSlow {
		t.Errorf("9000ms vs 100ms 应判慢档，实得 %d", m["b"])
	}

	// 只把 uids 传一半 ⇒ 只对传进来的那批分档，未传的号不在结果里。
	sub := p.LatencyTierMap("", []string{"a", "b"})
	if _, ok := sub["c"]; ok {
		t.Error("未传入的 uid 不该出现在分档结果里")
	}
	// 传入别的上游的 uid ⇒ 被过滤掉（跨上游不能比快慢）。
	//
	// 注意：这里给 uids 只传 {a, x}。x 属于 other 上游 ⇒ 不参与；剩下 a 一个可比对象
	// ⇒ 分档结果为 nil（"没有可比对象"）。所以断言拆成两件事：
	//   · 传入别的上游的号**不会**让本上游凭空多出分档（不 panic、不返回 x）
	//   · 需要在场至少两个本上游的号才能真的分档
	// other 上游要有**两个**号才可能分档（见 latencyGradesLocked 的 len(known)<2）。
	p.AddFor("other", &auth.Auth{UID: "x"}, nil)
	p.AddFor("other", &auth.Auth{UID: "y"}, nil)
	feedLatency(p, "x", 50, DefaultLatencyMinSamples)
	feedLatency(p, "y", 5000, DefaultLatencyMinSamples)
	if got := p.LatencyTierMap("", []string{"a", "x"}); got != nil {
		t.Errorf("只剩一个本上游可比对象时不该分档，实得 %v", got)
	}
	if got := p.LatencyTierMap("", []string{"a", "b"}); len(got) != 2 {
		t.Errorf("两个本上游的号应各得一档，实得 %v", got)
	}
	// ⚠ 只有 2 个号时**较快者不可能是"快档"**：base 是两者中较快那个，
	// 而快档要求 ≤ base/1.5 —— 没有比它更快的参照，它只能是普通档。
	// 这是"相对分档"的固有性质（不是 bug），单独钉住以免将来被"顺手优化"掉。
	if got := p.LatencyTierMap("other", []string{"a", "b", "x", "y"}); len(got) != 2 {
		t.Errorf("按 other 上游查时只应看到 x/y，实得 %v", got)
	} else if got["y"] != LatencyTierSlow || got["x"] == LatencyTierSlow {
		t.Errorf("other 上游内部应能分出快慢（50 vs 5000），实得 %v", got)
	}
}

// TestLatencyWeightFactorsDoNotStarveSlow 慢号被降权但**不被饿死**。
//
// 这是"轮询所有账号"在延迟维度上的落点：若慢档因子写成 0，
// pickWeighted 会永久跳过它 —— 而这正是用户实测抱怨的偏斜形态
// （见 weightOf 的注释：11 个号 10:1 偏斜、6 并发里 4 个号完全没被用到）。
func TestLatencyWeightFactorsDoNotStarveSlow(t *testing.T) {
	if LatencyWeightSlow <= 0 {
		t.Fatalf("慢档权重必须是正数（0 = 永久排除 = 饿死慢号），实得 %v", LatencyWeightSlow)
	}
	if !(LatencyWeightSlow < LatencyWeightNormal && LatencyWeightNormal < LatencyWeightFast) {
		t.Errorf("权重必须严格递增（慢<普通<快），实得 %v/%v/%v",
			LatencyWeightSlow, LatencyWeightNormal, LatencyWeightFast)
	}

	p := newLatencyPool(t)
	p.SetLatency(16, 0)
	for _, uid := range []string{"fast", "mid", "slow"} {
		p.Add(&auth.Auth{UID: uid})
	}
	feedLatency(p, "fast", 100, 5)
	feedLatency(p, "mid", 130, 5)
	feedLatency(p, "slow", 1000, 5)

	factors := p.LatencyWeightFactors("", nil)
	if factors == nil {
		t.Fatal("数据充足时应给出权重因子")
	}

	// 用一个**确定性**加权抽签统计份额（不用随机源，直接按权重算期望占比），
	// 断言：快号份额明显更高、慢号份额仍 > 10%（没被饿死）。
	var total float64
	for _, uid := range []string{"fast", "mid", "slow"} {
		total += factors[uid]
	}
	fastShare := factors["fast"] / total
	slowShare := factors["slow"] / total
	if fastShare <= slowShare {
		t.Errorf("快号份额 %.2f 应高于慢号 %.2f", fastShare, slowShare)
	}
	if slowShare < 0.10 {
		t.Errorf("慢号份额 %.2f < 10%% —— 会被饿死（违反「轮询所有账号」）", slowShare)
	}

	// 未知分档按"不降权"处理（与"无数据不惩罚"一致）。
	if got := LatencyWeightForGrade(LatencyTierFast); got != LatencyWeightFast {
		t.Errorf("快档权重=%v want %v", got, LatencyWeightFast)
	}
	if got := LatencyWeightForGrade(99); got != LatencyWeightFast {
		t.Errorf("未知分档应不降权，实得 %v", got)
	}
}

// TestLatencyGradingIgnoresStaleSamples 窗口外的样本不得影响分档。
//
// 这条守的是"自愈"：一个号变慢后被降档，若它的旧样本永远有效，
// 它就永远拿不到流量（自锁）。过期即"无数据" ⇒ 回到快档 ⇒ 重新被试。
func TestLatencyGradingIgnoresStaleSamples(t *testing.T) {
	p := newLatencyPool(t)
	p.SetLatency(16, 0)
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})

	feedLatency(p, "a", 100, 5)
	feedLatency(p, "b", 9000, 5)
	if m := p.LatencyTierMap("", nil); m == nil || m["b"] != LatencyTierSlow {
		t.Fatalf("前置条件：b 应被判为慢档，实得 %v", m)
	}

	// 全部样本过期 ⇒ 没有可比数据 ⇒ 不给出分档（而不是继续按旧数据降档）。
	p.SetLatency(16, time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	if m := p.LatencyTierMap("", nil); m != nil {
		t.Errorf("样本过期后不应再按旧数据分档，实得 %v", m)
	}
}

// TestLatencyPruneWaterBound 条目数超过水位时清扫过期条目（内存不无限增长）。
//
// 直接构造"大量已过期条目"不现实（要 sleep 很久），所以这里只钉住
// **可见的契约**：写入路径不会因为注册表变大而 panic，且过期条目在
// 水位触发后被清掉。用一个很短的窗口 + 水位以下的账号数来验证清理逻辑本身。
func TestLatencyPruneWaterBound(t *testing.T) {
	p := newLatencyPool(t)
	for i := 0; i < 20; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%02d", i)})
	}
	p.SetLatency(4, time.Nanosecond)
	for i := 0; i < 20; i++ {
		p.NoteLatency(fmt.Sprintf("u%02d", i), 100)
	}
	time.Sleep(2 * time.Millisecond)

	// 直接调用清理（与 NoteLatency 超过水位时走的是同一个函数）。
	p.mu.Lock()
	p.latencyBookLocked().pruneLocked(time.Now())
	n := len(p.latencyBookLocked().rings)
	p.mu.Unlock()
	if n != 0 {
		t.Errorf("过期条目应被清扫，剩余 %d 条", n)
	}

	// 账号数仍在池里的号，清的是样本不是账号：视图照常返回零值。
	st, ok := p.LatencyOf("u01")
	if !ok || st.Samples != 0 {
		t.Errorf("清扫样本后账号仍应在池里且样本为 0，实得 (%+v, %v)", st, ok)
	}
}
