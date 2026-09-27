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
