package qoder

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// mkToken 造一个带 iat/exp 的 JWT（cline 的 token 形态是 `workos:<jwt>`）。
func mkToken(t *testing.T, iat, exp int64, prefix string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"iat": iat, "exp": exp, "sub": "user_x"})
	if err != nil {
		t.Fatal(err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return prefix + enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

// TestRefreshSkewUsesHalfLifetime RefreshSkew 报的窗口必须让核心在"剩余一半"时触发。
//
// # 这条同时修两类问题
//
//  1. 此前 cline **没有**实现 RefreshSkewExt，核心兜底成
//     `acct.NeedsRefresh(10m)`，而池投影的 ExpiresAt 是 0（字段名不匹配），
//     `NeedsRefresh` 对 0 恒真 → **每个请求都续期**。
//  2. 即使修了那个，固定时长对 1 小时寿命的 token 仍是"已用 83% 才续"。
//
// 现在按比例：**剩余 ≤ 寿命的一半**时续期。
//
// # ⚠ 判据必须走**核心真实的判定式**，不能只看"窗口是不是 0"
//
// 我上一版这条测试的写法是错的：它断言"已用 60% → 窗口 == 0"，
// 那是**按"已用时间"表述**的实现会返回的值。而核心对 `skew <= 0` 的读法是
//
//	if skew <= 0 { return false }   // "不需要提前刷"
//
// 于是"窗口 == 0"在核心眼里是**永不续期** —— 我那次实现整个策略都没生效，
// 而这条测试却是绿的（它把 bug 当成了契约）。
//
// 现在改成：把 RefreshSkew 的返回值喂进核心的三条分支，断言
// "剩余 > 一半 → 不续；剩余 ≤ 一半 → 续"。
func TestRefreshSkewUsesHalfLifetime(t *testing.T) {
	const lifetime = 3600 // 1 小时（实测 cline token 寿命）

	// coreFires 用**核心的判定式**（handler.needsRefreshVia 的分支语义）：
	//   has=false → 回落通用窗口（本用例不涉及）
	//   skew<=0   → false（"不用提前刷"）
	//   skew>0    → now + skew >= exp
	coreFires := func(t *testing.T, p *Provider, exp int64, now time.Time) bool {
		t.Helper()
		skew, has := p.RefreshSkew(gateway.Credential{Secret: &Auth{
			AccessToken: mkToken(t, exp-lifetime, exp, ""),
		}})
		if !has {
			t.Fatal("应能算出窗口")
		}
		if skew <= 0 {
			return false
		}
		return now.Add(skew).Unix() >= exp
	}

	t.Run("剩余 70%（已用 30%）→ 不续期", func(t *testing.T) {
		now := time.Now()
		exp := now.Add(42 * time.Minute).Unix() // 剩余 70%
		if coreFires(t, NewWithConfig(Config{}), exp, now) {
			t.Error("剩余 70% 时不该续期 —— 阈值是「剩余 ≤ 一半」")
		}
	})

	t.Run("剩余 50%（刚好一半）→ 续期", func(t *testing.T) {
		now := time.Now()
		exp := now.Add(30 * time.Minute).Unix()
		if !coreFires(t, NewWithConfig(Config{}), exp, now) {
			t.Error("剩余 50% 时应续期 —— 阈值是「剩余 ≤ 寿命的一半」。" +
				"这条若红，说明返回的窗口不是寿命的一半（核心判 now+window>=exp）")
		}
	})

	t.Run("剩余 20%（已用 80%）→ 续期", func(t *testing.T) {
		now := time.Now()
		exp := now.Add(12 * time.Minute).Unix()
		if !coreFires(t, NewWithConfig(Config{}), exp, now) {
			t.Error("剩余 20% 时应续期")
		}
	})

	t.Run("窗口恒为寿命的一半", func(t *testing.T) {
		// 快照式断言：窗口是 token 的属性，与"现在几点"无关。
		iat := int64(1_000_000)
		p := NewWithConfig(Config{})
		skew, ok := p.RefreshSkew(gateway.Credential{Secret: &Auth{
			AccessToken: mkToken(t, iat, iat+lifetime, ""),
		}})
		if !ok {
			t.Fatal("应能算出窗口")
		}
		if want := time.Duration(lifetime/2) * time.Second; skew != want {
			t.Errorf("窗口 = %v，want %v（寿命 %d 秒的一半）—— "+
				"核心的判据是「剩余 <= window」，所以窗口必须恒等于寿命的一半",
				skew, want, lifetime)
		}
	})
}

// TestRefreshSkewStripsWorkOSPrefix cline 的 `workos:` 前缀不影响时间戳解析。
//
// ⚠ 那个前缀**不可剥离**（剥了即 401），所以本方法必须先剥前缀再解 ——
// 否则 cline 永远算不出比例，退化回兜底窗口。
func TestRefreshSkewStripsWorkOSPrefix(t *testing.T) {
	iat := time.Now().Add(-10 * time.Minute).Unix()
	p := NewWithConfig(Config{})

	withPrefix := p.mustSkew(t, &Auth{AccessToken: mkToken(t, iat, iat+3600, "workos:")})
	withoutPrefix := p.mustSkew(t, &Auth{AccessToken: mkToken(t, iat, iat+3600, "")})
	if withPrefix != withoutPrefix {
		t.Errorf("带 workos: 前缀算出 %v，不带算出 %v —— "+
			"前缀不该影响结果（本方法只读时间戳）", withPrefix, withoutPrefix)
	}
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
	cred := gateway.Credential{Secret: &Auth{AccessToken: mkToken(t, iat, iat+3600, "workos:")}}

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
