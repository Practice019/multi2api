package codearts

import (
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestAccountColumnsVocabulary 钉住 codearts 自报的列集与顺序。
//
// # 为什么这条要「全等」而不是「包含」
//
// 列集是**用户可见的契约**：多一列少一列、顺序变了，都是观感变化。
// 只断言"包含 token_expiry" 的话，顺手多加一列不会有任何测试变红。
func TestAccountColumnsVocabulary(t *testing.T) {
	p := NewWithConfig(Config{})
	got := p.AccountColumns()
	want := []string{
		gateway.AccountColProvider,
		gateway.AccountColNickname,
		gateway.AccountColUID,
		gateway.AccountColQuota,
		gateway.AccountColStatus,
		gateway.AccountColTokenExpiry,
		gateway.AccountColWelfare,
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
}

// TestAccountColumnsDoNotContainCheckin 钉住"codearts 没有签到"这条事实。
//
// codearts 的 Caps() 里**不声明** CapCheckin（它没有签到端点）。若它的账号列里
// 出现「今日签到」，那一列会永远是 `—` —— 用户会以为功能坏了，而不是
// "这个上游本来就没有"。两者必须一致。
func TestAccountColumnsDoNotContainCheckin(t *testing.T) {
	p := NewWithConfig(Config{})
	if p.Caps()&gateway.CapCheckin != 0 {
		t.Fatal("前置条件不成立：codearts 不该声明 CapCheckin")
	}
	for _, c := range p.AccountColumns() {
		if c == gateway.AccountColCheckin {
			t.Error("codearts 的列里出现了 checkin —— 它没有签到能力位，那一列会永远是「—」")
		}
		if c == gateway.AccountColToken {
			// 「Token」列读的是 `has_token`/`token_expire_sec`，而 codearts 的
			// accessToken 恒为空（它的凭证是 AK/SK/ST 三元组）→ 那一列也是「—」。
			// codearts 用 token_expiry（走 CredentialExpiryExt）代替。
			t.Error("codearts 的列里出现了 token —— 它的 AccessToken 恒空，" +
				"应当用 token_expiry（走 CredentialExpiryExt）")
		}
	}
}

// TestAccountColumnsFirstIsProvider 钉住"首列与其他上游一致"。
//
// # 为什么这条单独写（而不是靠上面那条全等断言）
//
// 上面那条只钉住"codearts 自己这份列集长什么样"。它挡不住一个**跨上游**的
// 回归：有人为了"清爽"把 provider 从**默认列集**（workbuddy 走的）
// `DefaultAccountColumns()` 里删掉 —— 那时 codearts 这份仍然含有 provider，
// 上一条测试照样绿，而**两张表又不统一了**。
//
// 用户的诉求原文是「和其他上游的表格统一一下第一个列」，
// 所以这条断言比较的正是**两份列集的首列**，而不是某一份的内容。
func TestAccountColumnsFirstIsProvider(t *testing.T) {
	p := NewWithConfig(Config{})

	got := p.AccountColumns()
	if len(got) == 0 {
		t.Fatal("列集为空")
	}
	def := gateway.DefaultAccountColumns()
	if len(def) == 0 {
		t.Fatal("默认列集为空")
	}
	// 默认列集就是**未自报列的上游**（workbuddy）的表头契约 ——
	// 见 gateway.DefaultAccountColumns 的注释。
	if got[0] != def[0] {
		t.Errorf("codearts 首列 = %q，而默认列集（workbuddy）首列 = %q —— "+
			"两张表的第一个格子必须描述同一件事，否则横向看过去对不齐",
			got[0], def[0])
	}
	if got[0] != gateway.AccountColProvider {
		t.Errorf("首列应当是 %q，得到 %q", gateway.AccountColProvider, got[0])
	}
}

// TestAccountColumnsAreKnown 防止自报的列 id 拼错。
//
// 拼错是**静默失效**：前端查不到那个 id 就跳过 → 那一列直接消失，
// 页面上看不出是漏了还是本来没有。所以必须在这里挡住。
func TestAccountColumnsAreKnown(t *testing.T) {
	p := NewWithConfig(Config{})
	for _, c := range p.AccountColumns() {
		if !gateway.IsKnownAccountColumn(c) {
			t.Errorf("列 id %q 不在 gateway 的规范词汇表里 —— 前端会静默跳过它", c)
		}
	}
}

// TestTokenExpiry 钉住三种返回形态。
//
// 重点是后两条：**"没有过期信息"必须与"过期时刻是 0"分开**。
// 混成一个的话，前端会把"未知"渲染成"1970 年就过期了" —— 比不显示更糟。
func TestTokenExpiry(t *testing.T) {
	p := NewWithConfig(Config{})

	t.Run("正常：返回真实秒数", func(t *testing.T) {
		at := time.Now().Add(2 * time.Hour).Unix()
		a := &Auth{AccessKey: "AK", SecretKey: "SK", ExpiresAt: at}
		got, ok := p.TokenExpiry(gateway.Credential{Secret: a})
		if !ok || got != at {
			t.Fatalf("TokenExpiry = (%d, %v)，want (%d, true)", got, ok, at)
		}
	})

	t.Run("ExpiresAt<=0：未知，不是 0", func(t *testing.T) {
		a := &Auth{AccessKey: "AK", SecretKey: "SK", ExpiresAt: 0}
		got, ok := p.TokenExpiry(gateway.Credential{Secret: a})
		if ok {
			t.Errorf("ExpiresAt=0 时 ok=true（got=%d）—— 会把「未知」渲染成「1970 年已过期」", got)
		}
	})

	t.Run("凭证类型不对：未知而不是 panic", func(t *testing.T) {
		got, ok := p.TokenExpiry(gateway.Credential{Secret: "不是 *Auth"})
		if ok || got != 0 {
			t.Errorf("类型不对时应当 (0, false)，得到 (%d, %v)", got, ok)
		}
	})

	t.Run("凭证为空：未知而不是 panic", func(t *testing.T) {
		got, ok := p.TokenExpiry(gateway.Credential{Secret: nil})
		if ok || got != 0 {
			t.Errorf("凭证为空时应当 (0, false)，得到 (%d, %v)", got, ok)
		}
	})
}

// TestTokenExpiryReflectsInPlaceRefresh 钉住"读到的是活值，不是启动快照"。
//
// 这正是上一轮修完的那类 bug 的形态：续期**原地**更新 `*Auth`，而任何
// "启动时投影出来的副本"都会立刻过期。所以 TokenExpiry 必须读同一个对象。
func TestTokenExpiryReflectsInPlaceRefresh(t *testing.T) {
	p := NewWithConfig(Config{})
	a := &Auth{AccessKey: "AK", SecretKey: "SK", ExpiresAt: 1000}
	cred := gateway.Credential{Secret: a}

	if got, _ := p.TokenExpiry(cred); got != 1000 {
		t.Fatalf("前置条件不成立：got=%d", got)
	}

	// 模拟续期原地更新（真实路径见 client.RefreshToken）
	a.Lock()
	a.ExpiresAt = 9999
	a.Unlock()

	if got, ok := p.TokenExpiry(cred); !ok || got != 9999 {
		t.Fatalf("续期后 TokenExpiry = (%d, %v)，want (9999, true) —— "+
			"读到的不是活对象，是快照", got, ok)
	}
}
