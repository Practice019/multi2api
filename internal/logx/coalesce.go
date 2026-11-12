// coalesce.go 把"同一件事的重复"折成**一条带计数的行**（用户 2026-11-12 的明确要求：
// "重复的能不能用一条日志输出？然后统计数量什么的，这样会不会更简洁一点？"）。
//
// # 与 Sample/Oncef 的区别（这是两件事，别混）
//
//	· Sample/Oncef：**减少**重复 —— 同一条只放行"首次 + 每 N 次 + 心跳"，其余静默；
//	  读者仍会看到多条（只是少了）。
//	· Coalesce：**合并**重复 —— 窗口内同一个 key 的事件**一条都不打**，
//	  窗口结束时打成**一行**：`×N · 细分统计 · 时间范围`。
//
// 用户看到的问题是"一屏 20 条同义行"，那是**合并**要解决的，不是采样。
//
// # 行为细节（每条都有理由）
//
//  1. **级别取窗口内的最高级**：把 WARN 折进 INFO 行等于把警告降级（那是最坏的降噪）。
//  2. **细分**（sub）：调用方给一个"子维度"标签（业务码 / 上游 / 账号），
//     输出 `100002×5 · 100003×1` —— 这才是"统计数量"的落点。
//  3. **首次/最近时间**：折叠会丢掉"什么时候开始的"，所以要显式补回来。
//  4. 窗口内**没有事件就不打**（静默期不打空行）。
package logx

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type coalesceBucket struct {
	lvl     Level
	scope   string
	n       int
	first   time.Time
	last    time.Time
	lastMsg string
	subs    map[string]int
}

var (
	coMu      sync.Mutex
	coBuckets = map[string]*coalesceBucket{}
)

// Coalesce 记一次"同一件事"的事件。窗口内**不打**，由 FlushCoalesced 折成一行输出。
//
//	key  同一件事的标识（上游 + 账号 + 原因类别；漏因素会让不同故障互相吃掉计数）
//	sub  子维度标签（业务码 / 上游 / 账号），空串表示不细分
//	msg  给读者看的最新一条正文（不含计数，计数由本函数加）
func Coalesce(l Level, scope, key, sub, format string, args ...any) {
	if !enabledAny() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	coMu.Lock()
	defer coMu.Unlock()
	b := coBuckets[key]
	t := now()
	if b == nil {
		b = &coalesceBucket{lvl: l, scope: scope, first: t, subs: map[string]int{}}
		coBuckets[key] = b
	}
	if l > b.lvl {
		b.lvl = l // 见文件头第 1 条：级别取最高
	}
	b.n++
	b.last = t
	b.lastMsg = msg
	if sub != "" {
		b.subs[sub]++
	}
}

// enabledAny 任一可见级别开着就继续收集（收集本身很便宜，真正的过滤在输出时）。
func enabledAny() bool {
	return Enabled(LevelInfo) || Enabled(LevelWarn) || Enabled(LevelError) || Enabled(LevelDebug)
}

// FlushCoalesced 把当前窗口的所有折叠桶折成行并清空（按"第一次出现"排序，便于对时间轴）。
func FlushCoalesced() []string {
	coMu.Lock()
	if len(coBuckets) == 0 {
		coMu.Unlock()
		return nil
	}
	keys := make([]string, 0, len(coBuckets))
	for k := range coBuckets {
		keys = append(keys, k)
	}
	buf := coBuckets
	coBuckets = map[string]*coalesceBucket{}
	coMu.Unlock()

	sort.Slice(keys, func(i, j int) bool { return buf[keys[i]].first.Before(buf[keys[j]].first) })
	var out []string
	for _, k := range keys {
		b := buf[k]
		if !Enabled(b.lvl) {
			continue
		}
		var parts []string
		parts = append(parts, b.lastMsg)
		if b.n > 1 {
			parts = append(parts, fmt.Sprintf("×%d", b.n))
		}
		if span := b.last.Sub(b.first); span >= time.Second {
			parts = append(parts, fmt.Sprintf("%s→%s", b.first.Format("15:04:05"), b.last.Format("15:04:05")))
		}
		if len(b.subs) > 0 {
			parts = append(parts, subLine(b.subs))
		}
		out = append(out, Line(b.lvl, b.scope, strings.Join(parts, " · ")))
	}
	return out
}

// subLine 把细分统计写成 `100002×5 · 100003×1`（按次数降序，最多 4 个）。
func subLine(subs map[string]int) string {
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(subs))
	for k, v := range subs {
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
		if i >= 4 {
			parts = append(parts, fmt.Sprintf("…共 %d 项", len(all)))
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", e.k, e.v))
	}
	return strings.Join(parts, " · ")
}

// CoalesceLoop 每 d 把折叠桶输出一次（d <= 0 = 不启动）。
// 与 StatsLoop 并行：StatsLoop 管"总体怎么样"，本函数管"刚才重复了什么"。
func CoalesceLoop(d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for range t.C {
		for _, line := range FlushCoalesced() {
			write(line)
		}
	}
}
