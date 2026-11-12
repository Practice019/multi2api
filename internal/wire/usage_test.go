package wire

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 全字段齐全的显式键形态。
func TestParseUsageExtrasExplicitKeys(t *testing.T) {
	got := ParseUsageExtras(map[string]any{
		"completion_tokens":          100.0,
		"credit":                     1.25,
		"completion_thinking_tokens": 42.0,
		"prompt_cache_hit_tokens":    64.0,
		"prompt_cache_miss_tokens":   36.0,
	})
	want := UsageExtras{Credit: 1.25, ThinkTokens: 42, CacheHitTokens: 64, CacheMissTokens: 36, CompletionTokens: 100}
	if got != want {
		t.Errorf("显式键: got %+v want %+v", got, want)
	}
}

// 思考 token 的变体：completion_tokens_details.reasoning_tokens。
func TestParseUsageExtrasThinkingFromDetails(t *testing.T) {
	got := ParseUsageExtras(map[string]any{
		"completion_tokens_details": map[string]any{"reasoning_tokens": 77.0},
	})
	if got.ThinkTokens != 77 {
		t.Errorf("details 变体: ThinkTokens=%d want 77", got.ThinkTokens)
	}
}

// 缓存命中的变体：cached_tokens。
func TestParseUsageExtrasCacheHitFromCachedTokens(t *testing.T) {
	got := ParseUsageExtras(map[string]any{"cached_tokens": 512.0})
	if got.CacheHitTokens != 512 {
		t.Errorf("cached_tokens 变体: CacheHitTokens=%d want 512", got.CacheHitTokens)
	}
}

// 两个变体同时存在时，显式键优先。
func TestParseUsageExtrasPrefersExplicitVariant(t *testing.T) {
	got := ParseUsageExtras(map[string]any{
		"completion_thinking_tokens": 42.0,
		"completion_tokens_details":  map[string]any{"reasoning_tokens": 77.0},
		"prompt_cache_hit_tokens":    64.0,
		"cached_tokens":              512.0,
	})
	if got.ThinkTokens != 42 {
		t.Errorf("显式 thinking 键应优先: got %d want 42", got.ThinkTokens)
	}
	if got.CacheHitTokens != 64 {
		t.Errorf("显式 cache hit 键应优先: got %d want 64", got.CacheHitTokens)
	}
}

// 显式键存在但为 0 时，允许回落到变体（0 视为「未提供」而非「明确为零」）。
// 回退的判据是「键不存在」，不是「值为 0」。
//
// 这是评审指出的真缺陷：曾写成 `if primary == 0 { 用 fallback }`，于是
// 「合法地为 0」被当作「缺失」。真实数据里 prompt_cache_hit_tokens 与
// cached_tokens **同时存在且同值**，一旦上游偶发不一致（或混流下两键来自
// 不同帧），真实的 0 命中率就会被另一个键顶掉 —— 静默高报。
//
// 规则（刻意保守）：**primary 键存在就认它，哪怕为 0**；仅当它缺席才回落。
// 不"取较大者"：那等于替上游编造数字，且方向不可控（若某天 fallback 语义变成
// 累计值，取 max 会把每行都高报）。评审建议过 max，这里评估后**不采纳**，
// 理由记在此处，避免后人反复摇摆。
func TestParseUsageExtrasFallbackOnlyWhenKeyAbsent(t *testing.T) {
	// 1. primary 存在（值 42）且 fallback=77 → 用 primary，不取 max
	got := ParseUsageExtras(map[string]any{
		"completion_thinking_tokens": 42.0,
		"completion_tokens_details":  map[string]any{"reasoning_tokens": 77.0},
	})
	if got.ThinkTokens != 42 {
		t.Errorf("primary 存在时应用 primary: got %d want 42", got.ThinkTokens)
	}

	// 2. 关键回归：primary 显式为 0 → 必须是 0（不再被 fallback 顶掉）
	got = ParseUsageExtras(map[string]any{
		"prompt_cache_hit_tokens": 0.0,
		"cached_tokens":           50.0,
	})
	if got.CacheHitTokens != 0 {
		t.Errorf("primary=0 是合法值，不该被 fallback 顶替: got %d want 0", got.CacheHitTokens)
	}

	// 3. primary 完全缺席 → 才回落
	got = ParseUsageExtras(map[string]any{"cached_tokens": 50.0})
	if got.CacheHitTokens != 50 {
		t.Errorf("primary 缺失时应回落: got %d want 50", got.CacheHitTokens)
	}
	got = ParseUsageExtras(map[string]any{
		"completion_tokens_details": map[string]any{"reasoning_tokens": 9.0},
	})
	if got.ThinkTokens != 9 {
		t.Errorf("thinking 缺席时应回落 details: got %d want 9", got.ThinkTokens)
	}

	// 4. primary 存在但值坏（"garbage"）→ 该键"存在"，归 0；不用 fallback 顶替
	//    （顶替会把"上游给了坏值"伪装成"一切正常"）。
	got = ParseUsageExtras(map[string]any{
		"prompt_cache_hit_tokens": "garbage",
		"cached_tokens":           77.0,
	})
	if got.CacheHitTokens != 0 {
		t.Errorf("坏值不该被 fallback 伪装成有效: got %d want 0", got.CacheHitTokens)
	}
}

// 非法十进制写法必须归 0，不能被 ParseFloat 的宽容读成「看似合理的数字」。
//
// 评审实测：曾让 "1e3"→1000、"0x1p10"→1024、"1_000"→1000。
// 对计数/积分字段来说这些都不是合法写法，读成有效值是**静默错误**，
// 比归零危险（归零只是"这条没有该信息"）。
func TestParseUsageExtrasRejectsNonDecimalStrings(t *testing.T) {
	bad := []string{
		"1e3", "1E3", "0x1p10", "1_000", "1e-3", "0x10", "0b101", "0o17",
		"1,234", "12abc", "1.2.3", "１２３", "٣", "Inf", "NaN", "infinity",
		"+", "-", ".", "5e",
	}
	for _, s := range bad {
		got := ParseUsageExtras(map[string]any{
			"prompt_cache_hit_tokens":  s,
			"prompt_cache_miss_tokens": s,
			"credit":                   s,
		})
		if got.CacheHitTokens != 0 || got.CacheMissTokens != 0 || got.Credit != 0 {
			t.Errorf("%q 应全部归 0，得到 %+v", s, got)
		}
	}
	// 合法形态仍要能吃下
	good := map[string]int{"12": 12, " 7 ": 7, "+5": 5, "12.5": 12, "5.": 5, ".9": 0}
	for s, want := range good {
		if got := ParseUsageExtras(map[string]any{"prompt_cache_hit_tokens": s}); got.CacheHitTokens != want {
			t.Errorf("%q: got %d want %d", s, got.CacheHitTokens, want)
		}
	}
}

// 所有数值类型形态都要吃下：JSON float64、Go int/int64/uint、字符串化数字。
func TestParseUsageExtrasAllNumericTypes(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int
	}{
		{"float64", 12.0, 12},
		{"float64 truncates", 12.9, 12},
		{"int", 12, 12},
		{"int64", int64(12), 12},
		{"int32", int32(12), 12},
		{"uint", uint(12), 12},
		{"uint64", uint64(12), 12},
		{"float32", float32(12), 12},
		{"string plain", "12", 12},
		{"string float", "12.9", 12},
		{"string padded", "  12  ", 12},
		{"string plus sign", "+12", 12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseUsageExtras(map[string]any{"prompt_cache_miss_tokens": c.value})
			if got.CacheMissTokens != c.want {
				t.Errorf("value %#v: got %d want %d", c.value, got.CacheMissTokens, c.want)
			}
		})
	}
}

// 垃圾值/缺失/null 一律 0，绝不 panic、绝不出现负数或 NaN/Inf。
func TestParseUsageExtrasGarbageAndMissing(t *testing.T) {
	junk := []any{
		nil,
		"",
		"abc",
		"12abc",
		true,
		false,
		[]any{1, 2},
		map[string]any{"nested": 1},
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		-5.0,
		-5,
		"-5",
	}
	for i, v := range junk {
		got := ParseUsageExtras(map[string]any{
			"credit":                     v,
			"completion_thinking_tokens": v,
			"prompt_cache_hit_tokens":    v,
			"prompt_cache_miss_tokens":   v,
		})
		if got.Credit != 0 || got.ThinkTokens != 0 || got.CacheHitTokens != 0 || got.CacheMissTokens != 0 {
			t.Errorf("junk[%d]=%#v: want all zero, got %+v", i, v, got)
		}
	}
}

// nil map 与空 map 都必须安全返回零值。
func TestParseUsageExtrasNilAndEmpty(t *testing.T) {
	if got := ParseUsageExtras(nil); got != (UsageExtras{}) {
		t.Errorf("nil map: got %+v want zero", got)
	}
	if got := ParseUsageExtras(map[string]any{}); got != (UsageExtras{}) {
		t.Errorf("empty map: got %+v want zero", got)
	}
}

// completion_tokens_details 为 nil 或非 map 时，思考 token 回落路径不能 panic。
func TestParseUsageExtrasBadNestedDetails(t *testing.T) {
	for _, v := range []any{nil, "abc", 42.0, []any{}} {
		got := ParseUsageExtras(map[string]any{"completion_tokens_details": v})
		if got.ThinkTokens != 0 {
			t.Errorf("details=%#v: ThinkTokens=%d want 0", v, got.ThinkTokens)
		}
	}
	// 子对象存在但缺 reasoning_tokens
	got := ParseUsageExtras(map[string]any{"completion_tokens_details": map[string]any{}})
	if got.ThinkTokens != 0 {
		t.Errorf("空 details: ThinkTokens=%d want 0", got.ThinkTokens)
	}
}

// 超大值必须夹紧而不是溢出成负数。
//
// 断言的是「有界且非负」而不是某个具体上限：把上限钉成 MaxInt 会固化一个
// 危险行为 —— 一处脏数据（1e30 或 +Inf）就会让下游每一次 SUM 被放大到 9.2e18，
// 整份统计报废。这里改为断言「落在合理区间内、且明显小于 MaxInt」。
func TestParseUsageExtrasHugeValuesClamp(t *testing.T) {
	got := ParseUsageExtras(map[string]any{
		"prompt_cache_hit_tokens": 1e30,
		// 字符串形态要写**十进制**写法："1e30" 会被 isPlainDecimal 拒掉（那是
		// 刻意的，见 TestParseUsageExtrasRejectsNonDecimalStrings）。
		"prompt_cache_miss_tokens": "1000000000000000000000000000000",
	})
	if got.CacheHitTokens < 0 || got.CacheMissTokens < 0 {
		t.Errorf("超大值溢出成负数: %+v", got)
	}
	// 必须被夹住，不能接近 MaxInt（接近就说明脏数据能污染聚合）
	const reasonable = int64(1) << 62
	for name, v := range map[string]int{
		"CacheHitTokens":  got.CacheHitTokens,
		"CacheMissTokens": got.CacheMissTokens,
	} {
		if int64(v) > reasonable {
			t.Errorf("%s=%d 过大（>2^62），脏数据会污染聚合求和", name, v)
		}
		if v == 0 {
			t.Errorf("%s=0，1e30 是有效数值，应夹紧到上限而不是归零", name)
		}
	}
	// 两个不同形态（float64 与 string）应得到同一个值
	if got.CacheHitTokens != got.CacheMissTokens {
		t.Errorf("float64 与 string 形态结果应一致: %d vs %d",
			got.CacheHitTokens, got.CacheMissTokens)
	}
}

// +Inf / -Inf / NaN 一律归零 —— 它们只可能来自脏数据或算术溢出，
// 夹到上限会污染聚合，归零则退化成「这条没有该信息」。
func TestParseUsageExtrasNonFiniteIsZero(t *testing.T) {
	for _, v := range []any{math.Inf(1), math.Inf(-1), math.NaN()} {
		got := ParseUsageExtras(map[string]any{
			"completion_thinking_tokens": v,
			"prompt_cache_hit_tokens":    v,
			"prompt_cache_miss_tokens":   v,
			"credit":                     v,
		})
		if got != (UsageExtras{}) {
			t.Errorf("%v 应全部归零，得到 %+v", v, got)
		}
	}
}

// 边界：MaxInt 附近的合法值不得因 float64 舍入而溢出。
//
// 这是实测过的真缝：float64(math.MaxInt) 会舍入成 2^63（比 MaxInt 大 1），
// 用它当上界比较时，2^63 附近的值判定为「未越界」，随后 int(f) 溢出。
func TestParseUsageExtrasNearMaxIntNoOverflow(t *testing.T) {
	for _, f := range []float64{
		float64(math.MaxInt),
		9.223372036854775e18, // 实测踩过的那个值
		1 << 62,
	} {
		got := ParseUsageExtras(map[string]any{"prompt_cache_hit_tokens": f})
		if got.CacheHitTokens < 0 {
			t.Errorf("%v 溢出成负数: %d", f, got.CacheHitTokens)
		}
	}
}

// credit 是浮点，不应被截断成整数；同时接受字符串形态。
func TestParseUsageExtrasCreditFloat(t *testing.T) {
	if got := ParseUsageExtras(map[string]any{"credit": 0.0037}); math.Abs(got.Credit-0.0037) > 1e-12 {
		t.Errorf("credit float: got %v want 0.0037", got.Credit)
	}
	if got := ParseUsageExtras(map[string]any{"credit": "1.5"}); got.Credit != 1.5 {
		t.Errorf("credit string: got %v want 1.5", got.Credit)
	}
	if got := ParseUsageExtras(map[string]any{"credit": int64(3)}); got.Credit != 3 {
		t.Errorf("credit int64: got %v want 3", got.Credit)
	}
	if got := ParseUsageExtras(map[string]any{"credit": "-1.5"}); got.Credit != 0 {
		t.Errorf("credit 负数应归零: got %v", got.Credit)
	}
}

// 真实 JSON 解码路径：整体反序列化后的 usage 与手工构造的 map 结果一致。
func TestParseUsageExtrasFromRealJSON(t *testing.T) {
	var chunk struct {
		Usage map[string]any `json:"usage"`
	}
	raw := `{"usage":{"completion_tokens":133,"credit":0.85,
		"completion_thinking_tokens":64,
		"completion_tokens_details":{"reasoning_tokens":64},
		"prompt_cache_hit_tokens":128,"prompt_cache_miss_tokens":32,
		"cached_tokens":128}}`
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := ParseUsageExtras(chunk.Usage)
	want := UsageExtras{CompletionTokens: 133, Credit: 0.85, ThinkTokens: 64, CacheHitTokens: 128, CacheMissTokens: 32}
	if got != want {
		t.Errorf("real JSON: got %+v want %+v", got, want)
	}
}

// 只给细节对象形态的真实报文（无顶层 completion_thinking_tokens）。
func TestParseUsageExtrasFromRealJSONDetailsOnly(t *testing.T) {
	var chunk struct {
		Usage map[string]any `json:"usage"`
	}
	raw := `{"usage":{"completion_tokens":200,"completion_tokens_details":{"reasoning_tokens":150},"cached_tokens":64}}`
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := ParseUsageExtras(chunk.Usage)
	if got.ThinkTokens != 150 || got.CacheHitTokens != 64 {
		t.Errorf("details-only: got %+v want {0 150 64 0}", got)
	}
}

// json.Number（UseNumber 解码）形态：usageInt/usageFloat 的 default 分支兜底。
func TestParseUsageExtrasJSONNumber(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(
		`{"prompt_cache_hit_tokens":42,"credit":0.25}`))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := ParseUsageExtras(m)
	if got.CacheHitTokens != 42 || got.Credit != 0.25 {
		t.Errorf("json.Number: got %+v want {0.25 0 42 0}", got)
	}
}

// ── Anthropic 形态 + 上游自己的 billing_usage 方言（2026-11-12 真机）────────────
//
// 背景：隔离实例的请求日志行是 `tok=0`，而同一次响应的 usage 里明明有输出与推理量。
// 根因：本函数此前**只认 OpenAI 键集**（completion_tokens / completion_thinking_tokens /
// completion_tokens_details.reasoning_tokens），而 Anthropic 系上游（本仓有 4 家）的 usage
// 用的是 input_tokens / output_tokens —— 一个都对不上 ⇒ 两个量恒 0。
//
// ⚠ 夹具是那份真机响应里的 usage **逐字复制**（未改一字符），它同时说明两件事：
//
//	① 上游把自己那套放在 `billing_usage.openai_usage` 里 —— 那是**它自己的方言**，
//	   **不是**"核心已经转换过"的证据（同一对象顶层还并列着 Anthropic 的 input_tokens/output_tokens，
//	   而且里面 `completion_tokens:32` 与 `output_tokens:0` 是**同义不同值**）；
//	② 真实的 think 只有一个来源：`billing_usage.openai_usage.completion_tokens_details.reasoning_tokens`。
func TestParseUsageExtrasAnthropicDialect(t *testing.T) {
	const raw = `{"billing_usage":{"openai_usage":{"claude_cache_creation_1_h_tokens":0,"claude_cache_creation_5_m_tokens":0,"completion_tokens":32,"completion_tokens_details":{"audio_tokens":0,"image_tokens":0,"reasoning_tokens":32,"text_tokens":0},"input_tokens":0,"input_tokens_details":null,"output_tokens":0,"prompt_tokens":39,"prompt_tokens_details":{"cached_tokens":0,"image_tokens":0,"text_tokens":0},"total_tokens":71},"semantic":"openai","source":"oai_chat"},"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"claude_cache_creation_1_h_tokens":0,"claude_cache_creation_5_m_tokens":0,"input_tokens":39,"output_tokens":32}`
	var usage map[string]any
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatalf("夹具解析失败: %v", err)
	}
	x := ParseUsageExtras(usage)
	if x.CompletionTokens != 32 {
		t.Errorf("CompletionTokens = %d, want 32 —— 它为 0 就是日志行 `tok=0` 的直接原因"+
			"（tok/s 的分母也用它）", x.CompletionTokens)
	}
	if x.ThinkTokens != 32 {
		t.Errorf("ThinkTokens = %d, want 32 —— 真机 reasoning_tokens=32，且这是**唯一**真实的 think 来源"+
			"（Anthropic 的 output_tokens 含 thinking，拆不出来）", x.ThinkTokens)
	}
}

// OpenAI 键与 Anthropic 键/方言**同时存在**时：OpenAI 键优先，兜底绝不许反超。
//
// 判据沿用本文件既有的"按**键存在**判、不按值判"（usage.go 的注释里有实测出处）。
//
// ⚠ **这条锁抓的是"兜底反超真值"，不是"把判据写成值非 0"**（验证者实测：专门把判据改成值判据时，
// 它仍然绿）。后者的锁在 TestParseUsageExtrasNewFallbacksArePresenceBased（⓪①③ 三档）。
// 两条锁分工不同，别删任意一条。
func TestParseUsageExtrasOpenAIKeysWinWhenBothPresent(t *testing.T) {
	got := ParseUsageExtras(map[string]any{
		"completion_tokens":          7.0,
		"completion_thinking_tokens": 3.0,
		// 下面两个是兜底路径的键，故意给**明显不同**的值：
		// 若兜底反超了真值，这两条断言会立刻红（这正是本用例的判别力所在）。
		"output_tokens": 999.0,
		"billing_usage": map[string]any{"openai_usage": map[string]any{
			"completion_tokens":         888.0,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 777.0},
		}},
	})
	if got.CompletionTokens != 7 {
		t.Errorf("CompletionTokens = %d, want 7（OpenAI 键存在时不许被兜底覆盖）", got.CompletionTokens)
	}
	if got.ThinkTokens != 3 {
		t.Errorf("ThinkTokens = %d, want 3（显式键存在时不许看兜底）", got.ThinkTokens)
	}
}

// 回归锁：**纯 OpenAI 形态**（既有上游那批）的取值一个都不许变。
func TestParseUsageExtrasOpenAIOnlyUnchanged(t *testing.T) {
	usage := map[string]any{
		"completion_tokens":          100.0,
		"credit":                     1.25,
		"completion_thinking_tokens": 42.0,
		"prompt_cache_hit_tokens":    64.0,
		"prompt_cache_miss_tokens":   36.0,
	}
	if got, want := ParseUsageExtras(usage), (UsageExtras{Credit: 1.25, ThinkTokens: 42, CacheHitTokens: 64, CacheMissTokens: 36, CompletionTokens: 100}); got != want {
		t.Errorf("纯 OpenAI 形态: got %+v want %+v", got, want)
	}
}

// TestParseUsageExtrasNewFallbacksArePresenceBased 2026-11-12 新增的两级兜底也必须"按**键存在**判"。
//
// # 为什么补这一条（验证者的变异 C1/C2）
//
// 那两级兜底（`completionTokensFallback` 与推理链第 ③ 级）当时的见证用例只覆盖了"键缺失 ⇒ 兜底生效"，
// 于是把它们改成"**值非 0** 才走兜底"时，`./internal/wire/` 与 `./internal/server/` **全绿** ——
// 而行为真的变了（0 → 32）。下面三条就是给这两级补的"0 值 / 坏值"锁：
//
//	⓪ **顶层** `completion_tokens` 存在且为 0 ⇒ 采信 0，**不许**落到兜底（那会把 0 换成 32）
//	① 方言里 `completion_tokens: 0` **存在** ⇒ 采信 0，**不许**再落到 `output_tokens: 32`
//	② 方言值**是坏值**（"abc"）⇒ 归零，同样不许落到 `output_tokens: 32`
//	③ 顶层 `completion_tokens_details` **存在**（其 reasoning_tokens=0）⇒ 推理链就停在那一级，
//	   不许因为"它为 0"而去看方言里那个 32
//
// 三条的共同判据是全仓那条纪律：**判「这一级有没有给出这个键」，不判「值是不是 0」**。
func TestParseUsageExtrasNewFallbacksArePresenceBased(t *testing.T) {
	dialect := func(inner map[string]any) map[string]any {
		return map[string]any{"billing_usage": map[string]any{"openai_usage": inner}}
	}

	// ⓪ 顶层显式 0：这一档专抓"把判据写成值非 0"（见上面 C1）。
	got := ParseUsageExtras(map[string]any{
		"completion_tokens": 0.0,
		"output_tokens":     32.0,
	})
	if got.CompletionTokens != 0 {
		t.Errorf("顶层 completion_tokens=0 **存在** ⇒ 采信 0，得到 %d（落到 output_tokens 就是拿另一个数替真值）",
			got.CompletionTokens)
	}

	// ① 方言里显式 0：必须采信 0。
	got = ParseUsageExtras(map[string]any{
		"output_tokens": 32.0,
		"billing_usage": map[string]any{"openai_usage": map[string]any{"completion_tokens": 0.0}},
	})
	if got.CompletionTokens != 0 {
		t.Errorf("方言里 completion_tokens=0 **存在** ⇒ 必须采信 0，得到 %d（落到 output_tokens 就是把真值换成了别的数）",
			got.CompletionTokens)
	}

	// ② 方言里是坏值：归零，不许改落到别的键。
	got = ParseUsageExtras(map[string]any{
		"output_tokens": 32.0,
		"billing_usage": map[string]any{"openai_usage": map[string]any{"completion_tokens": "abc"}},
	})
	if got.CompletionTokens != 0 {
		t.Errorf("方言值不可解析 ⇒ 0（键存在就已经定了这一级），得到 %d", got.CompletionTokens)
	}

	// ③ 顶层 details 存在（reasoning_tokens=0）⇒ 推理链停在②，不落到方言那一级。
	got = ParseUsageExtras(map[string]any{
		"completion_tokens_details": map[string]any{"reasoning_tokens": 0.0},
		"billing_usage": map[string]any{"openai_usage": map[string]any{
			"completion_tokens_details": map[string]any{"reasoning_tokens": 32.0},
		}},
	})
	if got.ThinkTokens != 0 {
		t.Errorf("顶层 details 给出了 reasoning_tokens=0 ⇒ 采信 0，得到 %d（看方言那一级 = 拿另一个数替真值）",
			got.ThinkTokens)
	}

	// 对照：同样的夹具，**顶层 details 缺席**时方言那一级必须生效（否则这条锁会把兜底整个关死）。
	got = ParseUsageExtras(dialect(map[string]any{
		"completion_tokens_details": map[string]any{"reasoning_tokens": 32.0},
	}))
	if got.ThinkTokens != 32 {
		t.Errorf("顶层 details 缺席时方言那一级必须生效，得到 %d", got.ThinkTokens)
	}
}
