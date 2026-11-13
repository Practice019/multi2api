package logx

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestCoalesceMergesIntoOneLine 核心判据：同一 key 的 N 次事件**折成一行**（带次数与时间范围）。
// 用户原话："重复的能不能用一条日志输出？然后统计数量什么的。"
func TestCoalesceMergesIntoOneLine(t *testing.T) {
	base := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	cur := base
	now = func() time.Time { return cur }
	defer func() { now = time.Now }()
	resetCoalesceForTest()
	_ = capture(t, LevelInfo)

	// ⚠ 样例文案跟着 2026-11-13 的精简换格式（`loomy 预热 · 未缓存 t0/i0 · …`）。
	//   折叠**按 key**（第三个参数 "warm"）、不看内容 ⇒ 文案换成什么都会折成一行；
	//   这里换样例是为了"测试里出现的日志与线上一致"（免得看日志的人对不上）。
	Coalesce(LevelInfo, "loomy", "warm", "", "loomy: 预热 · 未缓存 t0/i0 · 过期 t43/i43 · 号44 · 发起12")
	cur = base.Add(5 * time.Second)
	Coalesce(LevelInfo, "loomy", "warm", "", "loomy: 预热 · 未缓存 t0/i0 · 过期 t31/i31 · 号44 · 发起12")
	cur = base.Add(12 * time.Second)
	Coalesce(LevelWarn, "loomy", "warm", "", "loomy: 预热 · 未缓存 t0/i0 · 过期 t31/i31 · 号44 · 发起12 · 错 100002 登录已失效")

	lines := FlushCoalesced()
	if len(lines) != 1 {
		t.Fatalf("同一 key 三次应折成 **1 行**，实际 %d 行：\n%s", len(lines), strings.Join(lines, "\n"))
	}
	got := lines[0]
	for _, want := range []string{"WARN", "loomy: 预热", "×3", "14:00:00→14:00:12"} {
		if !strings.Contains(got, want) {
			t.Errorf("折叠行缺少 %q：%s", want, got)
		}
	}
	if again := FlushCoalesced(); len(again) != 0 {
		t.Errorf("清窗后不该还有行（静默期不打空行）：%v", again)
	}
}

// TestCoalesceSubBreakdown 细分统计：`100002×5 · 100003×1`（这就是"统计数量"的落点）。
func TestCoalesceSubBreakdown(t *testing.T) {
	base := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	cur := base
	now = func() time.Time { return cur }
	defer func() { now = time.Now }()
	resetCoalesceForTest()
	_ = capture(t, LevelInfo)
	for i := 0; i < 5; i++ {
		Coalesce(LevelWarn, "loomy", "quota:loomy", "100002", "额度查询失败 uid=2609…7497")
		cur = cur.Add(time.Second)
	}
	Coalesce(LevelWarn, "loomy", "quota:loomy", "100003", "额度查询失败 uid=2610…1234")
	lines := FlushCoalesced()
	if len(lines) != 1 {
		t.Fatalf("应折成 1 行，实际 %d", len(lines))
	}
	if !strings.Contains(lines[0], "×6") || !strings.Contains(lines[0], "100002×5") || !strings.Contains(lines[0], "100003×1") {
		t.Errorf("细分统计不对：%s", lines[0])
	}
}

// TestCoalesceKeysAreIsolated 不同 key 必须各成一行（合并错东西比不合并更坏）。
func TestCoalesceKeysAreIsolated(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC) }
	defer func() { now = time.Now }()
	resetCoalesceForTest()
	_ = capture(t, LevelInfo)
	Coalesce(LevelWarn, "loomy", "quota:loomy", "", "额度查询失败")
	Coalesce(LevelWarn, "qoder", "quota:qoder", "", "额度查询失败")
	lines := FlushCoalesced()
	if len(lines) != 2 {
		t.Fatalf("两个不同上游的失败不该合并成一行，实际 %d 行：%v", len(lines), lines)
	}
}

func resetCoalesceForTest() {
	coMu.Lock()
	coBuckets = map[string]*coalesceBucket{}
	coMu.Unlock()
}

var _ = bytes.MinRead
