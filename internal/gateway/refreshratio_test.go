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

// TestRefreshSkewAtRatioHalfLifetime 50% 阈值 = 剩一半寿命时触发。
//
// 这是用户要求的统一策略，用例把"一半"这个语义钉死。
func TestRefreshSkewAtRatioHalfLifetime(t *testing.T) {
	// 寿命 3600 秒（1 小时，与真实 cline token 同量级）
	iat, exp := int64(1_000_000), int64(1_003_600)
	tm := JWTTimes{IssuedAt: iat, ExpiresAt: exp}

	// 刚签发（已用 0%）→ 窗口 = 寿命的一半 = 1800 秒
	skew, ok := RefreshSkewAtRatio(tm, 0.5, time.Unix(iat, 0))
	if !ok {
		t.Fatal("应能算出窗口")
	}
	if skew != 1800*time.Second {
		t.Errorf("刚签发时窗口 = %v，want 30m（寿命的一半）", skew)
	}

	// 已用 49.9% → 还没到阈值，窗口是剩下的 1 秒多
	skew, ok = RefreshSkewAtRatio(tm, 0.5, time.Unix(iat+1796, 0))
	if !ok || skew <= 0 {
		t.Errorf("已用 49.9%% 时应还有正窗口，得到 %v (ok=%v)", skew, ok)
	}

	// 已用 50%（恰好在阈值）→ (0, true) 表示"现在就续"
	skew, ok = RefreshSkewAtRatio(tm, 0.5, time.Unix(iat+1800, 0))
	if !ok || skew != 0 {
		t.Errorf("已用 50%% 时 = (%v, %v)，want (0, true) 立即续期", skew, ok)
	}

	// 已用 80% → 早过了阈值，仍是"现在就续"
	skew, ok = RefreshSkewAtRatio(tm, 0.5, time.Unix(iat+2880, 0))
	if !ok || skew != 0 {
		t.Errorf("已用 80%% 时 = (%v, %v)，want (0, true)", skew, ok)
	}
}

// TestRefreshSkewRatioIsLifetimeIndependent 同一比例对不同寿命语义一致。
//
// # 这正是"统一"的意义
//
// 固定时长窗口的问题是"提前 10 分钟"对 1 小时 token 和 55 天 token
// 意义完全不同。比例判据下两者都是"用掉一半就换"。
//
// ⚠ 期望值是**寿命的一半**，不是"剩余的 25%" —— 我第一版写错过：
//
//	窗口 = exp − triggerAt，triggerAt = iat + 寿命×0.5
//	     = (iat + 寿命) − (iat + 0.5×寿命) = 0.5×寿命
//
// 它是"距过期还有多久时触发"，与"现在已用多少"**无关**。
// 已用 25% 只是说明"还没到阈值"，窗口本身是固定的半个寿命。
func TestRefreshSkewRatioIsLifetimeIndependent(t *testing.T) {
	now := int64(5_000_000)
	cases := []struct {
		name     string
		lifetime int64 // 秒
	}{
		{"1 小时（cline 量级）", 3600},
		{"2 小时（codearts 量级）", 7200},
		{"55 天（workbuddy 量级）", 4_752_000},
		{"10 分钟", 600},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 把 now 放在寿命的 25% 处（还没到 50% 阈值）
			iat := now - c.lifetime/4
			tm := JWTTimes{IssuedAt: iat, ExpiresAt: iat + c.lifetime}
			skew, ok := RefreshSkewAtRatio(tm, 0.5, time.Unix(now, 0))
			if !ok {
				t.Fatal("应能算出")
			}
			// 窗口 = 寿命的一半（与寿命成正比 —— 这就是"比例"的含义）
			want := time.Duration(c.lifetime/2) * time.Second
			if skew != want {
				t.Errorf("窗口 = %v，want %v（寿命 %d 秒的一半）", skew, want, c.lifetime)
			}
		})
	}
}

// TestRefreshSkewTriggersAtHalfLifetimeForAnyLifetime 触发点恒在寿命中点。
//
// 把"已用一半"这个语义对每种寿命都验证一次（不只看窗口时长）。
func TestRefreshSkewTriggersAtHalfLifetimeForAnyLifetime(t *testing.T) {
	for _, lifetime := range []int64{600, 3600, 7200, 4_752_000} {
		iat := int64(1_000_000)
		tm := JWTTimes{IssuedAt: iat, ExpiresAt: iat + lifetime}
		half := iat + lifetime/2

		// 中点前 1 秒 → 还没到，仍有正窗口
		if skew, ok := RefreshSkewAtRatio(tm, 0.5, time.Unix(half-1, 0)); !ok || skew <= 0 {
			t.Errorf("寿命 %d：中点前应是正窗口，得到 (%v, %v)", lifetime, skew, ok)
		}
		// 中点 → 立即续期
		if skew, ok := RefreshSkewAtRatio(tm, 0.5, time.Unix(half, 0)); !ok || skew != 0 {
			t.Errorf("寿命 %d：中点应触发续期（0,true），得到 (%v, %v)", lifetime, skew, ok)
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
			skew, ok := RefreshSkewAtRatio(c.tm, 0.5, time.Unix(1_500_000, 0))
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
	skew, ok := RefreshSkewAtRatio(tm, 0.5, time.Unix(1_500_000, 0))
	if ok || skew != 0 {
		t.Errorf("= (%v, %v)，want (0, false)", skew, ok)
	}
	// 相等（寿命 0）同理。
	tm = JWTTimes{IssuedAt: 1_000_000, ExpiresAt: 1_000_000}
	if _, ok := RefreshSkewAtRatio(tm, 0.5, time.Unix(1_000_000, 0)); ok {
		t.Error("寿命为 0 时应返回 ok=false")
	}
}

// TestRefreshSkewAtRatioInvalidRatioFallsBack ratio 越界时回落默认值（不 panic）。
func TestRefreshSkewAtRatioInvalidRatioFallsBack(t *testing.T) {
	iat, exp := int64(1_000_000), int64(1_003_600)
	tm := JWTTimes{IssuedAt: iat, ExpiresAt: exp}
	for _, r := range []float64{0, -0.5, 1, 2} {
		skew, ok := RefreshSkewAtRatio(tm, r, time.Unix(iat, 0))
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
