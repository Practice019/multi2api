// event_test.go 结构化事件日志的判据。
//
// # 用户的原话（2026-11-16）
//
//	「我不仅仅是希望它要进行折叠，它打印的内容也不需要太多。
//	  我感觉它打印的文本也需要重新规划一下，就是符合工程化的项目。
//	  它打印的东西文本要符合工程化的那种语言，因为我现在看到好像是
//	  很特别乱，特别的不像成熟的项目，不像那种企业级的项目的语言。」
//
// 所以这组判据钉四件事：
//
//	① 事件名点分小写（可被 grep/jq 聚合）
//	② 字段蛇形 k=v，值只在需要时加引号
//	③ 值里的换行/引号必须转义 —— 否则一行日志被拆成多行（最脏的那种脏）
//	④ 缩写与截断必须**统一且可见**
package logx

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// captureEvent 抓一条事件的输出。
func captureEvent(fn func()) string {
	var buf bytes.Buffer
	restore := CaptureTo(&buf)
	defer restore()
	fn()
	return buf.String()
}

// ★ ① 事件名在前、字段随后，且是点分小写。
func TestEventFormat(t *testing.T) {
	out := captureEvent(func() {
		Event(LevelInfo, "travel", "travel.depart.ok", "uid", "u_123", "location", 4)
	})
	if !strings.Contains(out, "travel.depart.ok uid=u_123 location=4") {
		t.Fatalf("行格式不符：%s", out)
	}
	// 前缀三段仍然在（时间 级别 作用域）。
	if !strings.Contains(out, "INFO") || !strings.Contains(out, "travel") {
		t.Fatalf("缺少级别/作用域：%s", out)
	}
}

// ★ ② 值只在需要时加引号。
func TestEventQuotesOnlyWhenNeeded(t *testing.T) {
	line := eventLine("x", []any{
		"plain", "abc",
		"spaced", "a b",
		"empty", "",
		"nil", nil,
		"quoted", `he said "hi"`,
		"eq", "a=b",
	})
	if !strings.Contains(line, "plain=abc") {
		t.Fatalf("普通值不该加引号：%s", line)
	}
	if !strings.Contains(line, `spaced="a b"`) {
		t.Fatalf("含空格要加引号：%s", line)
	}
	if !strings.Contains(line, "empty=-") || !strings.Contains(line, "nil=-") {
		t.Fatalf("空值应写成 -（而不是 key= 看着像截断）：%s", line)
	}
	if !strings.Contains(line, `quoted="he said \"hi\""`) {
		t.Fatalf("内部引号要转义：%s", line)
	}
	if !strings.Contains(line, `eq="a=b"`) {
		t.Fatalf("含等号要加引号（否则解析歧义）：%s", line)
	}
}

// ★ ③ 换行必须被压平 —— 一行日志被拆成多行是最脏的那种脏。
func TestEventFlattensNewlines(t *testing.T) {
	out := captureEvent(func() {
		Event(LevelError, "up", "up.stream.fail", "err", errors.New("line1\nline2\r\nline3"))
	})
	body := strings.TrimRight(out, "\n")
	if strings.Count(body, "\n") != 0 {
		t.Fatalf("★ 值里的换行把一行日志拆成了多行：\n%s", out)
	}
	if !strings.Contains(body, `line1\nline2`) {
		t.Fatalf("换行应被转义成 \\n（可见但不断行）：%s", body)
	}
}

// 时长的单位在字段名里，值里不再重复。
func TestEventDurationUsesMillis(t *testing.T) {
	line := eventLine("x", []any{"elapsed_ms", 38200 * time.Millisecond})
	if !strings.Contains(line, "elapsed_ms=38200") {
		t.Fatalf("时长应折成毫秒且不重复单位：%s", line)
	}
}

// 参数个数为奇数 ⇒ 明确标 malformed（不静默吞）。
func TestEventMalformedVisible(t *testing.T) {
	line := eventLine("x", []any{"a", 1, "dangling"})
	if !strings.Contains(line, "malformed=true") {
		t.Fatalf("★ 奇数个参数必须标出来（静默吞掉会让调用点的 bug 永远看不见）：%s", line)
	}
}

// ⚠ 这里**刻意没有** ShortUID 的判据：缩写规则归 `logbuf.ShortUID`
//（全仓唯一实现，T-403），本包只转发/直接调用。
// 在 logx 里再写一份判据等于把"规则住哪"这件事搞模糊 ——
// 而 `TestShortUIDHasExactlyOneImplementation` 也会因为多一份定义而红。

// ★ 截断必须可见（结尾 …）。
func TestShortStringTruncationIsVisible(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := ShortString(long, 10)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("★ 截断必须带 … （否则会让人以为原文就这样）：%q", got)
	}
	if ShortString("ok", 10) != "ok" {
		t.Fatal("不超长不该改动")
	}
	if strings.ContainsRune(ShortString("a\nb", 10), '\n') {
		t.Fatal("短形态里不该留换行")
	}
}

// Eventf 只在真有说明时才带 msg，且 msg 是唯一允许 CJK 的位置。
func TestEventfMsgOnlyWhenPresent(t *testing.T) {
	a := eventLine("x", []any{"uid", "u"})
	if strings.Contains(a, "msg=") {
		t.Fatalf("没有说明不该凭空造 msg：%s", a)
	}
	out := captureEvent(func() {
		Eventf(LevelWarn, "up", "up.quota.fail", "显示为未知，不填 0", "uid", "u")
	})
	if !strings.Contains(out, `msg="显示为未知，不填 0"`) {
		t.Fatalf("中文说明应进 msg= 并被引号包住：%s", out)
	}
}

// 事件名与字段的顺序由调用方给定，不被重排（主键在前的信息不能丢）。
func TestEventPreservesFieldOrder(t *testing.T) {
	line := eventLine("x", []any{"uid", "u", "action", "depart", "location", 4})
	iUID := strings.Index(line, "uid=")
	iAct := strings.Index(line, "action=")
	iLoc := strings.Index(line, "location=")
	if !(iUID < iAct && iAct < iLoc) {
		t.Fatalf("★ 字段顺序被重排了（调用方给的顺序有意义）：%s", line)
	}
}
