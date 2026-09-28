package gateway

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// mkJWT 造一个带 iat/exp 的 JWT（只填 payload，签名段随便 —— 我们不校验签名）。
func mkJWT(t *testing.T, iat, exp int64) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"iat": iat, "exp": exp})
	if err != nil {
		t.Fatal(err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

// TestParseJWTTimesReadsIatAndExp 读 iat / exp。
func TestParseJWTTimesReadsIatAndExp(t *testing.T) {
	tok := mkJWT(t, 1790619013, 1790622613)
	got := ParseJWTTimes(tok)
	if got.IssuedAt != 1790619013 {
		t.Errorf("IssuedAt = %d，want 1790619013", got.IssuedAt)
	}
	if got.ExpiresAt != 1790622613 {
		t.Errorf("ExpiresAt = %d，want 1790622613", got.ExpiresAt)
	}
}

// TestParseJWTTimesStripsSchemePrefix 带前缀的 token 也要能解。
//
// ⚠ cline 的 Authorization 是 `workos:<jwt>`，而那个前缀**不可剥离**
// （剥了即 401）。我们只是读本地时间戳，所以先剥前缀再解 ——
// 不影响真正发出去的凭据。
//
// 变异可检：去掉剥前缀那段 → cline 的 iat 读不到 → 该上游永远不会按比例续期。
func TestParseJWTTimesStripsSchemePrefix(t *testing.T) {
	tok := mkJWT(t, 100, 200)
	for _, prefixed := range []string{"workos:" + tok, "Bearer " + tok} {
		got := ParseJWTTimes(prefixed)
		if got.IssuedAt != 100 || got.ExpiresAt != 200 {
			t.Errorf("带前缀 %q 解析失败：iat=%d exp=%d",
				prefixed[:12], got.IssuedAt, got.ExpiresAt)
		}
	}
}

// TestParseJWTTimesOpaqueTokenReturnsZero 不透明串（非 JWT）返回零值、**不 panic**。
//
// codearts 的 securityToken 就是这种（华为云 STS，1 段）。
// 它必须安静地失败，让调用方回落固定窗口 —— 而不是报错或 panic。
func TestParseJWTTimesOpaqueTokenReturnsZero(t *testing.T) {
	for _, tok := range []string{
		"",
		"hQpjbi1ub3J0aC00AQAABW9IU1RBTT", // 真实 codearts securityToken 前缀形态
		"not-a-jwt",
		"a.b", // 段数够但 payload 不是 base64 JSON
		"....",
	} {
		got := ParseJWTTimes(tok)
		if got.IssuedAt != 0 || got.ExpiresAt != 0 {
			t.Errorf("ParseJWTTimes(%q) = %+v，want 零值", tok, got)
		}
	}
}

// TestNumToUnixHandlesMillis JWT 里写毫秒也认（有些实现会这么干）。
func TestNumToUnixHandlesMillis(t *testing.T) {
	if got := numToUnix(1790619013000); got != 1790619013 {
		t.Errorf("毫秒输入被转成 %d，want 1790619013（秒）", got)
	}
	if got := numToUnix(1790619013); got != 1790619013 {
		t.Errorf("秒输入被转成 %d，want 1790619013", got)
	}
	// 非法值一律 0（不编造）。
	for _, v := range []float64{0, -1, -100} {
		if got := numToUnix(v); got != 0 {
			t.Errorf("numToUnix(%v) = %d，want 0", v, got)
		}
	}
}

// TestRefreshSkewAtRatioMissingHalfReturnsFalse 缺 iat 或 exp → (0, false)。
//
// 判据是 `ok=false`（而不是 (0,true)）：调用方据此回落上游自报的固定窗口。
// 返回 (0,true) 会变成"永远该刷"—— 那正是四个新上游此前的 bug（每请求都续期）。
func TestRefreshSkewAtRatioMissingHalfReturnsFalse(t *testing.T) {
	cases := []struct {
		name string
		tm   JWTTimes
	}{
		{"缺 iat（codearts 的不透明串就是这种）", JWTTimes{ExpiresAt: 2_000_000}},
		{"缺 exp", JWTTimes{IssuedAt: 1_000_000}},
		{"两个都缺", JWTTimes{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skew, ok := RefreshSkewAtRatio(c.tm, 0.5)
			if ok || skew != 0 {
				t.Errorf("= (%v, %v)，want (0, false) —— "+
					"缺一半信息时必须说「算不出」，让调用方回落固定窗口；"+
					"返回 (0,true) 会变成「永远该刷」", skew, ok)
			}
		})
	}
}

// TestRefreshSkewAtRatioInvertedTimesReturnsFalse 签发晚于过期 → (0, false)。
//
// 时钟错乱或字段写反时不能假装算得出：`lifetime <= 0` 会让"一半"变成负数。
// ⚠ 特别不能返回 (0,true) —— 那会退化成"永远该刷"。
func TestRefreshSkewAtRatioInvertedTimesReturnsFalse(t *testing.T) {
	tm := JWTTimes{IssuedAt: 2_000_000, ExpiresAt: 1_000_000}
	skew, ok := RefreshSkewAtRatio(tm, 0.5)
	if ok || skew != 0 {
		t.Errorf("= (%v, %v)，want (0, false)", skew, ok)
	}
	// 相等（寿命 0）同理。
	tm = JWTTimes{IssuedAt: 1_000_000, ExpiresAt: 1_000_000}
	if _, ok := RefreshSkewAtRatio(tm, 0.5); ok {
		t.Error("寿命为 0 时应返回 ok=false")
	}
}

// TestRefreshSkewAtRatioInvalidRatioFallsBack ratio 越界时回落默认值（不 panic）。
func TestRefreshSkewAtRatioInvalidRatioFallsBack(t *testing.T) {
	iat, exp := int64(1_000_000), int64(1_003_600)
	tm := JWTTimes{IssuedAt: iat, ExpiresAt: exp}
	for _, r := range []float64{0, -0.5, 1, 2} {
		skew, ok := RefreshSkewAtRatio(tm, r)
		if !ok {
			t.Errorf("ratio=%v 应回落默认值而不是失败", r)
			continue
		}
		if skew != 1800*time.Second {
			t.Errorf("ratio=%v 的窗口 = %v，want 30m（回落 0.5）", r, skew)
		}
	}
}

// TestRefreshSkewFromToken 一步接入：从真实形态的 token 算出窗口。
func TestRefreshSkewFromToken(t *testing.T) {
	now := time.Now()
	iat := now.Add(-10 * time.Minute).Unix() // 已用 10 分钟
	exp := now.Add(50 * time.Minute).Unix()  // 还剩 50 分钟 → 寿命 1 小时
	tok := mkJWT(t, iat, exp)

	skew, ok := RefreshSkewFromToken(tok)
	if !ok {
		t.Fatal("应能算出")
	}
	// 已用 10/60 = 16.7% → 窗口 ≈ 寿命的一半 = 30 分钟
	// （triggerAt = iat + 30m，距 exp 还有 30 分钟）
	if skew < 29*time.Minute || skew > 31*time.Minute {
		t.Errorf("窗口 = %v，want ≈30m", skew)
	}
}

// TestRefreshSkewFromTokenOpaqueFallsBack 不透明 token → ok=false（回落固定窗口）。
func TestRefreshSkewFromTokenOpaqueFallsBack(t *testing.T) {
	if _, ok := RefreshSkewFromToken("hQpjbi1ub3J0aC00AQAABW9IU1RBTT"); ok {
		t.Error("不透明 token 应返回 ok=false")
	}
}
