// batch_test.go 批量结果聚合器的判据。
//
// # 用户的原话（2026-11-16）
//
//	「因为有很多账号嘛，然后他现在好像是一个账号，干什么事情之后，他就会打印一遍。
//	  我不希望这样，因为太多账号的话，那这样会刷屏。而我是希望这件事情结束之后，
//	  他直接统计成功的账号和不成功的账号什么，直接统计数字一行就可以了。」
//	「它打印的东西文本要符合工程化的那种语言，不像企业级项目的语言。」
//
// 所以这组判据钉四件事：
//
//	① 一趟任务只产出**一行**（逐账号不打日志）
//	② ok / fail / skip **分开计数**（跳过不是失败），零结果不出现
//	③ 失败样本**有上限**，且"没列全"必须**可见**
//	④ 文本是**工程化的**：事件名 + 蛇形 k=v，ASCII 键，可被 grep/jq 聚合
package logx

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// runBatch 跑一趟批量任务并把日志抓下来。
func runBatch(t *testing.T, fn func(b *Batch)) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	restore := CaptureTo(&buf)
	defer restore()

	b := NewBatch("codearts", "checkin.summary")
	fn(b)
	line := b.Done()
	return line, buf.String()
}

// ★ ① 一趟 50 个账号只产出**一行**日志。
func TestBatchEmitsExactlyOneLine(t *testing.T) {
	line, log := runBatch(t, func(b *Batch) {
		for i := 0; i < 50; i++ {
			b.OK()
		}
	})
	if strings.TrimSpace(line) == "" {
		t.Fatal("必须产出一行汇总")
	}
	body := strings.TrimSpace(log)
	if body == "" {
		t.Fatal("汇总行没有真的写进日志出口")
	}
	if n := strings.Count(strings.TrimRight(body, "\n"), "\n"); n > 0 {
		t.Fatalf("★ 一趟任务打了 %d 行（应只有 1 行）—— 逐账号打日志就是刷屏的根源：\n%s", n+1, log)
	}
}

// ★ ② 三类分开计数，零结果不出现。
func TestBatchCountsAndOmitsZeros(t *testing.T) {
	line, _ := runBatch(t, func(b *Batch) {
		for i := 0; i < 42; i++ {
			b.OK()
		}
		for i := 0; i < 3; i++ {
			b.Fail("u" + string(rune('a'+i)) + "… timeout")
		}
		for i := 0; i < 5; i++ {
			b.Skip("disabled")
		}
	})
	for _, want := range []string{"ok=42", "fail=3", "skip=5", "skip_reasons=disabled:5"} {
		if !strings.Contains(line, want) {
			t.Fatalf("汇总行应有 %q，实际：%s", want, line)
		}
	}

	// 全成功 ⇒ 不写 fail=0 / skip=0（把行留短）。
	only, _ := runBatch(t, func(b *Batch) { b.OK(); b.OK() })
	if strings.Contains(only, "fail=") || strings.Contains(only, "skip=") {
		t.Fatalf("零结果不该出现（否则每行都拖几个零）：%s", only)
	}
	if !strings.Contains(only, "ok=2") {
		t.Fatalf("应写 ok=2：%s", only)
	}
}

// ★ ④ 文本必须工程化：事件名 + 蛇形 ASCII 键。
func TestBatchSummaryIsStructured(t *testing.T) {
	line, _ := runBatch(t, func(b *Batch) {
		b.OK()
		b.Fail("u1… timeout")
		b.Skip("disabled")
	})
	if !strings.HasPrefix(line, "checkin.summary ") {
		t.Fatalf("★ 汇总行应以点分事件名开头（可被 grep/jq 聚合）：%s", line)
	}
	// 每个字段都必须是 `snake_case=值` 形态。
	kvRe := regexp.MustCompile(`^[a-z][a-z0-9_]*=.+$`)
	for _, f := range strings.Fields(line)[1:] {
		// 引号内的空格会切碎，故只校验"看起来像字段片段"的那些。
		if strings.Contains(f, "=") && !kvRe.MatchString(f) && !strings.HasPrefix(f, `"`) && !strings.HasSuffix(f, `"`) {
			t.Fatalf("★ 字段不是蛇形 k=v：%q（整行：%s）", f, line)
		}
	}
	// 键名必须是 ASCII（中文只能进 msg= 的值）。
	for _, k := range []string{"ok=", "fail=", "skip=", "elapsed_ms="} {
		if !strings.Contains(line, k) {
			t.Fatalf("缺少标准字段 %q：%s", k, line)
		}
	}
	if !strings.Contains(line, "elapsed_ms=") {
		t.Fatalf("★ 耗时字段恒在（最便宜的健康信号）：%s", line)
	}
}

// ★ ③ 三者全零 ⇒ empty=true（而不是一行空白被读成"跑挂了"）。
func TestBatchAllZeroMarksEmpty(t *testing.T) {
	line, _ := runBatch(t, func(b *Batch) {})
	if !strings.Contains(line, "empty=true") {
		t.Fatalf("★ 全零时必须明确标出来（空白会被读成失败）：%s", line)
	}
}

// ★ ④ 失败样本有上限，且"没列全"用 fail_more 可见。
func TestBatchFailureSamplesAreBoundedAndVisible(t *testing.T) {
	line, _ := runBatch(t, func(b *Batch) {
		for i := 0; i < 10; i++ {
			b.Fail("u" + string(rune('0'+i)) + "… fail")
		}
	})
	if !strings.Contains(line, "fail=10") {
		t.Fatalf("应写 fail=10：%s", line)
	}
	if !strings.Contains(line, "fail_samples=") {
		t.Fatalf("应有失败样本：%s", line)
	}
	// 样本里最多 3 条（分号分隔）。
	at := strings.Index(line, "fail_samples=")
	seg := line[at:]
	if strings.Contains(seg, " ") {
		seg = seg[:strings.Index(seg[13:], " ")+13]
	}
	if n := strings.Count(seg, ";"); n > batchSampleMax-1 {
		t.Fatalf("★ 样本不该超过 %d 条（分号 %d 个）：%s", batchSampleMax, n, line)
	}
	if !strings.Contains(line, "fail_more=") {
		t.Fatalf("★ 截断了就必须写 fail_more（悄悄截断会让人以为失败只有这几条）：%s", line)
	}
}

// 跳过原因按次数降序，且输出稳定（便于比对两次运行）。
func TestBatchSkipReasonsStable(t *testing.T) {
	mk := func() string {
		line, _ := runBatch(t, func(b *Batch) {
			b.Skip("not_open")
			for i := 0; i < 3; i++ {
				b.Skip("disabled")
			}
			b.Skip("already")
		})
		return line
	}
	a, b := mk(), mk()
	if a != b {
		t.Fatalf("★ 两次相同输入产出不同文本（顺序不稳定）：\n%s\n%s", a, b)
	}
	if strings.Index(a, "disabled:3") > strings.Index(a, "not_open:1") {
		t.Fatalf("应按次数降序：%s", a)
	}
}

// 有失败 ⇒ WARN；全成功 ⇒ INFO。
func TestBatchLevelReflectsFailure(t *testing.T) {
	_, okLog := runBatch(t, func(b *Batch) { b.OK() })
	if strings.Contains(okLog, "WARN") {
		t.Fatalf("全成功不该用 WARN：%s", okLog)
	}
	_, failLog := runBatch(t, func(b *Batch) { b.Fail("x… timeout") })
	if !strings.Contains(failLog, "WARN") {
		t.Fatalf("★ 有失败必须用 WARN（否则和正常流水混在一起）：%s", failLog)
	}
}

// Total 覆盖三类之和。
func TestBatchTotal(t *testing.T) {
	b := NewBatch("up", "x.summary")
	b.OK()
	b.Fail("e")
	b.Skip("r")
	if b.Total() != 3 {
		t.Fatalf("Total = %d，want 3", b.Total())
	}
}

// nil 安全（调用方可能拿到 nil Batch —— 例如上游未接线时）。
func TestBatchNilSafe(t *testing.T) {
	var b *Batch
	b.OK()
	b.Fail("x")
	b.Skip("y")
	_ = b.Total()
	if s := b.Summary(); s != "" {
		t.Fatalf("nil Batch 的 Summary 应为空，实际 %q", s)
	}
	if s := b.Done(); s != "" {
		t.Fatalf("nil Batch 的 Done 应为空，实际 %q", s)
	}
}
