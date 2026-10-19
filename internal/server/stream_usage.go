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

// sseUsage 上游 usage 块（只取我们日志要的三个数）。
type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// valid 判定这个 usage 是否"看起来是真数据"。
//
// ⚠ 判据照 new-api：PromptTokens > 0 且（CompletionTokens > 0 或 TotalTokens > 0）。
// 为什么要这种组合而不是"非零即可"：不少兼容网关会发 `{"usage":{"prompt_tokens":123}}`
// 这种只有输入的半截快照，直接采用会把输出当 0 —— 那比没有更糟，因为看不出来错。
func (u *sseUsage) valid() bool {
	if u == nil {
		return false
	}
	return u.PromptTokens > 0 && (u.CompletionTokens > 0 || u.TotalTokens > 0)
}

// total 取总 token（优先 total_tokens，缺失时用 输入+输出 兜底）。
func (u *sseUsage) total() int {
	if u == nil {
		return 0
	}
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
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
	return f.Usage
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
