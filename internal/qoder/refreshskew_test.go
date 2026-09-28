package qoder

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// mkToken 造一个带 iat/exp 的 JWT。
func mkToken(t *testing.T, iat, exp int64, prefix string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"iat": iat, "exp": exp, "sub": "user_x"})
	if err != nil {
		t.Fatal(err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return prefix + enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

// TestRefreshSkewUsesHalfLifetime 已用 ≥ 50% 就报"该续期"。
//
// # 这条同时修两类问题
//
//  1. 此前 cline **没有**实现 RefreshSkewExt，核心兜底成
//     `acct.NeedsRefresh(10m)`，而池投影的 ExpiresAt 是 0（字段名不匹配），
//     `NeedsRefresh` 对 0 恒真 → **每个请求都续期**。
//  2. 即使修了那个，固定时长对 1 小时寿命的 token 仍是"已用 83% 才续"。
//
// 现在按比例：剩一半寿命时触发。
//
// 变异可检：把 RefreshSkew 改回 `return 0, true`（= mimo/trae 的"不提前刷"）
// → 本用例的第一条子断言红（它就变成"永远该刷"了）。
func TestRefreshSkewUsesHalfLifetime(t *testing.T) {
	const lifetime = 3600 // 1 小时（实测 cline token 寿命）
	iat := time.Now().Add(-10 * time.Minute).Unix()

	t.Run("刚签发（已用 17%）→ 不报立即续期", func(t *testing.T) {
		p := NewWithConfig(Config{})
		cred := gateway.Credential{Secret: &Auth{
			AccessToken: mkToken(t, iat, iat+lifetime, ""),
		}}
		skew, ok := p.RefreshSkew(cred)
		if !ok {
			t.Fatal("应能算出窗口")
		}
		if skew == 0 {
			t.Error("已用 17% 时不该判「立即续期」(0) —— " +
				"那等于每个请求都续期（修复前的形态）")
		}
	})

	t.Run("已用 60% → 立即续期", func(t *testing.T) {
		// 签发于 36 分钟前（60% of 1h）
		oldIat := time.Now().Add(-36 * time.Minute).Unix()
		p := NewWithConfig(Config{})
		cred := gateway.Credential{Secret: &Auth{
			AccessToken: mkToken(t, oldIat, oldIat+lifetime, ""),
		}}
		skew, ok := p.RefreshSkew(cred)
		if !ok {
			t.Fatal("应能算出窗口")
		}
		if skew != 0 {
			t.Errorf("已用 60%% 时应判「立即续期」，得到窗口 %v —— "+
				"阈值是 50%%，过了就该续", skew)
		}
	})
}

// mustSkew 调 RefreshSkew 并要求成功。
func (p *Provider) mustSkew(t *testing.T, a *Auth) time.Duration {
	t.Helper()
	skew, ok := p.RefreshSkew(gateway.Credential{Secret: a})
	if !ok {
		t.Fatal("应能算出窗口")
	}
	return skew
}

// TestRefreshSkewOpaqueTokenFallsBack 解不出 iat/exp 时回落通用窗口（ok=false）。
//
// ⚠ 必须是 `(0, false)` 而不是 `(0, true)`：
// 后者是"永远该刷"，会让核心对每个请求都调续期。
func TestRefreshSkewOpaqueTokenFallsBack(t *testing.T) {
	p := NewWithConfig(Config{})
	skew, ok := p.RefreshSkew(gateway.Credential{Secret: &Auth{AccessToken: "not-a-jwt"}})
	if ok || skew != 0 {
		t.Errorf("= (%v, %v)，want (0, false) —— 不透明 token 应回落通用窗口；"+
			"(0,true) 会退化成「每请求都续期」", skew, ok)
	}
}

// TestRefreshSkewWrongCredentialTypeFallsBack 凭证类型不对时回落（不 panic）。
func TestRefreshSkewWrongCredentialTypeFallsBack(t *testing.T) {
	p := NewWithConfig(Config{})
	if _, ok := p.RefreshSkew(gateway.Credential{Secret: "not-an-auth"}); ok {
		t.Error("类型不对时应 ok=false，而不是假装算得出")
	}
	// 空 Secret 也不能 panic。
	if _, ok := p.RefreshSkew(gateway.Credential{}); ok {
		t.Error("空 Secret 应 ok=false")
	}
}

// TestRefreshSkewSkipsNetwork RefreshSkew **不发网络请求**。
//
// 契约要求纯本地判断：它在出站循环的每次请求上被调用（每个候选账号一次）。
// 若这里发了请求，等于每个请求都多一次往返。
//
// 判据：用一个必然失败的基址构造客户端；若本方法真发请求会超时/报错，
// 而它应当**瞬间返回**。
func TestRefreshSkewSkipsNetwork(t *testing.T) {
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1")})
	iat := time.Now().Add(-1 * time.Minute).Unix()
	cred := gateway.Credential{Secret: &Auth{AccessToken: mkToken(t, iat, iat+3600, "")}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.RefreshSkew(cred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshSkew 超过 2 秒 —— 它不该发网络请求（每次请求都被调用一次）")
	}
}
