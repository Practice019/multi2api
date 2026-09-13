package codearts

// errorclassifier.go —— codearts 通过 `gateway.ErrorClassifier` 自报
// 「我的错误体怎么分类」。
//
// # 为什么必须有它（这是 P2 缺口的落点，两条危害都发生在这里）
//
// 出站循环原先在**两条上游的响应上**都调 `upstream.Classify` ——
// 那是 **workbuddy 的**分类器。拿它判 codearts 的响应体，两个方向同时出错：
//
// # 危害 ①：额度漏判（额度耗尽被完全忽略）
//
// codearts 的额度耗尽原文是 `InferHub.4291.200 insufficient quota`
// （实测完整体见 quota.go 的注释）。而 workbuddy 的 hardMarkers 是：
//
//	"insufficient credit", "no credit", "credit exhausted", "out of credit",
//	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
//	"not enough credit", "积分不足", "额度不足", "余额不足", ...
//
// **一个都不匹配 `"insufficient quota"`**（注意 `"insufficient credit"`
// 与 `"insufficient quota"` 是不同的串）。于是：
//
//	upstream.Classify(200, `...insufficient quota`) → ErrNone
//	→ applyErrorPolicy 的 default 分支 → 只换号不罚
//	→ 账号不进长冷却、不标记额度，下一轮照旧选中它
//
// # 危害 ②：反向误伤（一个健康的号被永久禁用，后果最重）
//
// workbuddy 的 `sessionDeadMarkers` 含一个**裸数字 `"12153"`**。
// 它对 codearts 只是一段可能碰巧出现的子串：
//
//	upstream.Classify(200, `{"error":"InferHub.4005.200 12153 ..."}`) → ErrSessionDead
//	→ applyErrorPolicy 的 ErrSessionDead 分支 → **Pool.Disable**
//	→ 永久禁用，需人工重登
//
// # 本文件的判据从哪来（不发明第三套分类）
//
// 两条来源，都是 **codearts 自己的既有事实**：
//
//  1. `codearts.Classify`（client.go）—— HTTP 状态码 + hardMarkers 的分类。
//     它的 hardMarkers 含 `"insufficient"`（**子串匹配**），
//     因此 `"insufficient quota"` 本来就命中 —— 用它即可修掉危害 ①。
//  2. `codearts.DetectQuotaExhausted`（quota.go）—— 流内业务错误的识别。
//     它专门认 `InferHub.4291` / `"insufficient quota"` / `"余额不足"`，
//     而这正是 HTTP 200 + SSE 流内错误那种**没有非 2xx 状态码可依据**的形态。
//
// 本文件把两者**组合**起来（而不是重写判据）：见 Classify 的分派顺序。
//
// ⚠ codearts **没有** "session dead" 这个概念，所以这里**永远不返回**
// gateway.ErrKindSessionDead —— 那是 workbuddy 特有的恢复路径
// （必须人工重登）。codearts 的凭证失效是 ErrAuth（可自动续期恢复），
// 映射到 gateway.ErrKindAuth（core 侧只换号不罚）。

import (
	"workbuddy2api/internal/gateway"
)

// 编译期断言（与 gateway.Provider / AdminExt / CredentialLoader /
// CredentialRefresher / RefreshSkewExt 并列）。
var _ gateway.ErrorClassifier = (*Provider)(nil)

// Classify 按 HTTP 状态码 + 响应体判定错误类别（gateway.ErrorClassifier）。
//
// # 判据的分派顺序（顺序本身是有意义的）
//
//  1. 额度耗尽（DetectQuotaExhausted）—— **先判**
//  2. 其余交给 codearts.Classify，再翻译类型
//
// # 为什么额度判定必须在前面
//
// 因为额度耗尽的**标准形态是 HTTP 200 + 流内业务错误**
// （见 quota.go 的实测注释："它不是 HTTP 错误码，而是塞在 SSE 流里的业务错误"）。
// 先按状态码判的话，200 会被判成"成功"，额度错误就此消失 —— 这正是危害 ①。
//
// 反方向的误判也要挡住：`DetectQuotaExhausted` 只在**真的**看到
// `InferHub.4291` / `insufficient quota` / `insufficient balance` /
// 额度中文串时才返回 ok=true，范围很窄。它不会把普通的 429/500 误判成额度问题
// （`codearts.Classify` 的状态码判据在第二步仍然生效，两条是**叠加**而非替代）。
//
// # 为什么状态码判据不能省
//
// 只靠正文的话，一个空的 429 响应体会被判成 ErrKindNone
// （"只换号不罚"）—— 那会让限流不再触发软冷却。
// 两步叠加保证：**正文认不出时仍有状态码兜底**。
//
// # 纯函数（实现约束）
//
// 不发网络、不改共享状态（见 gateway.ErrorClassifier 的实现约束）。
// 刻意**不**在这里调 MarkQuotaExhausted：那需要"是哪个模型"，
// 而接缝签名里没有它，且"按模型标记额度"是与分类**无关**的第二件事。
// 额度标记由既有的探测路径（ProbeQuota）与后台任务负责。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	_ = p
	// ── 第一步：额度耗尽（含 200 + 流内业务错误）────────────────────
	//
	// ⚠ 这一条同时修掉危害 ①。用 DetectQuotaExhausted 而不是自己写关键词：
	// 那个函数是 codearts 额度事实的**唯一持有者**（quota.go 的注释明说
	// "导出给测试与 handler 复用"），复用它是本次任务的要求之一。
	if _, ok := DetectQuotaExhausted(body); ok {
		return gateway.ErrKindHardCredit
	}

	// ── 第二步：codearts 自己的状态码/关键词分类，再翻译类型 ────────
	//
	// 复用 client.go 的 Classify（**不重写判据**）：它的 hardMarkers 含
	// "insufficient"/"quota exceeded"/"额度不足" 等，且状态码判据
	// （401/403→Auth、429→SoftRate、404→NotFound、5xx→Server、其余4xx→Client）
	// 是 codearts 自己的事实。
	//
	// ⚠ 它**不含**任何 workbuddy 的判据 —— 尤其不含 "12153"。
	// 危害 ② 因此被彻底关掉：本函数不可能返回 SessionDead。
	return toGatewayKind(Classify(status, body))
}

// toGatewayKind 把 codearts.ErrKind 翻译成跨上游中立的 gateway.ErrorKind。
//
// # 为什么是显式 switch 而不是 `gateway.ErrorKind(k)`
//
// 裸类型转换依赖"两个枚举的取值顺序永远一致"这个**隐含**约定 ——
// 任何一边插一个新常量，转换就会**静默错位**。显式列举之后，
// 新增常量会落到 default，由测试立刻报红
// （见 TestToGatewayKindMapsEveryCodeartsKind）。
//
// # 翻译表（逐项）
//
//	codearts.ErrNone       → gateway.ErrKindNone
//	codearts.ErrHardCredit → gateway.ErrKindHardCredit
//	codearts.ErrSoftRate   → gateway.ErrKindSoftRate
//	codearts.ErrAuth       → gateway.ErrKindAuth        ⚠ 不是 SessionDead！
//	codearts.ErrNotFound   → gateway.ErrKindNotFound
//	codearts.ErrServer     → gateway.ErrKindServer
//	codearts.ErrClient     → gateway.ErrKindClient
//
// # ⚠ codearts.ErrAuth 为什么不映射到 SessionDead（危害 ② 的关键）
//
// 两个上游的"凭证坏了"恢复路径**完全不同**：
//
//	workbuddy  ErrSessionDead → 必须人工重登（所以 core 会 Disable）
//	codearts   ErrAuth        → RefreshToken 可自动恢复
//	                            （STS 凭证仅约 30 分钟，401 是常见路径而非异常，
//	                             见 client.go 的 MaxAuthRetry 重试逻辑与
//	                             credentialrefresher.go）
//
// 把 ErrAuth 映到 ErrKindSessionDead，就等于把"一次 401"升级成
// **永久禁用**。这是危害 ② 的另一半（第一半是裸数字 12153 的误命中）。
//
// 所以它映射到 ErrKindAuth：core 侧走"换号、不罚"的保守分支 ——
// 宁可多换一次号，也不能把可恢复的账号永久打掉。
//
// # default 为什么是 ErrKindNone
//
// 未知值**刻意不给任何惩罚性语义**。若 default 返 ErrKindSessionDead，
// 一次枚举错位就会演变成"批量永久禁用"。
func toGatewayKind(k ErrKind) gateway.ErrorKind {
	switch k {
	case ErrNone:
		return gateway.ErrKindNone
	case ErrHardCredit:
		return gateway.ErrKindHardCredit
	case ErrSoftRate:
		return gateway.ErrKindSoftRate
	case ErrAuth:
		return gateway.ErrKindAuth
	case ErrNotFound:
		return gateway.ErrKindNotFound
	case ErrServer:
		return gateway.ErrKindServer
	case ErrClient:
		return gateway.ErrKindClient
	default:
		return gateway.ErrKindNone
	}
}
