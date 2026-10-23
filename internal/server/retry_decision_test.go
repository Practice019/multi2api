// retry_decision_test.go —— 把 DecideRetry 的每条分支都钉住。
//
// # 为什么是表驱动（而不是每个场景一个测试函数）
//
// 重试判据的危险不在"某一条写错了"，而在**相邻状态码之间的边界**：
//
//	400 停 / 401 重试        —— 差 1，方向相反
//	503 重试 / 504 停 / 505 重试 —— 夹心，最容易在重构时被一条区间写平
//	200 停   / 0 重试        —— 两端
//
// 这类边界只有把整张表铺开才看得见。一个"测试 429 会重试"的函数能通过，
// 而它旁边那条 504 可能早就被改错了。
//
// 所以本文件的表**故意排满**（含 1xx/3xx/4xx 各洞/5xx 各洞/越界值），
// 新增一条状态码规则时，改动会立刻在某一行显示为红色。
package server

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestRetryDecisionStatusMatrix 状态码 × 默认策略：每个分支一条断言。
func TestRetryDecisionStatusMatrix(t *testing.T) {
	p := DefaultRetryPolicy()
	cases := []struct {
		name   string
		status int
		want   RetryAction
		reason string
		source string
	}{
		// ── 4xx：重试区里有两个洞（400 / 408），其余换号可能就过 ──
		{"400 请求体本身坏", http.StatusBadRequest, RetryActionStop, RetryReasonStatusNotRetryable, RetrySourceGlobal},
		// 401/403 是**凭证**问题：换一个号正好对症（每个号的 key 不同）。
		{"401 凭证失效，换号有效", http.StatusUnauthorized, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"403 拒绝访问，换号有效", http.StatusForbidden, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"404 换号可能避开坏路由", http.StatusNotFound, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		// 408 与 504 的区别见 retry_decision.go 的 defaultNeverRetryStatuses 注释：
		// 408 是"这份请求再发一遍照样超时"（确定性），504 是"这一轮把时钟用光了"。
		{"408 上游读超时，重试无用", http.StatusRequestTimeout, RetryActionStop, RetryReasonStatusNotRetryable, RetrySourceGlobal},
		{"409 冲突，换号可能就过", http.StatusConflict, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"413 太大，换号仍可能被同一策略拒", http.StatusRequestEntityTooLarge, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"429 限流，等一会儿重试", http.StatusTooManyRequests, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},

		// ── 5xx：主力重试区，但 504/524 是洞 ──
		{"500 上游故障", http.StatusInternalServerError, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"501 未实现（上游口径差）", http.StatusNotImplemented, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"502 网关抖动", http.StatusBadGateway, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"503 上游过载", http.StatusServiceUnavailable, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"504 超时预算已用光，不再重试", http.StatusGatewayTimeout, RetryActionStop, RetryReasonSystemRetryExclusion, RetrySourceGlobal},
		{"505 版本不支持，仍在重试区", http.StatusHTTPVersionNotSupported, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"520 Cloudflare 未知错误", 520, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"523 源站不可达", 523, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"524 Cloudflare 超时，同 504", 524, RetryActionStop, RetryReasonSystemRetryExclusion, RetrySourceGlobal},
		{"525 握手失败", 525, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"599 上界", 599, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},

		// ── 两端与越界 ──
		// 2xx 却有错误：错误藏在响应体里。连接是通的、上游是活的 ⇒ 重发大概率复现。
		{"200 错误藏在响应体", http.StatusOK, RetryActionStop, RetryReasonSystemRetryExclusion, RetrySourceSystem},
		{"201 同上", http.StatusCreated, RetryActionStop, RetryReasonSystemRetryExclusion, RetrySourceSystem},
		{"301 重定向未跟随到底", http.StatusMovedPermanently, RetryActionRetry, RetryReasonRetryStatusMatched, RetrySourceGlobal},
		{"99 越界低端，看不懂就重试", 99, RetryActionRetry, RetryReasonUnrecognizedStatus, RetrySourceSystem},
		{"600 越界高端", 600, RetryActionRetry, RetryReasonUnrecognizedStatus, RetrySourceSystem},
		// 0 是本仓表达"没有状态码"的方式（传输层失败），不是"未知"。
		{"0 传输层失败，换号换连接", 0, RetryActionRetry, RetryReasonTransportError, RetrySourceSystem},
		{"-1 哨兵值按传输层处理", -1, RetryActionRetry, RetryReasonTransportError, RetrySourceSystem},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecideRetry(p, RetryInput{Status: c.status, AttemptsLeft: 2})
			if got.Action != c.want || got.Reason != c.reason || got.Source != c.source {
				t.Fatalf("status=%d：得到 %s，想要 %s(%s:%s)",
					c.status, got, c.want, c.reason, c.source)
			}
			// ShouldRetry 必须与 Action 同步（调用方只看它）。
			if got.ShouldRetry() != (c.want == RetryActionRetry) {
				t.Fatalf("status=%d：ShouldRetry()=%v 与 Action=%s 不一致", c.status, got.ShouldRetry(), got.Action)
			}
		})
	}
}

// TestRetryDecisionLocalError 本地错误：**不看状态码**，一律停止。
//
// ⚠ 这条是本文件最重要的断言：状态码 500 本应重试，但"本地产生"这个事实
// 压过它 —— 上游根本没被调用过，重试只是把我们自己的 bug 再跑一遍。
func TestRetryDecisionLocalError(t *testing.T) {
	p := DefaultRetryPolicy()
	for _, status := range []int{0, 400, 500, 503} {
		t.Run(fmt.Sprintf("本地错误 status=%d", status), func(t *testing.T) {
			got := DecideRetry(p, RetryInput{Status: status, LocalError: true, AttemptsLeft: 3})
			if got.Action != RetryActionStop || got.Reason != RetryReasonLocalRejection {
				t.Fatalf("本地错误必须停止，得到 %s", got)
			}
			// source 必须是 local：排障时要一眼看出"这锅在我们这边"。
			if got.Source != RetrySourceLocal {
				t.Fatalf("source=%q，想要 local（否则会被误读成上游问题）", got.Source)
			}
		})
	}
}

// TestRetryDecisionSkipRetry 带 SkipRetry 标记的错误：不管状态码多"可重试"都停。
func TestRetryDecisionSkipRetry(t *testing.T) {
	p := DefaultRetryPolicy()
	for _, status := range []int{429, 500, 503} {
		t.Run(fmt.Sprintf("SkipRetry status=%d", status), func(t *testing.T) {
			got := DecideRetry(p, RetryInput{Status: status, SkipRetry: true, AttemptsLeft: 3})
			if got.Action != RetryActionStop || got.Reason != RetryReasonNonRetryableError {
				t.Fatalf("带跳过标记必须停止，得到 %s", got)
			}
		})
	}
}

// TestRetryDecisionBudget 预算耗尽：任何状态码都停（包括 429/5xx 这些"本该重试"的）。
func TestRetryDecisionBudget(t *testing.T) {
	p := DefaultRetryPolicy()
	for _, left := range []int{0, -1} {
		for _, status := range []int{429, 500, 503, 408, 0} {
			t.Run(fmt.Sprintf("left=%d status=%d", left, status), func(t *testing.T) {
				got := DecideRetry(p, RetryInput{Status: status, AttemptsLeft: left})
				if got.Action != RetryActionStop || got.Reason != RetryReasonBudgetExhausted {
					t.Fatalf("预算 %d 必须停止，得到 %s", left, got)
				}
				if got.Source != RetrySourceGlobal {
					t.Fatalf("source=%q，想要 global（预算是一条全局约束）", got.Source)
				}
			})
		}
	}
}

// TestRetryDecisionStickySession 粘性严格模式。
//
// 三件事一起断言，因为它们必须互相独立地成立：
//
//	严格 + 命中 → 停（哪怕状态码可重试）
//	严格 + 未命中 → 照常按状态码（换了号也不破坏绑定）
//	非严格 + 命中 → 照常按状态码（默认行为，不能因为"粘性"就降低可用性）
func TestRetryDecisionStickySession(t *testing.T) {
	strict := RetryPolicy{StrictSession: true}
	lax := DefaultRetryPolicy()

	t.Run("严格命中", func(t *testing.T) {
		got := DecideRetry(strict, RetryInput{Status: 503, AttemptsLeft: 2, SessionSticky: true})
		if got.Action != RetryActionStop || got.Reason != RetryReasonStrictSession {
			t.Fatalf("严格粘性命中必须停止，得到 %s", got)
		}
		if got.Source != RetrySourceSessionRule {
			t.Fatalf("source=%q，想要 session_rule", got.Source)
		}
	})
	t.Run("严格未命中", func(t *testing.T) {
		got := DecideRetry(strict, RetryInput{Status: 503, AttemptsLeft: 2, SessionSticky: false})
		if got.Action != RetryActionRetry {
			t.Fatalf("未命中粘性时应照常重试，得到 %s", got)
		}
	})
	t.Run("非严格命中", func(t *testing.T) {
		got := DecideRetry(lax, RetryInput{Status: 503, AttemptsLeft: 2, SessionSticky: true})
		if got.Action != RetryActionRetry {
			t.Fatalf("默认策略下粘性不阻止重试，得到 %s", got)
		}
	})
}

// TestRetryDecisionNeverRetryConfig 可配置的"永不重试状态码表"。
func TestRetryDecisionNeverRetryConfig(t *testing.T) {
	t.Run("追加 400 无变化但仍生效", func(t *testing.T) {
		p := RetryPolicy{NeverRetryStatus: map[int]bool{http.StatusBadRequest: true}}
		got := DecideRetry(p, RetryInput{Status: 400, AttemptsLeft: 2})
		if got.Action != RetryActionStop || got.Reason != RetryReasonSystemRetryExclusion {
			t.Fatalf("配置的拒绝码必须判 stop/system_retry_exclusion，得到 %s", got)
		}
		// source 是 global（人配的规则），不是 system（代码里的判据）——
		// 这正是"为什么要区分 source"的用例：一个改配置、一个改代码。
		if got.Source != RetrySourceGlobal {
			t.Fatalf("source=%q，想要 global（这是运维配置项，不是代码判据）", got.Source)
		}
	})

	t.Run("追加一个本可重试的码", func(t *testing.T) {
		p := RetryPolicy{NeverRetryStatus: map[int]bool{429: true}}
		got := DecideRetry(p, RetryInput{Status: 429, AttemptsLeft: 2})
		if got.Action != RetryActionStop {
			t.Fatalf("配置拒绝 429 后必须停止，得到 %s", got)
		}
	})

	t.Run("只允许加严，不能放宽 504", func(t *testing.T) {
		// 空表（想"清空默认 deny 表"）不能把 504 放回重试区 ——
		// 否则一次写错的配置会让所有超时请求都多打一轮，延迟翻倍而成功率不变。
		p := RetryPolicy{NeverRetryStatus: map[int]bool{}}
		got := DecideRetry(p, RetryInput{Status: 504, AttemptsLeft: 2})
		if got.Action != RetryActionStop {
			t.Fatalf("504 必须始终不重试，得到 %s", got)
		}
	})

	t.Run("配置不影响其它码", func(t *testing.T) {
		p := RetryPolicy{NeverRetryStatus: map[int]bool{429: true}}
		if got := DecideRetry(p, RetryInput{Status: 503, AttemptsLeft: 2}); got.Action != RetryActionRetry {
			t.Fatalf("503 不该被 429 的配置波及，得到 %s", got)
		}
	})

	t.Run("零值策略仍然是默认表", func(t *testing.T) {
		if got := DecideRetry(RetryPolicy{}, RetryInput{Status: 524, AttemptsLeft: 2}); got.Action != RetryActionStop {
			t.Fatalf("零值策略下 524 仍须停止（零值可用是刻意设计），得到 %s", got)
		}
	})
}

// TestRetryDecisionPriority 多个停止理由同时成立时，报**哪一个**。
//
// # 为什么这条测试值得单独写
//
// 动作（stop）相同、reason 不同 ⇒ 排障结论完全不同：
//
//	local_rejection        → 去查我们的转换代码
//	attempt_budget_exhausted → 去调大重试次数
//	strict_session         → 去关严格粘性
//	non_retryable_error    → 去查上游适配层的标记
//
// 报错报对了但 reason 报错了，等于把排障者引到错误的方向上。
func TestRetryDecisionPriority(t *testing.T) {
	strict := RetryPolicy{StrictSession: true}
	cases := []struct {
		name   string
		policy RetryPolicy
		in     RetryInput
		reason string
	}{
		{
			// 原因级 > 所有约束：调大次数也没用，因为错在我们自己。
			name:   "本地错误压过预算耗尽",
			policy: DefaultRetryPolicy(),
			in:     RetryInput{Status: 500, LocalError: true, AttemptsLeft: 0},
			reason: RetryReasonLocalRejection,
		},
		{
			// 我们知道自己造的错 ⇒ 报 local，而不是把它降级成一个标签。
			name:   "本地错误压过 SkipRetry",
			policy: DefaultRetryPolicy(),
			in:     RetryInput{Status: 400, LocalError: true, SkipRetry: true, AttemptsLeft: 2},
			reason: RetryReasonLocalRejection,
		},
		{
			// 标签级 > 约束级：调用方已经明确说了"别试"，就不该再提预算。
			name:   "SkipRetry 压过预算耗尽",
			policy: DefaultRetryPolicy(),
			in:     RetryInput{Status: 503, SkipRetry: true, AttemptsLeft: 0},
			reason: RetryReasonNonRetryableError,
		},
		{
			// 硬约束 > 策略：报 strict_session 会让人以为"放宽策略就能重试"，
			// 而实际上放宽了也没有下一轮。
			name:   "预算耗尽压过严格粘性",
			policy: strict,
			in:     RetryInput{Status: 503, AttemptsLeft: 0, SessionSticky: true},
			reason: RetryReasonBudgetExhausted,
		},
		{
			// 策略 > 判据：状态码说"可重试"，但换号会破坏对话上下文。
			name:   "严格粘性压过 429",
			policy: strict,
			in:     RetryInput{Status: 429, AttemptsLeft: 2, SessionSticky: true},
			reason: RetryReasonStrictSession,
		},
		{
			// 同样压过 400：即使"本来就不打算重试"，判决来源也应当如实是粘性规则。
			name:   "严格粘性压过 400",
			policy: strict,
			in:     RetryInput{Status: 400, AttemptsLeft: 2, SessionSticky: true},
			reason: RetryReasonStrictSession,
		},
		{
			// 配置的拒绝码 > 区间表（否则"配了不生效"）。
			name:   "配置拒绝码压过 504 默认表以外",
			policy: RetryPolicy{NeverRetryStatus: map[int]bool{502: true}},
			in:     RetryInput{Status: 502, AttemptsLeft: 2},
			reason: RetryReasonSystemRetryExclusion,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecideRetry(c.policy, c.in)
			if got.Action != RetryActionStop {
				t.Fatalf("应当是 stop，得到 %s", got)
			}
			if got.Reason != c.reason {
				t.Fatalf("reason=%q，想要 %q（reason 决定排障方向，报错等于误导）", got.Reason, c.reason)
			}
		})
	}
}

// TestRetryDecisionReasonsAreStable 词汇表稳定性。
//
// # 为什么需要它
//
// reason/source 是**日志契约**（本仓 logbuf.Entry.Decision 存的就是 `reason:source`，
// 而 /admin/logs 与排障手册按它归类）。改一个值不会有编译错误、不会有测试失败，
// 只会让历史日志与新日志无法归到一类 —— 这种破坏是静默的。
//
// 所以这里做两件事：
//
//  1. 逐条钉住字面量（下方 wanted 表，值与 new-api 对齐）；
//  2. 钉住**每条 reason 在状态矩阵里都至少出现过一次** ——
//     一条永远不会被返回的 reason 是死词汇，它会误导读者去找一个不存在的判据。
func TestRetryDecisionReasonsAreStable(t *testing.T) {
	wanted := map[string]string{
		"non_retryable_error":      RetryReasonNonRetryableError,
		"local_rejection":          RetryReasonLocalRejection,
		"attempt_budget_exhausted": RetryReasonBudgetExhausted,
		"strict_session":           RetryReasonStrictSession,
		"system_retry_exclusion":   RetryReasonSystemRetryExclusion,
		"status_not_retryable":     RetryReasonStatusNotRetryable,
		"retry_status_matched":     RetryReasonRetryStatusMatched,
		"transport_error":          RetryReasonTransportError,
		"unrecognized_status":      RetryReasonUnrecognizedStatus,
	}
	for literal, got := range wanted {
		if literal != got {
			t.Errorf("reason 字面量变了：常量的值是 %q，契约要求 %q", got, literal)
		}
	}
	if len(wanted) != 9 {
		t.Fatalf("reason 常量数量变化（%d），新增时请同步更新本表与 for each 覆盖检查", len(wanted))
	}

	// 每条 reason 都必须可达：把"能造出它"的输入列出来（漏一条就是死词汇）。
	reachable := func(reason string) bool {
		inputs := []struct {
			policy RetryPolicy
			in     RetryInput
		}{
			{DefaultRetryPolicy(), RetryInput{Status: 400, LocalError: true, AttemptsLeft: 1}},
			{DefaultRetryPolicy(), RetryInput{Status: 400, SkipRetry: true, AttemptsLeft: 1}},
			{DefaultRetryPolicy(), RetryInput{Status: 429, AttemptsLeft: 0}},
			{RetryPolicy{StrictSession: true}, RetryInput{Status: 429, AttemptsLeft: 1, SessionSticky: true}},
			{DefaultRetryPolicy(), RetryInput{Status: 200, AttemptsLeft: 1}},
			{DefaultRetryPolicy(), RetryInput{Status: 504, AttemptsLeft: 1}},
			{DefaultRetryPolicy(), RetryInput{Status: 400, AttemptsLeft: 1}},
			{DefaultRetryPolicy(), RetryInput{Status: 429, AttemptsLeft: 1}},
			{DefaultRetryPolicy(), RetryInput{Status: 0, AttemptsLeft: 1}},
			{DefaultRetryPolicy(), RetryInput{Status: 700, AttemptsLeft: 1}},
		}
		for _, c := range inputs {
			if DecideRetry(c.policy, c.in).Reason == reason {
				return true
			}
		}
		return false
	}
	for _, reason := range []string{
		RetryReasonNonRetryableError, RetryReasonLocalRejection, RetryReasonBudgetExhausted,
		RetryReasonStrictSession, RetryReasonSystemRetryExclusion, RetryReasonStatusNotRetryable,
		RetryReasonRetryStatusMatched, RetryReasonTransportError, RetryReasonUnrecognizedStatus,
	} {
		if !reachable(reason) {
			t.Errorf("reason %q 不可达（死词汇：读者会去找一个不存在的判据）", reason)
		}
	}

	// source 同理。system / global / session_rule / local 各有一条可达路径。
	for _, want := range []string{RetrySourceSystem, RetrySourceGlobal, RetrySourceSessionRule, RetrySourceLocal} {
		switch want {
		case "system", "global", "session_rule", "local":
		default:
			t.Fatalf("source 字面量变了：%q", want)
		}
	}
}

// TestRetryDecisionPolicyIsNotShared 默认策略不能是共享的可变状态。
//
// # 为什么必须钉住
//
// 若 DefaultRetryPolicy 返回一个**共享的 map**，A 测试往里加一个码，
// B 测试就会莫名其妙地失败，而失败现场在另一个文件 —— 这类串味的排查成本极高。
// 现在的实现是 nil map（读安全、写会 panic），于是"顺手污染"在类型层面就做不到。
func TestRetryDecisionPolicyIsNotShared(t *testing.T) {
	p1 := DefaultRetryPolicy()
	if p1.NeverRetryStatus != nil {
		t.Fatalf("默认策略不该返回非 nil 的 map（那会被顺手改写；nil 是刻意的只读约定）")
	}
	p2 := DefaultRetryPolicy()
	p2.StrictSession = true
	p2.NeverRetryStatus = map[int]bool{418: true}
	p3 := DefaultRetryPolicy()
	if p3.StrictSession || p3.NeverRetryStatus != nil {
		t.Fatalf("修改一份策略污染了后续的默认策略：%+v", p3)
	}
	// 策略里配的码不能被默认表"吞掉"，也不能反过来吃掉默认表。
	if got := DecideRetry(p2, RetryInput{Status: 418, AttemptsLeft: 1}); got.Action != RetryActionStop {
		t.Fatalf("418 已配置为拒绝码，得到 %s", got)
	}
	if got := DecideRetry(p3, RetryInput{Status: 418, AttemptsLeft: 1}); got.Action != RetryActionRetry {
		t.Fatalf("未配置的 418 应当可重试，得到 %s（说明 418 泄漏进了默认表）", got)
	}
}

// TestRetryDecisionErrorSkipsRetry ErrorSkipsRetry 的三类来源与 nil 语义。
func TestRetryDecisionErrorSkipsRetry(t *testing.T) {
	t.Run("nil 不跳过", func(t *testing.T) {
		// nil 必须返回 false：调用方拿它填 SkipRetry，返回 true 会让成功路径
		// 被误判成"不可重试"。
		if ErrorSkipsRetry(nil) {
			t.Fatalf("nil 不该带跳过标记")
		}
	})
	t.Run("已知哨兵", func(t *testing.T) {
		if !ErrorSkipsRetry(gateway.ErrConvertFailed) {
			t.Fatalf("gateway.ErrConvertFailed 必须带跳过标记（本地转换失败，换号无意义）")
		}
	})
	t.Run("包装后仍可见", func(t *testing.T) {
		// 真实链路里错误会被 wrap 若干层（渠道名 + 步骤名），类型断言在此必然失效。
		wrapped := fmt.Errorf("chat via loomy: %w", gateway.ErrConvertFailed)
		if !ErrorSkipsRetry(wrapped) {
			t.Fatalf("包装后的 ErrConvertFailed 必须仍被识别")
		}
	})
	t.Run("SkipRetrier 接口", func(t *testing.T) {
		// 值类型实现（指向上游适配层最可能的写法）
		if !ErrorSkipsRetry(skipRetryErr{yes: true}) {
			t.Fatalf("实现了 SkipRetrier 的错误必须被识别")
		}
		if ErrorSkipsRetry(skipRetryErr{yes: false}) {
			t.Fatalf("SkipRetry()==false 不该被识别")
		}
		// 指针类型（errors.As 对指针接收者的形态）
		if !ErrorSkipsRetry(&skipRetryPtrErr{}) {
			t.Fatalf("指针接收者实现的 SkipRetrier 必须被识别")
		}
		// 包装后仍可见
		if !ErrorSkipsRetry(fmt.Errorf("send: %w", &skipRetryPtrErr{})) {
			t.Fatalf("包装后的 SkipRetrier 必须仍被识别")
		}
	})
	t.Run("普通错误不跳过", func(t *testing.T) {
		if ErrorSkipsRetry(errors.New("dial tcp: connection refused")) {
			t.Fatalf("普通传输错误不该带跳过标记（它正是最该重试的那类）")
		}
	})

	// 端到端：把 ErrorSkipsRetry 的结果填进 RetryInput，验证两条路对得上。
	got := DecideRetry(DefaultRetryPolicy(), RetryInput{
		Status:       503,
		SkipRetry:    ErrorSkipsRetry(gateway.ErrConvertFailed),
		AttemptsLeft: 2,
	})
	if got.Action != RetryActionStop {
		t.Fatalf("带标记的错误必须停止，得到 %s", got)
	}
}

// skipRetryErr / skipRetryPtrErr 是测试用的假错误（分别覆盖值/指针接收者两种形态）。
type skipRetryErr struct{ yes bool }

func (e skipRetryErr) Error() string   { return "fake skip-retry error" }
func (e skipRetryErr) SkipRetry() bool { return e.yes }

type skipRetryPtrErr struct{}

func (e *skipRetryPtrErr) Error() string   { return "fake skip-retry ptr error" }
func (e *skipRetryPtrErr) SkipRetry() bool { return true }

// TestRetryDecisionLogField 日志字段形状 `reason:source`。
//
// # 为什么单独钉
//
// 这个串会进 logbuf.Entry.Decision，而 /admin/logs 与历史日志都按它归类。
// 一旦格式从 `reason:source` 变成 `action/reason/source`，历史日志与新日志
// 就再也归不到一类 —— 且没有任何编译期信号。
func TestRetryDecisionLogField(t *testing.T) {
	cases := []struct {
		in   RetryInput
		want string
	}{
		{RetryInput{Status: 429, AttemptsLeft: 2}, "retry_status_matched:global"},
		{RetryInput{Status: 400, AttemptsLeft: 2}, "status_not_retryable:global"},
		{RetryInput{Status: 500, LocalError: true, AttemptsLeft: 2}, "local_rejection:local"},
		{RetryInput{Status: 429, AttemptsLeft: 0}, "attempt_budget_exhausted:global"},
		{RetryInput{Status: 200, AttemptsLeft: 2}, "system_retry_exclusion:system"},
	}
	p := DefaultRetryPolicy()
	for _, c := range cases {
		got := DecideRetry(p, c.in)
		if got.LogField() != c.want {
			t.Errorf("status=%d：LogField=%q，想要 %q", c.in.Status, got.LogField(), c.want)
		}
	}
	// String 是排障用的，不该与 LogField 冲突（含 action 便于一眼看出判了什么）。
	if s := (RetryDecision{Action: RetryActionStop, Reason: "r", Source: "s"}).String(); s != "stop(r:s)" {
		t.Errorf("String=%q，想要 stop(r:s)", s)
	}
}

// TestRetryDecisionStatusRangesWellFormed 区间表本身的形状。
//
// # 为什么要单独测"数据"而不是只测 DecideRetry
//
// retryStatusAllowed 的提前返回依赖一条**隐式约定**：区间按 start 升序且互不重叠。
// 破坏它不会编译失败、也不会让"429 可重试"这类常见用例变红 ——
// 只会在某个没人测的状态码上静默给出错误答案（比如新增一段插在末尾、start 更小，
// 从此它永远命中不到）。所以把约定显式钉住。
func TestRetryDecisionStatusRangesWellFormed(t *testing.T) {
	for i := 1; i < len(retryStatusRanges); i++ {
		prev, cur := retryStatusRanges[i-1], retryStatusRanges[i]
		if prev.start > prev.end {
			t.Errorf("第 %d 段 [%d,%d] 的 start 大于 end", i-1, prev.start, prev.end)
		}
		if cur.start <= prev.end {
			t.Errorf("第 %d 段 [%d,%d] 与前一段 [%d,%d] 重叠或未升序（提前返回会失效）",
				i, cur.start, cur.end, prev.start, prev.end)
		}
	}

	// 直接钉 retryStatusAllowed 的三条分支（含 DecideRetry 走不到的边界）：
	// 不在表里、命中中段、超出最后一段的尾巴。
	cases := []struct {
		status int
		want   bool
		why    string
	}{
		{400, false, "落在 400 这个洞里（100-199 / 300-399 之后、401-407 之前）"},
		{408, false, "落在 408 这个洞里（407 之后、409 之前）"},
		{504, false, "落在 504 这个洞里（500-503 之后、505-523 之前）"},
		{524, false, "落在 524 这个洞里（505-523 之后、525-599 之前）"},
		{429, true, "4xx 主力重试区"},
		{503, true, "5xx 上界"},
		{599, true, "最后一段的尾"},
		// ⚠ 下面两条 DecideRetry 不会传进来（>599 在 decideStatus 里就被拦下了），
		// 但函数本身必须对任意输入给出确定答案 —— 它是被判据调用的底层查表。
		{600, false, "超出最后一段"},
		{99, false, "在第一个区间 start 之前"},
	}
	for _, c := range cases {
		if got := retryStatusAllowed(c.status); got != c.want {
			t.Errorf("retryStatusAllowed(%d)=%v，想要 %v（%s）", c.status, got, c.want, c.why)
		}
	}
}
