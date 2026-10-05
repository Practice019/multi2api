package gateway

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// jwtWith 造一个指定字段的 JWT（不签名 —— 本函数只读时间戳）。
func jwtWith(t *testing.T, fields map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"HS256"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

// TestParseJWTTimesFallsBackToNbf 缺 `iat` 时用 `nbf` 当寿命起点。
//
// # 这是 raccoon 的回归测试（实测的真缺陷）
//
// raccoon 的 access_token **没有 `iat`，只有 `nbf`**：
//
//	{"exp":1791151395,"iss":"726734","nbf":1791140590,…}
//
// 抽样本仓全部上游，**只有 raccoon** 是这种形态
// （cline / codearts / lobsterai / trae / workbuddy 都有 iat）。
//
// 只读 `iat` 的后果：IssuedAt=0 → RefreshSkewAtRatio 要求两者都 >0
// → 返回 (0,false) → raccoon 的 RefreshSkewExt **整个空转**
// → 核心回落到通用兜底窗口 → 配合池投影 ExpiresAt=0 → 每请求都续期。
//
// ⚠ 而 raccoon 的 refresh_token 是**一次性**的 —— 每请求续期 =
// 每请求烧一个 token，很快耗尽。这正是用户报障的机制。
func TestParseJWTTimesFallsBackToNbf(t *testing.T) {
	const nbf = 1_791_140_590
	const exp = 1_791_151_395

	t.Run("只有 nbf（raccoon 的真实形态）", func(t *testing.T) {
		got := ParseJWTTimes(jwtWith(t, map[string]any{"nbf": nbf, "exp": exp}))
		if got.IssuedAt != nbf {
			t.Errorf("IssuedAt = %d，want %d（nbf 回退）—— "+
				"不回退的话 raccoon 永远算不出比例窗口", got.IssuedAt, nbf)
		}
		if got.ExpiresAt != exp {
			t.Errorf("ExpiresAt = %d，want %d", got.ExpiresAt, exp)
		}
	})

	t.Run("iat 优先于 nbf（不改既有上游行为）", func(t *testing.T) {
		const iat = 1_791_140_000
		got := ParseJWTTimes(jwtWith(t, map[string]any{"iat": iat, "nbf": nbf, "exp": exp}))
		if got.IssuedAt != iat {
			t.Errorf("IssuedAt = %d，want %d —— 有 iat 时必须以它为准", got.IssuedAt, iat)
		}
	})

	t.Run("两个都没有 → 0（调用方回落）", func(t *testing.T) {
		got := ParseJWTTimes(jwtWith(t, map[string]any{"exp": exp}))
		if got.IssuedAt != 0 {
			t.Errorf("IssuedAt = %d，want 0", got.IssuedAt)
		}
	})
}

// TestRefreshSkewWorksForNbfOnlyToken 端到端：raccoon 形态的 token 必须能算出窗口。
//
// 这条把"回退"与"扩展点真的生效"连起来断言 —— 只测 ParseJWTTimes
// 无法证明 RefreshSkew 不再空转。
func TestRefreshSkewWorksForNbfOnlyToken(t *testing.T) {
	now := time.Now()
	const lifetime = 3 * 3600 // 实测 raccoon token 寿命约 3 小时
	nbf := now.Add(-40 * time.Minute).Unix()
	exp := nbf + lifetime

	tok := jwtWith(t, map[string]any{"nbf": nbf, "exp": exp})
	skew, ok := RefreshSkewFromToken(tok)
	if !ok {
		t.Fatal("raccoon 形态的 token 算不出窗口 —— RefreshSkewExt 会整个空转，" +
			"核心于是回落兜底窗口，配合池投影 ExpiresAt=0 变成每请求都续期")
	}
	if want := time.Duration(lifetime/2) * time.Second; skew != want {
		t.Errorf("窗口 = %v，want %v（寿命 %d 秒的一半）", skew, want, lifetime)
	}
}
