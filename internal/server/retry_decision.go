// retry_decision.go —— 一次失败之后：还要不要再打一轮？
//
// # 这个文件解决什么（PLAN 的 T4 / 差距 G2）
//
// 出站循环（internal/server/handler.go）的重试判据此前是**散落在循环里的 if**：
//
//	status >= 400 时按 kind 粗分（ErrKindServer 加次数、ErrKindFatalClient 直接 break…）
//	terr != nil 时一律"网络层抖动 ⇒ 换号"
//	循环走完 ⇒ 503
//
// 每一处单看都合理，但它们**没有共同词汇**：没人能回答"这条 400 为什么不重试"，
// 因为答案分散在四段代码里，而其中一段（`terr != nil ⇒ 换号`）对一个
// 本地转换错误是**完全错误**的（实测代价：44 个账号全试一遍，见 gateway/errors.go 的注释）。
//
// new-api 把这件事收敛成一个纯函数：`decideTaskRetry`（controller/relay.go:828）
// 与 `DecideRelayRetry`（service/relay_error.go:23），返回
// `PolicyDecision{Action, Reason, Source}`（service/request_policy.go:19）。
// 本文件做同一件事，只是输入换成**本仓已有的那几种事实**：
//
//	(HTTP 状态码, 是否本地错误, 剩余重试次数, 是否命中会话粘性, 是否带 SkipRetry 标记)
//
// # 与 new-api 的关系：抄语义，不抄代码
//
// 抄的是**判据与词汇**（reason/source 的取名逐字对齐，日志两边可 grep 对照；
// 本仓 decision_test.go 早已按这个约定写了 `attempt_budget_exhausted` / `:global`）。
// 不抄的是它的 gin.Context 依赖 —— 本仓用 net/http，且决策应当是**纯函数**：
// 不读 ctx、不查缓存、不改状态。这样它能在任何层被调用，也能被表驱动测试穷举。
//
// # 为什么必须纯函数（这是本文件最重要的设计约束）
//
// 一个会读 ctx / 查渠道配置 / 改计数器的"决策"是无法被穷举测试的：
// 你只能测到"我构造的那几种 ctx"。而重试判据的错误恰恰落在**边缘状态码**上
// （504 该不该重试、408 为什么特殊、本地错误的 400 算谁的）——
// 那些状态码在集成测试里几乎不会被构造出来，只能靠表驱动穷举来钉住。
//
// 所以：状态、预算、粘性命中、错误标记**全部由调用方在入参里说清**，
// 本文件只做判断。策略（哪些状态码永不重试、粘性是否严格）作为显式参数
// 传入，不用包级可变全局 —— 全局可变量会让"测试之间互相污染"变成默认行为。
package server

import (
	"errors"
	"fmt"
	"net/http"

	"workbuddy2api/internal/gateway"
)

// RetryAction 决策的动词。取值只有两个，且**逐字**对齐 new-api 的 PolicyDecision.Action。
//
// # 为什么用字符串而不是 bool
//
// bool 只能表达"要不要重试"，而日志需要表达"判的是什么"。
// 字符串让 `retry`/`stop` 能直接进日志与前端（new-api 的
// web/src/features/system-settings/request-policies/api.ts 就是这么消费的）。
type RetryAction string

const (
	// RetryActionRetry 再打一轮（换号 / 换档 / 等一小会儿）。
	RetryActionRetry RetryAction = "retry"
	// RetryActionStop 结束，把错误如实交给客户端。
	RetryActionStop RetryAction = "stop"
)

// Reason 取值表。**这是对外（日志 / 面板）的词汇，改一个值就是一次可观测性破坏**：
// 排障手册、面板的"为什么没重试"归类、以及本仓 decision_test.go 的断言都按字面量比较。
// 因此与 errcode.go 同一条纪律：**只加不改**，每条都有测试钉住（TestRetryDecisionReasonsAreStable）。
const (
	// RetryReasonNonRetryableError 错误自己带了 SkipRetry 标记（调用方/上游适配层
	// 明确声明"这条重试没有意义"）。对应 new-api 的 types.IsSkipRetryError 分支。
	RetryReasonNonRetryableError = "non_retryable_error"

	// RetryReasonLocalRejection 错误是**我们自己产生的**（请求体转换失败、
	// 参数校验不过）。换号无效：每个账号背后是同一套代码、收到的是同一份 body。
	RetryReasonLocalRejection = "local_rejection"

	// RetryReasonBudgetExhausted 换号预算用尽。对应 new-api 的 retryTimes <= 0。
	RetryReasonBudgetExhausted = "attempt_budget_exhausted"

	// RetryReasonStrictSession 会话粘性是**严格**的，且本次命中了绑定号 ⇒
	// 换号会破坏对话上下文，宁可直接失败。对应 new-api 的 strict_session。
	RetryReasonStrictSession = "strict_session"

	// RetryReasonSystemRetryExclusion 被一条**显式排除**规则挡住：
	// 2xx（错误藏在响应体里）、或命中了"永不重试状态码表"。
	// 对应 new-api 的 system_retry_exclusion。
	RetryReasonSystemRetryExclusion = "system_retry_exclusion"

	// RetryReasonStatusNotRetryable 状态码本身表明"再来一次也是同一个结果"
	// （400 请求体坏 / 408 上游读超时）。对应 new-api 的 status_not_retryable。
	RetryReasonStatusNotRetryable = "status_not_retryable"

	// RetryReasonRetryStatusMatched 状态码落在可重试区间（429 / 5xx 的主力路径）。
	// 对应 new-api 的 retry_status_matched。
	RetryReasonRetryStatusMatched = "retry_status_matched"

	// RetryReasonTransportError **没有** HTTP 状态码、也不是本地错误 ⇒
	// 传输层失败（连不上 / 连接被重置 / 客户端超时）。换号有意义。
	//
	// ⚠ 这一条是本仓新增的（new-api 没有独立词汇，它把这类并进 unrecognized_status）。
	// 理由：本仓实测里"传输层失败"与"上游给了个奇怪状态码"是**两类完全不同**的故障，
	// 排障时要看的东西也不同（前者看 DNS/证书/代理，后者看上游口径变化）。
	// 混成一个词，日志读者还得靠 status 去猜。
	RetryReasonTransportError = "transport_error"

	// RetryReasonUnrecognizedStatus 状态码不在 100..599 内（上游给了个离谱的数）。
	// 对应 new-api 的 unrecognized_status —— 它那边选择**重试**（见 decideTaskRetry 的
	// default 分支），因为"看不懂"不足以证明"再试一次没用"。
	RetryReasonUnrecognizedStatus = "unrecognized_status"
)

// Source 取值表：**谁**做的判决。对应 new-api 的 PolicyDecision.Source。
//
// # 为什么要区分 source（不能只留 reason）
//
// 同一个 reason 可能来自不同决策者：`system_retry_exclusion` 既可能是
// "2xx 不该重试"（系统判据），也可能是运维配的 deny 表（人配的规则）。
// 排障时前者要改代码、后者要改配置 —— 不给 source 就只能猜。
const (
	// RetrySourceSystem 系统级判据（状态码语义 / 错误标记）。
	RetrySourceSystem = "system"
	// RetrySourceGlobal 全局策略（预算、可配置的状态码表）。
	RetrySourceGlobal = "global"
	// RetrySourceSessionRule 会话粘性规则。
	RetrySourceSessionRule = "session_rule"
	// RetrySourceLocal 本地（网关自己产生错误的那一层）。
	//
	// ⚠ 与 new-api 的细微差异：它的 decideTaskRetry 在本地错误分支上把 Source
	// 留作 "system"，而在 RecordPolicyFailure 里又用 "local" 表示同一件事。
	// 我们统一取 "local" —— 两处不一致对读日志的人没有价值。
	RetrySourceLocal = "local"
)

// RetryDecision 一次重试判决。
type RetryDecision struct {
	Action RetryAction
	Reason string
	Source string
}

// ShouldRetry 是调用方唯一该看的判据。
func (d RetryDecision) ShouldRetry() bool { return d.Action == RetryActionRetry }

// LogField 返回可直接写进 logbuf.Entry.Decision 的 `reason:source`。
//
// # 为什么不带 Action
//
// 与 logbuf.Entry.Decision 的既有取舍一致：Action 已经由**状态码**体现了
// （重试成功就是 200、预算用尽就是 502/503），再存一份是冗余；
// 而"为什么"是行为里看不出来的。详见 logbuf/logbuf.go 的 Entry.Decision 注释。
func (d RetryDecision) LogField() string { return d.Reason + ":" + d.Source }

// String 便于测试失败时一眼看出判了什么（不进日志、不进错误体）。
func (d RetryDecision) String() string {
	return fmt.Sprintf("%s(%s)", d.Action, d.LogField())
}

// defaultNeverRetryStatuses 默认的"永不重试"状态码表。
//
// # 为什么是 504 与 524（而不是所有 5xx）
//
// 这两个的共同点不是"上游坏了"，而是**时钟已经用光**：
//
//	504 Gateway Timeout   我们（或中间代理）已经等满了自己的超时预算才放弃
//	524 (Cloudflare 专有) 边缘节点等源站超时
//
// 也就是说：拿到 504 时，我们**已经**为这一次尝试烧掉了几十秒；
// 再换一个号重来，总耗时翻倍，而客户端自己的 deadline 大概率已经过了 ——
// 它会在我们第二轮还没回来时就断开，我们白烧一次上游额度与一个连接。
//
// ⚠ 这正是 new-api 的 alwaysSkipRetryStatusCodes = {504, 524}
// （setting/operation_setting/status_code_ranges.go:31）。它的注释只写"超时不重试"，
// 本仓把理由写明：不是"超时=不可重试"，而是"超时已经把预算用完了"。
//
// ⚠ 注意 408 的处理与这两个**不同类**：408 是上游说"我没收到完整请求"
// （通常意味着请求体在传输中被截断/太慢），它是**确定性**失败 ——
// 同样的 body 再发一次照样超时，所以它连"可重试区间"都进不去（见 retryStatusRanges）。
var defaultNeverRetryStatuses = map[int]bool{
	http.StatusGatewayTimeout: true, // 504
	524:                       true, // Cloudflare: A Timeout Occurred（net/http 无此常量）
}

// retryStatusRanges 可重试状态码**区间表**（闭区间，按 start 升序）。
//
// # 为什么用"区间表"而不是一堆 `status == 429 || status/100 == 5`
//
// 因为真实规则是**带洞的**，而洞的位置不能靠 if 的书写顺序来表达：
//
//	400 不重试（请求体本身坏）
//	408 不重试（见 defaultNeverRetryStatuses 的注释）
//	504 / 524 不重试（超时预算已用光）
//
// 用区间表，"哪些重试"是一份**可直接读、可直接测**的数据；
// 用 if 链，"为什么 401 重试但 408 不重试"只能靠读者把条件按顺序心算一遍。
//
// ⚠ 取值**逐字**对齐 new-api 的 AutomaticRetryStatusCodeRanges
// （setting/operation_setting/status_code_ranges.go:19，其注释自称
// "matches legacy hardcoded retry rules in controller/relay.go shouldRetry"）。
// 刻意不"优化"成 `>=400 && !=400 && !=408`：那会把将来上游新增的怪状态码
// 自动纳入重试，而"没见过的码"应当由人来决定，不该被一个通配条件吞掉。
var retryStatusRanges = [...]struct{ start, end int }{
	{100, 199}, // 1xx：协议层异常（上游正常不会把 1xx 当最终响应）
	{300, 399}, // 3xx：重定向没被跟随到底，换号/换路径有意义
	// ⚠ 401-407 重试而 400 不重试：401/403 是**凭证**问题，换号正好对症；
	//    407 是代理认证。400 是**请求体**问题，换到哪都是同一份 body。
	{401, 407},
	{409, 499}, // 409 冲突 / 413 太大 / 429 限流 / 其余 4xx：换号可能就过
	{500, 503}, // 5xx 主力：上游故障
	{505, 523}, // 跳过 504（见 defaultNeverRetryStatuses）
	{525, 599}, // 跳过 524（Cloudflare 超时）
}

// RetryPolicy 一次判决用的策略参数。
//
// # 为什么是显式参数，而不是包级全局变量
//
// 全局可变量在测试里会**互相污染**（A 测试改了 deny 表，B 测试随机失败，
// 而失败现场在另一个文件），在多实例/多租户下更是直接错误。
// 调用方（Handler 的配置）持有一份，测试自己造一份。
//
// # 为什么零值可用
//
// 零值 = "不严格粘性 + 只用默认 deny 表" = **本仓当前的行为**（最小意外原则）。
// 于是"忘了传策略"不会改变线上行为，只会少一层可配置能力。
type RetryPolicy struct {
	// StrictSession 粘性命中时是否**禁止换号**重试。
	//
	// # 为什么这个开关默认关闭，以及它到底买什么
	//
	// 本仓的粘性语义是"会话 → 账号"的绑定（见 internal/session）。当一个会话
	// 带着上下文在多轮之间切换账号时，上游侧看到的是一个**没有前文的新对话** ——
	// 它会正常回一段话，只是答得不对。用户看到的是"模型突然失忆"，
	// 而日志里一切正常（200、有 token）。这是最难归因的一类故障。
	//
	// 打开它 ⇒ 粘性命中且失败时宁可如实报错，也不换号（new-api 的 strict 模式，
	// service/channel_affinity.go:632）。
	//
	// ⚠ 代价（必须写明）：可用性下降。最常见的情形是**429 限流**，此时换号
	// 明明能出结果。所以默认关闭 —— 这是**产品语义**选择、不是技术默认值，
	// 由部署方决定"要连续性还是要成功率"。
	StrictSession bool

	// NeverRetryStatus 运维额外声明的"永不重试"状态码（**追加**在默认表之上）。
	//
	// # 为什么只允许加严，不允许放宽
	//
	// 默认表里的 504/524 不是"保守取值"，是"重试必然亏本"的结论（见上）。
	// 允许配置删掉它们，等于给"配置错误导致雪崩"留一个入口：一次写错的配置
	// 会让所有超时请求都再多打一轮，延迟翻倍而成功率不变。
	// 想放宽只能改代码 —— 那需要一次 review。
	NeverRetryStatus map[int]bool
}

// DefaultRetryPolicy 返回"本仓当前行为"对应的策略（粘性非严格、默认 deny 表）。
//
// ⚠ 返回的结构体里 NeverRetryStatus 为 nil：nil map 的读取是安全的（返回零值），
// 且**不与任何全局共享** —— 调用方往里写东西不会污染别人（有测试钉住）。
func DefaultRetryPolicy() RetryPolicy { return RetryPolicy{} }

// neverRetry 报告该状态码是否被排除重试（默认表 + 运维追加表）。
func (p RetryPolicy) neverRetry(status int) bool {
	if p.NeverRetryStatus[status] {
		return true
	}
	return defaultNeverRetryStatuses[status]
}

// RetryInput 一次失败的**全部已知事实**。
//
// # 为什么是结构体而不是 5 个位置参数
//
// 调用点会连续传 4 个状态码/bool/int —— 那种签名一旦改动就是一次静默错位
// （把 AttemptsLeft 和 Status 传反，编译照过，行为全错）。
// 具名字段让调用点自解释，也让将来加字段不破坏已有调用。
//
// # 为什么这里**没有** error 字段
//
// 决策只需要"这条错误属于哪一类"这几个布尔事实，不需要错误对象本身。
// 让调用方先把 err 翻译成事实（LocalError / SkipRetry），有三个好处：
//   - 本文件不依赖具体错误类型（网关层/上游层的错误继续各自演化）；
//   - 每个事实都能在测试里单独构造，不必为测一个判据去拼一个 error；
//   - 决策保持纯函数（见文件头）。
//
// 翻译工作有现成帮手：ErrorSkipsRetry 处理 SkipRetry 标记。
type RetryInput struct {
	// Status 上游（或本地 HTTP 层）给出的状态码；0 表示**没有状态码**（传输层失败）。
	//
	// ⚠ 流内错误（HTTP 200 + 响应体里的 error 信封）必须由调用方**翻译成**
	// 语义对应的状态码再传进来（额度/限流 → 429、上游故障 → 502、请求体坏 → 400），
	// 而不是原样传 200。原因见 decideStatus 里 2xx 分支的注释。
	Status int

	// LocalError 这条错误是**我们自己产生的**（body 转换失败、参数校验不过），
	// 与上游无关。同类事实的现成哨兵见 gateway.ErrConvertFailed。
	LocalError bool

	// SkipRetry 错误自己带了"别重试"的显式标记（new-api 的 ErrOptionWithSkipRetry）。
	// 用 ErrorSkipsRetry 从 error 里取，不要手写。
	SkipRetry bool

	// AttemptsLeft 还能再打几轮（**不含**刚刚失败的那一次）。
	// 0 或负数 = 预算已经用光。
	//
	// ⚠ 语义必须是"剩余"而不是"总次数"：传成总数会让每个请求多打一轮，
	// 而多打的那一轮在最坏情况下是又一次几十秒的上游等待。
	AttemptsLeft int

	// SessionSticky 本次尝试用的账号是**会话粘性命中的那一个**。
	//
	// 只有 RetryPolicy.StrictSession 为 true 时才影响判决（否则只用于日志）。
	SessionSticky bool
}

// DecideRetry 是本仓**唯一**的重试判据。
//
// # 判决优先级（从上到下，先命中先返回）
//
//	① 本地错误            —— 原因级：我们自己造的错，换号必然重复同一个错
//	② SkipRetry 标记      —— 原因级：调用方已经声明过这条不该重试
//	③ 剩余次数 <= 0       —— 约束级：无论判据怎么说，都没有下一轮了
//	④ 粘性严格命中        —— 策略级：能重试，但换了会破坏对话上下文
//	⑤ 2xx                —— 判据级：错误在响应体里，重发大概率复现
//	⑥ 永不重试状态码表    —— 判据级（可配置）
//	⑦ 可重试区间表        —— 判据级（默认规则）
//	⑧ 无状态码            —— 传输层失败，重试
//	⑨ 离谱状态码          —— 看不懂，重试
//
// # 为什么是"原因 > 约束 > 策略 > 判据"这个次序
//
// 它决定了**多个理由同时成立时报哪一个**，而这直接决定排障时的第一直觉：
//
//	"本地错误 + 预算已尽" ⇒ 报 local_rejection。
//	  报 budget_exhausted 会让人去调大重试次数 —— 而调大之后照样失败，
//	  因为真正的原因是我们的转换代码处理不了这份 body。
//	"粘性严格 + 预算已尽" ⇒ 报 attempt_budget_exhausted。
//
// ③ 排在 ④ 之前的理由：预算是最**硬**的约束 —— 它决定"还能不能"，
// 而粘性是策略，决定"该不该"。两者都停时报"不能"，因为那是无论策略怎么配
// 都成立的结论；报 strict_session 会让读者以为"放宽策略就能重试"，
// 而实际上放宽了也没有下一轮。
//
// ⚠ 与 new-api 的差异登记：decideTaskRetry 把 affinity（粘性）检查排在预算**之前**，
// 我们刻意相反。差别只影响"两个都停时日志写哪个 reason"，不影响动作。
func DecideRetry(p RetryPolicy, in RetryInput) RetryDecision {
	// ① 本地错误：换 N 个号 = 白跑 N 次往返。
	//
	// 实测（见 gateway/errors.go）：转换失败曾与"网络抖动"共用同一条
	// `fail(uid); continue`，44 个账号全试了一遍（耗时 6.7s~45s），
	// 最后报成 503 "all accounts unavailable —— 0 禁用、0 冷却" ——
	// 把一个本地错误误报成池子故障。
	//
	// ⚠ 本分支**不看 Status**：本地错误的 Status 是调用方顺手套的
	// （转换失败常套 400），但"400"不是这条错误的性质，"本地"才是。
	if in.LocalError {
		return RetryDecision{Action: RetryActionStop, Reason: RetryReasonLocalRejection, Source: RetrySourceLocal}
	}

	// ② SkipRetry：显式声明优于一切启发式。
	//
	// 它与 ① 的区别：① 是"我们知道自己造的"，② 是"别人告诉我们别试"
	// （上游适配层从响应/协议里读出来的确定性结论）。
	// 两者可能同时成立，此时报 ① —— 因为我们能确定原因，而 ② 只是一个标签。
	if in.SkipRetry {
		return RetryDecision{Action: RetryActionStop, Reason: RetryReasonNonRetryableError, Source: RetrySourceSystem}
	}

	// ③ 预算用尽：没有下一轮了。对应 new-api 的 retryTimes <= 0
	// （service/relay_error.go:44，reason 逐字相同，方便两边日志对照）。
	//
	// ⚠ 必须在所有状态码判据之前：放在后面就会出现"429 可重试，于是去重试"
	// 而预算已经是 0 —— 循环要么越界，要么靠调用方额外兜底（那正是判据
	// 分散在不同地方的老毛病）。
	if in.AttemptsLeft <= 0 {
		return RetryDecision{Action: RetryActionStop, Reason: RetryReasonBudgetExhausted, Source: RetrySourceGlobal}
	}

	// ④ 粘性严格模式：能重试，但不该换号。
	//
	// 见 RetryPolicy.StrictSession 的长注释：换号的代价是"模型突然失忆"，
	// 而那在日志里看起来是完全正常的 200。
	if p.StrictSession && in.SessionSticky {
		return RetryDecision{Action: RetryActionStop, Reason: RetryReasonStrictSession, Source: RetrySourceSessionRule}
	}

	return decideStatus(p, in.Status)
}

// decideStatus 是决策的"判据级"部分，单独拆出来是为了让 ⑤~⑨ 这五条
// 状态码规则能在测试里被单独穷举（不必每次都构造整个输入结构体）。
func decideStatus(p RetryPolicy, status int) RetryDecision {
	// ⑤ 2xx 却有错误：错误藏在响应体里（上游的"200 + error 信封"）。
	//
	// # 为什么这里判 stop（与 new-api 一致），而本仓现状是重试
	//
	// 语义上 stop 是对的：状态码是 2xx 意味着**这条连接是通的、上游是活的**，
	// 它在业务层拒绝了这次请求。同样的 body 再发一次，绝大多数情况下复现同一个拒绝。
	//
	// ⚠⚠ 集成注意（这条务必让调用方看见）：本仓 handler.go 对"流内错误"目前是
	// `applyErrorPolicy + continue`（重试）。要让两者不打架，调用方必须把
	// **流内错误翻译成语义状态码**再传进来（见 RetryInput.Status 的注释），
	// 而不是原样传 200 —— 否则本分支会把"上游抖了一下（流内 5xx 语义）"
	// 判成"不该重试"，可用性反而下降。
	if status >= 200 && status <= 299 {
		return RetryDecision{Action: RetryActionStop, Reason: RetryReasonSystemRetryExclusion, Source: RetrySourceSystem}
	}

	// ⑥ 永不重试状态码表（默认 504/524 + 运维追加）。
	//
	// 排在区间表之前：它是**显式排除**，必须能压过"5xx 可重试"这条通则，
	// 否则加了配置也不生效（配置看起来生效了、其实被后面的区间表吃掉 ——
	// 这种"配了没用"是最难发现的运维坑）。
	if p.neverRetry(status) {
		return RetryDecision{Action: RetryActionStop, Reason: RetryReasonSystemRetryExclusion, Source: RetrySourceGlobal}
	}

	// ⑦ 可重试区间表。
	if status >= 100 && status <= 599 {
		if retryStatusAllowed(status) {
			return RetryDecision{Action: RetryActionRetry, Reason: RetryReasonRetryStatusMatched, Source: RetrySourceGlobal}
		}
		// 落在 100..599 却不在区间里 ⇒ 只有 400 / 408 这两个洞
		// （400 请求体本身坏，408 上游读超时；见 retryStatusRanges 的注释）。
		return RetryDecision{Action: RetryActionStop, Reason: RetryReasonStatusNotRetryable, Source: RetrySourceGlobal}
	}

	// ⑧ 没有状态码：传输层失败（连不上 / 连接被重置 / 客户端超时）。
	//
	// 这类错误**必须重试**：上游根本没给出判断，而换一个号换一条 TCP 连接
	// 恰恰是最有效的动作。历史上它和"本地错误"共用同一条 `fail(uid); continue`，
	// 所以这里刻意保留"继续"的语义 —— 那条路径对传输层是对的，只对本地错误是错的。
	if status <= 0 {
		return RetryDecision{Action: RetryActionRetry, Reason: RetryReasonTransportError, Source: RetrySourceSystem}
	}

	// ⑨ 100..599 之外的状态码（上游发了离谱值，或适配层传了个哨兵值）。
	//
	// 选择重试而不是停止：**看不懂不足以证明"再试一次没用"**。
	// 停止的代价是确定性的失败；重试的代价只是一次往返 —— 而"看不懂"本身
	// 极可能来自一次错误的路由/协议错配，换号正好能躲开。
	// （与 new-api decideTaskRetry 的 default 分支同取向。）
	return RetryDecision{Action: RetryActionRetry, Reason: RetryReasonUnrecognizedStatus, Source: RetrySourceSystem}
}

// retryStatusAllowed 查区间表（线性扫描，最多 7 段；不值得为它引二分或建 map）。
func retryStatusAllowed(status int) bool {
	for _, r := range retryStatusRanges {
		if status < r.start {
			return false // 区间按 start 升序 ⇒ 后面的更不可能命中
		}
		if status <= r.end {
			return true
		}
	}
	return false
}

// ErrorSkipsRetry 报告 error 是否**自带**"别重试"的标记。
//
// # 为什么需要这个帮手（而不是让每个调用点自己 errors.Is）
//
// 本仓现有的标记是 gateway.ErrConvertFailed（本地转换失败，见 gateway/errors.go 的长注释），
// 但它只是**第一个**。将来会有更多（上下文超限、内容拦截…），而"哪些错误带标记"
// 这件事必须只有一个答案 —— 否则某个新错误忘了接上，就会退回"换 N 个号白跑"。
//
// 所以判据收在这里，两路合并：
//
//	errors.Is(err, gateway.ErrConvertFailed)   已知的哨兵
//	SkipRetrier 接口                           上游/网关自报（无需改本文件即可加新标记）
//
// ⚠ nil 返回 false（不是 "unknown"）：调用方拿它填 RetryInput.SkipRetry，
// 而"没有错误"在业务上等价于"没有跳过标记" —— 返回 true 会让一次成功路径被误判成不可重试。
func ErrorSkipsRetry(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gateway.ErrConvertFailed) {
		return true
	}
	var sr SkipRetrier
	if errors.As(err, &sr) {
		return sr.SkipRetry()
	}
	return false
}

// SkipRetrier 让任何错误类型**自己**声明"别重试"，无需修改本文件。
//
// # 为什么用接口而不是一堆 errors.Is
//
// 每加一类不可重试错误就去 ErrorSkipsRetry 里加一行 errors.Is，意味着
// "新增标记"必须回到本文件 —— 一个在上游包里发生的确定性判断，要跨包改核心，
// 这正是"扩展点"该避免的。new-api 的做法是 `types.ErrOptionWithSkipRetry()`
// 把标记挂在错误对象上；接口是同一思想在不引入错误包装框架时的最小实现。
//
// # 实现约束
//
//   - 必须纯函数、不得 panic（它在出站循环的每次失败上被调用）。
//   - 只有**确定**同一份请求再发一次会得到同样结果时才返回 true。
//     拿不准时返回 false：错判成"可重试"只损失一次往返，错判成
//     "不可重试"会让一次本可成功的请求失败。
type SkipRetrier interface {
	SkipRetry() bool
}
