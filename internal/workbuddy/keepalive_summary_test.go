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
func TestKeepaliveSummaryReportsSkips(t *testing.T) {
	got := keepaliveSummary("schedule", 2, 5, 1, 1)
	for _, want := range []string{"后台续期完成", "续期 2", "跳过 6", "其中寿命未过半 5", "失败 1"} {
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
	if !strings.Contains(got, "续期 0") || !strings.Contains(got, "跳过 0") {
		t.Errorf("全零也要如实出这一行: %s", got)
	}
}
