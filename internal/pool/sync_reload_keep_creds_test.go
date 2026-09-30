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

// TestPoolEntryKeepsCredsWhenProjectionMeetsRealSecret ★ 钉住用户报的 bug 形态。
//
// # 守的是什么
//
// 两条入池路径（reload / oauth）历史上都做过同一件危险事：
// **把扫描结果投影成 uid+nickname 再交给池**，而真凭证在 secret 通道里。
// 结果池条目 e.a 是空投影，而所有取号点读的都是 e.a：
//
//	AuthByUID / creds() / authOf() → e.a      ← 空 → 「无可用凭证」
//	SecretOf                      → e.secret  ← 有凭证（这几处不读它）
//
// 用户实测表现：号在池里可见、大模型请求也通，但额度探测恒 401、
// 猫猫旅行/成长计划报「无可用凭证」，**重启网关即恢复**
// （启动时 LoadDir 从磁盘读回了真凭证）。
//
// 修法在**入池前**（admin.poolAuthsFromCreds：secret 是 *auth.Auth 时直接采用它），
// 而不是改 pool 的通用语义（AuthByUID 的契约"无凭证时 RefreshToken 为空"
// 是既有约定，admin 的 credential_token_test 正是钉它的）。
//
// # 本用例锁的是**兜底**这一层
//
// 即便调用方传了投影，只要 secret 是真凭证，池也必须让取号点拿到凭证 ——
// 否则"刷新走 secret、取号走 e.a"两条路会永久分叉。
//
// 变异：把 upsertSecretLocked 里那段"e.a 无凭证时采用 secret"删掉 → 本用例必红。
func TestPoolEntryKeepsCredsWhenProjectionMeetsRealSecret(t *testing.T) {
	p := New("")

	// ⚠ 刻意模仿**危险接线**：auths 传投影（uid+nickname），secret 传真凭证。
	projection := &auth.Auth{UID: "wb-1", Nickname: "妖精七七", FilePath: "auths/workbuddy/workbuddy-wb-1.json"}
	real := &auth.Auth{
		UID: "wb-1", Nickname: "妖精七七",
		AccessToken: "AT-REAL", RefreshToken: "RT-REAL",
	}
	p.SyncToDirWithSecrets("workbuddy", []*auth.Auth{projection}, map[string]any{"wb-1": real})

	a := p.AuthByUID("wb-1")
	if a == nil {
		t.Fatal("账号应已进池")
	}
	if a.RefreshToken == "" {
		t.Fatalf("❌ 取号点读到的 e.a.RefreshToken 为空 —— workbuddy 的 creds()/authOf()\n"+
			"会判定「无可用凭证」，而额度刷新/签到走 SecretOf 却正常，\n"+
			"于是表现为『凭据明明能用、部分功能却失效』，且重启才恢复。\n"+
			"实际: %+v", a)
	}
	if a.AccessToken != "AT-REAL" {
		t.Errorf("e.a 应采用 secret 里的真凭证，实际 AccessToken=%q", a.AccessToken)
	}

	// 二次同步（仍是投影）也不能把凭证弄丢
	p.SyncToDirWithSecrets("workbuddy", []*auth.Auth{projection}, map[string]any{"wb-1": real})
	if a = p.AuthByUID("wb-1"); a == nil || a.RefreshToken != "RT-REAL" {
		t.Fatalf("二次同步后凭证丢失: %+v", a)
	}
}
