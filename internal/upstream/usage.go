// usage.go 上游 usage 对象的结构化抽取。
//
// 背景：SSE 透传路径把 chunk["usage"] 当不透明 map[string]any 原样转发给客户端，
// 网关自身只从中取 completion_tokens 做表格日志，其余字段（消费额度、推理 token、
// prompt 缓存命中/未命中）全部丢弃 —— 而这些恰恰是事后做用量/成本聚合必须的。
// 本文件提供纯函数把它们抽出来，供日志落盘侧写入 logbuf.Entry。
//
// 设计约束（这层是「上游脏数据 → 网关内部类型」的唯一出入口，必须绝对安全）：
//   - 不 panic：任何类型断言都走 comma-ok，nil map / nil 值 / 嵌套缺失一律安全；
//   - 不返回 NaN/Inf/负数：缺失或不可解析统一 0；入库字段是累加量，负值无意义；
//   - 同时吃多种数值类型：上游可能在 JSON（float64）、Go 内部构造（int/int64）、
//     字符串化数字（"123" / "12.5"）三种形态间摇摆，全部归一化。
package upstream

import (
	"math"
	"strconv"
	"strings"
)

// UsageExtras 从上游 usage 对象抽出的扩展字段。
//
// 零值即「全部缺失」，因此调用方只需判断返回值是否为零值就能决定是否写 0，
// 不需要额外的 ok 标志。所有整数字段保证 >= 0；Credit 保证为有限非负值。
type UsageExtras struct {
	// Credit 本次请求实际消耗的额度，对应上游 usage.credit。
	Credit float64
	// ThinkTokens 推理（思维链）token 数。
	ThinkTokens int
	// CacheHitTokens 命中 prompt 缓存的 token 数。
	CacheHitTokens int
	// CacheMissTokens 未命中 prompt 缓存的 token 数。
	CacheMissTokens int
}

// ParseUsageExtras 从上游 usage 对象抽取扩展字段（Credit/ThinkTokens/缓存计数）。
//
// 两处「双键名变体」的取值顺序（显式键优先，变体键兜底）：
//
//	ThinkTokens     : completion_thinking_tokens                → completion_tokens_details.reasoning_tokens
//	CacheHitTokens  : prompt_cache_hit_tokens                   → cached_tokens
//
// 之所以要兜底：上游在不同模型/接口形态下对同一个量挂了不同键名 ——
// 推理 token 有时平铺在顶层，有时塞进 completion_tokens_details 子对象；
// 缓存命中有时叫 prompt_cache_hit_tokens，有时是简写 cached_tokens。
// 两个键同时存在时以**显式**那个为准（顶层具名键比子对象/简写更具体，
// 也避免简写键被别处复用导致误读）。
//
// **回退的判据是「键不存在」，不是「值为 0」** —— 这一点是评审指出的真缺陷：
// 曾经写成 `if primary == 0 { 用 fallback }`，于是「合法地为 0」与「键不存在」
// 被混为一谈。真实数据里 `prompt_cache_hit_tokens` 与 `cached_tokens` **同时存在
// 且同值**，所以只要上游偶发给出不一致（或混流下两键来自不同帧），
// 一个真实的 0 命中率就会被 fallback 覆盖成非零，**静默高报**。
// 现在只有 primary 键确实缺席时才看 fallback。
//
// 返回值保证：永不为负、永不为 NaN/Inf；usage 为 nil 或某键缺失/类型不符/字符串
// 不可解析时，对应字段为 0。本函数不 panic。
func ParseUsageExtras(usage map[string]any) UsageExtras {
	var out UsageExtras
	if usage == nil {
		return out
	}

	out.Credit = usageFloat(usage["credit"])

	// 推理 token：顶层显式键**存在就认它**（哪怕为 0），只在它缺席时才看 details。
	//
	// 为什么不"取较大者"：两个键名义上描述同一个量，但上游并未承诺它们
	// 永远一致（评审实测出 5 vs 40 这种分歧）。取 max 等于**替上游编造一个数**，
	// 且方向不可控 —— 万一某天 details 里的 reasoning_tokens 语义变成
	// 「含前缀的累计值」，取 max 就会把每一行都高报。宁可忠实采用显式键，
	// 并在两键都存在且不一致时保持"显式优先"这个可预测的规则。
	out.ThinkTokens = usageInt(usage["completion_thinking_tokens"])
	if _, present := usage["completion_thinking_tokens"]; !present {
		if details, ok := usage["completion_tokens_details"].(map[string]any); ok && details != nil {
			out.ThinkTokens = usageInt(details["reasoning_tokens"])
		}
	}

	// 缓存命中：同上，prompt_cache_hit_tokens 存在就认它，缺席才回落简写键。
	out.CacheHitTokens = usageInt(usage["prompt_cache_hit_tokens"])
	if _, present := usage["prompt_cache_hit_tokens"]; !present {
		out.CacheHitTokens = usageInt(usage["cached_tokens"])
	}

	out.CacheMissTokens = usageInt(usage["prompt_cache_miss_tokens"])
	return out
}

// UsageInt 把任意形态的 usage 值归一化成非负 int；缺失/不可解析/负数一律 0。
//
// 导出是刻意的：internal/server 里提取 completion_tokens 的地方（流式末帧与
// 非流式聚合各一处）此前只认 float64，而那个值既当哨兵又**门控**着新字段落盘，
// 一旦上游把数字字符串化就会连能读的 credit 一起丢掉。两处共用本函数即可对齐。
//
// 接受的形态：
//
//	float64 / float32 —— JSON 数字的默认落点（先判 NaN/Inf/越界再截断）
//	int / int64 / int32 / uint / uint64 等 —— Go 侧构造 map 时的常见类型
//	json.Number（底层 string）—— 启用 UseNumber 解码时
//	string —— "123"、"12.5"、带空白的 " 7 "（上游偶有字符串化数字）
//
// 截断策略：乘法式转换用 float64 中间值，超过 int 范围的一律夹紧（见 clampToInt），
// 不产生未定义行为。
func UsageInt(v any) int { return usageInt(v) }

// usageInt 是 UsageInt 的内部实现（见其文档）。
func usageInt(v any) int {
	switch n := v.(type) {
	case nil:
		return 0
	case int:
		return nonNeg(n)
	case int8:
		return nonNeg(int(n))
	case int16:
		return nonNeg(int(n))
	case int32:
		return nonNeg(int(n))
	case int64:
		return clampToInt(float64(n))
	case uint:
		return clampToInt(float64(n))
	case uint8:
		return nonNeg(int(n))
	case uint16:
		return nonNeg(int(n))
	case uint32:
		return clampToInt(float64(n))
	case uint64:
		return clampToInt(float64(n))
	case float32:
		return clampToInt(float64(n))
	case float64:
		return clampToInt(n)
	case string:
		return intFromString(n)
	default:
		// json.Number 等命名 string 类型走不到上面的 string 分支，这里兜底。
		if s, ok := v.(interface{ String() string }); ok {
			return intFromString(s.String())
		}
		return 0
	}
}

// usageFloat 把任意形态的 usage 值归一化成有限非负 float64；其余一律 0。
func usageFloat(v any) float64 {
	switch n := v.(type) {
	case nil:
		return 0
	case float64:
		return nonNegFloat(n)
	case float32:
		return nonNegFloat(float64(n))
	case int:
		return nonNegFloat(float64(n))
	case int64:
		return nonNegFloat(float64(n))
	case int32:
		return nonNegFloat(float64(n))
	case uint:
		return nonNegFloat(float64(n))
	case uint64:
		return nonNegFloat(float64(n))
	case string:
		return floatFromString(n)
	default:
		if s, ok := v.(interface{ String() string }); ok {
			return floatFromString(s.String())
		}
		return 0
	}
}

// intFromString 解析字符串化数字；不可解析/负数/非法值一律 0。
func intFromString(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if i, err := strconv.Atoi(s); err == nil {
		return nonNeg(i)
	}
	// 兜底前先挡掉 ParseFloat 的「过于宽容」：它会接受科学计数、十六进制浮点、
	// 以及 Go 1.13 起的下划线分隔，于是 "1e3"→1000、"0x1p10"→1024、"1_000"→1000。
	// 对一个**计数**字段来说，这些都不是合法写法 —— 把 "1e3" 悄悄读成 1000 属于
	// 「看起来合理的错误值」，比直接归 0 危险得多（评审实测指出）。
	// 只放行纯十进制形态：数字、可选的单个正负号、最多一个小数点。
	if !isPlainDecimal(s) {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return clampToInt(f)
}

// isPlainDecimal 报告 s 是否是「纯十进制」写法：可选正负号 + 数字 + 最多一个小数点。
//
// 刻意拒绝：e/E（科学计数）、x/X 与 p/P（十六进制浮点）、_（下划线分隔）。
// 这些被 strconv.ParseFloat 接受，但对 usage 计数没有意义。
// 接受 "12.5"、"+3"、" 7 "（空白已在调用前 TrimSpace）、".5"、"5."。
func isPlainDecimal(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i++
	}
	if i >= len(s) {
		return false // 只有符号
	}
	digits, dots := 0, 0
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			// 其余一律拒绝（含 e/E/x/X/p/P/_/空白/逗号/非 ASCII）
			return false
		}
	}
	return digits > 0
}

// floatFromString 解析字符串化浮点；不可解析/负数/NaN/Inf 一律 0。
//
// 与 intFromString 用同一套形态校验：credit 是**积分**，同样不该接受
// "1e3"（会被读成 1000 分）这种非十进制写法。
func floatFromString(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || !isPlainDecimal(s) {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return nonNegFloat(f)
}

// clampToInt 把 float64 安全转成 int：NaN/±Inf → 0；负数 → 0；越界 → 夹紧到 int 上限。
//
// 直接 int(f) 在越界时是实现相关的（可能得到负数），故必须显式夹紧。
//
// 为什么 Inf 归 0 而不是夹到 MaxInt：本函数的输入是 **usage 计数**（token 数、
// 缓存命中数），不是容量上限。Inf 只可能来自脏数据或算术溢出，把它变成
// 9.2e18 会让下游每一次 SUM 都被污染（一处脏数据毁掉全部统计），
// 而 0 只是退化成「这条记录没有该信息」，与缺失字段的处理一致。
// 同理，真正越界的大数也只在有限范围内夹紧，不放大成 MaxInt。
func clampToInt(f float64) int {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0
	}
	// 注意：不能拿 float64(math.MaxInt) 当上界比较。
	// float64(MaxInt) 会被舍入成 9223372036854775808（比 MaxInt 大 1），
	// 于是 (MaxInt 附近的合法值) >= 该常量 为 false，却仍会在 int(f) 处溢出 ——
	// 实测 9.223372036854775e18 就落在这个缝里，转出 9223372036854774784。
	// 用一个有安全余量的上界（2^62）避开整个舍入区。
	const safeMax = float64(1 << 62)
	if f >= safeMax {
		return int(safeMax)
	}
	return int(f)
}

// nonNeg 负数归零（上游不该给负数，但脏数据不能污染聚合）。
func nonNeg(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// nonNegFloat 负数/NaN/Inf 一律归零。
func nonNegFloat(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return f
}
