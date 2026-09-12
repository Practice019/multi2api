// accounts.go workbuddy 对账号池的**消费方视图**。
//
// # 为什么不让 workbuddy 直接 import internal/pool
//
// 架构约束（gateway/arch_test.go 的 TestUpstreamsDoNotDependOnCore）明确禁止
// 上游包依赖 pool/admin/server/scheduler —— 否则"上游可插拔"不成立：
// 拔掉一个上游会牵连账号池，换一个池实现又要改所有上游。
//
// 但上游业务**确实需要**账号：成长/旅行的每一步都要拿凭证打上游接口，
// 还要把刷新到的余额写回池。解法是 Go 的隐式接口：
// **接口由消费方（本包）声明，pool.Pool 已经满足它**，无需 pool 做任何改动，
// 也无需本包 import pool 的类型 —— 因为本包只用到几个字段，
// 用本地声明的窄视图即可（见 Account / AccountPool）。
//
// 接线在 cmd/server：把 *pool.Pool 经 cmd/server 的薄适配器传进来
// （适配器做一次 []pool.Status → []Account 的逐字段转换）。
//
// # 这个接口为什么这么窄
//
// 只列本包真正用到的：列账号、取凭证、读状态、写余额、写解冻、写禁用。
// 刻意不把 *pool.Pool 的公开方法全抄一遍 —— 接口越宽，上游对核心的
// 隐含依赖越深，将来换池实现越难。
package workbuddy

import "workbuddy2api/internal/auth"

// Account 账号池在本包看来是什么样（只保留本包真正读的字段）。
//
// 为什么用本地类型而不是 pool.Status：后者会让本包 import pool，
// 直接违反架构约束。本包对账号的全部需求就是下面这四项。
type Account struct {
	UID      string
	Nickname string
	Disabled bool
	Cooling  bool
}

// AccountPool 账号池在本包看来提供哪些能力。
//
// cmd/server 用一个薄适配器把 *pool.Pool 包装成它。
type AccountPool interface {
	// List 全部账号（含禁用）。
	List() []Account
	// AuthByUID 取账号凭证；不存在返回 nil。无凭证时返回的 Auth 里 RefreshToken 为空。
	AuthByUID(uid string) *auth.Auth
	// Has 报告账号是否存在。
	Has(uid string) bool
	// SetCredits 更新账号余额（单值形态）。
	SetCredits(uid string, credits int64)
	// ReenableIfCredits 签到/领奖后按"余额 > 0 即可用"解冻账号。
	ReenableIfCredits(uid string, remain int64)
	// Disable 永久禁用账号（session 死亡）。
	Disable(uid, reason string)
}
