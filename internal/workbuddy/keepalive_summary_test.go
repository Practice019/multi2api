package workbuddy

import (
	"strings"
	"testing"
)

// TestKeepaliveSummaryReportsSkips 收尾文案必须把"跳过"写出来。
//
// # 这条测试守的是什么
//
// 门槛（寿命过半才刷）生效后，被跳过的账号原本**一行日志都没有** ⇒ 用户问
// "门槛到底在不在工作"时只能去读 JWT 的 iat 反推。这条测试钉住"跳过"必须可见，
// 并且"其中寿命未过半"这个数要单列 —— 那正是门槛生效的直接证据。
// ⚠ 2026-11-16 形态改了（`续期 2` → `refreshed=2`），但**意图没变**：
// 四个数字（续期 / 跳过 / 其中寿命未过半 / 失败）一个都不能丢，
// 且都必须带字段名（带字段名比旧形态更强 —— 机器可聚合）。
func TestKeepaliveSummaryReportsSkips(t *testing.T) {
	got := keepaliveSummary("schedule", 2, 5, 1, 1)
	for _, want := range []string{
		"refresh.summary",
		"refreshed=2",
		"skipped=6",           // halfLife(5) + otherSkipped(1)
		"half_life_skipped=5", // 其中"寿命未过半"那 5 个
		"failed=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("收尾文案里缺 %q: %s", want, got)
		}
	}
}

// TestKeepaliveSummarySilentForManual 手动保活不打印汇总（界面本来就逐账号显示结果）。
func TestKeepaliveSummarySilentForManual(t *testing.T) {
	if got := keepaliveSummary("manual", 2, 0, 0, 0); got != "" {
		t.Errorf("manual 不该有汇总行，实际 %q", got)
	}
}

// TestKeepaliveSummaryCountsZeroHonestly 全零也要出这一行。
//
// 理由："扫了 0 个"与"没扫"是两件事 —— 后者说明调度没跑起来（那是另一个 bug，
// 而它恰恰只能靠这一行区分）。
func TestKeepaliveSummaryCountsZeroHonestly(t *testing.T) {
	got := keepaliveSummary("schedule", 0, 0, 0, 0)
	if !strings.Contains(got, "refreshed=0") || !strings.Contains(got, "skipped=0") {
		t.Errorf("全零也要如实出这一行: %s", got)
	}
}
