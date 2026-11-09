// Package dailycheckin 签到类动作的**共享驱动**。
//
// # 为什么需要它（用户要求「所有上游共用一个签到接口」）
//
// 签到是**跨上游同构**的动作：都是"扫一遍我自己的账号 → 逐个查状态 →
// 没签就签 → 结果写 checkinlog"。而它此前在每个上游各写一遍：
//
//	workbuddy  checkin.go      自己扫、自己记、自己挂 DailyAction 与 Job
//	trae       extensions.go   同上（注释里还记着"忘了写历史"那个 bug）
//	codearts   admin.go        福利领取（同一形态，另一个 kind）
//
// 复制的代价在移植时立刻显现 —— lobsterai 与 qoder 移植过来了签到端点、
// Caps 也声明了 CapCheckin，但**三处接线全漏**：
//
//	账号行没有按钮（没实现 DailyActionExt）
//	没有自动签到（没实现 JobExt）
//	签到不写历史（那一列读 checkinlog，于是永远显示 `—`）
//
// 也就是说：同一个动作，谁抄漏一处就少一块功能，而**漏了不会报错**。
//
// 本包把这件事收敛成**一个接口 + 一个驱动**：
//
//	上游只实现 Upstream（三个方法：列账号、查状态、领取）
//	按钮 / 自动任务 / 单账号端点 / 全量端点 / 写历史 全部由本包提供
//
// # ⚠ 本包**不 import internal/gateway**（架构硬约束，不是偷懒）
//
// 架构判据把"internal/ 下依赖 gateway 的包"一律当成**上游实现**
// （见 gateway/arch_test.go 的 discoverUpstreams），而"上游之间不得互相依赖"。
// 所以本包一旦 import gateway 就会被判成上游，于是：
//
//	lobsterai 依赖 dailycheckin → 上游依赖上游 → **架构违规**
//
// 而这正是它第一次构建时被 arch_test 抓住的形态。
//
// 两条已有的豁免通道都走不通：
//
//	corePackages         → 会被 TestDiscoveryHasNoBlindSpot 判成"绕过约束"
//	discoveryExemptPackages → 豁免的前提是**不依赖 gateway**，与需求矛盾
//
// 所以正确答案不是"改豁免名单"，而是**本包根本不该认识 gateway 的类型**：
// 它是共享基础设施，只描述"签到这件事怎么办"；把结果翻译成
// gateway.DailyAction / gateway.Job / gateway.AdminRoute 是**上游侧**的事
// （每个上游三行，见各包的 checkin.go）。
//
// 这也是更干净的层次：驱动不依赖契约，契约也不依赖驱动。
// # ⚠ 这个包**已经是**跨上游共享的（不要再"抽"一次）——  判定记录：T-335 调查 / T-349 落档
//
// 4 个上游有签到功能，其中 lobsterai 与 qoder **早已共用本包**（各 44 / 45 处引用）。
// 它们的 checkin.go 仍然各自存在，是因为还有各自的协议细节（请求体、响应字段、错误码）；
// **流程骨架**（领什么、什么时候领、失败了怎么记）已经收敛在这里。
//
// ⇒ 搜索"跨上游重复的签到实现"会命中 4 个 checkin.go，但真正重复的只有各自那几十行协议适配，
//   而那部分**必须**分开：各家接口字段不同，归一就等于给"读错字段"开门
//   （判例见 gateway.CredentialLoader 上方的 LoadDir 实测：差异全在字段名与前缀上）。
//
// ⇒ 结论：**不抽**。下一个 agent 不必再为签到做一轮"重复度调查"。

package dailycheckin
