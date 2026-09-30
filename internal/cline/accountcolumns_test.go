package cline

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestAccountColumnsDropsCheckin cline 的列集**不含** checkin。
//
// # 用户报的问题
//
// 账号池里 cline 那一行显示「今日签到 —」—— 一个恒空的列。
// 用户会以为"今天还没签到"，实际是 cline **没有签到端点**。
//
// 判据：列集里不得出现 checkin，且**其余默认列都要保留**
// （只去掉那一列，不要顺手改成一份新的硬编码列表 ——
//
//	那样核心给默认列集加列时 cline 会静默不跟随）。
func TestAccountColumnsDropsCheckin(t *testing.T) {
	p := NewWithConfig(Config{})
	cols := p.AccountColumns()

	for _, c := range cols {
		if c == gateway.AccountColCheckin {
			t.Error("列集里仍有 checkin —— cline 没有签到端点，" +
				"这一列恒为 `—`，显示它等于告诉用户「今天还没签到」")
		}
	}

	// 其余默认列一个都不能少。
	inSet := map[string]bool{}
	for _, c := range cols {
		inSet[c] = true
	}
	for _, c := range gateway.DefaultAccountColumns() {
		if c == gateway.AccountColCheckin {
			continue
		}
		if !inSet[c] {
			t.Errorf("默认列 %q 被误删了 —— 只该去掉 checkin 一列", c)
		}
	}
	if len(cols) != len(gateway.DefaultAccountColumns())-1 {
		t.Errorf("列数 = %d，want %d（默认列集减一）",
			len(cols), len(gateway.DefaultAccountColumns())-1)
	}
}

// TestAccountColumnsImpliesNoCheckinCapability 列集与能力位**必须一致**。
//
// 两者是同一件事的两面：
//
//	有 CapCheckin  → 有签到端点 → 该显示 checkin 列
//	无 CapCheckin  → 没有端点   → 不该显示（否则是个恒失败/恒空的入口）
//
// 若哪天 cline 真的加了签到，这条会红并提醒同时改两处。
func TestAccountColumnsImpliesNoCheckinCapability(t *testing.T) {
	p := NewWithConfig(Config{})
	hasCap := p.Caps().Has(gateway.CapCheckin)

	hasCol := false
	for _, c := range p.AccountColumns() {
		if c == gateway.AccountColCheckin {
			hasCol = true
		}
	}
	if hasCap != hasCol {
		t.Errorf("能力位 CapCheckin=%v 与列集含 checkin=%v 不一致 —— "+
			"两者必须同进同退（有端点才显示该列）", hasCap, hasCol)
	}
}
