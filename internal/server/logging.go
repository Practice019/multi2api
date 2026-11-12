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

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/logx"
	"workbuddy2api/internal/wire"
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
	// decision 本请求的**重试/换号判决留痕**（`reason:source`，见 logbuf.Entry.Decision）。
	// ⚠ 它由出口层在**每一个判决点**写（粘性命中失败 / 池子选不出 / 预算用尽…），
	//   成功时留空。⚠ 这是排障时唯一需要而原来没有的信息。
	decision string

	// usage 上游 usage 对象的原样引用（流式来自末帧，同步来自聚合响应）；
	// nil 表示上游没给 usage。扩展字段（credit/推理/缓存）在落盘时现场解析，
	// 刻意不在此处缓存解析结果：usage 只被引用、不复制，落盘是一次性的。
	usage map[string]any

	// apikeyID 本次请求鉴权命中的 API key（空 = 管理 key / 未启用多 key）。
	// bump 请求出口时把用量记给该 key（nil = 不计）。
	apikeyID string
	bump     func(toks int, ok bool)

	// noteLatency 把本次请求的首字节耗时喂给上游级统计（2026-10-18 加）。
	// ⚠ 用回调而不是直接调 Pool：`chatStat` 拿不到 Handler（与 `bump` 同一取舍）。
	// ⚠ 只在**成功**请求上喂：失败时的耗时是"失败前的等待"，不能代表这家上游有多快。
	noteLatency func(provider string, ms int64)

	// preMs 从请求进入网关 → 调用 chatVia（真正发上游）之间的耗时。
	// ⚠ 它是"我们自己花了多少时间"的唯一直接证据，见 logbuf.Entry.PreMs 的注释。
	preMs time.Duration
	// clientIP / clientUA 请求来源（纯观测，见 logbuf.Entry.ClientIP 的注释）。
	clientIP string
	clientUA string

	// attempts / spentMs / lastFail 换号重试的**观测**（2026-11-02 加）。
	//
	// 起因：用户直连上游实测"单次尝试 40 秒"（34.2s / 46.2s / 成功也要 38.9s），
	// 而网关只回一句含糊的 503 —— 看不到"试了几个号、一共等了多久、最后一次为什么失败"，
	// 只能翻容器日志。这三项在失败收尾时拼进 decision，一眼可判"是上游慢还是池子空"。
	attempts int
	spentMs  int64
	lastFail string
	// upstreamReqRef 实际发给上游的请求体（**引用，不拷贝**；仅失败时由
	// logChatRowProvider 转成 string 并落盘）。
	//
	// ⚠ 为什么是 []byte 而不是 string：body 实测可达 135 KB，`string(bytes)` 是
	//   完整拷贝。而 99% 的请求是成功的（成功不写这个字段）—— 为了 1% 的失败
	//   让 100% 的请求付一次拷贝，是第一版的真实缺陷（当时注释还写着"不拷贝"）。
	//   引用在本请求生命周期内一直有效（outBody 由 handler 持有到循环结束）。
	upstreamReqRef []byte
	// reqDump 落盘器（装配层注入，nil = 不落盘）。
	reqDump *logbuf.ReqDump
	// upstreamErr 上游错误**原文**（未脱敏；脱敏与截断在 logChatRowProvider 里统一做）。
	//
	// 与 lastFail 的分工：lastFail 是"一句话摘要"（进 decision 串，便于 grep），
	// upstreamErr 是"原文"（进独立字段，便于看清为什么）。两者都要 ——
	// 摘要可读性好，原文信息全（2026-11-09 codar 403 就是缺原文才无法定性）。
	upstreamErr string

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

// upstreamReqForLog 只在**失败**时把请求体转成 string（成功请求零成本）。
//
// # 为什么单独抽一个方法
//
// 请求体以 []byte 引用形式存着（见 upstreamReqRef 的注释），转 string 是完整拷贝。
// 若在 done() 里无条件转，就等于"为了 1% 的失败让 100% 的请求付一次拷贝"——
// 那正是第一版的缺陷。这里把转换关在失败分支里。
//
// # 重试时的语义
//
// 重试循环里每次尝试都会刷新这个引用（每个账号拿到的是同一份 outBody，
// 所以实际值相同）。若中途换了上游/改了 model，最后留下的是**最后一次尝试**
// 发出去的那份 —— 与 upstreamErr（也是记最后一次失败）口径一致。
func (s *chatStat) upstreamReqForLog() string {
	if s == nil || len(s.upstreamReqRef) == 0 || s.status < 400 {
		return ""
	}
	return string(s.upstreamReqRef)
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	// API key 用量记录（请求出口，任何路径都会走到）：
	// 成功按实际 token 计、失败计失败数；usage 缺失（toks<0）按 0 计。
	if s.bump != nil {
		toks := s.toks
		if toks < 0 {
			toks = 0
		}
		s.bump(toks, s.status >= 200 && s.status < 300)
	}
	// 失败请求补上"试了几个号 / 一共等了多久 / 最后一次为什么失败"（2026-11-02 加）。
	//
	// 起因：用户直连上游实测**单次尝试 40 秒**（34.2/46.2/成功也要 38.9s），
	// 而网关只回一句含糊的 503 —— 日志里看不到"试了几个号、一共等了多久、
	// 最后一次为什么失败"，只能翻容器日志。现在一行就能读到：
	//	attempts=3 spentMs=118000 last=loomy server 408
	//
	// ⚠ 只在**失败**时追加：成功请求的 decision 留空是既有契约（见 chatStat.decision 的注释）。
	decisionForLog := s.decision
	if s.status >= 400 && s.attempts > 0 {
		// ⚠ spentMs 是**池循环内**的耗时（预算的度量对象），不含循环外的选号/转换/收尾。
		// 真机实测：客户端总耗时 110.6s，而 spentMs=70436ms —— 差额就是循环外开销。
		// 两个数都写进日志，避免"日志说 70 秒、客户端等 110 秒"这种口径分歧：
		//   spentMs=…  → 预算用掉多少（决定"还该不该再试"的那个数）
		//   totalMs=…  → 客户端实际等了多久（已由 entry.TotalMS 记录）
		decisionForLog = appendAttemptTrace(decisionForLog, s.attempts, s.spentMs, s.lastFail)
	}
	logChatRowProvider(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.provider, s.status, s.toks, s.usage, decisionForLog, s.upstreamReqForLog(), s.upstreamErr, s.reqDump, s.preMs.Milliseconds(), s.clientIP, s.clientUA)

	// ⚠ 喂上游级"首字节耗时"统计（2026-10-18 加）。
	//
	// ⚠ 放在 done() 而不是成功路径的某一句：这里**覆盖所有出口**
	// （同步、流式、工具循环、降级重试…），而只要漏一条，统计就会偏。
	//
	// ⚠ 非流式请求没有独立的"首字节"时刻（stats 的 TTFB 为 0）——
	// 用**总耗时**代替：对它而言两者本来就是一回事（收完才拿到全部）。
	// ⚠ 不做这个兜底的话，只发非流式的部署会永远拿不到速度数据
	//（实测踩到：发 stream:false 两个请求后 /status 的 providers 是空列表）。
	if s.noteLatency != nil && s.provider != "" && s.status >= 200 && s.status < 300 {
		ms := s.ttfb.Milliseconds()
		if ms <= 0 {
			ms = time.Since(s.start).Milliseconds()
		}
		s.noteLatency(s.provider, ms)
	}
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

// SetReader 把底层流换成 rc（nil 时以空流占位）。
//
// # 为什么需要它（工具循环的场景）
//
// 工具循环要先建统计器、**之后**才拿到上游流（每一轮一个流）。
// 用一个"可换底层流"的包装器，就能让统计逻辑与普通路径**共用同一份**
// （而不是在工具循环里重写一遍 usage 解析 —— 那份知识只该有一个实现）。
//
// ⚠ 只在**还没读过任何数据**时安全。工具循环每轮换一次流，而每轮的
// 统计要累积（TTFB 只记一次、tokens 用最后一轮的值）—— 所以换流时
// 刻意**不重置** seen/tokens/usage，让它们跨轮累积。
func (s *chatStatsReader) SetReader(r io.Reader) {
	if r == nil {
		r = strings.NewReader("")
	}
	s.br = bufio.NewReaderSize(r, 64*1024)
	s.pend = nil
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
	// 用 wire.UsageInt 而不是裸 float64 断言：与 completionTokens 同因 ——
	// 该值门控着扩展字段，不该因为"数字被字符串化"就把 credit 一起丢掉。
	// ⚠ 2026-10-20 修：原来这里是
	//     s.tokens = wire.UsageInt(chunk.Usage["completion_tokens"])
	// 即**流式账号池路径也只取输出**，prompt 全丢。
	//
	// 实测后果（2026-10-20 11:13，重启到 5efd4b9 之后仍复现）：
	//   tokens=2   out=2   hit=640  miss=13   ← 真实处理量应是 655
	//   tokens=20  out=20  hit=640  miss=13   ← 真实应是 673
	// 而同一时刻 csg（走渠道）是 tokens=530751 out=1147 ✓ 对 ——
	// 因为渠道路径用的是 sseUsage.total()（f46cdcd 修过）。
	//
	// ⇒ 加上 5efd4b9 修的非流式那处，这是**同一个 bug 的第三个副本**。
	//   三条路（渠道 / 账号池流式 / 账号池非流式）必须共用一个算法，
	//   所以这里也走 totalTokens()，不再各自解析字段。
	s.tokens = totalTokens(map[string]any{"usage": chunk.Usage})
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
// 用 wire.UsageInt 而不是直接断言 float64：这个返回值是**哨兵**（-1 表示
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
	return wire.UsageInt(v)
}

// totalTokens 一条非流式响应的**总 token**（照 new-api 的算法自己加）。
//
// ⚠ 2026-10-20 修：账号池的非流式路径原来用 `completionTokens(resp)` 当 toks，
// 那**只取 completion_tokens，把 prompt 全丢了**。实测后果（2026-10-20 11:05 两条
// loomy 请求）：
//
//	tokens=3  out=3  hit=640  miss=16      ← toks 等于输出，而真实处理量是 659
//
// 渠道路径早就在用采集器算（`cap.Result()` → `sseUsage.total()`，已按 new-api 改成
// promptTotal + completion），但账号池非流式这条路漏了 —— 两条路的算法必须一致，
// 否则同一条上游、同样一次调用，走渠道和走账号池会记出差几十倍的数。
//
// 算法与 `sseUsage.total()` 逐条对应：
//  1. 能还原总输入（prompt_tokens，或缓存命中+未命中）⇒ promptTotal + completion
//  2. 输入完全无从得知 ⇒ 退回 total_tokens（那种情况下它是唯一线索）
//  3. 连 total_tokens 都没有 ⇒ 只有 completion（下界，但至少不是 -1）
func totalTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1 // usage 缺失：保留 -1 哨兵，与 completionTokens 一致
	}
	x := wire.ParseUsageExtras(u)

	// ① 总输入：优先上游给的 prompt_tokens（它已含缓存，不能再加缓存）
	pt := wire.UsageInt(u["prompt_tokens"])
	// ⚠ 2026-11-12 兜底：Anthropic 系上游给的是 `input_tokens`（一个 OpenAI 键都没有），
	//   而它把 OpenAI 形态另塞在 `billing_usage.openai_usage` 里（**上游自己的方言**）。
	//   判据与全仓一致：**这一级给出了这个键**才采用，不是"上一个值为 0 就换一个"。
	//   症状出处：日志行 `tok=0`，而同一次响应里 input_tokens:39 / output_tokens:32 都在。
	if _, has := u["prompt_tokens"]; !has {
		if _, hasIn := u["input_tokens"]; hasIn {
			pt = wire.UsageInt(u["input_tokens"])
		} else {
			pt = wire.UsageIntAt(u, "billing_usage", "openai_usage", "prompt_tokens")
		}
	}
	// ⚠ 上面（以及 ③′）这两级兜底读的是**上游方言**，只对「未经协议转换的形态」有效：
	//   响应若先过了 `anthroconv`，方言会被 `mapUsage` 丢掉（anthropic.go:671-687 只返回三个键的新 map）。
	//   ⇒ 那类上游的 `think_tokens` 恒为 0（**信息缺失，不是漏读**）；完整机制与为什么不去改
	//   转换层，口径出处只有一处：`internal/wire/usage.go` 推理取值链第 ③ 级上方的注释。
	// ② 只有缓存两项时才用它们还原（互斥的输入划分）
	if pt <= 0 && x.CacheHitTokens > 0 && x.CacheMissTokens > 0 {
		pt = x.CacheHitTokens + x.CacheMissTokens
	}
	if pt > 0 {
		return pt + x.CompletionTokens
	}
	// ③ 输入无从得知
	if t := wire.UsageInt(u["total_tokens"]); t > 0 {
		return t
	}
	// ③′ 同上：上游方言里的 total_tokens（真机夹具里它在 billing_usage.openai_usage）。
	if t := wire.UsageIntAt(u, "billing_usage", "openai_usage", "total_tokens"); t > 0 {
		return t
	}
	return x.CompletionTokens
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
	logChatRowProvider(ttfb, total, model, mode, uid, "", status, toks, usage, "", "", "", nil, 0, "", "")
}

// tokPerSecField 组装终端表格的 tok/s 列。
//
// # 口径与网页端 app.js 的 tokPerSec **逐条一致**（2026-11-12 修）
//
// 旧实现是 `toks / total`：分子是 totalTokens()（**含 prompt** 的总处理量），
// 分母是含 TTFB 的总耗时。而网页端用的是 `out_tokens / (total − TTFB)`。
// ⇒ 同一条请求两个数。实测（用户 2026-11-12 的日志）一条 prompt 约 4.2 万的请求，
//
//	终端打 19255 tok/s —— 那其实是"总处理量 ÷ 总耗时"的吞吐，不是生成速度；
//	而 `tok/s` 这个名字、以及仓库夹具里的判据（"契约必须与 stdout 表格一致，
//	否则同一条请求在终端与网页上会显示两个不同的速率"）都指向"生成速度"。
//
// 精度也照网页的 fmtTps：≥10 取整、<10 一位小数。
// ⚠ 算式与精度必须**同时**一致：只对齐算式、精度仍两套，同一条请求照样两个数。
func tokPerSecField(mode string, outTokens int, total, ttfb time.Duration) string {
	if mode != "" && mode != "stream" {
		return "-" // 非流式：没有"首字之后的生成阶段"这回事（与前端同一条判据）
	}
	if outTokens <= 0 {
		return "-" // 上游没给 completion_tokens：不能拿总处理量冒充输出
	}
	if total <= 0 {
		return "0.0" // 时钟粒度内完成：显示 0.0 而不是 Inf/NaN
	}
	gen := total - ttfb
	if gen <= 0 {
		return "-" // 首字即末字：除零会得到 Inf
	}
	return fmtTpsGo(float64(outTokens) / gen.Seconds())
}

// fmtTpsGo 是 app.js `fmtTps` 的 Go 版：≥10 取整、<10 保留一位小数。
// 来源是 DSH 官方 formatTokensPerSecond 的规则（大数不需要小数位，小数才需要）。
func fmtTpsGo(v float64) string {
	// ⚠ 2026-11-12：实现搬到 logx.FmtTps —— **唯一实现**。这里保留函数名（与它的测试）
	//   只是转调，避免同一条规则出现第二份拷贝（那正是 tok/s 口径分叉的成因）。
	return logx.FmtTps(v)
}

// logChatRowProvider 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。provider 为空表示未标注。
//
// usage 为上游 usage 对象（nil = 缺失）。扩展字段（credit/推理 token/缓存命中未命中）
// 由此处解析并写入环形缓冲：**只有 toks>=0（usage 存在）时才填**，
// usage 缺失时保持 0 而不是 -1 —— 新字段没有哨兵语义，-1 会污染后续求和聚合。
// logChatRowProvider 记一条请求日志。extra 是**可选**的渠道维度：
// extra[0] = 渠道 id，extra[1] = 改写后的上游模型名（都没有 = 走的是账号池）。
//
// ⚠ 为什么用变参而不是加两个正式参数：这个函数有十来个调用点，而渠道是**后加**的维度。
// 改签名会让每个调用点都来一遍"补两个空串"的机械改动 —— 那种改动最容易顺手改错一个。
func logChatRowProvider(ttfb, total time.Duration, model, mode, uid, provider string, status int, toks int, usage map[string]any, decision, upstreamReq, upstreamErr string, reqDump *logbuf.ReqDump, preMs int64, clientIP, clientUA string, extra ...string) {
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
	// ⚠ 判决留痕、发上游前的耗时、请求来源（见 logbuf.Entry 三个字段的注释）。
	entry.Decision = decision
	// 上游原文：**在这里统一脱敏 + 截断**，调用方只管把原文递进来。
	//
	// ⚠ 脱敏理由见 logbuf.Entry.UpstreamErr 的注释（会渲染到网页，不能带 host/IP）。
	// ⚠ 只在非 2xx 时记：成功请求的上游 body 是正常流量，记它等于把日志当日志+报文双写。
	if status >= 400 {
		if upstreamErr != "" {
			entry.UpstreamErr = logbuf.TruncateUTF8(gateway.MaskErrorText(upstreamErr), logbuf.UpstreamErrMaxLen)
		}
		// 请求体：**只脱敏一次**，两个消费方（JSONL 头尾 + 完整落盘文件）共用。
		//
		// ⚠ 第一版对同一份 body 调了两次 MaskErrorText（JSONL 一次、落盘一次），
		//   而那是 8 个正则 + url.Parse 的流水线，body 实测可达 135 KB ⇒ 白付一倍 CPU。
		//   这里先算一次 masked，再分别截断/落盘。
		if upstreamReq != "" {
			masked := gateway.MaskErrorText(upstreamReq)
			// JSONL：头尾各留一半（判据分布在两端：头部有 model/tools，尾部有最后一条 user 消息）。
			entry.UpstreamReq = logbuf.TruncateHeadTail(masked, logbuf.UpstreamReqMaxLen)
			// 完整 body 落到**独立文件**（不进 JSONL：整份 body 可达数百 KB，
			// 既会翻几十倍日志体积，也会撑爆 /admin/logs 的环形缓冲）。
			//
			// ⚠ 只在配置了落盘器时执行（装配层注入 nil = 不落盘，零成本）。
			// ⚠ 只有**失败**才落（这个分支本身已在 status>=400 里）。
			if reqDump != nil {
				reqDump.Write(model, provider, itoaStatus(status), map[string]string{
					"seq":      itoa64(entry.Seq),
					"mode":     mode,
					"uid":      uid,
					"decision": decision,
				}, []byte(masked))
			}
		}
	}
	entry.PreMs = preMs
	entry.ClientIP = clientIP
	entry.ClientUA = clientUA
	// 渠道维度（没有 = 账号池路径，见 logbuf.Entry.Channel 的注释）
	if len(extra) > 0 {
		entry.Channel = extra[0]
	}
	if len(extra) > 1 {
		entry.UpstreamModel = extra[1]
	}
	// tokens<0 是「usage 缺失」的哨兵；此时 usage 对象即使非 nil 也不可信
	// （例如上游给了半截 usage），扩展字段一律留 0。
	if toks >= 0 {
		x := wire.ParseUsageExtras(usage)
		entry.Credit = x.Credit
		entry.ThinkTokens = x.ThinkTokens
		entry.CacheHitTokens = x.CacheHitTokens
		// 纯输出 token：tok/s 的分子。上游没给 completion_tokens 时这里为 0，
		// 前端据此显示 "—" 而不是拿 total 硬算（见 logbuf.Entry.OutTokens 的注释）。
		entry.OutTokens = x.CompletionTokens
		entry.CacheMissTokens = x.CacheMissTokens
	}
	chatLogRing.Push(entry)
	// 汇总（**人看的那一层**）：控制台只留周期性关键数字，见 internal/logx/stats.go。
	logx.Observe(logx.Outcome{
		Provider: provider,
		Status:   status,
		OutToks:  entry.OutTokens,
		TTFB:     ttfb,
		Total:    total,
	})
	if len(model) > 11 {
		model = model[:11]
	}
	// ⚠ 逐条请求行**照旧打**（2026-11-12 用户明确要求）：
	//   汇总是"另外一条"，不是"替代它" —— 人既要看"什么在发生"，也要看"总体怎么样"。
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		// ⚠ 2026-11-12：tok/s 改成**与网页端同一口径**（app.js 的 tokPerSec）——
		//   分子用 entry.OutTokens（纯输出 token，不是 toks 那份"总处理量"），分母扣掉 TTFB。
		//   旧实现 `toks/total` 会让同一条请求在终端与网页显示两个不同的速率。
		tokpsField = tokPerSecField(mode, entry.OutTokens, total, ttfb)
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

// itoaStatus / itoa64 小工具：避免为两个数字引入 strconv 的依赖面。
func itoaStatus(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
