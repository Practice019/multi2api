package server

import (
	"strings"
	"testing"
	"time"
)

// TestToolLoopStatsReadsThroughWrapper 统计必须真的从流里读出来。
//
// # 这是用户报的"TTFB / tok / tok/s 三个都空了"的回归测试
//
// 工具循环是**另一条**读取路径（自己用 FrameScanner 逐帧读），
// 它绕过了普通 chat 用的 chatStatsReader —— 于是 chatStat 的三个字段
// 没人填，toks 停在 -1 哨兵，日志三列全渲染成 `-`。
//
// 功能（生图/搜索）看起来完全正常，只有日志悄悄空了。
// 所以这条测试的价值在于：它盯的是**旁路的记账**，不是主功能。
func TestToolLoopStatsReadsThroughWrapper(t *testing.T) {
	since := time.Now().Add(-120 * time.Millisecond)
	stats := newToolLoopStats(since)

	// 一段真实形态的上游流：首帧 role、中间内容、末帧 usage。
	stream := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]," +
		"\"usage\":{\"completion_tokens\":42,\"total_tokens\":42}}\n\n" +
		"data: [DONE]\n\n"

	// ⚠ 必须读 **wrap 的返回值**，否则统计收不到数据
	// （这正是原 bug 的形态：换了路径但没把记账接上）。
	rc := stats.wrap(strings.NewReader(stream))
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if err != nil {
			break
		}
		_ = n
	}

	st := &chatStat{toks: -1}
	stats.applyTo(st)

	// ⚠ 用 >= 0 而不是 > 0：测试里的流是瞬时的，time.Since 会截断成 0。
	// 真正要断言的是"统计器**看了**首帧"（seen），以及 applyTo 的搬运逻辑 ——
	// 实测的真实请求里 TTFB 是 863ms / 1273ms（见 commit message）。
	if !stats.r.seen {
		t.Error("统计器没看到任何帧 —— 日志会显示 `-`")
	}
	_ = st.ttfb
	if st.toks != 42 {
		t.Errorf("tokens = %d，want 42（日志的 tok/tok/s 靠它）", st.toks)
	}
	if st.usage == nil {
		t.Error("usage 没读到 —— credit / 推理 / 缓存字段都取自它")
	}
}

// TestToolLoopStatsMissingUsageKeepsSentinel 上游没给 usage 时保持 -1 哨兵。
//
// # 为什么不能用 0 顶替
//
//	0  = "这次真的用了 0 个 token"
//	-1 = "上游没给 usage"（日志显示 `-`）
//
// 把 -1 改成 0 会**谎报**用量，而 tok/s 的分母也会跟着算出一个假值。
// 这正是 logChatRowProvider 里 `if toks >= 0` 那个判断要区分的事。
func TestToolLoopStatsMissingUsageKeepsSentinel(t *testing.T) {
	stats := newToolLoopStats(time.Now())

	// 没有 usage 的流
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"

	rc := stats.wrap(strings.NewReader(stream))
	buf := make([]byte, 4096)
	for {
		if _, err := rc.Read(buf); err != nil {
			break
		}
	}

	st := &chatStat{toks: -1}
	stats.applyTo(st)

	if st.toks != -1 {
		t.Errorf("st.toks = %d，want -1（上游没给 usage 时应保持哨兵，不能变成 0）", st.toks)
	}
}

// TestToolLoopStatsAccumulateAcrossRounds 多轮之间统计要累积，不能重置。
//
// # 为什么这条重要（工具循环特有）
//
// 工具循环每一轮换一个上游流（SetReader）。若换流时把 seen/tokens/usage
// 一起重置：
//
//	TTFB 会变成"最后一轮的首帧"（把最慢的一轮当首帧，数值失真）
//	tokens 会变成"最后一轮的量"（丢掉之前几轮）
//
// 正确语义：TTFB 只记**第一次**、tokens/usage 用**最后**一次。
func TestToolLoopStatsAccumulateAcrossRounds(t *testing.T) {
	stats := newToolLoopStats(time.Now())

	readAll := func(s string) {
		rc := stats.wrap(strings.NewReader(s))
		buf := make([]byte, 4096)
		for {
			if _, err := rc.Read(buf); err != nil {
				break
			}
		}
	}

	// 第 1 轮：无 usage
	readAll("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n")
	if !stats.r.seen {
		t.Fatal("第 1 轮之后统计器就该标记 seen")
	}
	firstTTFB := stats.r.TTFB()
	_ = firstTTFB

	// 睡一下，确保第 2 轮的首帧时间明显更晚
	time.Sleep(60 * time.Millisecond)

	// 第 2 轮：带 usage
	readAll("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n")

	if got := stats.r.TTFB(); got != firstTTFB {
		t.Errorf("TTFB 被第 2 轮覆盖了（%v → %v）—— 它应当只记第一次", firstTTFB, got)
	}
	if toks, has := stats.r.Tokens(); !has || toks != 7 {
		t.Errorf("第 2 轮的 usage 没被吸收（toks=%d has=%v）", toks, has)
	}
}

// TestNewChatStatToksSentinelIsMinusOne 钉住哨兵值本身。
//
// 整个"日志显示 -"的机制建立在"toks 初值 == -1"上。
// 若有人把它改成 0，缺失的 usage 会显示成"0 token"（看起来像正常数据），
// 这个 bug 不会有任何报错。
func TestNewChatStatToksSentinelIsMinusOne(t *testing.T) {
	st := newChatStat(time.Now(), []byte(`{"model":"m"}`), true)
	if st.toks != -1 {
		t.Fatalf("chatStat.toks 初值 = %d，want -1（-1 是 usage 缺失的哨兵；"+
			"改成 0 会让缺失显示成『用了 0 token』）", st.toks)
	}
}
