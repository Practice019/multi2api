// pool_hint_test.go 503 自愈文案的纯函数测试（真实困惑的回归钉：
// "key 有效但 402 冷却"不能被报成含糊的 all accounts unavailable）。
package server

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

func TestPoolUnavailableHintOf(t *testing.T) {
	if got := poolUnavailableHintOf(nil); !strings.Contains(got, "暂无账号") {
		t.Errorf("空池提示: %q", got)
	}
	cool := []pool.Status{{UID: "3146522385xxxx", Cooling: true, Reason: "额度不足", Until: time.Now().Add(30 * time.Minute)}}
	got := poolUnavailableHintOf(cool)
	if !strings.Contains(got, "额度不足") || !strings.Contains(got, "自动恢复") {
		t.Errorf("冷却提示应含原因与恢复时刻: %q", got)
	}
	if strings.Contains(got, "3146522385xxxx") {
		t.Errorf("uid 应截断: %q", got)
	}
	dis := []pool.Status{{UID: "u1", Disabled: true, Reason: "会话失效"}, {UID: "u2", Cooling: true}}
	got2 := poolUnavailableHintOf(dis)
	if !strings.Contains(got2, "会话失效") || !strings.Contains(got2, "u2 冷却中") {
		t.Errorf("混合状态: %q", got2)
	}
	many := make([]pool.Status, 0, 4)
	for i := 0; i < 4; i++ {
		many = append(many, pool.Status{UID: "u", Disabled: true})
	}
	if got3 := poolUnavailableHintOf(many); !strings.Contains(got3, "4 个账号") {
		t.Errorf("多账号应计数: %q", got3)
	}
	// ⚠ 窗口内失败这一维（2026-11-12 加）。
	//
	//   上游 5xx 的处罚是 `NoteError` ⇒ **熔断线**（连续 N 次，默认 3），
	//   不是禁用也不是软冷却 ⇒ 在熔断触发之前，提示里**什么都看不到**。
	//   实测语境：一次请求轮换 5 个号、每号各失败 1 次 ⇒ 提示「0 冷却」⇒
	//   用户据此判断「它没有惩罚任何账号、也没自动禁用」，而真相是
	//   「每号离熔断还差 2 次」。这一维让「离熔断还差几次」变成可读的事实。
	brk := make([]pool.Status, 0, 4)
	for i := 0; i < 4; i++ {
		brk = append(brk, pool.Status{UID: "u", BreakerFails: 1})
	}
	got4 := poolUnavailableHintOf(brk)
	if !strings.Contains(got4, "4 个已有窗口内失败") {
		t.Errorf("应报出窗口内失败的账号数（5xx 走熔断线，不在禁用/冷却里）: %q", got4)
	}
	if !strings.Contains(got4, "最多 1 次") {
		t.Errorf("应报出最大连续失败次数（读者据此判断离熔断还差几次）: %q", got4)
	}
}
