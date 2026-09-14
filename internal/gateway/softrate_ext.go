// softrate_ext.go 扩展点：上游自报「我的限流是不是精确到某个模型的某时刻」。
//
// # 为什么它是独立扩展点，而不是并进 ErrorClassifier
//
// `ErrorClassifier.Classify(status, body) ErrorKind` 只能回答"这属于哪一类"。
// 而模型级限流比"哪一类"多两个信息：
//
//	① 一个**时刻**（上游明说的重置墙钟）—— 冷却截止不该再靠指数退避猜
//	② 一个**范围**（只对该模型生效）—— 换模型应立即可用
//
// 两者都是"数据"而非"类别"。硬塞进 ErrorKind 只能表达成"软限流的一个子类"，
// 而那丢掉的数据恰恰是收窄冷却所需的全部内容。
//
// 也不并进 `ErrorClassifier` 的方法集：那会破坏它的**最小性**——
// 该接口刻意只有一个方法（有测试钉住 `TestErrorClassifierInterfaceIsMinimal`），
// 因为接口越大，上游被迫实现的东西越多，"加新上游核心零改动"越难守。
//
// # 为什么入参里没有"当前请求的模型"
//
// 因为上游的错误体里**没有**模型名 —— 模型是**调用方**知道的（就是本次请求
// 请求的那个）。接口只负责回答"重置于何时"，范围由 core 用 reqModel 补上。
// 让接口收 model 参数会诱使实现去猜一个它根本拿不到的值。
package gateway

import "time"

// SoftRateExt 上游自报"这次限流是否精确到某模型的某时刻"。
//
// # 实现约束（与 ErrorClassifier 一致）
//
//   - **必须纯函数**：不发网络请求、不读写共享状态。它在出站循环的
//     每次软限流上被调用一次。
//   - **不得 panic**。
//   - **只有真正解析出上游给的重置时刻才返回 ok=true**。拿不准时返回
//     ok=false 更安全 —— 那会退回"基数 × 指数退避"的既有行为，
//     而错误地返回一个时刻会让冷却被收窄（账号过早回到候选集，继续撞限流）。
type SoftRateExt interface {
	// SoftRateReset 解析 (status, body) 里上游给出的"将在 … 重置"时刻。
	//
	// ok=false 表示"这不是一次能定位到时刻的模型级限流"，
	// core 会走既有的账号级软冷却路径（softRate 基数 + 指数退避）。
	SoftRateReset(status int, body string) (resetAt time.Time, ok bool)
}
