// stats.go 把"人看的控制台"从"一行一请求"改成**周期性汇总**。
//
// # 为什么（2026-11-12，用户："人看的话只要看一些总结的信息，关键数字什么的"）
//
// 控制台面向的是**人**：他要的是节拍与关键数字（多少请求、成功失败几比几、吐了多少 token、
// 快不快、号健康不健康），不是每条请求的流水。逐条流水属于排障，应该按需开（-log-level debug）。
//
// 成熟项目的控制台也是这个形态：只留 status/stats 行与异常，原始流水进文件或采集器。
//
// ⚠ 两条不能省（否则"简洁"会变成"看不见"）：
//
//	· 有失败时必须把**失败数 + 是哪个上游**打出来（只报"1 失败"等于没说）；
//	· 长时间一条请求都没有时，也要留一条心跳（否则分不清"安静"与"死了"）。
package logx

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Outcome 一条请求的结果（字段都是"汇总要用到的关键数字"）。
type Outcome struct {
	Provider string
	Status   int
	OutToks  int // 纯输出 token
	TTFB     time.Duration
	Total    time.Duration
}

// PoolCounts 账号池的规模（由装配层注入，本包不依赖 pool）。
type PoolCounts struct {
	Total, Healthy, Cooling, Disabled int
}

type window struct {
	n, ok, fail int
	outToks     int
	ttfb, total []time.Duration // 留样本算 p50（窗口有限，样本量可控）
	byProvider  map[string]int  // 失败数（按上游）
	first       time.Time
	last        time.Time
}

var (
	statsMu sync.Mutex
	w       = &window{}
	// maxSamples 每个窗口最多留多少条耗时样本（p50 用；防止静默期无限增长）
	maxSamples = 4096
)

// Observe 记一条请求结果进当前窗口。
func Observe(o Outcome) {
	statsMu.Lock()
	defer statsMu.Unlock()
	nowT := now()
	if w.n == 0 {
		w.first = nowT
	}
	w.last = nowT
	w.n++
	if o.Status >= 200 && o.Status < 400 {
		w.ok++
	} else {
		w.fail++
		if o.Provider != "" {
			if w.byProvider == nil {
				w.byProvider = map[string]int{}
			}
			w.byProvider[o.Provider]++
		}
	}
	if o.OutToks > 0 {
		w.outToks += o.OutToks
	}
	if len(w.ttfb) < maxSamples && o.TTFB > 0 {
		w.ttfb = append(w.ttfb, o.TTFB)
	}
	if len(w.total) < maxSamples && o.Total > 0 {
		w.total = append(w.total, o.Total)
	}
}

// Flush 把当前窗口收成**人看的汇总行**并清零。
//
// pool 为 nil 表示不报账号池规模（测试与不持有 pool 的调用方）。
// 返回空串 = 这一窗没什么可说的（既没请求、也不是心跳时刻）——
// 调用方据此决定"静默"，而不是打一条空的噪声。
func Flush(pool func() PoolCounts) string {
	statsMu.Lock()
	cur := w
	w = &window{}
	statsMu.Unlock()

	if cur.n == 0 {
		// 静默期：不打"0 请求"，但也不能永远不响 —— 由调用方按心跳间隔决定是否问。
		return ""
	}
	span := cur.last.Sub(cur.first).Round(time.Second)
	if span < time.Second {
		span = time.Second
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s · 请求 %d（成功 %d / 失败 %d）", humanSpan(span), cur.n, cur.ok, cur.fail)
	if cur.outToks > 0 {
		gen := cur.totalGen()
		rate := 0.0
		if gen > 0 {
			rate = float64(cur.outToks) / gen.Seconds()
		}
		fmt.Fprintf(&b, " · 输出 %s tok", humanTokens(cur.outToks))
		if rate > 0 {
			fmt.Fprintf(&b, " · %s tok/s", FmtTps(rate))
		}
	}
	if ms := p50(cur.ttfb); ms > 0 {
		fmt.Fprintf(&b, " · TTFB p50 %s", ms.Round(100*time.Millisecond))
	}
	if ms := p50(cur.total); ms > 0 {
		fmt.Fprintf(&b, " · 时长 p50 %s", ms.Round(100*time.Millisecond))
	}
	if pool != nil {
		c := pool()
		fmt.Fprintf(&b, " · 号 %d（可用 %d / 冷却 %d / 禁用 %d）", c.Total, c.Healthy, c.Cooling, c.Disabled)
	}
	if cur.fail > 0 {
		fmt.Fprintf(&b, " · ⚠ 失败来自 %s", topProviders(cur.byProvider))
	}
	return b.String()
}

// totalGen 生成阶段总时长（扣 TTFB）——与终端 tok/s 同一口径（见 tokPerSecField）。
func (x *window) totalGen() time.Duration {
	var gen time.Duration
	for i, d := range x.total {
		g := d
		if i < len(x.ttfb) {
			g = d - x.ttfb[i]
		}
		if g > 0 {
			gen += g
		}
	}
	return gen
}

func p50(xs []time.Duration) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), xs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}

func topProviders(m map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	var parts []string
	for i, e := range all {
		if i >= 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", e.k, e.v))
	}
	return strings.Join(parts, "、")
}

func humanSpan(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("近 %d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("近 %d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("近 %d 小时", int(d.Hours()))
}

// humanTokens 把 token 数写成 8.2k / 1.3M 这种"一眼能读"的形态。
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// StatsLoop 每 d 打一条汇总行（pool 为 nil = 不报账号规模；d <= 0 = 不启动）。
//
// ⚠ 静默期：连续空窗到达 heartbeatWindows 时打一条**极短**的心跳
//
//	（否则"安静"与"进程死了"在控制台上长得一模一样）。
func StatsLoop(d time.Duration, pool func() PoolCounts) {
	if d <= 0 {
		return
	}
	t := time.NewTicker(d)
	defer t.Stop()
	empty := 0
	for range t.C {
		line := Flush(pool)
		if line != "" {
			empty = 0
			Infof("stats", "%s", line)
			continue
		}
		empty++
		if empty >= heartbeatWindows {
			empty = 0
			silent := time.Duration(heartbeatWindows) * d
			if pool != nil {
				c := pool()
				Infof("stats", "近 %s 无请求 · 号 %d（可用 %d / 冷却 %d / 禁用 %d）",
					humanSpan(silent), c.Total, c.Healthy, c.Cooling, c.Disabled)
			} else {
				Infof("stats", "近 %s 无请求", humanSpan(silent))
			}
		}
	}
}

// heartbeatWindows 连续多少个空窗后打一条心跳（默认 3 个 ⇒ 3 倍汇总间隔）。
const heartbeatWindows = 3
