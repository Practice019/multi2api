// wire_reexport.go upstream 到中立线协议包（internal/wire）的转发层。
//
// # 为什么要这一层（而不是让调用方直接 import wire）
//
// 出站请求体改写（强制 stream / 归一 role 与 tool_choice / 注入 thinking /
// 脱敏指纹 / 裁剪 max_tokens）**与具体上游无关** —— 那是"OpenAI 兼容线协议"
// 的规范化，不是 workbuddy 的私有知识。
//
// 但它原先住在 internal/upstream（workbuddy 的私有 SDK）里，于是
// **codearts（另一个上游）为了改写自己的请求体，不得不 import workbuddy 的 SDK**。
// 后果不只是类型耦合：codearts 的 SanitizeFingerprints=true，于是 workbuddy 的
// 反指纹改写规则（含剥离裸数字 11-128）会逐字作用在 codearts 流量上。
//
// 现在实体搬到了 internal/wire，本文件只做**转发**：
//
//	internal/wire      真正的实现（零内部依赖，只用标准库）
//	internal/upstream  本文件的前向别名（工作簿历史：既有调用点一行不改）
//	internal/codearts  直接用 wire，不再碰 upstream
//
// # 为什么用 var 别名而不是函数包装
//
// 函数包装会让每一层多一次调用与一份签名副本；var 别名让 upstream 的这个名字
// 与 wire 里的是**同一个函数值**，行为与签名都不可能漂移。
package upstream

import "workbuddy2api/internal/wire"

// 出站请求体改写（三层签名是历史顺序，见 wire/payload.go 的注释）。
var (
	// PrepareBodyOpt 单 pass 改写。
	PrepareBodyOpt = wire.PrepareBodyOpt
	// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级。
	PrepareBodyOptWithEfforts = wire.PrepareBodyOptWithEfforts
	// PrepareBodyOptWithLimits 最完整的导出改写入口（额外接受 limits 与 efforts）。
	PrepareBodyOptWithLimits = wire.PrepareBodyOptWithLimits
	// RewriteModelField 改写请求体顶层的 model 字段（仅在确实不同时重编码）。
	RewriteModelField = wire.RewriteModelField
)

// prepareBodyOptWithLimits 未导出形态的内部实现入口。
//
// ⚠ 它是 wire 里的**未导出**函数，upstream 无法直接别名 ——
// 而它只被同包的测试用（测三层签名的等价性）。
// 故这里不转发；需要它的测试已随之搬进 wire 包。

// usage 抽取（原先住在本包，2026-09 搬到 wire）。
//
// # 为什么它也属于中立层
//
// `usage` 对象是 **OpenAI 线协议**的一部分（`prompt_tokens` /
// `completion_tokens` / `prompt_tokens_details.cached_tokens` …），
// 与"是哪个上游"无关。而 server（出口层）要从每个 chunk 里取它做日志 ——
// 那个需求同样与上游无关。
//
// 它原先住在这里，于是 **server 为了读 usage 不得不 import workbuddy 的 SDK**
// （这是 privateSDKKnownDebt 里 server 那条债的一半）。
//
// 类型也要转发：`UsageExtras` 是 `ParseUsageExtras` 的返回类型，
// 调用方要能写出 `var x upstream.UsageExtras = ...` ——
// 只转发函数不转发类型会留下一个"函数能调但结果存不下"的怪状态。
type UsageExtras = wire.UsageExtras

var (
	// ParseUsageExtras 从 usage map 里抽取结构化字段（缓存命中、推理 token 等）。
	ParseUsageExtras = wire.ParseUsageExtras
	// UsageInt 把 usage 里的数值字段安全地取成 int（容忍 string / float / 缺失）。
	UsageInt = wire.UsageInt
)

// ParseSoftRateReset 429 code=6004 的"重置时刻"解析（原先住在本包，2026-09 搬到 wire）。
//
// # 为什么它也属于中立层
//
// "从错误文案里抠出「将在 … 重置」的时刻"是**线协议层的文本解析**，
// 与是哪个上游无关。而它有两个消费者，一个属上游、一个属核心：
//
//	workbuddy  softrate_ext.go —— 它实现 gateway 的软限流扩展点
//	server     handler.go      —— 单上游回退路径（Provider == nil）
//
// 后者是核心包，**不该为了读一段文案依赖某个上游的 SDK**。
var ParseSoftRateReset = wire.ParseSoftRateReset
