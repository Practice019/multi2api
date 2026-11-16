// batch.go 批量任务的**结果聚合器**：逐账号只计数，结束时打**一行**汇总
// （用户 2026-11-16 的明确要求：「这件事情结束之后，直接统计成功的账号和
// 不成功的账号，直接统计数字一行就可以了」）。
//
// # 它解决什么
//
// 账号池有几十上百个账号。改造前每条路径各打各的：
//
//	checkin  user_123: ok
//	travel   user_123: depart ok location=3
//	travel   user_123: claim ok record=5 reward=300
//	checkin  user_456: ok
//	...（一个账号一趟 3~11 行）
//
// 一趟总结下来就是几百行，真正的失败被淹在里面。
// 而且这些行的**单条信息量极低** —— "某个号今天签到了"不需要单独一行，
// 需要的是"今天 42 个成功、3 个失败"。
//
// # 与 Coalesce 的分工（两件事，别混）
//
//	Coalesce   把**同一件事的重复**折成一条带计数的行（时间窗内滚动输出）
//	Batch      **一次批量任务**的全部结果，在任务**结束时**打一行
//
// Coalesce 的窗口是时间（每 N 秒 flush），Batch 的边界是任务本身。
// 一个任务里既有大量成功也有少量失败时：成功只该进 Batch 的计数，
// 失败除了进计数，还应继续走 Coalesce（运维要看得见重复失败的模式）。
//
// # 为什么失败样本要限条数
//
// 「失败 3 个」之后如果附 3 条样本，那是有用的；如果附 300 条，就回到了刷屏。
// 所以固定上限（3 条），并在超出时明确写「还有 N 条」——
// 让"我没有全部列出来"这件事本身是**可见**的，而不是悄悄截断。
package logx

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// batchSampleMax 汇总行里最多附几条失败样本。
//
// 3 条足以让人判断"是不是同一个原因"，又不会把一行撑成一段。
const batchSampleMax = 3

// Batch 一次批量任务的结果聚合器。
//
// ⚠ 不是并发安全的：它对应"一个任务的一趟执行"，天然单线程。
// 若将来要在 goroutine 里用，调用方自己加锁 —— 这里不预先付那个代价。
type Batch struct {
	scope string // logx 作用域（上游 / 子系统 id），如 "codearts"
	event string // 事件名（点分小写），如 "checkin.summary"
	start time.Time

	ok   int
	fail int
	skip int

	// skipReasons 跳过原因 → 次数（"跳过 5" 里那 5 个分别为什么）。
	skipReasons map[string]int

	// failSamples 失败样本（最多 batchSampleMax 条）。
	failSamples []string
	// failExtra 没进样本的失败条数（用来写"还有 N 条"）。
	failExtra int
}

// NewBatch 开一趟批量任务。
//
//	scope 日志作用域（与 Infof 的 scope 同一套：上游 / 子系统 id，**ASCII**）
//	event 事件名（点分小写），如 "checkin.summary" / "travel.summary"
//
// ⚠ scope 与 event 都必须是 ASCII 标识符 —— 它们要能被 grep 与 jq 聚合。
// 中文只允许出现在 Eventf 的 msg= 字段里（见 event.go）。
func NewBatch(scope, event string) *Batch {
	return &Batch{
		scope:       scope,
		event:       event,
		start:       time.Now(),
		skipReasons: map[string]int{},
	}
}

// OK 记一次成功（**不打日志** —— 成功只需要进计数）。
//
// detail 目前只用于将来可能的调试输出，不进汇总行（成功不需要样本）。
func (b *Batch) OK() {
	if b == nil {
		return
	}
	b.ok++
}

// Fail 记一次失败。
//
// `sample` 是**给人看的一小段**（账号短标识 + 原因），会进汇总行的样本区。
// ⚠ 调用方负责把它压短（见 shortUID 的做法）—— 长错误在这里不做截断，
// 因为"截断到多少"取决于上下文，本包不该猜。
func (b *Batch) Fail(sample string) {
	if b == nil {
		return
	}
	b.fail++
	if len(b.failSamples) < batchSampleMax {
		s := strings.TrimSpace(sample)
		if s != "" {
			b.failSamples = append(b.failSamples, s)
		}
		return
	}
	b.failExtra++
}

// Skip 记一次跳过，并记原因。
//
// ⚠ 跳过与失败**必须分开**：跳过是"这件事对它不适用/不需要做"
// （已禁用账号 / 上游没开这个活动 / 今天已签到），
// 失败是"做了但没成"。混成一类会让用户去查一个不存在的故障。
func (b *Batch) Skip(reason string) {
	if b == nil {
		return
	}
	b.skip++
	r := strings.TrimSpace(reason)
	if r == "" {
		r = "未说明"
	}
	b.skipReasons[r]++
}

// Total 本趟处理过的账号总数（已完成 + 失败 + 跳过）。
func (b *Batch) Total() int {
	if b == nil {
		return 0
	}
	return b.ok + b.fail + b.skip
}

// Summary 汇总行文本（**一行**）。不写日志，便于测试与复用。
//
// 形态：
//
//	统一签到: 成功 42 · 失败 3 · 跳过 5（已禁用×5）· 38s — 失败样本: abc…401 / def…超时
//
// # 几条刻意的选择
//
//   - **零结果的项不出现**：全成功时不写"失败 0"，把行留短。
//     但三者全零时写"没有可处理的账号"—— 否则一行空白会被读成"跑挂了"。
//   - **跳过原因附带**：只说"跳过 5"不解释，用户还得自己猜。
//   - **耗时总是有**：它是最便宜的健康信号（突然从 3s 变 90s 值得看）。
//
// Summary 汇总行的**正文**（不含时间/级别/作用域三段）。
//
// 形态（工程化：事件名 + 蛇形 k=v，可被 grep / jq 聚合）：
//
//	checkin.summary ok=42 fail=3 skip=5 skip_reasons="disabled:5" elapsed_ms=38200
//	checkin.summary fail=10 fail_samples="u_1…timeout; u_2…401" fail_more=7 elapsed_ms=900
//
// # 几条刻意的选择
//
//   - **零结果的项不出现**：全成功时不写 fail=0，把行留短。
//     但三者全零时写 empty=true —— 否则一行空白会被读成"跑挂了"。
//   - **skip_reasons 附带**：只说 skip=5 不解释，用户还得自己猜。
//   - **elapsed_ms 总是有**：它是最便宜的健康信号（突然从 3s 变 90s 值得看）。
//   - **fail_samples 有上限且 fail_more 可见**：截断了就必须说"还有几条"。
func (b *Batch) Summary() string {
	if b == nil {
		return ""
	}
	kv := make([]any, 0, 12)
	if b.ok > 0 {
		kv = append(kv, "ok", b.ok)
	}
	if b.fail > 0 {
		kv = append(kv, "fail", b.fail)
	}
	if b.skip > 0 {
		kv = append(kv, "skip", b.skip)
		if detail := b.skipReasonLine(); detail != "" {
			kv = append(kv, "skip_reasons", detail)
		}
	}
	if b.ok == 0 && b.fail == 0 && b.skip == 0 {
		kv = append(kv, "empty", true)
	}
	if len(b.failSamples) > 0 {
		kv = append(kv, "fail_samples", strings.Join(b.failSamples, "; "))
		if b.failExtra > 0 {
			kv = append(kv, "fail_more", b.failExtra)
		}
	}
	kv = append(kv, "elapsed_ms", time.Since(b.start))

	ev := b.event
	if strings.TrimSpace(ev) == "" {
		ev = "batch.summary"
	}
	return eventLine(ev, kv)
}

// skipReasonLine 把跳过原因写成 `disabled:5,not_open:2`（按次数降序，ASCII）。
func (b *Batch) skipReasonLine() string {
	if len(b.skipReasons) == 0 {
		return ""
	}
	type kv struct {
		k string
		n int
	}
	list := make([]kv, 0, len(b.skipReasons))
	for k, n := range b.skipReasons {
		list = append(list, kv{k, n})
	}
	// 次数降序；同次数按名字升序（保证输出稳定，便于比对两次运行）。
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].k < list[j].k
	})
	// 最多 3 种原因，其余折成 `+N`（让"没列全"可见）。
	const maxReasons = 3
	out := make([]string, 0, maxReasons)
	for i, x := range list {
		if i >= maxReasons {
			return strings.Join(out, ",") + fmt.Sprintf(",+%d", len(list)-maxReasons)
		}
		out = append(out, fmt.Sprintf("%s:%d", x.k, x.n))
	}
	return strings.Join(out, ",")
}

// Done 打汇总行并返回它（便于调用方在测试里断言）。
//
// 级别选择：**有失败用 WARN，否则 INFO**。
// 一个"全成功"的批量任务不该占 WARN；而"失败 3"必须是 WARN ——
// 否则它和正常流水混在一起，扫日志时看不见。
func (b *Batch) Done() string {
	if b == nil {
		return ""
	}
	line := b.Summary()
	if line == "" {
		return ""
	}
	lvl := LevelInfo
	if b.fail > 0 {
		lvl = LevelWarn
	}
	Logf(lvl, b.scope, "%s", line)
	return line
}
