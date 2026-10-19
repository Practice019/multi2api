// stream_usage_test.go 采集器的四条判据（2026-10-20）。
//
// 测试设计的原则：**每条都对应一个"如果我们想错了会静默出错"的场景**。
// 采集器最坏的失败方式不是返回错数字，而是永远返回"没有 usage"且不报错 ——
// 那样日志里 tokens 全是 -1，页面上看是空的，但没有任何异常可查。
package server

import (
	"strings"
	"testing"
	"time"
)

// sseJoin 把若干帧拼成合法 SSE 文本（空字符串表示发一个空 data 行）。
func sseJoin(frames ...string) string {
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("data: ")
		sb.WriteString(f)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// feedAll 一次喂入。
func feedAll(s *streamUsage, chunks ...string) {
	for _, c := range chunks {
		s.Feed([]byte(c))
	}
}

// TestStreamUsageLastFrame 形态①：末帧带完整 usage。
//
// ⚠ ttfb 断言必须**注入更早的 start**，不能拿 time.Now() 当基准等几微秒：
// Windows 的 time.Now() 精度约 1ms，同一毫秒内 time.Since() 会返回 0，
// 于是"ttfb > 0"变成碰运气的断言（第一次跑碰巧差值 > 0 就过，CI 上抖一下就红）。
// 这不是产品问题 —— 真实网络请求的 TTFB 远大于 1ms，0 只可能出现在合成测试里。
func TestStreamUsageLastFrame(t *testing.T) {
	body := sseJoin(
		`{"choices":[{"delta":{"content":"你好"}}]}`,
		`{"choices":[{"delta":{"content":"世界"}}]}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
	)
	s := newStreamUsage(time.Now().Add(-5 * time.Millisecond))
	feedAll(s, body)
	tokens, ttfb, src := s.Result()
	if src != usageFromLastFrame {
		t.Fatalf("来源应为 %q，实际 %q", usageFromLastFrame, src)
	}
	if tokens != 30 {
		t.Fatalf("token 应为 total_tokens 30，实际 %d", tokens)
	}
	if ttfb < 5*time.Millisecond {
		t.Fatalf("ttfb 应 ≥ 注入的 5ms（说明首帧时刻确实按 start 计算），实际 %v", ttfb)
	}
}

// TestStreamUsageSecondLastFrame 形态②：**完整 usage 附在倒数第二帧，末帧是空事件**。
//
// 这是 new-api 源码注释专门写的一条兼容性事实：
// 「部分兼容网关把完整的累计 usage 附在倒数第二个事件上，随后发送一个空的最后事件」。
// 如果不做这一级回退，这类上游的每一条请求都会走到估算 —— 数据系统性偏低。
func TestStreamUsageSecondLastFrame(t *testing.T) {
	body := sseJoin(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}`,
		`{"choices":[{"delta":{}}]}`, // 末帧空，无 usage
	)
	s := newStreamUsage(time.Now())
	feedAll(s, body)
	tokens, _, src := s.Result()
	if src != usageFromSecondLast {
		t.Fatalf("应回退到倒数第二帧（%q），实际 %q", usageFromSecondLast, src)
	}
	if tokens != 18 {
		t.Fatalf("token 应为 18，实际 %d", tokens)
	}
}

// TestStreamUsageEstimatesWhenMissing 形态③：全程无 usage ⇒ 按输出文本估算，绝不返回 0/-1。
func TestStreamUsageEstimatesWhenMissing(t *testing.T) {
	body := sseJoin(
		`{"choices":[{"delta":{"content":"这是一段中文输出内容，用来估"}}]}`,
		`{"choices":[{"delta":{"content":"算 token 数量。"}}]}`,
		`[DONE]`,
	)
	s := newStreamUsage(time.Now())
	feedAll(s, body)
	tokens, _, src := s.Result()
	if src != usageEstimated {
		t.Fatalf("来源应为 %q，实际 %q", usageEstimated, src)
	}
	if tokens <= 0 {
		t.Fatalf("估算结果必须 > 0（0 会被聚合当成'没花钱'），实际 %d", tokens)
	}
	// 中文按 1.5 字/token：这里约 19 个字 ⇒ 13 左右。给一个宽松区间，
	// 钉的是"在同一量级"，不是"精确等于某数"（估算法本来就不精确）。
	if tokens < 8 || tokens > 30 {
		t.Fatalf("中文估算应在 8~30 量级，实际 %d", tokens)
	}
}

// TestStreamUsageEmptyStream 形态④：空流 ⇒ Unavailable 且 token = 0（调用方据此记 -1）。
func TestStreamUsageEmptyStream(t *testing.T) {
	s := newStreamUsage(time.Now())
	feedAll(s, "")
	tokens, ttfb, src := s.Result()
	if src != usageUnavailable {
		t.Fatalf("空流来源应为 %q，实际 %q", usageUnavailable, src)
	}
	if tokens != 0 {
		t.Fatalf("空流 token 应为 0，实际 %d", tokens)
	}
	if ttfb != 0 {
		t.Fatalf("空流没有有效帧，ttfb 应为 0，实际 %v", ttfb)
	}
}

// TestStreamUsageSurvivesChunkSplit 事件被 TCP 分片切断时必须仍能解析。
//
// ⚠ 这是整个采集器最容易被写错的地方：按 chunk 边界盲目解析，
// 会让几乎每一帧都解析失败，而失败是静默的 —— 表现就是"永远没有 usage"。
// 这里把每个事件在**任意位置**切成两半个字符喂，验证结果与整块喂入一致。
func TestStreamUsageSurvivesChunkSplit(t *testing.T) {
	body := sseJoin(
		`{"choices":[{"delta":{"content":"abc"}}]}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
	)
	// 逐字节喂（最坏情况：每个字节一个 chunk）
	s := newStreamUsage(time.Now())
	for i := 0; i < len(body); i++ {
		s.Feed([]byte{body[i]})
	}
	tokens, _, src := s.Result()
	if src != usageFromLastFrame {
		t.Fatalf("逐字节分片后来源应为 %q，实际 %q（说明跨 chunk 缓冲失效）", usageFromLastFrame, src)
	}
	if tokens != 8 {
		t.Fatalf("逐字节分片后 token 应为 8，实际 %d", tokens)
	}
}

// TestStreamUsageRejectsPartialUsage 只有 prompt_tokens 的半截 usage 不能被采用。
//
// 判据照 new-api：PromptTokens > 0 **且**（CompletionTokens > 0 或 TotalTokens > 0）。
// 为什么要这么严：兼容网关会发 `{"usage":{"prompt_tokens":123}}` 这种半截快照，
// 直接采用会把输出当 0 —— 那比"没有"更糟，因为页面上看不出错。
func TestStreamUsageRejectsPartialUsage(t *testing.T) {
	body := sseJoin(
		`{"choices":[{"delta":{"content":"x"}}]}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":123}}`, // 缺输出侧
	)
	s := newStreamUsage(time.Now())
	feedAll(s, body)
	_, _, src := s.Result()
	if src == usageFromLastFrame {
		t.Fatalf("半截 usage 不应被当成末帧有效用量（来源 %q）", src)
	}
	if src != usageEstimated {
		t.Fatalf("应落到估算（%q），实际 %q", usageEstimated, src)
	}
}

// TestEstimateTokensCJKvsLatin 估算系数：中文不该按英文的 4 字符/token 算。
//
// 单一系数在中文场景会低估近 3 倍 —— 这类偏差不会让页面报错，
// 只会让"消耗 token"这个数系统性地偏小，用户拿去核对账单才发现。
func TestEstimateTokensCJKvsLatin(t *testing.T) {
	cjk := estimateTokens(strings.Repeat("中", 12), 0)   // 12 字中文 ⇒ 约 8
	latin := estimateTokens(strings.Repeat("a", 12), 0) // 12 字符英文 ⇒ 约 3
	if cjk <= latin {
		t.Fatalf("同样长度下中文估算(%d)应大于英文(%d)", cjk, latin)
	}
	if cjk < 6 || cjk > 12 {
		t.Fatalf("12 个中文字符应估出 6~12 token，实际 %d", cjk)
	}
}

// TestEstimateTokensToolCallCompensation 工具调用按每次 7 token 补偿（照 new-api）。
func TestEstimateTokensToolCallCompensation(t *testing.T) {
	base := estimateTokens("abc", 0)
	withTools := estimateTokens("abc", 2)
	if withTools-base != 14 {
		t.Fatalf("两个工具调用应补 14 token，实际补了 %d", withTools-base)
	}
}
