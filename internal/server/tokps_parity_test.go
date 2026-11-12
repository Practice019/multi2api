package server

import (
	"testing"
	"time"
)

// TestTokPerSecFieldMatchesFrontend 钉住「终端与网页的 tok/s 是同一个口径」。
//
// # 为什么单独立一条（2026-11-12）
//
// 这两处曾经是**两套算式**：终端 `toks / total`（分子含 prompt 的总处理量、分母含 TTFB），
// 网页 `out / (total − TTFB)`。于是同一条请求在终端打 19255 tok/s、网页端按输出算只有约 1k。
// 用户 2026-11-12 正是看着终端那一列报「每秒 token 是不是有问题」。
//
// 判据用**网页夹具里的同一组数字**（tests/frontend/gen_tokps.js 的 `ok(c[8] === ...)`）。
// ⇒ 只改一边不改另一边，这条会红（这正是它存在的理由）。
func TestTokPerSecFieldMatchesFrontend(t *testing.T) {
	const ms = time.Millisecond
	cases := []struct {
		name string
		mode string
		out  int
		tot  time.Duration
		ttfb time.Duration
		want string
		fx   string
	}{
		{"100 tok / 1.0s → 100（≥10 取整）", "stream", 100, 1000 * ms, 0, "100",
			"gen_tokps.js: c[8] === '100'"},
		{"<10 保留一位小数（15/2.0s = 7.5）", "stream", 15, 2000 * ms, 0, "7.5",
			"gen_tokps.js: fmtTps 的 <10 分支（≥10 才取整）"},
		{"usage 缺失（out<0）→ -", "stream", -1, 1000 * ms, 0, "-",
			"gen_tokps.js: c[8] === '-'"},
		{"out 缺失（0）→ -（不拿总处理量冒充输出）", "stream", 0, 1000 * ms, 0, "-",
			"gen_tokps.js: c[8] === '-'"},
		{"耗时 0 → 0.0（不是 Inf/NaN）", "stream", 50, 0, 0, "0.0",
			"gen_tokps.js: c[8] === '0.0'"},
		{"首字等待已从分母扣除（176/(2.256−1.262)≈177）", "stream", 176, 2256 * ms, 1262 * ms, "177",
			"gen_tokps.js: c[8] === '177'（这条证明分母扣了 TTFB）"},
		{"非流式 → -（没有生成阶段这回事）", "nonstream", 100, 1000 * ms, 0, "-",
			"app.js tokPerSec 的 mode 判据"},
		{"首字即末字（gen=0）→ -", "stream", 100, 1000 * ms, 1000 * ms, "-",
			"app.js: genMs <= 0 ⇒ '-'"},
	}
	for _, c := range cases {
		if got := tokPerSecField(c.mode, c.out, c.tot, c.ttfb); got != c.want {
			t.Errorf("%s：got %q want %q（对照 %s）", c.name, got, c.want, c.fx)
		}
	}
}

// TestFmtTpsGoMatchesFrontendRule 精度也必须一致：≥10 取整、<10 一位小数。
// ⚠ 只对齐算式不精度，同一条请求照样两个数（19255.4 vs 19255）。
func TestFmtTpsGoMatchesFrontendRule(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{100, "100"}, // ≥10：取整（JS String(Math.round(100))）
		{19255.4, "19255"},
		{7.5, "7.5"}, // <10：一位小数
		{3, "3"},     // JS String(Math.round(3)*1) 去掉小数位 ⇒ "3"（不是 "3.0"）
		{9.96, "10"}, // 四舍五入到 10 之后走 ≥10 分支 ⇒ JS 也是 "10"
		{-5, "0"},    // 负值夹到 0（与 fmtTps 的 Math.max(0, v) 一致）
	}
	for _, c := range cases {
		if got := fmtTpsGo(c.in); got != c.want {
			t.Errorf("fmtTpsGo(%v) = %q，want %q（必须与 app.js fmtTps 同规则）", c.in, got, c.want)
		}
	}
}
