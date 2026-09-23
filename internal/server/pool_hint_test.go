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
}
