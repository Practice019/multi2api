// auditreport 拒绝重试的审计报告 —— 移植自 dsh-purge 的 scripts/refusal-report.mjs。
//
// # 它解决什么
//
// 「审计落了没人看等于没落」：破解层每次命中拒绝都会往 data/audit.jsonl 写一行，
// 但那是个 JSONL，人不会去 tail 它。这个命令把审计 + 请求日志合成一份**一眼能读**的报告。
//
// # 口径照原件，但只报我们真有的东西（这一点是硬纪律）
//
// 原件的输出里有「助手回答数 / 拒绝式数 / 拒绝率」——它靠**会话文件里的回答正文**统计。
// 本仓的请求日志（internal/logbuf）**不落回答正文**（只落 model/provider/status/tokens 这类元数据）
// ⇒ 那几个数**算不出来**。所以这里显式打印一行"无法计算 + 原因"，而**不是**用近似值冒充：
// 报告里出现一个编出来的百分比，比没有这个百分比危险得多。
//
// # 覆盖轮转文件（否则历史会"悄悄消失"）
//
// internal/auditlog 是 1 MiB 轮转、保留 2 份（audit.jsonl / .1 / .2）。只读本体的话，
// 超过 1 MiB 之后更早的事件在报告里**直接不见了** —— 而报告看起来完全正常，
// 这种"数字偏小但没人察觉"的偏差最难查。所以这里把轮转件一起读进来：
// 按 at 合并排序（不再假设文件内部/文件之间有序），并按**整行内容**去重。
// 保留份数**直接取自 internal/auditlog.Keep** —— 落盘策略一改，报告自动跟着改，
// 不会出现"轮转份数变了、报告少读一个文件"那种沉默偏差。
//
// 去重的依据：审计行自带 at（纳秒）+ pid + event，内容完全相同的两行就是同一事件；
// 轮转是 rename，理论上不该重复，但读到一半又轮转、或部署习惯是复制而非移动时就会重复。
//
// # 退出码（给脚本判断用）
//
//	0  正常（审计与请求日志都读到了东西）
//	2  数据源缺失或为空（会说明是哪一个）
//	1  用法错误
//
// 用法：
//
//	go run ./cmd/auditreport [--audit data/audit.jsonl] [--request-log data/request-log.jsonl] [--days 7]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/auditlog"
	"workbuddy2api/internal/prompt"
)

// exitUsage / exitNoData 见文件头的退出码约定。
const (
	exitOK     = 0
	exitUsage  = 1
	exitNoData = 2
)

// maxLastRowChars 报告里"最近一次"那一行的截断长度（原件是 200）。
const maxLastRowChars = 200

func main() {
	fs := flag.NewFlagSet("auditreport", flag.ContinueOnError)
	auditPath := fs.String("audit", filepath.Join("data", "audit.jsonl"), "审计文件（internal/auditlog 写的 JSONL）")
	logPath := fs.String("request-log", filepath.Join("data", "request-log.jsonl"), "请求日志（internal/logbuf 写的会话表）")
	days := fs.Int("days", 7, "统计窗口（天）")
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitUsage)
	}
	if *days <= 0 {
		fmt.Fprintln(os.Stderr, "--days 必须 > 0")
		os.Exit(exitUsage)
	}
	out, code := buildReport(*auditPath, *logPath, *days, time.Now())
	fmt.Print(out)
	os.Exit(code)
}

// buildReport 生成整份报告。抽成纯函数是为了让测试直接喂临时文件断言数字。
func buildReport(auditPath, logPath string, days int, now time.Time) (string, int) {
	var b strings.Builder
	since := now.AddDate(0, 0, -days)
	fmt.Fprintf(&b, "窗口：最近 %d 天（自 %s 起）\n", days, since.Format(time.RFC3339))

	auditRows, auditSources, auditAny := readAuditRotated(auditPath, since)
	logRows, logErr := readJSONL(logPath, since)

	// 数据源表：让"这份报告到底覆盖了多少历史"一眼可见（缺的、重复的都写出来）。
	b.WriteString("审计数据源（本体 + 轮转件）：\n")
	totalLines, totalKept, totalDupes := 0, 0, 0
	for _, s := range auditSources {
		if !s.Exists {
			fmt.Fprintf(&b, "  %s  不存在\n", s.Path)
			continue
		}
		fmt.Fprintf(&b, "  %s  存在  %d 行（去重后 %d，丢弃重复 %d）\n", s.Path, s.Lines, s.Rows, s.Dupes)
		totalLines += s.Lines
		totalKept += s.Rows
		totalDupes += s.Dupes
	}
	fmt.Fprintf(&b, "  合并：读入 %d 行，去重后 %d 行（丢弃重复 %d），窗口内 %d 行\n", totalLines, totalKept, totalDupes, len(auditRows))
	fmt.Fprintf(&b, "请求日志：%s\n\n", logPath)

	// 审计侧
	b.WriteString("=== 拒绝重试（审计）===\n")
	if !auditAny {
		fmt.Fprintf(&b, "无法读取：审计数据源一个都不存在（%s 及其轮转件 .1….%d）\n", auditPath, auditlog.Keep)
	} else if len(auditRows) == 0 {
		fmt.Fprintf(&b, "窗口内没有任何审计事件（文件存在，但 %d 天内 0 行）\n", days)
	} else {
		renderAudit(&b, auditRows)
	}

	// 请求侧
	b.WriteString("\n=== 请求（请求日志）===\n")
	if logErr != nil {
		fmt.Fprintf(&b, "无法读取：%v\n", logErr)
	} else if len(logRows) == 0 {
		fmt.Fprintf(&b, "窗口内没有任何请求记录（文件存在，但 %d 天内 0 行）\n", days)
	} else {
		renderRequests(&b, logRows)
	}

	// 拒绝率：显式说明算不出来（见文件头）——不许编。
	b.WriteString("\n=== 拒绝率 ===\n")
	b.WriteString("拒绝率：无法计算 —— 本仓请求日志不落回答正文（原件靠会话文件里的回答文本统计）。\n")
	b.WriteString("  可用的替代观测量：上面的 refusal_retry 次数（= 真正触发过重试的次数）。\n")

	code := exitOK
	// 注意判据是 auditAny（"有没有源"）而不是"本体在不在"：
	// 本体缺失但轮转件有数据时，这份报告仍然是有效的 ⇒ 不该给非零退出码。
	if !auditAny || logErr != nil || len(auditRows) == 0 || len(logRows) == 0 {
		code = exitNoData
	}
	return b.String(), code
}

// auditSource 一个审计数据源的读取结果（本体或某个轮转件）。
type auditSource struct {
	Path   string
	Exists bool
	Lines  int // 文件里的非空行数
	Rows   int // 其中被采用（非重复、非畸形）的行数
	Dupes  int // 被判为重复而丢弃的行数
}

// auditPaths 返回审计文件本体 + 轮转件（.1 … .Keep）。
//
// 保留份数**直接取自 internal/auditlog.Keep**：落盘策略与读侧共用同一个常量，
// 否则轮转份数一改，这里就开始少读文件，而报告表面上看不出任何异常。
func auditPaths(base string) []string {
	out := make([]string, 0, auditlog.Keep+1)
	out = append(out, base)
	for i := 1; i <= auditlog.Keep; i++ {
		out = append(out, fmt.Sprintf("%s.%d", base, i))
	}
	return out
}

// readAuditRotated 读本体 + 全部轮转件，合并 → 去重 → 按 at 排序 → 按窗口过滤。
//
// # 为什么要排序（而不是"照文件顺序拼"）
//
// 轮转之后文件的先后顺序与事件的时间顺序不再一致：
//
//	base  = 最新的一批（轮转是 base→.1，新的写在 base）
//	.1    = 稍早的一批
//	.2    = 更早的一批
//
// 前提是**每一份内部**递增。但报告要回答"最近一次是哪个事件"，
// 那个问题只有把合并后的集合按 at 排好才答得对。所以这里显式排序，不依赖文件顺序。
//
// # 没有 at 的行
//
// 与 readJSONL 同一纪律：解析不出时间就**不过滤**（宁可多报，不可静默丢），
// 排序时放在末尾（它们无法参与时间序）。
func readAuditRotated(base string, since time.Time) (rows []map[string]any, sources []auditSource, anyExists bool) {
	seen := map[string]bool{}
	var timed, untimed []map[string]any

	for _, p := range auditPaths(base) {
		src := auditSource{Path: p}
		raw, err := os.ReadFile(p)
		if err != nil {
			sources = append(sources, src)
			continue
		}
		src.Exists = true
		anyExists = true
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			src.Lines++
			// 按**整行内容**判重：审计行自带 at（纳秒）+ pid + event，
			// 内容完全相同就是同一事件（跨文件复现的唯一可能是轮转/复制）。
			if seen[line] {
				src.Dupes++
				continue
			}
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				continue // 畸形行跳过（与 readJSONL 同一条纪律：一行坏数据不能毁掉整份报告）
			}
			seen[line] = true
			src.Rows++
			if _, ok := rowTime(row); ok {
				timed = append(timed, row)
			} else {
				untimed = append(untimed, row)
			}
		}
		sources = append(sources, src)
	}

	// 合并排序（稳定：同一时刻的行保持读入顺序，便于 diff）
	sort.SliceStable(timed, func(i, j int) bool {
		ti, _ := rowTime(timed[i])
		tj, _ := rowTime(timed[j])
		return ti.Before(tj)
	})
	for _, r := range timed {
		if t, ok := rowTime(r); ok && t.Before(since) {
			continue
		}
		rows = append(rows, r)
	}
	rows = append(rows, untimed...)
	return rows, sources, anyExists
}

// readJSONL 读 JSONL 并按 at 过滤窗口。返回错误表示"文件读不了"（与"读到了但窗口内没行"不同）。
//
// 畸形行**跳过并计数**，不让一行坏数据把整份报告弄没（审计文件是追加写的，写一半被 kill 过是有可能的）。
func readJSONL(path string, since time.Time) ([]map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if t, ok := rowTime(row); ok && t.Before(since) {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// rowTime 取行里的 at（RFC3339，纳秒精度；解析不了就当作"无时间"，不过滤掉）。
func rowTime(row map[string]any) (time.Time, bool) {
	s, _ := row["at"].(string)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// renderAudit 审计侧：重试次数 / 结果分布 / 命中规则分布 / 最近一次 / 检测器复核。
func renderAudit(b *strings.Builder, rows []map[string]any) {
	var retries, outcomes []map[string]any
	other := map[string]int{}
	for _, r := range rows {
		switch str(r["event"]) {
		case "refusal_retry":
			retries = append(retries, r)
		case "refusal_retry_outcome":
			outcomes = append(outcomes, r)
		default:
			other[str(r["event"])]++
		}
	}
	fmt.Fprintf(b, "重试事件（audit refusal_retry）：%d 次\n", len(retries))

	// outcome 分布（原件的 refusal_retry_outcome 只表示**传输形态**，不能当成功判据）
	if len(outcomes) > 0 {
		dist := map[string]int{}
		for _, r := range outcomes {
			dist[str(r["outcome"])]++
		}
		fmt.Fprintf(b, "重试结果分布：%s\n", formatCounts(dist))
	}

	// 命中规则分布（match = 规则下标，-1 表示未命中）
	if len(retries) > 0 {
		rules := map[string]int{}
		for _, r := range retries {
			rules[fmt.Sprintf("rule[%s]", str(r["match"]))]++
		}
		fmt.Fprintf(b, "命中规则分布：%s\n", formatCounts(rules))
	}

	// 检测器复核：用**现成的** prompt.LooksLikeRefusal 再判一次记录的 head。
	//
	// 为什么值得做：审计里的 head 是当时截下来的文本片段。若检测器后来被改过（规则增删），
	// 历史事件与新检测器就会不一致 —— 这一行让那种漂移一眼可见（否则只能靠人肉回放）。
	if len(retries) > 0 {
		match := 0
		for _, r := range retries {
			if prompt.LooksLikeRefusal(str(r["head"])) {
				match++
			}
		}
		fmt.Fprintf(b, "检测器复核：%d/%d 条历史 head 仍被判为拒绝（现检测器：internal/prompt.LooksLikeRefusal）\n", match, len(retries))
	}

	if len(other) > 0 {
		fmt.Fprintf(b, "其它事件：%s\n", formatCounts(other))
	}

	if len(rows) > 0 {
		last := rows[len(rows)-1]
		if blob, err := json.Marshal(last); err == nil {
			fmt.Fprintf(b, "最近一次：%s\n", truncate(string(blob), maxLastRowChars))
		}
	}
}

// renderRequests 请求侧：总数 / 状态分布 / 失败数 / model 与 provider 前 10。
func renderRequests(b *strings.Builder, rows []map[string]any) {
	status := map[string]int{}
	byModel := map[string]int{}
	byProvider := map[string]int{}
	failed := 0
	for _, r := range rows {
		var code float64
		if v, ok := r["status"].(float64); ok {
			code = v
		}
		status[fmt.Sprintf("%d", int(code))]++
		if int(code) >= 400 {
			failed++
		}
		byModel[str(r["model"])]++
		byProvider[str(r["provider"])]++
	}
	fmt.Fprintf(b, "请求总数：%d 条 · 失败（状态码 >= 400）：%d 条\n", len(rows), failed)
	fmt.Fprintf(b, "状态码分布：%s\n", formatCounts(status))
	fmt.Fprintf(b, "按模型（前 10）：%s\n", formatTop(byModel, 10))
	fmt.Fprintf(b, "按上游（前 10）：%s\n", formatTop(byProvider, 10))
}

// formatCounts 把计数表渲染成 "a=1, b=2"，按次数降序、次数相同按名字升序（输出稳定，便于 diff）。
func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// formatTop 同 formatCounts，但只取前 n 项。
func formatTop(m map[string]int, n int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// str 把任意 JSON 值当字符串用（数字/缺失都给一个稳定的字面量，不 panic）。
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "(空)"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// truncate 按**字符**截断（中文一个字 3 字节，按字节截会切出乱码）。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
