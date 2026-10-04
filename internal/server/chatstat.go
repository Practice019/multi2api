// chatstat.go 工具循环路径的**统计回传**。
//
// # 为什么需要它（用户报"TTFB / tok / tok/s 三个都空了"）
//
// 正常 chat 路径把上游流包在 `chatStatsReader` 里读（见 handler.go 的
// `stats := newChatStatsReaderSince(rc, st.start)`），读完后把三个数写进
// `chatStat`：
//
//	st.ttfb  = stats.TTFB()    首帧到达耗时
//	st.toks  = stats.Tokens()  usage.completion_tokens
//	st.usage = stats.Usage()   末帧 usage 对象（credit/推理/缓存字段都由它来）
//
// 而工具循环是**另一条**读取路径（自己用 wire.NewFrameScanner 逐帧读），
// 它完全绕过了 chatStatsReader —— 于是那三个字段没人填，
// `chatStat.toks` 停在构造时的哨兵值 `-1`，日志三列全渲染成 `-`。
//
// 这是"换了一条读取路径，忘了把旁路的记账接回来"的典型形态：
// 功能（生图/搜索）看起来完全正常，只有日志悄悄空了。
//
// # 修法：**复用** chatStatsReader，而不是自己再解析一遍 usage
//
// chatStatsReader 已是 `io.Reader` 包装器 —— 把它套在上游 body 上，
// 再拿它的输出去喂 FrameScanner 即可。这样：
//
//	统计判据只有一份（sseData 的教训：两处各写一遍必然漂移）
//	TTFB/tokens/usage 的语义与普通路径**逐字节一致**
//	不用在本文件里重写"什么算 usage、completion_tokens 怎么取"
//
// 所以本文件只做两件事：持有那个 reader、把它的读数搬进 chatStat。
package server

import (
	"io"
	"strings"
	"time"
)

// toolLoopStats 工具循环期间累积的统计读数。
//
// 内部包一个 chatStatsReader：上游 body 穿过它，读数就自动齐了。
type toolLoopStats struct {
	r    *chatStatsReader
	seen bool // 是否至少读过一帧（决定 TTFB 有没有意义）
}

// newToolLoopStats 造一个统计容器。
//
// since 是 TTFB 的计时起点 —— 必须是**请求进入 handler 的时刻**，
// 不能是循环内部取的时间（那会少算鉴权/选号/建连的开销，把 TTFB 报小）。
func newToolLoopStats(since time.Time) *toolLoopStats {
	return &toolLoopStats{r: newChatStatsReaderSince(strings.NewReader(""), since)}
}

// wrap 把上游 body 包进统计 reader，返回可直接读的流。
//
// ⚠ 调用方必须读**返回值**（而不是原 body），否则统计收不到数据 ——
// 这正是本文件要修的 bug 的形态。
func (s *toolLoopStats) wrap(rc io.Reader) io.Reader {
	if s == nil || s.r == nil {
		return rc
	}
	s.r.SetReader(rc)
	return s.r
}

// ttfb 首帧到达耗时。
func (s *toolLoopStats) ttfb() time.Duration {
	if s == nil || s.r == nil {
		return 0
	}
	return s.r.TTFB()
}

// applyTo 把统计写进 chatStat。
//
// # 只在**确实读到过东西**时覆盖
//
//	toks >= 0 → 写（-1 是"usage 缺失"哨兵，留着它日志显示 `-`，语义正确）
//	ttfb  > 0 → 写
//
// 这样"工具循环没读到 usage"与"普通路径没读到 usage"表现一致，
// 不会出现"工具循环谎报 0 token"。
//
// ⚠ 注意 st.toks 的初值是 -1（newChatStat 里设的哨兵）。本方法不把它
// 改成 0 —— 0 是"这次真的用了 0 个 token"，与"上游没给 usage"是两回事。
func (s *toolLoopStats) applyTo(st *chatStat) {
	if s == nil || st == nil || s.r == nil {
		return
	}
	if ttfb := s.r.TTFB(); ttfb > 0 {
		st.ttfb = ttfb
	}
	if toks, has := s.r.Tokens(); has {
		st.toks = toks
	}
	if u := s.r.Usage(); u != nil {
		st.usage = u
	}
}
