// errcode.go 出站错误码：把网关的 ErrorKind 收敛成一份**对客户端承诺**的字符串。
//
// # 为什么需要它（对应 new-api 的 relaykit/types/error.go 里那张 ErrorCode 表）
//
// 改造前，出站错误体里的 code 是一串**手工散写**的字符串：
//
//	internal/server/handler.go   "upstream_error" / "no_healthy_account" / "content_blocked" / "invalid_request"
//	internal/server/images.go    "not_ready" / "image_not_supported" / "missing_model"
//	internal/account/http.go     "rate_limited" / "bad_credentials" …
//
// 每一处单看都合理，但**没有任何一份清单**说清哪些 code 是契约、哪些只是随手写的。
// 而 code 是客户端唯一能做分支的东西（重试 / 改参数 / 找人）：
//
//	按 code 判"限流，等一下再试"的客户端，在 code 被改成另一个词之后**不会报错**，
//	它只会走进 else —— 于是从"等 3 秒重试"变成"直接放弃"。
//
// 这种破坏是静默的：我们的测试只断言自己写下的字面量，客户端的分支我们看不见。
// new-api 把它集中在 types.ErrorCode 常量表里，就是为了让"改一个 code"
// 变成一次**可见的**契约变更。本文件做同一件事。
//
// # 与 ErrorKind 的分工（关键：两者不得混用）
//
//	ErrorKind  内部**诊断/处置**分类：决定冷却多久、要不要禁用、喂不喂熔断。
//	           它是我们的实现细节，会随上游策略调整，**不承诺**稳定。
//	           （soft_rate / session_dead / fatal_client …）
//
//	ErrorCode  出站**契约**：客户端按它分支。一旦发布，改动即破坏兼容。
//	           （rate_limited / no_healthy_account / invalid_request …）
//
// 两者刻意**不用同一套取值**：如果直接把 kind.String() 当 code 发出去，
// 那么"把 ErrKindAuth 并进 ErrKindClient"这种纯内部重构，就会**静默改变**
// 客户端看到的 code —— 内部语义一调整，外部契约跟着碎。有测试钉住这条
// （见 errcode_test.go 的 TestCodesAreNotInternalKindNames）。
//
// ⚠ 有一个**已存在**的重合：ErrKindContentBlocked.String() 与 CodeContentBlocked
// 都叫 content_blocked。这不是设计，是历史 —— 那条 code 已经在出站路径上发了很久，
// 且 internal/server 的降级测试按字面量断言它。改名属于契约破坏，得单独排期
// （同时改 handler 与客户端），不在本次收敛范围内，所以这里**登记为已知例外**
// 而不是假装没发生。
//
// # 为什么不「在每种错误现场手写 code」
//
// 现场手写必然分叉，而且分叉是本仓**已经发生**的事实（不是假想）：
//
//	同一个"太频繁"：internal/account 写 "rate_limited"，
//	                internal/server 的 API key 限速写 "rate_limit_exceeded"
//	同一个"请求体不合法"："invalid_request" / "invalid_json" / "bad_request" 各写各的
//	同一个"上游失败"：  "upstream_error" / "upstream_parse" / "channel_error"
//
// 客户端要维护多套判断，且没人说得清哪些词是真的契约。表驱动把
// "哪一类错误对客户端说什么"收进**一个地方**；新增处置分支（新的 ErrorKind）时
// 编译器不会拦，但这张表和它的测试会。
package gateway

// ErrorCode 出站错误体里的 error.code（OpenAI 兼容形态）。
//
// 取值只允许小写 snake_case（有测试钉住）：客户端普遍用代码生成器
// 或枚举比较来判定它，驼峰、大写、带空格在不同语言 SDK 里会被归一化成
// 不同形态，同一个 code 在两种语言里就认不出来了。
type ErrorCode string

const (
	// CodeInvalidRequest 请求本身有问题，**原样重试没有意义**。
	//
	// 映射：ErrKindClient、ErrKindFatalClient。
	//
	// # 为什么 ErrKindClient 也归到这里
	//
	// ErrKindClient 的处置是"只换号不罚"—— 意思是"也许别的号能过"。
	// 但**能走到出口**说明号已经换遍了、每个号都给了 4xx：那就不是号的问题，
	// 是这份请求的问题。此时告诉客户端"请求无效"，比让它反复重试一个
	// 必然失败的东西更诚实（也更省我们的上游额度）。
	CodeInvalidRequest ErrorCode = "invalid_request"

	// CodeRateLimited 限流，**等一会儿重试有意义**。
	//
	// 映射：ErrKindSoftRate。
	//
	// ⚠ 刻意不叫 "soft_rate"：那是内部 kind 的名字，词义只有我们懂。
	// 客户端（以及任何 OpenAI 兼容 SDK）认得的是 rate limit 这套说法。
	CodeRateLimited ErrorCode = "rate_limited"

	// CodeInsufficientQuota 上游额度/余额耗尽，重试无用，得换号或等额度恢复。
	//
	// 映射：ErrKindHardCredit。
	//
	// 与 new-api 的 insufficient_user_quota 同词根：客户端看到它应当**停止重试**，
	// 而不是按 5xx 那样退避重打 —— 那正是"额度白烧到底"的成因。
	CodeInsufficientQuota ErrorCode = "insufficient_quota"

	// CodeUpstreamUnavailable 上游不可用（5xx / 连不上 / 空流），**可重试**。
	//
	// 映射：ErrKindServer，以及 ErrKindNone（见下）。
	//
	// # 为什么 ErrKindNone 也映到这里
	//
	// ErrKindNone 在分类器语义里是"成功 / 我不认识的错误 / 不需要惩罚"。
	// 但它出现在**出口**时（此时已确定要返回错误）只可能是这几种情形：
	//
	//	传输层失败（连不上、超时）  → 上游不可用
	//	上游 2xx 但流是空的        → 上游不可用
	//	池子在选号阶段就空了       → 没有可用的上游容量
	//
	// 三者对客户端是同一件事：**现在拿不到结果，稍后重试**。
	// 所以这里不是"把 none 当成错误"，而是"到了出口，none 的唯一含义就是它"。
	//
	// ⚠ 命名变更留痕：改造前这条路径写的是字面量 "upstream_error"。
	// 新名字把语义写明（unavailable = 暂时性，可重试）。两套词汇不要混用 ——
	// 内部诊断仍用 kind.String()（那里 server 就叫 "server"）。
	// CodeUpstreamUnavailable 上游故障（重试无用或已重试仍失败）。
//
// ⚠ 线上值刻意保持 "upstream_error" 而不是跟常量名一致的 "upstream_unavailable"：
// 这是**已经发布出去的契约值**，改它会让既有客户端的分支判断失配
// （它们的 switch/case 里写的是 upstream_error）。
// 常量名用 UpstreamUnavailable 是因为语义更准（5xx 与"连不上"都归它），
// 但对外词不跟着改 —— 契约稳定优先于命名一致性。
CodeUpstreamUnavailable ErrorCode = "upstream_error"

	// CodeNoHealthyAccount 池子里没有可用账号（全冷却 / 全禁用 / 凭证失效）。
	//
	// 映射：ErrKindSessionDead、ErrKindAuth。
	//
	// # 为什么这两种 kind 共用一个 code
	//
	// 它们对客户端的结论完全一样：**这个网关现在无法代你调用上游**，
	// 重试无用，得有人去控制台处理。而它们的差别（workbuddy 的会话死了要人工重登、
	// codearts 的凭证能自动续期）是**我们的运维事实**，客户端既看不懂也用不上。
	//
	// ⚠ 沿用既有字面量：这条 code 此前就在 handler.go / images.go 的 503 路径上，
	// 已经是对外事实。选它而不是新造 "session_dead"，是为了**不制造**一次
	// 无收益的契约破坏。
	CodeNoHealthyAccount ErrorCode = "no_healthy_account"

	// CodeContentBlocked 上游**内容策略**拦截：换号无用，要改的是提示词。
	//
	// 映射：ErrKindContentBlocked。
	//
	// 这条 code 是给客户端**降级重试**用的（见 handler 的 retryWithDegradedPrompt）。
	// 若混进 invalid_request，客户端会去改参数；混进 upstream_unavailable，
	// 客户端会退避重打 —— 两者都改不了内容策略，只会让我们多烧几轮上游往返。
	CodeContentBlocked ErrorCode = "content_blocked"

	// CodeModelNotFound 上游不认识这个模型（典型：InferHub.002002009.404，
	// "The model is not registered"）。
	//
	// 映射：ErrKindNotFound。
	//
	// # 取舍（本表里唯一有争议的一条）
	//
	// ErrKindNotFound 的注释说它是"上游偶发 404"，语义上偏暂时性；但本仓实测到的
	// 404 几乎都是"模型未注册/未开通" —— 那对客户端是**确定性**的、可自修的
	// （换个模型名），给一个能定位到"模型"的 code 比笼统的 unavailable 有用得多。
	//
	// 代价：真的偶发 404 时，客户端会以为模型名错了而不重试。
	// 我们接受 —— 它只影响这一种 kind，且错误体的 message 仍带上游原委。
	CodeModelNotFound ErrorCode = "model_not_found"

	// CodeUnknown 兜底：出现了一个**本表不认识**的 ErrorKind。
	//
	// # 为什么兜底不能是空串，也不能是 "internal_error"
	//
	// 空串会让 error.code 字段消失，客户端的 code 分支拿到 null；
	// 而 "internal_error" 是在**断言**"这是网关自己的 bug" —— 我们并不知道。
	// 兜底必须如实表达"我不知道"，且必须**稳定**（客户端可据此上报给网关方排查）。
	//
	// 对应 new-api 在 code 缺失/非字符串时回落 "unknown_error"
	// （relaykit/types/error.go 的 WithOpenAIError）—— 同一取舍，同一拼写。
	CodeUnknown ErrorCode = "unknown_error"
)

// errorCodeByKind 是 kind → 出站 code 的**唯一**映射表。
//
// # 为什么是显式表而不是 switch
//
// switch 的 default 会**吞掉**新增的 kind：有人给 ErrorKind 加了第 11 个值，
// 编译照过，出站悄悄变成 unknown_error —— 上线后才发现客户端在这个新场景
// 收不到有意义的 code。表 + 测试（TestEveryDeclaredKindIsMapped）把这种
// "忘了登记"变成一条红色测试。
//
// ⚠ 只追加；不改任何既有行的 Code —— 这张表的值就是对外契约。
// 调整某个 kind 的处置策略（冷却时长/是否禁用）时，**不该**顺手动这里的 code：
// 后者是客户端契约，前者是我们的实现细节。
var errorCodeByKind = []struct {
	Kind ErrorKind
	Code ErrorCode
}{
	{ErrKindNone, CodeUpstreamUnavailable},
	{ErrKindHardCredit, CodeInsufficientQuota},
	{ErrKindSoftRate, CodeRateLimited},
	{ErrKindSessionDead, CodeNoHealthyAccount},
	{ErrKindNotFound, CodeModelNotFound},
	{ErrKindAuth, CodeNoHealthyAccount},
	{ErrKindServer, CodeUpstreamUnavailable},
	{ErrKindClient, CodeInvalidRequest},
	{ErrKindContentBlocked, CodeContentBlocked},
	{ErrKindFatalClient, CodeInvalidRequest},
}

// ErrorCodeForKind 把内部错误分类翻译成**对客户端承诺**的错误码。
//
// # 调用时机（重要）
//
// 只在**已经确定要写出错误响应**时调用。它不是"这次请求成不成功"的判据 ——
// 那个判据是 kind 本身（ErrKindNone 表示成功或中性）。拿它去判成功/失败会把
// "成功"读成 upstream_unavailable（见 CodeUpstreamUnavailable 的注释）。
//
// # 为什么参数里没有 status / body
//
// 这张表是**纯函数、只依赖 kind**：一旦它开始看状态码或响应体，它就成了
// 第二个分类器，与 ErrorClassifier 抢职责 —— 而"这次错误属于哪一类"已由
// 上游自己的分类器决定（见 errorclassifier.go）。这里只做**词汇转换**，
// 不做判断。这也让它能被任何层（server / admin / 将来的 SDK）安全调用。
//
// # 未知 kind 的兜底
//
// 表里没有的取值（新增 kind 忘了登记，或某处写了 gateway.ErrorKind(n)）
// 一律返回 CodeUnknown，**不 panic、也不回落成某个看似合理的具体 code**：
// 编一个具体 code 等于对外断言一个我们没有依据的结论。
func ErrorCodeForKind(k ErrorKind) ErrorCode {
	for _, m := range errorCodeByKind {
		if m.Kind == k {
			return m.Code
		}
	}
	return CodeUnknown
}

// IsKnown 报告该 code 是否为**本表声明过**的契约值。
//
// # 为什么需要它
//
// 集成时（以及将来写网关自己的文档/OpenAPI）需要能校验"这个 code 是契约值
// 还是随手写的字面量"。没有这个判据，新代码里的手写 code 会一直扩散，
// 那这次收敛就白做了。
func (c ErrorCode) IsKnown() bool {
	switch c {
	case CodeInvalidRequest, CodeRateLimited, CodeInsufficientQuota,
		CodeUpstreamUnavailable, CodeNoHealthyAccount, CodeContentBlocked,
		CodeModelNotFound, CodeUnknown:
		return true
	}
	return false
}

// String 便于日志输出。ErrorCode 是 string 的具名类型，直接塞进 map[string]any
// 会被 encoding/json 编成字符串，不需要额外转换。
func (c ErrorCode) String() string { return string(c) }
