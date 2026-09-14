// errorclassifier.go —— 第七个扩展点：上游自报「我的错误怎么分类」。
//
// # 为什么必须有这个扩展点（这是出站修复的又一处漏网，且后果最重）
//
// 出站循环（internal/server/handler.go）在两条上游的响应上**都**调同一个分类器：
//
//	kind := upstream.Classify(status, string(respBody))
//	h.applyErrorPolicy(acct.UID, kind)
//
// 而 `upstream.Classify` 是 **workbuddy 的**分类器 —— 它的判据里有：
//
//	hardMarkers      = ["insufficient credit", "no credit", "quota exceeded", ...]
//	sessionDeadMarkers = ["Offline user session not found", "12153"]
//
// 这两串判据都是 **workbuddy 专有的事实**。拿它去判 codearts 的响应体，
// 两个方向同时出错：
//
// # 危害 ①：额度漏判（静默饿死）
//
// codearts 的额度耗尽原文是 `InferHub.4291.200 insufficient quota`
// （见 codearts/quota.go 的实测注释）。而 workbuddy 的 hardMarkers 含
// `"quota exceeded"` / `"quota exhaust"` / `"额度不足"` / `"余额不足"`，
// **一个都不匹配 `"insufficient quota"`**。
//
// 若上游返 200 + 流内业务错误（codearts 的形态就是如此，见 quota.go：
// "它不是 HTTP 错误码，而是塞在 SSE 流里的业务错误"），
// 分类结果是 `ErrNone` → 额度耗尽被**完全忽略**：
// 账号不进长冷却、不标记额度、下一轮照旧选中它，把额度白烧到底。
//
// 实测：
//
//	upstream.Classify(200, `{"error_code":"InferHub.4291.200","error_msg":"insufficient quota"}`)
//	  → none   ← 应当被识别为额度耗尽
//
// # 危害 ②：反向误伤（后果最重 —— 一个健康的号被永久禁用）
//
// `sessionDeadMarkers` 里有一个**裸数字 `"12153"`**。它在 workbuddy 里是
// "offline session" 的业务码，但对 codearts 只是一个碰巧出现的子串：
//
//	codearts 任何错误体里只要含 "12153"，就被判成 ErrSessionDead
//	  → applyErrorPolicy 的 ErrSessionDead 分支 → **Pool.Disable**
//	  → 永久禁用，需人工重登
//
// 实测：
//
//	upstream.Classify(200, `{"error":"InferHub.4005.200 12153 something"}`) → session_dead
//	upstream.Classify(429, `12153`)                                          → session_dead
//
// 一个**健康**的账号因为一个无关的数字被永久禁用，而界面上只会显示
// "已禁用"，没有任何线索指向"它其实是被另一家上游的判据误伤的"。
//
// # 为什么不是「在 core 里按 provider ID 分派」
//
// 那会让核心知道每个上游的错误码表 —— 与凭证格式、额度形态同理，
// 错误分类是**上游的事实**，核心不该解释它（判据 1）。
//
// # 机制（与其它六个扩展点一致）
//
// 上游只实现自己有的，核心用类型断言发现：
//
//	if ec, ok := gateway.ExtOf[gateway.ErrorClassifier](pv); ok {
//	    kind = ec.Classify(status, body)
//	} else {
//	    kind = gateway.DefaultErrorKind(status, body)   // 通用兜底
//	}
//
// **没有实现该扩展点的上游不算错**：核心回落到通用的按状态码分类
// （见 DefaultErrorKind）—— 那是明确的保守兜底，不是"核心假装知道
// 某个上游的错误码表"。
package gateway

// ErrorKind 是**跨上游中立**的错误分类。
//
// # 为什么必须有它（而不是让上游直接返回自己的 ErrKind）
//
// 两个上游各自定义了一个 `ErrKind`：
//
//	internal/upstream.ErrKind   —— workbuddy，含 ErrSessionDead / ErrHardCredit / ...
//	internal/codearts.ErrKind   —— codearts，含 ErrHardCredit / ErrAuth / ...
//
// 它们是**不同包的不同类型**。core 的 `applyErrorPolicy` 只认
// `upstream.ErrKind`，于是"按上游分派分类"这件事在**类型上就做不到** ——
// 这正是这个缺口长期存在的技术原因：不是没人发现，是接缝处没有共同词汇。
//
// 于是把「分类结果」提升到 gateway 层：它是 core 与上游之间唯一的接缝，
// 也是唯一两个方向都合法依赖的包（上游依赖 gateway 是契约要求，
// core 依赖 gateway 是既有事实）。
//
// # 为什么不新建第三套分类语义
//
// 本类型**不是**第三套语义，它是 upstream.ErrKind 的**重命名搬运**：
// 每个常量与 upstream.ErrKind / codearts.ErrKind 的对应项一一对齐
// （见每个常量上的映射注释），取值顺序也与 upstream.ErrKind 逐字一致，
// 因此映射层是**逐项直译、无信息损失**的（有测试钉住，见
// errorclassifier_test.go 的 TestErrorKindMirrorsUpstreamErrKind）。
//
// 换句话说：不发明新分类，只把已有的那两套**共同子集**提取成一个中立名字，
// 让接缝处能传递它。
type ErrorKind int

const (
	// ErrKindNone 成功 / 无法归类为中性的业务错误。
	//
	// 映射：upstream.ErrNone、codearts.ErrNone（以及 codearts 的 ErrClient 见下）。
	ErrKindNone ErrorKind = iota

	// ErrKindHardCredit 额度/余额耗尽 → 长冷却到上游给出的下次重置时刻。
	//
	// 映射：upstream.ErrHardCredit、codearts.ErrHardCredit。
	//
	// ⚠ 这正是危害 ① 的落点：codearts 的 `insufficient quota` 必须归到这一类，
	// 而不是 ErrKindNone。
	ErrKindHardCredit

	// ErrKindSoftRate 限流（429）→ 短冷却。
	//
	// 映射：upstream.ErrSoftRate、codearts.ErrSoftRate。
	ErrKindSoftRate

	// ErrKindSessionDead 会话已死 → 永久禁用（需人工重登）。
	//
	// 映射：upstream.ErrSessionDead。
	//
	// ⚠ **codearts 没有对应项**，这是刻意且关键的：
	// codearts 的「凭证失效」是 ErrKindAuth（可自动续期恢复），
	// 与 workbuddy 的 session dead（必须人工重登）**不是同一件事**。
	// 把 codearts 的某类错误映到这里，就等于把可自动恢复的故障
	// 升级成永久禁用 —— 正是危害 ② 的实质。
	ErrKindSessionDead

	// ErrKindNotFound 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）。
	//
	// 映射：upstream.ErrNotFound、codearts.ErrNotFound。
	ErrKindNotFound

	// ErrKindAuth 凭证失效/需续期。
	//
	// 映射：**仅** codearts.ErrAuth。
	//
	// workbuddy 没有独立的 auth 类别 —— 它的 401 要么是 session dead
	// （含 12153），要么落到 ErrClient（只换号不罚）。
	//
	// # codearts 的 ErrAuth 为什么映射到「只换号不罚」而不是 Disable
	//
	// 见 ErrKindSessionDead 的注释：codearts 的凭证失效可以由
	// RefreshCredential 恢复（后台任务 + 请求路径都会续期），
	// 所以它**不得**触发永久禁用。core 对 ErrKindAuth 的处理是
	// "换号、不罚"（与 ErrKindClient 同路径），这是保守方向：
	// 宁可多换一次号，也不能把可恢复的账号永久打掉。
	ErrKindAuth

	// ErrKindServer 5xx 上游故障 → 喂熔断计数。
	//
	// 映射：upstream.ErrServer、codearts.ErrServer。
	ErrKindServer

	// ErrKindClient 其他 4xx / 业务错误 → 只换号不罚（防雪崩），不喂熔断。
	//
	// 映射：upstream.ErrClient、codearts.ErrClient、codearts.ErrAuth（见上）。
	ErrKindClient

	// ErrKindContentBlocked 上游**内容策略**拦截 → 不罚账号，由提示词降级重试消化。
	//
	// 映射：upstream.ErrContentBlocked（**仅** workbuddy）。
	//
	// # 为什么它必须是一个独立类别，而不是并到 ErrKindClient
	//
	// 两者的**处置完全不同**：
	//
	//	ErrKindClient        客户端/参数问题 → 换号重试（别的号可能就能过）
	//	ErrKindContentBlocked 内容问题       → 换号**没有意义**（每个号都会被同一套策略拦），
	//	                                       正确的动作是换提示词重试
	//
	// 并到 ErrKindClient 的后果是：一次内容拦截会连着消耗 MaxRotate 个账号，
	// 每个都白跑一次往返，最后返回"所有账号不可用" —— 一个把
	// "内容被拦"误报成"账号池故障"的错误结论。
	//
	// # 为什么 core 要单独识别它
	//
	// 因为处置动作（触发降级 + 用降级提示词重试）是 core 的职责：
	// 提示词替换发生在出站客户端（见 internal/prompt 与 upstream 的 applyPrompt），
	// 而"观察到了拦截"这件事只有出站循环知道。core 需要这个类别来搭桥。
	//
	// # 为什么它在枚举末尾（值 8）而不是插在中间
	//
	// ErrorKind 的取值会被写进日志、也可能被按整数传递。
	// 插在中间会让所有既有取值的数字全部位移 —— 历史日志与新日志无法比对，
	// 任何 `gateway.ErrorKind(n)` 形式的代码也会静默错位。
	// 因此**只追加，不插入**。有测试钉住（TestErrorKindValuesAreStable）。
	ErrKindContentBlocked
)

// String 人类可读的分类名（与 upstream.ErrKind 的拼写**逐字一致**，
// 便于日志在两个上游之间可 grep、可对比）。
//
// ⚠ ErrKindAuth 单独返回 "auth"（upstream 侧没有这个值）。
func (k ErrorKind) String() string {
	switch k {
	case ErrKindHardCredit:
		return "hard_credit"
	case ErrKindSoftRate:
		return "soft_rate"
	case ErrKindSessionDead:
		return "session_dead"
	case ErrKindNotFound:
		return "not_found"
	case ErrKindAuth:
		return "auth"
	case ErrKindServer:
		return "server"
	case ErrKindClient:
		return "client"
	case ErrKindContentBlocked:
		return "content_blocked"
	default:
		return "none"
	}
}

// ErrorClassifier 上游自报「我返回的错误体怎么分类」。
//
// # 为什么参数是 (status, body) 而不是一个 error
//
// 因为真正需要分类的那份数据**还不是 error**：出站循环拿到的是
// (HTTP 状态码, 上游响应体原文)。上游的错误码表是按这两个东西查的
// （workbuddy 查 body 里的 12153，codearts 查 InferHub.4291），
// 先包成 error 再解包只会丢掉正文。
//
// 这也让实现保持**纯函数**：不读网络、不改状态，可以在请求路径上被调用。
type ErrorClassifier interface {
	// Classify 按 HTTP 状态码 + 响应体判定错误类别。
	//
	// # 实现约束
	//
	//   - **必须纯函数**：不发网络请求、不读/写共享状态。它在出站循环的
	//     每次非 2xx 上被调用一次。
	//   - **不得 panic**（与 Provider 的契约一致）。
	//   - 传入的 body 已被调用方限制过长度（1MB 上限），实现不必再截断。
	//   - **只有自己真正认识的判据才返回硬分类**。拿不准时返回
	//     ErrKindNone（"我不认识这个错误"）比猜一个类别更安全 ——
	//     猜错的代价见包注释的两个危害。
	//
	// # 返回值语义
	//
	//	ErrKindNone      我认识这个错误，它不需要惩罚（或我不认识它）
	//	其余             core 会据此施加冷却/禁用/熔断，见 ErrorKind 各常量
	//
	// ⚠ 返回 ErrKindNone 时 core **只换号不惩罚** —— 这是安全的默认方向：
	// 漏判的代价是"额度耗尽被忽略"（包注释危害 ①），
	// 所以**上游应当把"我确实额度耗尽了"这件事明确报出来**，
	// 而不是让 core 去猜。
	Classify(status int, body string) ErrorKind
}

// DefaultErrorKind 是**通用**兜底分类：只按 HTTP 状态码判断，不看正文。
//
// # 为什么兜底必须存在，且必须不看正文
//
// 出站循环的调用方是 core，它**不认识任何具体上游**（判据 1）。
// 一个没实现 ErrorClassifier 的上游仍然要被正确处理，
// 所以需要一份"没有任何上游专有知识"的兜底判据 —— 状态码是唯一
// 跨上游真的有共识的东西（RFC 定义的语义）。
//
// # 为什么兜底**不**含余额关键词
//
// 关键（这是本文件的核心取舍）：把 workbuddy 的 hardMarkers 写进兜底，
// 等于让每个上游都被 workbuddy 的词汇表解释 —— 那正是本扩展点要修的 bug。
// 所以兜底**刻意**只映射状态码：
//
//	402 → ErrKindHardCredit    （Payment Required，RFC 语义就是"要付钱"）
//	429 → ErrKindSoftRate
//	404 → ErrKindNotFound
//	5xx → ErrKindServer
//	其他 4xx → ErrKindClient
//	< 400 → ErrKindNone
//
// 正文层面的判据（额度关键词、会话码）**必须由上游自己报**。
// workbuddy 的实现在它自己的包里（internal/upstream 的适配），
// codearts 的实现在 codearts 包里，两者互不影响。
//
// ⚠ 这也意味着：`upstream.Classify` 的行为**没有**被这个兜底取代。
// 单上游模式（Provider == nil）仍然**逐字**走 upstream.Classify，
// 保证既有部署行为逐字节不变（见 handler.go 的回落分支）。
func DefaultErrorKind(status int, body string) ErrorKind {
	_ = body // 刻意不读：见函数注释「为什么兜底不含余额关键词」
	switch {
	case status == 402:
		return ErrKindHardCredit
	case status == 429:
		return ErrKindSoftRate
	case status == 404:
		return ErrKindNotFound
	case status >= 500:
		return ErrKindServer
	case status >= 400:
		return ErrKindClient
	}
	return ErrKindNone
}
