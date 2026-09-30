package qoder

import (
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestTokenExpiryFallsBackToJWT 没有 expire_time 时回落 token 的 `exp`。
//
// 覆盖"手工导入、只带了 JWT 没有 expire_time"的凭证。
func TestTokenExpiryFallsBackToJWT(t *testing.T) {
	p := NewWithConfig(Config{})
	exp := time.Now().Add(30 * time.Minute).Unix()
	cred := gateway.Credential{Secret: &Auth{
		AccessToken: mkToken(t, exp-3600, exp, ""),
	}}
	at, ok := p.TokenExpiry(cred)
	if !ok {
		t.Fatal("应能从 JWT 的 exp 回落到期时刻")
	}
	if at != exp {
		t.Errorf("过期时刻 = %d，want %d（JWT 的 exp）", at, exp)
	}
}

// TestTokenExpiryUnknownReturnsFalse 无过期信息时 `(0, false)`，**不编造 0**。
//
// ⚠ 这是契约里最要紧的一条：`(0, true)` 会被前端渲染成
// "1970 年已过期"，比显示 `—` 更糟（本项目实测过这个形态）。
//
// 变异可检：把 `return 0, false` 改成 `return 0, true` → 本用例红。
func TestTokenExpiryUnknownReturnsFalse(t *testing.T) {
	p := NewWithConfig(Config{})
	cases := []struct {
		name string
		cred gateway.Credential
	}{
		{"不透明 token，无 expire_time", gateway.Credential{Secret: &Auth{AccessToken: "opaque"}}},
		{"空凭证", gateway.Credential{Secret: &Auth{}}},
		{"Secret 类型不对", gateway.Credential{Secret: "not-auth"}},
		{"Secret 为 nil", gateway.Credential{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at, ok := p.TokenExpiry(c.cred)
			if ok {
				t.Errorf("= (%d, true)，want (0, false) —— "+
					"不知道时必须说不知道；编造 0 会被渲染成「1970 年已过期」", at)
			}
			if at != 0 {
				t.Errorf("ok=false 时 at 应为 0，得到 %d", at)
			}
		})
	}
}

// TestTokenExpirySkipsNetwork 纯本地判断，不发网络。
//
// 它在账号列表渲染路径上被调用（每个账号一次）。
func TestTokenExpirySkipsNetwork(t *testing.T) {
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1")})
	cred := gateway.Credential{Secret: &Auth{AccessToken: "opaque"}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.TokenExpiry(cred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TokenExpiry 超过 2 秒 —— 它不该发网络请求")
	}
}
