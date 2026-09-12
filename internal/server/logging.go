// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/upstream"
)

// chatSeq 进程级请求序号（只用于 stdout 表格的 #%03d 显示）。
//
// 注意：落盘与 /admin/logs 的 seq 列来自 logbuf.Ring 自己的计数（Push 会覆盖 Entry.Seq），
// 这两个计数器互不相干但都从 1 起。启动时用同一个落盘最大值播种两者，
// 才能保证「序号依次变大」跨重启成立（见 SeedChatSeq 与 logbuf.Ring.SeedSeq）。
var chatSeq atomic.Int64

// SeedChatSeq 用落盘日志里的最大 seq 播种 stdout 序号计数器。
// n<=0 视为无历史，保持从 1 开始。
func SeedChatSeq(n int64) {
	if n <= 0 {
		return
	}
	// 只前进不后退：避免并发或重复调用把已经用掉的号段退回去。
	for {
		cur := chatSeq.Load()
		if n <= cur {
			return
		}
		if chatSeq.CompareAndSwap(cur, n) {
			return
		}
	}
}

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start time.Time
	model string
	mode  string // "stream" | "sync"
	uid   string // 完整 uid，展示时只取前 8 位
	// provider 本次请求最终走的上游标识。空串 = 未标注（历史/无上下文）。
	// 它由选号结果决定，因此只在真正选中账号后才被填上 —— 请求失败在选号前时保持空。
	provider string
	ttfb     time.Duration
	toks     int // <0 表示 usage 缺失 → 显示 "-"
	status   int

	// usage 上游 usage 对象的原样引用（流式来自末帧，同步来自聚合响应）；
	// nil 表示上游没给 usage。扩展字段（credit/推理/缓存）在落盘时现场解析，
	// 刻意不在此处缓存解析结果：usage 只被引用、不复制，落盘是一次性的。
	usage map[string]any

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRowProvider(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.provider, s.status, s.toks, s.usage)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage 精确值（completion_tokens
// 及各扩展字段），并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br       *bufio.Reader
	start    time.Time
	ttfb     time.Duration
	seen     bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage bool // 末帧是否带 usage
	tokens   int
	usage    map[string]any // 末帧 usage 原样保留，供扩展字段解析
	pend     []byte         // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// Usage 返回末帧 usage 对象的原样引用（无 usage 时为 nil）。
// 返回的是 map 引用而非副本：调用方只读，且请求结束后该对象不再被写入。
func (s *chatStatsReader) Usage() map[string]any { return s.usage }

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.usage = chunk.Usage
	// completion_tokens 缺失时保持 0（与原有「末帧 usage 覆盖前值」的行为一致）。
	// 用 upstream.UsageInt 而不是裸 float64 断言：与 completionTokens 同因 ——
	// 该值门控着扩展字段，不该因为"数字被字符串化"就把 credit 一起丢掉。
	s.tokens = upstream.UsageInt(chunk.Usage["completion_tokens"])
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
//
// 用 upstream.UsageInt 而不是直接断言 float64：这个返回值是**哨兵**（-1 表示
// usage 缺失），而它同时**门控**着 credit/推理/缓存三个新字段的落盘
// （见 logChatRow）。若只认 float64，上游一旦把 completion_tokens 字符串化，
// 就会连"能读的 credit"一起被丢掉 —— 解析器比它旁边的提取器宽容，这里对齐。
// 真正的"缺 usage"仍返回 -1，哨兵语义不变。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"]
	if !ok {
		return -1
	}
	return upstream.UsageInt(v)
}

// usageOf 从 Aggregate 返回的响应中取 usage 对象；缺失返回 nil。
func usageOf(resp map[string]any) map[string]any {
	u, _ := resp["usage"].(map[string]any)
	return u
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（单上游形态，provider 留空）。
//
// 保留这个 8 参数签名是为了**向后兼容**：既有测试直接调它，
// 且单上游部署下确实没有 provider 可标。多上游路径用 logChatRowProvider。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int, usage map[string]any) {
	logChatRowProvider(ttfb, total, model, mode, uid, "", status, toks, usage)
}

// logChatRowProvider 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。provider 为空表示未标注。
//
// usage 为上游 usage 对象（nil = 缺失）。扩展字段（credit/推理 token/缓存命中未命中）
// 由此处解析并写入环形缓冲：**只有 toks>=0（usage 存在）时才填**，
// usage 缺失时保持 0 而不是 -1 —— 新字段没有哨兵语义，-1 会污染后续求和聚合。
func logChatRowProvider(ttfb, total time.Duration, model, mode, uid, provider string, status int, toks int, usage map[string]any) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	// 同时进环形缓冲（/admin/logs 的数据源）。这里用完整 model/uid，
	// 截断只影响 stdout 表格的排版，不应污染可供追溯的结构化数据。
	entry := logbuf.Entry{
		At:       time.Now(),
		Model:    model,
		Mode:     mode,
		Status:   status,
		UID:      uid,
		Provider: provider,
		TTFBMS:   ttfb.Milliseconds(),
		Tokens:   toks,
		TotalMS:  total.Milliseconds(),
	}
	// tokens<0 是「usage 缺失」的哨兵；此时 usage 对象即使非 nil 也不可信
	// （例如上游给了半截 usage），扩展字段一律留 0。
	if toks >= 0 {
		x := upstream.ParseUsageExtras(usage)
		entry.Credit = x.Credit
		entry.ThinkTokens = x.ThinkTokens
		entry.CacheHitTokens = x.CacheHitTokens
		entry.CacheMissTokens = x.CacheMissTokens
	}
	chatLogRing.Push(entry)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
