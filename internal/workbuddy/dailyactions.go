// dailyactions.go workbuddy 自报的每日动作（gateway.DailyActionExt 实现）。
//
// # 这个文件是**纯声明**，不含任何业务实现
//
// 签到/保活的实现一直在 adminendpoints.go，本文件一个字都不改它们。
// 这里做的只有一件事：把"workbuddy 有哪几个每日动作、各自叫什么、
// 打在哪个端点上"从**前端的硬编码**搬成**上游的自报**。
//
// # ⚠ 逐字回归约束（这是本次改动最容易搞砸的地方）
//
// 改造前，前端为**每个上游的每个账号**写死了三个按钮：
//
//	checkin    → POST /admin/checkin     文案「签到」  title「单账号签到」
//	credits    → POST /admin/credits/refresh
//	keepalive  → POST /admin/keepalive   文案「保活」  title「刷新 token」
//
// 抽象之后，workbuddy 这三条的**端点、文案、顺序**必须与上面逐字一致 ——
// 它们现在能用，行为漂移就是回归。
// 有测试逐字钉住这三点（见 dailyactions_test.go）。
//
// # 为什么 credits（刷新积分）**不在**这份清单里
//
// 它**不是每日动作** —— 它是"把余额读一遍写回池"，可以随时点、点几次都行，
// 与"每天一次"的语义无关。用户要统一的是**签到/福利领取**这一类
// 「每天能领一次的东西」，把额度刷新塞进来会让这个槽位的语义糊掉。
//
// 额度刷新的按钮仍然渲染（见 webui.html 的 accountRow）—— 它走的是
// 另一条路径：CapQuotaProbe 能力位 + /admin/credits/refresh 端点。
// 也就是说：本次改动**只接管签到与保活**，其余行内按钮一个都不动。
package workbuddy

import "workbuddy2api/internal/gateway"

// 动作 ID。用常量而不是散落的字面串：前端按 ID 写 data-act，
// 两处必须一致，且它们会出现在测试断言里。
//
// ⚠ 这两个值**必须**与改造前 webui.html 里的 data-act 逐字相同
//（"checkin" / "keepalive"）—— 前端的动作分派表、busyRun 文案表、
// 以及 tests/frontend/verify_account_buttons.js 的断言都以它们为准。
const (
	DailyActionCheckin   = "checkin"
	DailyActionKeepalive = "keepalive"
)

// DailyActions workbuddy 的每日动作（gateway.DailyActionExt）。
//
// # 顺序就是按钮顺序，而且刻意与改造前一致
//
//	账号行内：签到 → 保活 →（额度刷新由能力位路径补上）
//	账号池顶部：全部签到
//
// # ⚠ 保活报 Batch=false —— 这是**用户决定**，不是"上游不支持"
//
// 这一条我第一版写错了：我按"有没有全量端点"填了 Batch=true，
// 结果界面上冒出一个「全部保活（workbuddy）」按钮 —— 而那个按钮
// **是用户明确要求删掉的**（原因见 webui.html 里那段长注释）：
//
//	POST /admin/keepalive（不带 uid）与**自动保活槽**调的是同一个函数
//	RunKeepaliveFor，唯一差别是标签。自动保活默认开启，到点就会跑一遍。
//	所以"全部保活"= 让它们提前跑一遍，而 token 保活刷的是有有效期的凭证，
//	**提前几小时刷没有收益**。它唯一真实的用途（"刚加完账号想确认 token
//	有效"）已经被账号行的 TOKEN 胶囊覆盖了 —— 那个直接显示剩余有效期，
//	比事后点一次更有信息量。
//
// 所以 Batch 在这里表达的是"**界面要不要给它一个全量入口**"，
// 而这正是"上游能力"与"产品决策"的分离点：
//
//	/admin/keepalive 不带 uid **确实**是全量（能力在）
//	但界面**不展示**这个入口（决策如此）
//
// 把两者混成一个"有没有全量端点"的推断，就会把用户删掉的按钮**偷偷加回来**。
// 有测试钉住这一条（TestWorkbuddyKeepaliveHasNoBulkButton）。
func (p *Provider) DailyActions() []gateway.DailyAction {
	return []gateway.DailyAction{
		{
			ID:     DailyActionCheckin,
			Label:  "签到",
			Title:  "单账号签到",
			OneURL: "/admin/checkin",
			AllURL: "/admin/checkin",
			Batch:  true,
		},
		{
			ID:     DailyActionKeepalive,
			Label:  "保活",
			Title:  "刷新 token",
			OneURL: "/admin/keepalive",
			// AllURL 仍然如实报（端点是存在的），但 Batch=false
			// 让前端**不渲染**「全部保活」—— 见上面那段注释。
			AllURL: "/admin/keepalive",
			Batch:  false,
		},
	}
}

// 编译期断言：Provider 实现了每日动作扩展点。
//
// 不实现会在这里编译失败，而不是等到"前端某个按钮神秘地不出现"。
var _ gateway.DailyActionExt = (*Provider)(nil)
