// account_columns.go 第六个扩展点：上游自报「我的账号在控制台账号池里该显示哪些列」。
//
// # 为什么需要它（用户实测的诉求）
//
// 账号池原先是**一张共用表头的表**：11 列写死在页面的静态 HTML 里，
// 所有上游的账号都堆在同一张表内。但**列不是每个上游都共有的**：
//
//	workbuddy  有 accessToken + 有效期（实测约 60 天）、有「今日签到」
//	codearts   凭证是 AK/SK/SecurityToken + 一次性的 refresh_token，
//	           它没有「今日签到」这个概念（它的每日动作叫「领取福利」），
//	           而它**有**的 STS 有效期却因为读不到而一直渲染成「—」
//
// 结果就是用户看到的：codearts 那几列全是空的。共用表头强迫每个上游
// 接受同一套列，而它们的事实并不相同。
//
// # 前端不许硬编码上游名（本项目最硬的判据）
//
// 所以「谁显示哪些列」**不能**在前端写 `if (provider === 'codearts')`：
// 那样加第三个上游就要改前端。正确方向是**上游自己报**（与
// AdminExt / JobExt / LoginFlow / AuthDirExt / CredentialLoader 同构），
// 前端只认**列 id**，不认识任何上游名。
//
// # 只报 id，不报标题与渲染函数
//
// 标题与渲染是**前端的事实**（它知道字号、配色、怎么画胶囊）。上游能报的
// 只有「我需要哪几列」—— 那是数据契约，不是表现。所以这里传的是 id 数组。
//
// 未登记的 id（契约漂移）由前端 console.warn + 跳过，**不炸页面**。
package gateway

// 账号池列 id 的规范词汇表。
//
// 这是前后端之间**唯一**的列契约：Go 侧用它做默认列与校验，前端用它查
// 「列 id → 标题 + 单元格渲染器」的注册表。
//
// ⚠ 加一列要同时改三处：这里、`DefaultAccountColumns()`、
// 以及 webui.html 的列注册表。少改一处不会编译失败 —— 前端会对未知 id
// 报警并跳过（这是刻意的：宁可少一列，也不要整张表炸掉）。
const (
	AccountColProvider    = "provider"     // 上游
	AccountColNickname    = "nickname"     // 昵称
	AccountColUID         = "uid"          // UID
	AccountColQuota       = "quota"        // 额度
	AccountColStatus      = "status"       // 状态（正常/冷却/禁用/熔断）
	AccountColToken       = "token"        // Token（剩余有效期）
	AccountColTokenExpiry = "token_expiry" // Token 到期（绝对值 + 相对时间）
	AccountColCheckin     = "checkin"      // 今日签到
	AccountColWelfare     = "welfare"      // 福利（今天领过没有 / 还能领几项）
	AccountColSuccess     = "success"      // 成功次数
	AccountColBreaker     = "breaker"      // 熔断次数
	AccountColInFlight    = "in_flight"    // 在途请求
	AccountColOps         = "ops"          // 操作
)

// DefaultAccountColumns 未自报列的上游使用的那一套（= 改造前写死的 11 列）。
//
// # 为什么默认值必须与改造前**逐列一致**
//
// 用户明确要求 workbuddy「直接复用现在的标题」。而 workbuddy 不实现本扩展点，
// 走的就是这条默认路径 —— 所以这个数组的顺序与内容**就是**它的表头契约。
// 改动它会直接改变 workbuddy 的观感，属于可见回归。
//
// 返回**副本**：调用方可能排序或裁剪，不该让默认表被就地改写
//（那会让下一次调用拿到被改过的"默认值"，且不报错）。
func DefaultAccountColumns() []string {
	return []string{
		AccountColProvider,
		AccountColNickname,
		AccountColUID,
		AccountColQuota,
		AccountColStatus,
		AccountColToken,
		AccountColCheckin,
		AccountColSuccess,
		AccountColBreaker,
		AccountColInFlight,
		AccountColOps,
	}
}

// IsKnownAccountColumn 报告 id 是否在规范词汇表里。
//
// 用途：上游自报时做一次校验（拼错列名是**静默**失效 —— 那一列直接消失，
// 界面上看不出来是漏了还是本来没有）。调用方应当对未知 id 记日志。
func IsKnownAccountColumn(id string) bool {
	switch id {
	case AccountColProvider, AccountColNickname, AccountColUID, AccountColQuota,
		AccountColStatus, AccountColToken, AccountColTokenExpiry, AccountColCheckin,
		AccountColWelfare, AccountColSuccess, AccountColBreaker, AccountColInFlight,
		AccountColOps:
		return true
	}
	return false
}

// AccountColumnsExt 上游自报「账号池里我要显示哪些列」（有序）。
//
// # 不实现它意味着什么
//
// **不实现 = 用核心默认列**（`DefaultAccountColumns()`，即改造前的 11 列）。
// 这与"实现了但返回空数组"是**两件不同的事**：
//
//	未实现        → 默认 11 列（workbuddy 的现状，用户要求保持）
//	实现了、返回空 → 明确说"我一列都不要"（合法但无意义，调用方照做）
//
// 用 `ExtOf` 的存在性区分这两者，而不是用一个空数组当哨兵 ——
// 空数组是**合法值**，拿它当哨兵会让"这个上游没有列"与"这个上游用默认列"
// 无法区分，而那正是本项目反复吃过的"静默回落"形态。
type AccountColumnsExt interface {
	// AccountColumns 返回本上游要显示的列 id（有序，决定表头顺序）。
	//
	// 只报 id，不报标题与渲染 —— 那是前端的事实（见本文件顶部注释）。
	// 未知 id 由调用方记日志、前端跳过该列，不阻断渲染。
	AccountColumns() []string
}
