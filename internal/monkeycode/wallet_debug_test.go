package monkeycode

import (
	"encoding/json"
	"testing"
)

// TestWalletPayloadParsesLimit 钉住 WalletPayload 真的能解出 daily_token_limit。
//
// # 为什么需要这条（实测踩到）
//
// 上游 `GET /users/wallet` 确实返回了 `daily_token_limit: 10000000`，
// 但界面上的分项上限一直是空。而 json.Unmarshal 对**字段名不符**
// **不会报错** —— 那个字段只是静默保持零值。
//
// 所以「Free 有值、FreeLimit 恒 0」在编译、单测、真机启动里都不显眼，
// 只有盯着界面数格子才能发现。用真实响应体钉一遍才可靠。
func TestWalletPayloadParsesLimit(t *testing.T) {
	const body = `{"code":0,"message":"success","data":{
		"id":"00000000-0000-0000-0000-000000000000",
		"balance":299965,
		"daily_token_balance":9944867,
		"daily_token_limit":10000000}}`

	data, err := unwrap([]byte(body))
	if err != nil {
		t.Fatalf("unwrap 失败: %v", err)
	}
	var p WalletPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.Balance != 299965 {
		t.Errorf("Balance: got %d want 299965", p.Balance)
	}
	if p.DailyTokenBalance != 9944867 {
		t.Errorf("DailyTokenBalance: got %d want 9944867", p.DailyTokenBalance)
	}
	if p.DailyTokenLimit != 10000000 {
		t.Errorf("DailyTokenLimit: got %d want 10000000 —— 字段没解出来时 Unmarshal 不报错，只会静默保持 0", p.DailyTokenLimit)
	}
}
