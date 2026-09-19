// credential_token_test.go —— 「has_token 的第二个来源」契约测试。
//
// # 守的是哪件事
//
// 账号列表的 `has_token` 原来只读核心投影 `Pool.AuthByUID()`（`*auth.Auth`）。
// 但按上游分子目录之后，管理台两条入池路径（accountsReload / pollViaFlow）
// 交给池子的都是**裸投影**（只有 UID/Nickname），真凭证走池的不透明 secret 通道：
//
//	auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname})
//
// 于是第二个实例（workbuddy-intl）的账号投影里永远没有 token：
//
//	workbuddy-intl: has_token=false   ← 用户实测（secret 里明明有 accessToken）
//	workbuddy:      has_token=true    ← 默认上游，投影里就带着 token
//
// 用户据此以为"登录没拿到 token"，而凭证完全正常。
//
// # 为什么每条都要有反例桩（与 account_columns_test.go 同一条理由）
//
// 若把判据写成"永远给 true"，"实现了的样本"那条断言照样绿 —— 那样的守卫是装饰品。
// 所以这里配了"没实现扩展点"与"投影里已经有 token"两种桩，断言它们行为不同。
package admin

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// tokenStub 实现了 CredentialTokenExt。
//
// 行为刻意做成"从 secret 里读" —— 与真实上游（workbuddy 断言回 *auth.Auth）
// 同构：它证明**核心不需要认识 secret 的类型**，只是把它原样交回给上游。
type tokenStub struct {
	stubProvider
	// hasToken 是被问到时返回的结论（false 表示"这份凭证没有可用 token"）。
	hasToken bool
}

func (s *tokenStub) HasToken(cred gateway.Credential) bool { return s.hasToken }

// TestAccountViewsFillsHasTokenFromProvider ★ 本条在修复前必红。
//
// 形态与 workbuddy-intl 完全一致：入池时是**裸投影**（UID/Nickname），
// 真凭证在 secret 里。修复前 has_token 恒为 false（界面显示 `—`）。
func TestAccountViewsFillsHasTokenFromProvider(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&tokenStub{
		stubProvider: stubProvider{id: "wbintl", caps: gateway.CapChat},
		hasToken:     true,
	}); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	// ⚠ 裸投影 + secret —— 这正是 pollViaFlow / accountsReload 入池的形状。
	p.AddFor("wbintl", &auth.Auth{UID: "wb-1", Nickname: "uxjxxx"}, &auth.Auth{
		UID: "wb-1", AccessToken: "real-access-token", ExpiresAt: 1821328439,
	})

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "wbintl"}))[0]

	// 前置条件：投影里确实没有 token（否则这条测试没测到东西）。
	if v.Status.UID != "wb-1" {
		t.Fatalf("uid = %q，want wb-1", v.Status.UID)
	}
	if p.AuthByUID("wb-1") == nil || p.AuthByUID("wb-1").AccessToken != "" {
		t.Fatal("前置条件不成立：这个桩的通用投影不该带 token")
	}

	if !v.HasToken {
		t.Fatal("★ 裸投影入池的账号 has_token=false —— " +
			"界面上那一列永远是「—」，用户会以为登录没拿到 token，" +
			"而 secret 里的 accessToken 明明是好的（workbuddy-intl 实测）")
	}
}

// TestAccountViewsHasTokenUnknownWhenProviderSilent 上游不实现扩展点时**逐字节不变**。
//
// 这是防"确定性假象"：不实现 = 行为与改造前完全一致（false → `—`），
// 绝不因为"核心自己猜"而翻成 true。
func TestAccountViewsHasTokenUnknownWhenProviderSilent(t *testing.T) {
	reg := gateway.NewRegistry()
	// plainStub 不实现 CredentialTokenExt。
	if err := reg.Register(&plainStub{
		stubProvider: stubProvider{id: "carts", caps: gateway.CapChat},
	}); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	p.AddFor("carts", &auth.Auth{UID: "ca-1"}, "不透明的 secret")

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "carts"}))[0]
	if v.HasToken {
		t.Error("上游没实现 CredentialTokenExt 时 has_token 不该翻成 true —— " +
			"那是核心在猜，而不是上游在答")
	}
}

// TestAccountViewsHasTokenPrefersProjection 投影里已经有 token 时**不问上游**。
//
// 顺序不能反：默认上游（workbuddy）的 token 一直在通用投影里。
// 若先问上游，而某个上游的 HasToken 恰好返回 false，
// workbuddy 的 has_token 会被一个错误的否定覆盖 —— 那是可见回归。
func TestAccountViewsHasTokenPrefersProjection(t *testing.T) {
	reg := gateway.NewRegistry()
	// 桩刻意说"没有 token"。
	if err := reg.Register(&tokenStub{
		stubProvider: stubProvider{id: "wb", caps: gateway.CapChat},
		hasToken:     false,
	}); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	// 投影里**带着** token —— 启动时经 LoadDir 装载的形态。
	p.AddFor("wb", &auth.Auth{UID: "wb-1", AccessToken: "from-projection"}, nil)

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "wb"}))[0]
	if !v.HasToken {
		t.Error("投影里有 token 时 has_token 必须为 true —— " +
			"先问上游会把默认上游的 true 覆盖成 false")
	}
}
