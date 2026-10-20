// stream_usage.go SSE 旁路用量采集器（2026-10-20）。
//
// # 这个文件解决什么
//
// 渠道转发是纯字节 tee：上游给什么就转发什么，我们**从不解析**它。
// 后果是请求日志里 tokens 只能记 -1（"未知"哨兵），而 -1 进了聚合就要被排除 ——
// 等于这些请求的用量永久丢失，tok/s 也算不出来。
//
// 这里做一个**只读旁路**：把已经要转发出去的 chunk 顺手喂进来，从 SSE 事件里
// 抓 usage。转发字节一个都不碰（Feed 只读入参、自己复制）。
//
// # 三级兜底（照 new-api relay/channel/openai/relay-openai.go 的做法）
//
//	① 末帧带 usage ⇒ 直接取
//	② 末帧没有 ⇒ 回退**倒数第二帧**。new-api 源码注释原话：
//	   「部分兼容网关把完整的累计 usage 附在倒数第二个事件上，随后发送一个空的最后事件」
//	   这不是臆想 —— 是它们踩出来的兼容性事实。
//	③ 两级都没有 ⇒ 按**已接收的输出文本估算**，绝不记 -1 或 0 充数。
//	   估算不精确，但它是可用的下界，且能让 tok/s 有值。
//
// # 为什么只留两帧
//
// 内存恒定。累积全部帧对一个长对话会吃掉几十 MB，而我们需要的信息只在末尾两帧里。
package server

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// usageSource 用量的来源，写进日志便于区分"上游给的"与"我们估的"。
//
// ⚠ 这个标记必须落盘：估算值若被当成上游真实用量，用户在算钱时会发现对不上，
// 而且**没有任何线索**能追到是哪里估的。
type usageSource string

const (
	usageFromLastFrame  usageSource = "upstream_last"        // 末帧带 usage
	usageFromSecondLast usageSource = "upstream_second_last" // 回退倒数第二帧
	usageEstimated      usageSource = "estimated"            // 按输出文本估算
	usageUnavailable    usageSource = "none"                 // 空流/纯错误体，无可估内容
)

// sseUsage 上游 usage 块（日志要的字段）。
//
// ⚠ 2026-10-20 加缓存两项：实测 loomy 上游只发
// `{"completion_tokens":483,"prompt_cache_hit_tokens":63360,"prompt_cache_miss_tokens":722}`
// —— **既没有 total_tokens 也没有 prompt_tokens**。旧实现因此两处都错：
//
//	· valid() 要求 PromptTokens>0 ⇒ 整帧判"无效"被丢弃，total 退化成估算/输出
//	· total() 兜底只有 Prompt+Completion ⇒ 63360 的缓存读取凭空消失
//
// 现象（用户报"数据全都不对"）：日志里 toks=483 而 out=483 完全相等，
// 同时 cache_hit_tokens=63360 —— 缓存量是"总量"的 131 倍，一眼就能看出矛盾。
type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// 缓存命中/未命中（命中读取不算"新输入"，但它是上下文的一部分，
	// 必须计入 total —— 否则"这条请求碰了多少上下文"这个数就是错的）。
	CacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	CacheMissTokens int `json:"prompt_cache_miss_tokens"`
	// ⚠ hasCacheHit / hasCacheMiss 记录**键是否出现过**，用来区分
	//   「miss 真的是 0」（全命中）与「上游压根没给 miss」（未知）。
	//   光看数值分不开这两种情况 —— 都是 0。
	//   不区分会犯两类错，方向相反：
	//     · 当成未知 ⇒ 全命中的合法请求被判"输入未知"，整帧丢弃（比没修更糟）
	//     · 当成 0    ⇒ 只给 hit 的上游被当成"全命中"，未命中那部分凭空消失
	hasCacheHit  bool
	hasCacheMiss bool
}

// promptTotal 返回"总输入"，兼容三种上游形态：
//  1. 给 prompt_tokens（OpenAI 标准）⇒ 直接用，它已含缓存
//  2. 缓存两项齐备（键都存在，值是 0 也算）⇒ 相加（命中/未命中是互斥的输入划分）
//  3. 都不给或只给一项 ⇒ 0（调用方按未知处理，不要瞎猜）
func (u *sseUsage) promptTotal() int {
	if u == nil {
		return 0
	}
	if u.PromptTokens > 0 {
		return u.PromptTokens
	}
	// ⚠ 判据是**键存在**而不是**值 >0**：全命中（miss=0）与全未命中（hit=0）
	//   都是合法且常见的请求，用值做判据会把它们整帧丢掉。
	if u.hasCacheHit && u.hasCacheMiss {
		return u.CacheHitTokens + u.CacheMissTokens
	}
	return 0
}

// valid 判定这个 usage 是否"看起来是真数据"。
//
// ⚠ 判据照 new-api 的意图（要有输出，不能是半截快照），但**输入侧放宽**：
//
//	· 标准上游：PromptTokens > 0
//	· 只发缓存的网关：promptTotal() > 0（缓存两项能还原出总输入）
//
// 放宽的原因：原判据把"只发缓存不给 prompt_tokens"的整帧扔掉，
// 结果 total 退化成估算，而缓存量又照实记了 —— 两者拼在一起就是矛盾数据。
func (u *sseUsage) valid() bool {
	if u == nil {
		return false
	}
	if u.CompletionTokens <= 0 {
		return false
	}
	return u.promptTotal() > 0 || u.TotalTokens > 0
}

// total 取总 token（优先 total_tokens，缺失时用 输入+输出 兜底）。
func (u *sseUsage) total() int {
	if u == nil {
		return 0
	}
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.promptTotal() + u.CompletionTokens
}

// sseFrame 一帧 SSE data 里我们要的两个字段。
type sseFrame struct {
	Usage   *sseUsage `json:"usage"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// streamUsage 旁路采集器。零值不可用，用 newStreamUsage 构造。
type streamUsage struct {
	start time.Time

	// 未闭合的事件尾部。⚠ 必须跨 chunk 保留：
	// 一个 `data: {...}` 事件经常被 TCP 分片切成两半，按 chunk 边界盲目解析
	// 会让几乎每一帧都解析失败 —— 那样采集器永远返回"没有 usage"，
	// 表面上却毫无报错，是最难查的那种。
	buf []byte

	lastFrame       string
	secondLastFrame string

	// 已接收的输出文本（仅用于第三级估算）
	text      strings.Builder
	toolCalls int

	// ttfb 首个**有效 data 帧**到达的时刻（相对 start）。
	// ⚠ 不用"首个字节"：SSE 开头的注释行/心跳/空行不代表模型开始了，
	// 拿它们当首字会让 TTFB 虚低，看起来比真实快。
	ttfb   time.Duration
	sawAny bool
}

// newStreamUsage 以请求发起时刻为基准构造采集器。
func newStreamUsage(start time.Time) *streamUsage {
	return &streamUsage{start: start}
}

// Feed 喂入一个已读出的 chunk。**只读 chunk，不修改、不持有引用**。
//
// 调用方在把 chunk 写给客户端**之前或之后**调用都可以 —— 本函数不产生任何副作用，
// 也不做 IO，因此不会影响 flush 时机。
func (s *streamUsage) Feed(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	s.buf = append(s.buf, chunk...)
	// 按空行切事件：SSE 规范里事件以 `\n\n` 结束（有的网关发 `\r\n\r\n`）。
	for {
		idx, sepLen := findEventBoundary(s.buf)
		if idx < 0 {
			break // 尾部事件未闭合，留着等下一 chunk
		}
		event := s.buf[:idx]
		s.buf = s.buf[idx+sepLen:]
		s.consumeEvent(event)
	}
	// 防失控：极端情况下上游一直不给空行（非 SSE 的错误体），buf 会无限涨。
	// 超过 1 MB 直接丢弃已缓冲内容 —— 采不到就算了，不能吃内存。
	if len(s.buf) > 1<<20 {
		s.buf = s.buf[:0]
	}
}

// findEventBoundary 找到第一个事件结束位置，返回事件长度与分隔符长度。
func findEventBoundary(b []byte) (int, int) {
	i := strings.Index(string(b), "\n\n")
	j := strings.Index(string(b), "\r\n\r\n")
	switch {
	case i < 0 && j < 0:
		return -1, 0
	case i < 0:
		return j, 4
	case j < 0:
		return i, 2
	case i <= j:
		return i, 2
	default:
		return j, 4
	}
}

// consumeEvent 处理一个完整事件（不含尾部空行）。
func (s *streamUsage) consumeEvent(event []byte) {
	data := extractSSEData(event)
	if data == "" {
		return // 心跳、注释行、空事件
	}
	if data == "[DONE]" {
		return // 终止标记，不是 JSON
	}
	var f sseFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return // 非 JSON（部分网关的错误体），跳过
	}
	// 首帧时刻：只认解析成功的 JSON 帧
	if !s.sawAny {
		s.sawAny = true
		s.ttfb = time.Since(s.start)
	}
	// 保留最后两帧（照 new-api）
	if s.lastFrame != "" {
		s.secondLastFrame = s.lastFrame
	}
	s.lastFrame = data

	// 累积输出文本与工具调用（供第三级估算）
	for _, c := range f.Choices {
		if c.Delta.Content != "" {
			s.text.WriteString(c.Delta.Content)
		}
		s.toolCalls += len(c.Delta.ToolCalls)
	}
}

// extractSSEData 从事件块里取出 data 字段（支持多行 data 拼接）。
func extractSSEData(event []byte) string {
	var sb strings.Builder
	for _, line := range strings.Split(string(event), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		v := strings.TrimPrefix(line, "data:")
		v = strings.TrimPrefix(v, " ")
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(v)
	}
	return sb.String()
}

// frameUsage 从一帧 JSON 里取 usage（可能为 nil）。
func frameUsage(frame string) *sseUsage {
	if frame == "" {
		return nil
	}
	var f sseFrame
	if err := json.Unmarshal([]byte(frame), &f); err != nil {
		return nil
	}
	if f.Usage == nil {
		return nil
	}
	// ⚠ 记录缓存两项的键**是否出现过**（不是值是否 >0）。
	//   解析成 struct 之后就分不开「缺失」与「0」了，所以这里再解析一次 usage 原始键。
	//   代价是每帧多一次 unmarshal 到 map —— 只在携带 usage 的帧上发生（流的最末一两帧），
	//   不在逐 chunk 的热路径上。
	var raw struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(frame), &raw); err == nil {
		if _, ok := raw.Usage["prompt_cache_hit_tokens"]; ok {
			f.Usage.hasCacheHit = true
		}
		if _, ok := raw.Usage["prompt_cache_miss_tokens"]; ok {
			f.Usage.hasCacheMiss = true
		}
	}
	return f.Usage
}

// Completion 返回**纯输出** token 数与来源（0 = 无从得知）。
//
// 与 Result() 分开是为了让 tok/s 的口径独立：Result() 的 tokens 是 total
// （输入+输出，用于统计消耗），而速度必须只算输出。两者混用会让
// "2408ms 生成 37948 token" 这种物理上不可能的数字出现在页面上。
//
// ⚠ 估算分支同样返回估算值（estimted 的就是输出文本量），这是可接受的 ——
// 估算的本来就是输出；但来源会标 estimated，不与上游真实值混淆。
func (s *streamUsage) Completion() (int, usageSource) {
	if u := frameUsage(s.lastFrame); u.valid() {
		if u.CompletionTokens > 0 {
			return u.CompletionTokens, usageFromLastFrame
		}
		// 上游给了 total 但没给 completion：仍然算"上游数据"，只是输出侧缺失
		return 0, usageFromLastFrame
	}
	if u := frameUsage(s.secondLastFrame); u.valid() {
		if u.CompletionTokens > 0 {
			return u.CompletionTokens, usageFromSecondLast
		}
		return 0, usageFromSecondLast
	}
	if est := estimateTokens(s.text.String(), s.toolCalls); est > 0 {
		return est, usageEstimated
	}
	return 0, usageUnavailable
}

// Result 返回（token 总数, 首帧耗时, 来源）。
//
// token 为 0 时调用方应记 -1（"未知"），**不要**把 0 当真实用量写盘 ——
// 0 会被聚合当成"这个请求没花钱"，与"不知道"是两件事。
func (s *streamUsage) Result() (tokens int, ttfb time.Duration, src usageSource) {
	// ① 末帧
	if u := frameUsage(s.lastFrame); u.valid() {
		return u.total(), s.ttfb, usageFromLastFrame
	}
	// ② 倒数第二帧（兼容"完整 usage 附在倒数第二个事件、末帧为空"的网关）
	if u := frameUsage(s.secondLastFrame); u.valid() {
		return u.total(), s.ttfb, usageFromSecondLast
	}
	// ③ 按已接收的输出文本估算
	if est := estimateTokens(s.text.String(), s.toolCalls); est > 0 {
		return est, s.ttfb, usageEstimated
	}
	return 0, s.ttfb, usageUnavailable
}

// estimateTokens 按输出文本估算 token 数。
//
// # 为什么不用 tokenizer
//
// 引入一个真正的 BPE tokenizer 会让二进制变大、依赖变重，而这里要的只是
// "比没有强、能算 tok/s" 的量级估计。精确值应当来自上游 usage（前两级）；
// 走到这一级说明上游根本没给，再精确也只是猜。
//
// # 系数依据
//
// 中日韩字符在主流分词器里大致 1~1.5 字/token，拉丁字母大致 4 字符/token。
// 分开计数再相加，比"字符数/3"这种单一系数在中文场景下准得多
// （中文按 4 字符/token 会低估近 3 倍）。
//
// 工具调用按每次 7 token 补偿 —— 这个常数照抄 new-api 的 `toolCount * 7`。
func estimateTokens(text string, toolCalls int) int {
	var cjk, other int
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else if !unicode.IsSpace(r) {
			other++
		}
	}
	est := (cjk*2+2)/3 + (other+3)/4 // 向上取整：1.5 字/token、4 字符/token
	if est == 0 && utf8.RuneCountInString(text) > 0 {
		est = 1 // 有内容就不能估成 0
	}
	est += toolCalls * 7
	return est
}
