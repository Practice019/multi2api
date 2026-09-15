// sync_reload_keep_creds_test.go —— 重复导入保护（用户报 bug）回归测试。
//
// bug 形态：管理台「重载 auths」/OAuth 登录后 reload 传进来的是**投影**
// （只有 UID/Nickname，无 token）。修复前 upsert 无条件 `e.a = a`，
// 池内 workbuddy 账号的 token 被空投影清掉 → has_token=False → 401。
package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestSyncToDirForKeepsExistingCreds 已有凭证的账号被投影同步时不丢 token。
func TestSyncToDirForKeepsExistingCreds(t *testing.T) {
	p := New("")
	// 1. 装载真实凭证（workbuddy 形态：token 在 auth.Auth 上）。
	p.AddFor("workbuddy", &auth.Auth{UID: "wb1", Nickname: "旧昵称", AccessToken: "at-old", RefreshToken: "rt-old"}, nil)
	// 2. reload 投影同步（只有 UID/Nickname，token 为空 —— 管理台重载的真实形态）。
	p.SyncToDirFor("workbuddy", []*auth.Auth{{UID: "wb1", Nickname: "新昵称"}})
	// 3. 池内凭证必须保留（否则 401）。
	a := p.AuthByUID("wb1")
	if a == nil || a.AccessToken != "at-old" || a.RefreshToken != "rt-old" {
		t.Fatalf("投影同步清掉了已有凭证: %+v", a)
	}
	if a.Nickname != "新昵称" {
		t.Errorf("昵称应被投影同步更新: %q", a.Nickname)
	}
}

// TestSyncToDirForOverwritesWhenNewCreds 新同步携带真实凭证时正常覆盖（启动/刷新）。
func TestSyncToDirForOverwritesWhenNewCreds(t *testing.T) {
	p := New("")
	p.AddFor("workbuddy", &auth.Auth{UID: "wb1", AccessToken: "at-old"}, nil)
	p.SyncToDirFor("workbuddy", []*auth.Auth{{UID: "wb1", AccessToken: "at-new", RefreshToken: "rt-new"}})
	a := p.AuthByUID("wb1")
	if a == nil || a.AccessToken != "at-new" {
		t.Fatalf("携带凭证的同步应覆盖旧值: %+v", a)
	}
}

// TestSyncToDirWithSecretsKeepsCreds 带 secret 通道的同步同样不丢 e.a 凭证。
func TestSyncToDirWithSecretsKeepsCreds(t *testing.T) {
	p := New("")
	p.AddFor("wb1", &auth.Auth{UID: "wb1", AccessToken: "at-old"}, nil)
	p.SyncToDirWithSecrets("wb1", []*auth.Auth{{UID: "wb1", Nickname: "n"}}, map[string]any{"wb1": "secret-payload"})
	a := p.AuthByUID("wb1")
	if a == nil || a.AccessToken != "at-old" {
		t.Fatalf("secret 同步不应清掉 e.a 凭证: %+v", a)
	}
	if got, ok := p.SecretOf("wb1"); !ok || got != "secret-payload" {
		t.Errorf("secret 通道应照常更新: %v, %v", got, ok)
	}
}

// TestProjectionAuthHasNoCreds 投影判定：无 token 的 auth 不视为携带凭证。
func TestProjectionAuthHasNoCreds(t *testing.T) {
	if authHasCreds(&auth.Auth{UID: "x", Nickname: "n"}) {
		t.Error("纯投影不应视为携带凭证")
	}
	if !authHasCreds(&auth.Auth{UID: "x", AccessToken: "t"}) {
		t.Error("有 access token 应视为携带凭证")
	}
	if !authHasCreds(&auth.Auth{UID: "x", RefreshToken: "t"}) {
		t.Error("有 refresh token 应视为携带凭证")
	}
}
