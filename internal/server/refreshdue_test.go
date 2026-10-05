package server

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// TestRefreshDueAtPrefersLiveCredential 判据必须优先用**活凭证**的过期时刻。
//
// # 这是用户报障的回归测试（「每个请求都续期」→ raccoon 账号被刷爆）
//
// `acct` 是池投影，而各上游的投影都刻意不抄 ExpiresAt
// （见 raccooncreds.go / clinecreds.go 的「投影纪律」）。实测
// `/admin/accounts` 里 `expires_at` 键**一个上游都没有** ⇒ acct.ExpiresAt=0。
//
// 而 `auth.NeedsRefresh` 对 `ExpiresAt <= 0` **恒返回 true**，于是
// 「该不该刷」对每个上游都恒真 ⇒ 每个 chat 请求先做一次续期往返。
//
// 危害按 refresh_token 形态分两档：
//
//	可重复使用（cline 等）→ 白费一次往返（实测：token 被换了）
//	**一次性**（raccoon / codearts）→ **每请求烧一个 token** → 账号禁用
func TestRefreshDueAtPrefersLiveCredential(t *testing.T) {
	now := time.Now()

	t.Run("活凭证剩 2 小时 → 不刷（池投影是 0 也不能恒真）", func(t *testing.T) {
		acct := &auth.Auth{UID: "u"} // ExpiresAt = 0（池投影的真实形态）
		cred := gateway.Credential{ExpiresAt: now.Add(2 * time.Hour)}
		if refreshDueAt(acct, cred, 10*time.Minute) {
			t.Error("剩 2 小时却判该刷 —— 说明又回落到池投影（ExpiresAt=0 恒真）这条错误路径")
		}
	})

	t.Run("活凭证剩 5 分钟（窗口 10 分钟）→ 刷", func(t *testing.T) {
		acct := &auth.Auth{UID: "u"}
		cred := gateway.Credential{ExpiresAt: now.Add(5 * time.Minute)}
		if !refreshDueAt(acct, cred, 10*time.Minute) {
			t.Error("剩 5 分钟、窗口 10 分钟 → 该刷")
		}
	})

	t.Run("活凭证已过期 → 刷", func(t *testing.T) {
		acct := &auth.Auth{UID: "u"}
		cred := gateway.Credential{ExpiresAt: now.Add(-time.Minute)}
		if !refreshDueAt(acct, cred, 10*time.Minute) {
			t.Error("已过期 → 该刷")
		}
	})

	t.Run("活凭证与池投影都有值时，以**活凭证**为准", func(t *testing.T) {
		// 池投影的陈旧值说"快过期了"，活凭证说"还有 2 小时" → 不该刷。
		acct := &auth.Auth{UID: "u", ExpiresAt: now.Add(1 * time.Minute).Unix()}
		cred := gateway.Credential{ExpiresAt: now.Add(2 * time.Hour)}
		if refreshDueAt(acct, cred, 10*time.Minute) {
			t.Error("应当以活凭证为准（它是过期权威），而不是启动快照的陈旧值")
		}
	})

	t.Run("两边都无过期信息 → 保守地刷（保持既有取舍，本次不改）", func(t *testing.T) {
		acct := &auth.Auth{UID: "u"}
		if !refreshDueAt(acct, gateway.Credential{}, 10*time.Minute) {
			t.Error("完全不知道过期时刻时，既有取舍是保守地刷 —— 本次不该改这个取舍")
		}
	})

	t.Run("acct 为 nil 也不 panic", func(t *testing.T) {
		_ = refreshDueAt(nil, gateway.Credential{}, 10*time.Minute)
	})
}
