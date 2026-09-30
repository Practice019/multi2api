package gateway

import (
	"testing"
	"time"
)

// TestRatioWindowActuallyFiresAtHalf — 按比例算出的窗口**真的**能让核心触发续期。
//
// # 这条测试是为一个我自己造成的真实 bug 补的
//
// 我第一版实现是这样的：
//
//	triggerAt = iat + 寿命×0.5
//	window    = exp - triggerAt        // = 寿命的一半
//	if now >= triggerAt { return 0, true }   // ← 这一句是错的
//	return window, true
//
// 看起来没问题，但**核心对 `skew <= 0` 的解释完全不同**：
//
//	if skew <= 0 {
//	    // 上游明确声明"不需要提前刷"（只在 401 后被动续期）。
//	    return false        // ← 返回 0 被读成"永不主动续期"
//	}
//	return acct.NeedsRefresh(skew)
//
// 所以"已用 ≥ 50% 时返回 (0,true)"的实际效果是：
// **恰好在该续期的那一刻，核心收到了"不需要续期"** ——
// 整个比例策略退化成 trae 那种"只在 401 后被动续"，
// 与"按寿命比例提前续期"完全相反。
//
// 本测试把 `RefreshSkew` 的返回值喂进**核心真实的判定式**
// （handler.needsRefreshVia 的三条分支），断言在该续期的时刻它真的返回 true。
//
// 变异可检：把 `return window, true` 改成"过了触发点就 return 0, true"
// → 第二条子用例红。
func TestRatioWindowActuallyFiresAtHalf(t *testing.T) {
	const lifetime = 3600 // 1 小时（实测 cline token 寿命量级）
	iat := int64(1_000_000)
	exp := iat + lifetime
	tm := JWTTimes{IssuedAt: iat, ExpiresAt: exp}

	// needsRefreshVia 的核心判定（照抄 handler.go 的三条分支语义）：
	//   has=false        → 回落通用窗口（这里不涉及）
	//   skew<=0          → false（"不用提前刷"）
	//   skew>0           → now + skew >= expiresAt
	firesAt := func(nowUnix int64) bool {
		skew, has := RefreshSkewAtRatio(tm, DefaultRefreshRatio)
		if !has {
			t.Fatal("应能算出窗口")
		}
		if skew <= 0 {
			return false // ← 核心就是这么读的
		}
		return time.Unix(nowUnix, 0).Add(skew).Unix() >= exp
	}

	cases := []struct {
		name     string
		elapsed  float64 // 已用比例
		wantFire bool
	}{
		{"已用 0%", 0, false},
		{"已用 17%（真实 cline 刚签发）", 0.17, false},
		{"已用 40%", 0.40, false},
		{"已用 49%", 0.49, false},
		{"已用 50%（刚好一半）", 0.50, true},
		{"已用 60%", 0.60, true},
		{"已用 90%", 0.90, true},
		{"已用 99%", 0.99, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := iat + int64(float64(lifetime)*c.elapsed)
			if got := firesAt(now); got != c.wantFire {
				t.Errorf("已用 %.0f%% → 核心判定 = %v，want %v。"+
					"阈值是「剩余 ≤ 寿命的一半」，即已用 ≥ 50%%。"+
					"若这里不对，整个比例策略要么永不触发、要么过早触发。",
					c.elapsed*100, got, c.wantFire)
			}
		})
	}
}

// TestRatioWindowIsLifetimeHalf 返回的窗口恒等于寿命的一半。
//
// # 为什么窗口必须是**常量**（这是用户指出的正确表述）
//
// 核心的判定是 `now + window >= exp`，等价于 **`剩余 <= window`**。
// 所以"剩余 ≤ 寿命的一半"这个策略，**只需要返回 window = 寿命×0.5**：
//
//	剩余 <= 寿命/2   ⟺   已用 >= 寿命/2
//
// 两种说法**数学等价**（已用 + 剩余 = 寿命），但**按"剩余"表述的实现
// 不需要读 now** —— 于是没有"什么时候该返回 0"这种会出错的分支。
//
// 我第一版读 now 并在"过了触发点"时返回 0，正是因此踩了核心契约的坑
// （见上一条测试）。用户指出"应该按剩余时间算"是对的。
func TestRatioWindowIsLifetimeHalf(t *testing.T) {
	const lifetime = 3600
	iat := int64(1_000_000)
	tm := JWTTimes{IssuedAt: iat, ExpiresAt: iat + lifetime}

	// 无论"现在"是什么时刻，窗口都必须是寿命的一半 ——
	// 它是**token 的属性**，与当下时间无关。
	//
	// ⚠ 函数签名里已经没有 now 参数了（这正是修掉那个 bug 的方式）：
	// 一旦能读 now，就会有人写回"过了触发点就 return 0"那条错误分支。
	for _, elapsed := range []float64{0, 0.1, 0.5, 0.9, 0.99} {
		skew, ok := RefreshSkewAtRatio(tm, 0.5)
		if !ok {
			t.Fatalf("已用 %.0f%%：应能算出窗口", elapsed*100)
		}
		if skew != lifetime/2*time.Second {
			t.Errorf("已用 %.0f%% 时窗口 = %v，want %v（寿命的一半，恒定）—— "+
				"窗口若随时间变化，核心的 `剩余 <= window` 判据就不再是「剩余 ≤ 一半」",
				elapsed*100, skew, lifetime/2*time.Second)
		}
	}
}

// TestRatioOtherRatios 其它比例（0.25 / 0.8）也要落在正确的触发点。
//
// 用户要求的是 50%，但实现支持任意比例 —— 把它一并钉住，
// 免得将来改比例时又踩"返回 0"的坑。
func TestRatioOtherRatios(t *testing.T) {
	const lifetime = 10000
	iat := int64(1_000_000)
	exp := iat + lifetime
	tm := JWTTimes{IssuedAt: iat, ExpiresAt: exp}

	for _, ratio := range []float64{0.25, 0.5, 0.8} {
		skew, ok := RefreshSkewAtRatio(tm, ratio)
		if !ok {
			t.Fatalf("ratio=%v 应能算出", ratio)
		}
		// ⚠ 期望值必须用**同样的整数四舍五入**算，不能用
		// `time.Duration(float64(lifetime)*(1-ratio))` ——
		// 后者会把 0.8 算成 1999.99… 再截断成 1999（少 1 秒），
		// 于是断言红而**实现是对的**。我第一版就是这么写的，
		// 教训：期望值算式与实现算式不一致时，红的不一定是实现。
		wantSec := (lifetime*int64((1-ratio)*1_000_000) + 500_000) / 1_000_000
		want := time.Duration(wantSec) * time.Second
		if skew != want {
			t.Errorf("ratio=%v 窗口 = %v，want %v（寿命的 %.0f%%）",
				ratio, skew, want, (1-ratio)*100)
		}
		// 触发点验证：剩余恰好 = 窗口 时应当触发。
		triggerNow := exp - int64(skew/time.Second)
		if time.Unix(triggerNow, 0).Add(skew).Unix() < exp {
			t.Errorf("ratio=%v：剩余 = 窗口 时应触发", ratio)
		}
		// 剩余比窗口多 1 秒 → 不该触发。
		beforeNow := triggerNow - 1
		if time.Unix(beforeNow, 0).Add(skew).Unix() >= exp {
			t.Errorf("ratio=%v：剩余 > 窗口 时不该触发", ratio)
		}
	}
}
