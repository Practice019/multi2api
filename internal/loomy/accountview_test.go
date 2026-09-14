// accountview_test.go —— 「列集」与「凭证会不会过期」的守卫。
//
// # 这一组对应用户报的那三格空白
//
//	「Token」      → 从列集里**去掉**（accessToken 恒空，它永远是 `—`）
//	「今日签到」    → 从列集里**去掉**（loomy 没有签到端点）
//	「Token 到期」  → 保留，但答案从 `—` 变成「永久」（CredentialLifetimeExt）
//
// # 为什么"首列"要单独断言
//
// 用户对 codearts 提的要求原文是「和其他上游的表格统一一下第一个列」——
// 那是一条**跨上游**的约束。只断言"loomy 自己的列集"挡不住有人把
// provider 从**默认列集**里删掉（那时 loomy 这份仍含 provider 却与
// workbuddy 不一致）。所以这条比较的是**两份列集的首列**。
package loomy

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestLoomyAccountColumns 列集必须逐列全等。
//
// 列集是**用户可见的契约**：多一列少一列、顺序变了都是观感变化。
// 只断言"包含某一列"的话，顺手多加一列不会有任何测试变红。
func TestLoomyAccountColumns(t *testing.T) {
	p := NewWithConfig(Config{})
	got := p.AccountColumns()
	want := []string{
		gateway.AccountColProvider,
		gateway.AccountColNickname,
		gateway.AccountColUID,
		gateway.AccountColQuota,
		gateway.AccountColStatus,
		gateway.AccountColTokenExpiry,
		gateway.AccountColSuccess,
		gateway.AccountColOps,
	}
	if len(got) != len(want) {
		t.Fatalf("列数 = %d，want %d\n  实际: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 列 = %q，want %q（顺序是表头顺序，不能变）", i, got[i], want[i])
		}
	}
	// 用户明确要求删掉「熔断」「在途」—— 反向断言它们不在。
	for _, removed := range []string{gateway.AccountColBreaker, gateway.AccountColInFlight} {
		for _, id := range got {
			if id == removed {
				t.Errorf("loomy 列里仍有 %q —— 用户要求删掉熔断/在途", removed)
			}
		}
	}
}

// TestLoomyAccountColumnsFirstIsProvider 首列必须与其他上游一致。
func TestLoomyAccountColumnsFirstIsProvider(t *testing.T) {
	def := gateway.DefaultAccountColumns() // workbuddy 走的那份（未自报列的上游）
	got := NewWithConfig(Config{}).AccountColumns()
	if len(def) == 0 || len(got) == 0 {
		t.Fatal("列集不应为空")
	}
	if got[0] != def[0] {
		t.Errorf("loomy 首列 = %q，而默认列集（workbuddy）首列 = %q —— "+
			"两张表的第一个格子必须描述同一件事，否则横向看过去对不齐",
			got[0], def[0])
	}
	if got[0] != gateway.AccountColProvider {
		t.Errorf("首列应当是 %q，得到 %q", gateway.AccountColProvider, got[0])
	}
}

// TestLoomyAccountColumnsDropTokenAndCheckin 两列不适用的必须不在。
//
// 两个判据都来自**能力位**，而不是"我觉得不好看"：
//
//	「Token」     读的是 has_token（AccessToken != ""），loomy 恒为空
//	「今日签到」   读的是 checkinlog 的签到记录，loomy **没有**签到动作
func TestLoomyAccountColumnsDropTokenAndCheckin(t *testing.T) {
	p := NewWithConfig(Config{})
	if p.Caps().Has(gateway.CapCheckin) {
		t.Fatal("前置条件不成立：loomy 不该声明 CapCheckin")
	}
	for _, c := range p.AccountColumns() {
		switch c {
		case gateway.AccountColToken:
			// loomy 的凭证是 session 字符串，不是 Bearer accessToken：
			// 通用投影里 AccessToken 恒空 → 这一列永远 `—`。
			// 它该用 token_expiry（走 CredentialLifetimeExt）。
			t.Error("loomy 的列里出现了 token —— 它的 accessToken 恒空，" +
				"那一列会永远是「—」")
		case gateway.AccountColCheckin:
			t.Error("loomy 的列里出现了 checkin —— 它没有签到动作" +
				"（日额度由服务端按自然日自动重置，没有任何可点的领取）")
		}
	}
	// 反过来：token_expiry 必须在（它是「永久」的落点）。
	found := false
	for _, c := range p.AccountColumns() {
		if c == gateway.AccountColTokenExpiry {
			found = true
		}
	}
	if !found {
		t.Error("loomy 的列里必须有 token_expiry —— " +
			"「Token 到期」是用户点名要的三项之一，它的答案是「永久」")
	}
}

// TestLoomyAccountColumnsAreKnown 防止自报的列 id 拼错。
//
// 拼错是**静默失效**：前端查不到那个 id 就跳过 → 那一列直接消失，
// 页面上看不出是漏了还是本来没有。
func TestLoomyAccountColumnsAreKnown(t *testing.T) {
	for _, c := range NewWithConfig(Config{}).AccountColumns() {
		if !gateway.IsKnownAccountColumn(c) {
			t.Errorf("列 id %q 不在 gateway 的规范词汇表里 —— 前端会静默跳过它", c)
		}
	}
}

// TestNeverExpires 凭证类型对时回 true，类型不对时回 false。
//
// # 为什么类型不对必须回 false
//
// 回 true 就等于**替别人的凭证**断言"永久有效"。而契约（gateway.
// CredentialLifetimeExt）写明：不确定时必须返回 false ——
// 「永久」是个强断言，说错了会让用户以为手里的号永远可用。
func TestNeverExpires(t *testing.T) {
	p := NewWithConfig(Config{})

	if !p.NeverExpires(gateway.Credential{Secret: &Auth{Session: fixtureSession}}) {
		t.Error("loomy 的 session 凭证必须是「永久」（无 TTL、无 refresh token）")
	}
	cases := []struct {
		name   string
		secret any
	}{
		{"nil", nil},
		{"错误类型", "not-a-loomy-auth"},
		{"nil 指针", (*Auth)(nil)},
		{"session 为空", &Auth{UID: "u"}},
	}
	for _, c := range cases {
		if p.NeverExpires(gateway.Credential{Secret: c.secret}) {
			t.Errorf("%s：拿不准时必须返回 false（不能替别人断言永久有效）", c.name)
		}
	}
}

// TestAccountColumnsReturnsFreshSlice 每次返回新切片。
//
// 调用方可能排序或裁剪（核心不复制就直接下发）。共享底层数组会让
// 下一次调用拿到被改过的"列集"，而且不报错。
func TestAccountColumnsReturnsFreshSlice(t *testing.T) {
	p := NewWithConfig(Config{})
	a := p.AccountColumns()
	b := p.AccountColumns()
	a[0] = "被改坏了"
	if b[0] == "被改坏了" {
		t.Error("AccountColumns 返回的是共享底层数组 —— 调用方排序会污染下一次调用")
	}
}
