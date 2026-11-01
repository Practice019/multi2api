package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile 写一个临时文件（测试全部用 t.TempDir()，绝不碰 data/ 下的真文件）。
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// now 固定"现在"，让窗口过滤可断言（否则测试会随运行时间漂移）。
func nowForTest() time.Time {
	return time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
}

// TestReportCountsAuditAndRequests 造数据 → 断言报告里的每个关键数字。
func TestReportCountsAuditAndRequests(t *testing.T) {
	dir := t.TempDir()
	audit := writeFile(t, dir, "audit.jsonl", strings.Join([]string{
		`{"at":"2026-11-01T11:00:00+08:00","event":"refusal_retry","chars":23,"head":"I can't help with that.","match":0,"pid":1}`,
		`{"at":"2026-11-01T11:00:01+08:00","event":"refusal_retry_outcome","outcome":"text","chunks":4,"chars":21,"pid":1}`,
		`{"at":"2026-11-01T11:05:00+08:00","event":"refusal_retry","chars":30,"head":"I cannot help with that.","match":3,"pid":1}`,
		`{"at":"2026-11-01T11:05:01+08:00","event":"refusal_retry_outcome","outcome":"tool_call","chunks":9,"chars":5,"pid":1}`,
		// 窗口外（40 天前）：必须被过滤掉
		`{"at":"2026-09-20T11:00:00+08:00","event":"refusal_retry","chars":1,"head":"old","match":9,"pid":1}`,
		// 畸形行：必须被跳过而不是让整份报告失败
		`{ 这不是 JSON`,
	}, "\n"))
	logPath := writeFile(t, dir, "request-log.jsonl", strings.Join([]string{
		`{"at":"2026-11-01T11:00:00+08:00","model":"workbuddy/auto","provider":"workbuddy","status":200,"seq":1}`,
		`{"at":"2026-11-01T11:00:01+08:00","model":"loomy/DeepSeek-V4.1-Flash","provider":"loomy","status":200,"seq":2}`,
		`{"at":"2026-11-01T11:00:02+08:00","model":"definitely-not-a-model-xyz","provider":"workbuddy","status":400,"seq":3}`,
		`{"at":"2026-11-01T11:00:03+08:00","model":"workbuddy/auto","provider":"workbuddy","status":500,"seq":4}`,
	}, "\n"))

	out, code := buildReport(audit, logPath, 7, nowForTest())
	if code != exitOK {
		t.Errorf("退出码 = %d，want %d\n%s", code, exitOK, out)
	}
	for _, want := range []string{
		"重试事件（audit refusal_retry）：2 次",
		"重试结果分布：text=1, tool_call=1", // 各 1 次 ⇒ 按名字升序
		"rule[0]=1, rule[3]=1",
		"请求总数：4 条 · 失败（状态码 >= 400）：2 条",
		"状态码分布：200=2, 400=1, 500=1",
		"workbuddy/auto=2",
		"按上游（前 10）：workbuddy=3, loomy=1", // 前缀在行首，键只是上游名
		"检测器复核：2/2 条历史 head 仍被判为拒绝",
		"拒绝率：无法计算",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("报告里缺 %q\n---\n%s", want, out)
		}
	}
	// 窗口外的规则下标 9 不许出现
	if strings.Contains(out, "rule[9]") {
		t.Error("窗口外的事件没被过滤掉")
	}
	// 最近一次 = 时间上最后一条（outcome tool_call）
	if !strings.Contains(out, "最近一次：") || !strings.Contains(out, "\"outcome\":\"tool_call\"") {
		t.Errorf("最近一次应指向最后一条事件\n%s", out)
	}
}

// TestReportReadsRotationsMergesAndDedupes 轮转件必须一起读；合并后按时间排序；跨文件重复行只算一次。
//
// # 为什么值得单测
//
// 轮转一旦发生（1 MiB），只读本体就会**静默少报**历史：报告看起来完全正常，
// 只是数字偏小 —— 这种偏差没有任何外部迹象，只能靠测试钉住。
func TestReportReadsRotationsMergesAndDedupes(t *testing.T) {
	dir := t.TempDir()

	// .2 = 最早的一批
	writeFile(t, dir, "audit.jsonl.2", strings.Join([]string{
		"{\"at\":\"2026-11-01T09:00:00+08:00\",\"event\":\"refusal_retry\",\"chars\":10,\"head\":\"old-a\",\"match\":1,\"pid\":1}",
		"{\"at\":\"2026-11-01T09:30:00+08:00\",\"event\":\"refusal_retry\",\"chars\":11,\"head\":\"old-b\",\"match\":2,\"pid\":1}",
	}, "\n")+"\n")
	// .1 = 中间一批；**故意与 .2 有一行完全相同**（轮转边界复现）
	writeFile(t, dir, "audit.jsonl.1", strings.Join([]string{
		"{\"at\":\"2026-11-01T09:30:00+08:00\",\"event\":\"refusal_retry\",\"chars\":11,\"head\":\"old-b\",\"match\":2,\"pid\":1}",
		"{\"at\":\"2026-11-01T10:00:00+08:00\",\"event\":\"refusal_retry_outcome\",\"outcome\":\"text\",\"chunks\":3,\"chars\":20,\"pid\":1}",
	}, "\n")+"\n")
	// base = 最新一批；**故意乱序**（最新事件写在最前面），并含一行文件内重复
	writeFile(t, dir, "audit.jsonl", strings.Join([]string{
		"{\"at\":\"2026-11-01T11:30:00+08:00\",\"event\":\"refusal_retry\",\"chars\":23,\"head\":\"I can't help with that.\",\"match\":0,\"pid\":2}",
		"{\"at\":\"2026-11-01T10:30:00+08:00\",\"event\":\"refusal_retry\",\"chars\":12,\"head\":\"mid\",\"match\":3,\"pid\":2}",
		"{\"at\":\"2026-11-01T10:30:00+08:00\",\"event\":\"refusal_retry\",\"chars\":12,\"head\":\"mid\",\"match\":3,\"pid\":2}",
		"{\"at\":\"2026-09-20T11:00:00+08:00\",\"event\":\"refusal_retry\",\"chars\":1,\"head\":\"too-old\",\"match\":9,\"pid\":2}",
		"{ 坏行",
	}, "\n")+"\n")
	logPath := writeFile(t, dir, "request-log.jsonl",
		"{\"at\":\"2026-11-01T11:00:00+08:00\",\"model\":\"workbuddy/auto\",\"provider\":\"workbuddy\",\"status\":200}"+"\n")

	out, code := buildReport(filepath.Join(dir, "audit.jsonl"), logPath, 7, nowForTest())
	if code != exitOK {
		t.Fatalf("退出码 = %d，want %d\n%s", code, exitOK, out)
	}

	// ① 数据源表：三份都在，逐份报出行数/采用数/重复数
	for _, want := range []string{
		"audit.jsonl  存在  5 行（去重后 3，丢弃重复 1）",
		// 去重按**读取顺序**（base→.1→.2）：old-b 那行先被 .1 收下，所以重复归到 .2 这一侧。
		// （第一版期望写反了，测试当场点出来。）
		"audit.jsonl.1  存在  2 行（去重后 2，丢弃重复 0）",
		"audit.jsonl.2  存在  2 行（去重后 1，丢弃重复 1）",
		"合并：读入 9 行，去重后 6 行（丢弃重复 2），窗口内 5 行",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("数据源表缺 %q\n---\n%s", want, out)
		}
	}

	// ② 去重生效：old-b 只算一次
	if !strings.Contains(out, "rule[2]=1") {
		t.Errorf("去重后应仍看到 rule[2]=1（唯一一条 old-b）\n%s", out)
	}
	if strings.Contains(out, "rule[9]") {
		t.Error("窗口外事件没被过滤")
	}
	// ③ 合并排序：最近一次必须是 11:30 那条（它在 base 文件里是**第一行**，只有排序才对）
	if !strings.Contains(out, "最近一次：") || !strings.Contains(out, "\"head\":\"I can't help with that.\"") {
		t.Errorf("最近一次应按合并后的时间序取 11:30 那条\n%s", out)
	}
	// ④ 重试事件总数 = 09:00/09:30/10:30/11:30 四条（去重后）
	if !strings.Contains(out, "重试事件（audit refusal_retry）：4 次") {
		t.Errorf("重试次数应为去重后的 4\n%s", out)
	}
}

// TestReportOnlyBaseMatchesPreviousBehavior 只有本体时（还没轮转过）行为与改造前一致。
func TestReportOnlyBaseMatchesPreviousBehavior(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "audit.jsonl", strings.Join([]string{
		"{\"at\":\"2026-11-01T11:00:00+08:00\",\"event\":\"refusal_retry\",\"chars\":23,\"head\":\"I can't help with that.\",\"match\":0,\"pid\":1}",
		"{\"at\":\"2026-11-01T11:05:00+08:00\",\"event\":\"refusal_retry_outcome\",\"outcome\":\"text\",\"chunks\":4,\"chars\":21,\"pid\":1}",
	}, "\n")+"\n")
	logPath := writeFile(t, dir, "request-log.jsonl",
		"{\"at\":\"2026-11-01T11:00:00+08:00\",\"model\":\"workbuddy/auto\",\"provider\":\"workbuddy\",\"status\":200}"+"\n")

	out, code := buildReport(filepath.Join(dir, "audit.jsonl"), logPath, 7, nowForTest())
	if code != exitOK {
		t.Fatalf("退出码 = %d，want %d\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "重试事件（audit refusal_retry）：1 次") {
		t.Errorf("只有本体时计数应与改造前一致\n%s", out)
	}
	for _, want := range []string{"audit.jsonl.1  不存在", "audit.jsonl.2  不存在", "丢弃重复 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("轮转件不存在时应如实标注，缺 %q\n%s", want, out)
		}
	}
}

// TestReportRotationAloneStillReports 本体缺失但轮转件有数据 ⇒ 仍是一份有效报告（退出码 0）。
//
// 这正是"只读本体"那版的盲区：它会把这种情形报成"无法读取"，
// 而历史其实就在 .1 里 —— 用户会以为审计没在写。
func TestReportRotationAloneStillReports(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "audit.jsonl.1",
		"{\"at\":\"2026-11-01T11:00:00+08:00\",\"event\":\"refusal_retry\",\"chars\":23,\"head\":\"I can't help with that.\",\"match\":0,\"pid\":1}"+"\n")
	logPath := writeFile(t, dir, "request-log.jsonl",
		"{\"at\":\"2026-11-01T11:00:00+08:00\",\"model\":\"workbuddy/auto\",\"provider\":\"workbuddy\",\"status\":200}"+"\n")
	out, code := buildReport(filepath.Join(dir, "audit.jsonl"), logPath, 7, nowForTest())
	if code != exitOK {
		t.Fatalf("本体缺失但轮转件有数据 ⇒ 退出码应为 0，实际 %d\n%s", code, out)
	}
	if !strings.Contains(out, "重试事件（audit refusal_retry）：1 次") {
		t.Errorf("应从 .1 里读到那一条\n%s", out)
	}
	if !strings.Contains(out, "audit.jsonl  不存在") {
		t.Errorf("本体不存在也要如实标注\n%s", out)
	}
}

// TestReportMissingFileIsExplicit 文件不存在 ⇒ 如实说明 + 非零退出码（不许静默给 0）。
func TestReportMissingFileIsExplicit(t *testing.T) {
	dir := t.TempDir()
	logPath := writeFile(t, dir, "request-log.jsonl", `{"at":"2026-11-01T11:00:00+08:00","model":"m","provider":"p","status":200}`+"\n")
	out, code := buildReport(filepath.Join(dir, "没有这个文件.jsonl"), logPath, 7, nowForTest())
	if code != exitNoData {
		t.Errorf("退出码 = %d，want %d", code, exitNoData)
	}
	if !strings.Contains(out, "无法读取") {
		t.Errorf("应当如实说明读不了\n%s", out)
	}
	if !strings.Contains(out, "拒绝率：无法计算") {
		t.Error("即使数据缺失，也要说明拒绝率算不出来（不许编数）")
	}
}

// TestReportEmptyFile 空文件 ⇒ 说明"窗口内 0 行" + 非零退出码。
func TestReportEmptyFile(t *testing.T) {
	dir := t.TempDir()
	empty := writeFile(t, dir, "audit.jsonl", "")
	out, code := buildReport(empty, empty, 7, nowForTest())
	if code != exitNoData {
		t.Errorf("退出码 = %d，want %d", code, exitNoData)
	}
	if !strings.Contains(out, "0 行") && !strings.Contains(out, "没有任何") {
		t.Errorf("应当说明窗口内没有数据\n%s", out)
	}
}

// TestReportNoRefusalEvents 有请求但无重试事件 ⇒ 重试次数为 0，且不出现分布行。
func TestReportNoRefusalEvents(t *testing.T) {
	dir := t.TempDir()
	audit := writeFile(t, dir, "audit.jsonl", `{"at":"2026-11-01T11:00:00+08:00","event":"other_thing","pid":1}`+"\n")
	logPath := writeFile(t, dir, "request-log.jsonl", `{"at":"2026-11-01T11:00:00+08:00","model":"workbuddy/auto","provider":"workbuddy","status":200}`+"\n")
	out, code := buildReport(audit, logPath, 7, nowForTest())
	if code != exitOK {
		t.Errorf("有一行审计（虽不是重试事件）就算有数据 ⇒ 退出码应为 0，实际 %d\n%s", code, out)
	}
	if !strings.Contains(out, "重试事件（audit refusal_retry）：0 次") {
		t.Errorf("应报 0 次\n%s", out)
	}
	if strings.Contains(out, "命中规则分布") {
		t.Error("没有重试事件时不该打印规则分布（空分布是无意义的噪声）")
	}
	if !strings.Contains(out, "其它事件：other_thing=1") {
		t.Errorf("非重试事件应归到「其它事件」\n%s", out)
	}
}
