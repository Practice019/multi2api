package gateway

import (
	"bytes"
	"errors"
	"io"
	"strconv"
)

// usagetap.go 从**已经在收的**对话回执里读"这次花了多少额度" —— 零额外请求。
//
// # 它为什么在接缝层（而不是某个上游里）
//
// 实测（2026-10-15，逐个上游打过一次对话回执，见下）哪些上游会把消耗写进回执：
//
//	loomy          usage.points_consumed        整数积分 —— **能用**
//	qoder/qodercn  usage.credits/original_credits + billable
//	                                             小数 credits —— 字段能用，单位见下
//	cline          usage.cost / cost_details.*  这是**成本（钱）**，不是余额 ⇒ 只能统计
//	trae           usage.cache_read/creation_input_tokens …  只有 token ⇒ 用不了
//	workbuddy(-intl) usage.cached_tokens / completion_thinking_tokens …  只有 token
//	lobsterai      usage.prompt_cache_hit/miss_tokens …       只有 token
//	raccoon        只有 token
//	zcode          回执 503（账号不可用，与本机制无关）
//
// 也就是说"从回执里读消耗"这件事**只有少数上游能做**，但**做法完全一样** ——
// 所以它属于接缝层：一个实现、按 spec 参数化、各上游各自声明。
//
// # 四条纪律（前三条是在 loomy 上验证过的，第四条是抽象带来的安全阀）
//
//	① **不改一个字节**：它是透传层，只旁听。转发出去的内容必须与不挂它时
//	   逐字节相同（SSE 的换行与分隔符都有语义）。
//	② **不缓冲、不阻塞**：只在每次 Read 的返回值上扫一遍，最多留一小段
//	   "跨读边界"的尾巴。绝不把整条流攒起来 —— 那会把流式变成批式。
//	③ **读不出来也不影响转发**：解析失败/字段缺失/格式变了，一律静默。
//	   它是"顺手多拿一个数"，不是"转发的前置条件"。
//	④ **语义必须显式声明**（Mode 必填）—— 见 UsageTapSpec 的说明。

// UsageMode 回执里那个数**是什么语义** —— 决定"能不能拿它去扣额度"。
//
// ⚠ 这是抽象层的**安全阀**，不是分类癖：`cline` 的 `usage.cost`（美元成本）与
// `loomy` 的 `usage.points_consumed`（消耗的积分）**长得都像一个数**，而语义
// 完全不同。把成本当成"消耗掉的额度"去扣，就是在**编数据** —— 会把一个还有
// 额度的账号判成用完（于是它被跳过，或者整池被判成"免费额度用完"而开始烧钱）。
//
// 所以调用方必须显式声明语义，而不能"有数字就扣"。
type UsageMode int

const (
	// UsageUnknown 未声明语义。**不可用于扣减** —— 声明成它是配置错误。
	UsageUnknown UsageMode = iota
	// UsageConsumed 本次**消耗掉的额度**（与账号余额同一种单位）⇒ 可以扣减。
	UsageConsumed
	// UsageCost 本次的**成本/费用**（钱/计价），不是余额的减少量 ⇒ 只用于统计。
	UsageCost
)

func (m UsageMode) String() string {
	switch m {
	case UsageConsumed:
		return "consumed"
	case UsageCost:
		return "cost"
	default:
		return "unknown"
	}
}

// UsageTapSpec 一个上游"从对话回执里读消耗"的声明。
type UsageTapSpec struct {
	// Key 回执里那个字段的 JSON 键名，例如 "points_consumed" / "credits"。
	//
	// ⚠ 只按**键名**找、不解析 JSON 结构：上游加字段、换外层包装都不影响。
	Key string
	// Scale 把回执里的数换算成**整数**的倍数。
	//
	//	整数单位（loomy 的积分）       → 1
	//	小数单位（qoder 的 credits）   → 1e6
	//
	// ⚠ 回调拿到的是**换算后**的整数：本层用整数避免浮点累计误差
	//（"每来一次就 round 一次"会漂）。
	//
	// ⚠ 但**不要**因为 Scale≠1 就把换算后的值直接写进展示层 ——
	// 展示值必须用账号余额自己的单位（用户对 qoder 明确要求过"显示原始值
	// 不做换算"）。Scale 只用于**内部累计与比较**。
	Scale int64
	// Mode 语义（见 UsageMode）。**必填** —— UsageUnknown 会在 Validate 里报错。
	Mode UsageMode
}

// ErrUsageModeUnset Mode 没声明。
var ErrUsageModeUnset = errors.New("gateway: UsageTapSpec.Mode 必须显式声明（Unknown 不可用于扣减）")

// Validate 报告这个 spec 能不能用。
//
// ⚠ 它存在的唯一目的是**让"忘了声明语义"变成一个显式错误**，而不是
// "默认当成消耗去扣"。后者会静默地编数据。
func (s UsageTapSpec) Validate() error {
	if s.Mode == UsageUnknown {
		return ErrUsageModeUnset
	}
	if s.Key == "" {
		return errors.New("gateway: UsageTapSpec.Key 不能为空")
	}
	if s.Scale <= 0 {
		return errors.New("gateway: UsageTapSpec.Scale 必须为正（整数单位用 1）")
	}
	return nil
}

// usageTapCarryMax 跨读边界保留的尾巴长度。
//
// 关键字可能被两次 Read 切开（生产上取决于网络分片），所以要留一小段。
// 64 字节足够容纳 `"points_consumed"` 这类键名 + 一个数字。
const usageTapCarryMax = 64

type usageTap struct {
	rc   io.ReadCloser
	spec UsageTapSpec
	// onValue 结束时回调一次，参数是**换算并取整后**的值（Scale 已应用）。
	//
	// ⚠ 只有 Mode==UsageConsumed 才会回调：把 cost 当 consumed 去扣额度是编数据。
	onValue func(scaled int64)

	carry  []byte
	last   int64
	lastOK bool
	closed bool
	// value 是否存在（用来区分"读到 0"与"没读到"）。
	// 0 是合法值（比如缓存命中的请求真的消耗 0），不能与"没读到"混为一谈。
}

// NewUsageTap 包一层透传 Reader，旁听从它流过的字节。
//
// ⚠ spec 非法时返回 error（而不是默默不回调）：调用方应当**在装配期**
// 拿到这个错，而不是在生产上发现"额度从来不更新"。
func NewUsageTap(rc io.ReadCloser, spec UsageTapSpec, onValue func(scaled int64)) (io.ReadCloser, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if rc == nil {
		rc = io.NopCloser(bytes.NewReader(nil))
	}
	return &usageTap{rc: rc, spec: spec, onValue: onValue}, nil
}

func (t *usageTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.scan(p[:n])
	}
	return n, err
}

func (t *usageTap) Close() error {
	// ⚠ 只在 Close 时回调一次：Read 会被调用很多次，而"这次请求消耗了多少"
	// 只有结束时才知道是最终值。
	if !t.closed {
		t.closed = true
		// 只有"消耗"语义才驱动扣减；成本语义只旁听不驱动。
		if t.spec.Mode == UsageConsumed && t.lastOK && t.onValue != nil {
			t.onValue(t.last)
		}
	}
	return t.rc.Close()
}

// scan 在**这次读到的字节**里找 `"<Key>"` 后面的数字。
//
// ⚠ 不解析 JSON、不假设结构（见 UsageTapSpec.Key 的说明）。
func (t *usageTap) scan(b []byte) {
	var buf []byte
	if len(t.carry) > 0 {
		buf = append(append([]byte{}, t.carry...), b...)
	} else {
		buf = b
	}
	if len(buf) > usageTapCarryMax {
		t.carry = append([]byte{}, buf[len(buf)-usageTapCarryMax:]...)
	} else {
		t.carry = append([]byte{}, buf...)
	}

	key := []byte(`"` + t.spec.Key + `"`)
	for {
		i := bytes.Index(buf, key)
		if i < 0 {
			return
		}
		rest := buf[i+len(key):]
		j := 0
		for j < len(rest) && (rest[j] == ':' || rest[j] == ' ' || rest[j] == '\t') {
			j++
		}
		k := j
		for k < len(rest) && (rest[k] == '-' || rest[k] == '+' || rest[k] == '.' ||
			(rest[k] >= '0' && rest[k] <= '9') || rest[k] == 'e' || rest[k] == 'E') {
			k++
		}
		if k > j {
			// ⚠ 用浮点解析再乘 Scale 后取整：回执里可能是小数
			//（qoder 的 credits 是 0.006352…）或科学计数法（cline 的 1.47e-05）。
			if f, err := strconv.ParseFloat(string(rest[j:k]), 64); err == nil {
				// ⚠ **覆盖**而不是累加：实测流式里 usage 会出现在**两块** SSE 中
				//（finish_reason 那块与最后那块，值相同）。累加会翻倍扣减 ⇒
				// 账号余额被过早判成用完 ⇒ 一个还有额度的号被踢出池子。
				t.last = int64(f * float64(t.spec.Scale))
				t.lastOK = true
			}
		}
		buf = rest
		if len(buf) == 0 {
			return
		}
	}
}
