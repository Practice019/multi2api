// dailyactions.go codearts 自报的每日动作（gateway.DailyActionExt 实现）。
//
// # 用户的判据（原话）
//
//	"签到也就是福利领取，就是签到。能不能统一一下呢，就是领取一下福利呗"
//	"那个按钮前端的签到按钮能不能和这个统一一下呢"
//
// 也就是说：对 codearts 而言，「领取福利」**就是**它的「签到」。
// 用户要的不是"给 codearts 造一个签到"，而是"前端那排按钮按**各上游
// 真有的动作**渲染" —— codearts 该出现的是「领取福利」，不是「签到」。
//
// # ⚠ 这里**刻意不报** checkin / keepalive
//
// 这是本文件最重要的一条，也是最容易被"顺手补上"的一条：
//
//	codearts 的 Caps() 里**没有** CapCheckin（见 provider.go 的注释：
//	"CapCheckin 没有每日签到端点"）。
//
// 所以它必须也不报 checkin 这个每日动作 —— 两处是**同一件事实**的
// 两种表达。报了的话前端会渲染出一个「签到」按钮，而它背后的
// POST /admin/checkin 是 **workbuddy 挂的路由**：
//
//	· 轻则 404（这条路由不存在于 codearts 的 AdminRoutes 里）
//	· 重则作用在**别家上游的账号**上 —— 历史上真发生过
//	  （见 internal/workbuddy/upstream_isolation_test.go 里那 703 条脏记录）
//
// 这正是"假按钮"的形态，也正是 gateway.LoginFlow.Configured 那条注释
// 反复强调要避免的东西。
//
// # 为什么 AllURL / Batch 都留空
//
// /admin/welfare/claim 的语义是**对某一个账号**领取它当前可领的全部福利
// （体是 {"uid": ...}，见 codearts/admin.go 的 handleWelfareClaim）。
// 它没有"对全部账号一键领取"这种端点 —— 核心也没有遍历各上游账号的权限
// （上游包不得依赖 internal/pool，arch_test.go 强制）。
//
// 所以 Batch 如实报 false。前端的"全部 X"按钮只对有全量端点的动作出现，
// codearts 的「领取福利」就只在**账号行内**出现。这是事实，不是缺陷。
package codearts

import "workbuddy2api/internal/gateway"

// DailyActionWelfare 福利领取动作的 ID。
//
// ⚠ 它**不是** "checkin"。用户在概念上把它们等同了，但 ID 是**协议**：
// 前端按它写 data-act，而 data-act 决定点下去调哪个端点。
// 两个上游的领取端点完全不同（workbuddy 的 /v2/billing/meter/daily-checkin
// 与 codearts 的 /v1/ops/claim），共用一个 ID 会让前端的
// `button[data-act="..."]` 选择器与端点查表在跨上游时拿到错的那个。
//
// 概念上的统一由**槽位**承担（都是 DailyAction），不是由 ID 承担。
const DailyActionWelfare = "welfare"

// DailyActions codearts 的每日动作（gateway.DailyActionExt）。
//
// # 文案为什么是「领取福利」而不是「签到」
//
// 因为按钮背后真的是**福利中心**的领取（可领多个活动，返回 results 列表），
// 不是一个"打卡"动作。写成「签到」会让用户在点完之后看到
// "福利领取 uid=... 本次领到 N/M" 的回执，两边对不上。
//
// 用户的原话是"就是领取一下福利呗" —— 他要的是统一**入口位置与交互**，
// 不是把文案也强行改成"签到"。
// DailyActions codearts 的每日动作（gateway.DailyActionExt）。
//
// # 文案为什么是「签到」（用户本轮明确要求）
//
// 上一版这里写「领取福利」，理由是"按钮背后真的是福利中心的领取"。
// 用户后来明确要求：codearts 的按钮文案改成「签到」（在他的概念里
// codearts 的福利领取就是它的签到）。
//
// ⚠ 变的是**文案**（Label），**不是** ID（仍为 welfare）也不是端点
// （/admin/welfare/claim）。ID 是协议：前端按它写 data-act、查端点表，
// 两个上游的领取端点完全不同，共用一个 ID 会让跨上游时选错端点
// （见 DailyActionWelfare 的注释）。概念统一由槽位承担，不由 ID 承担。
func (p *Provider) DailyActions() []gateway.DailyAction {
	return []gateway.DailyAction{
		{
			ID:     DailyActionWelfare,
			Label:  "签到",
			Title:  "领取该账号当前可领的全部福利（codearts 的签到）",
			OneURL: "/admin/welfare/claim",
			// AllURL 空 + Batch false：上游没有全量端点，如实报。
		},
	}
}

// 编译期断言：Provider 实现了每日动作扩展点。
var _ gateway.DailyActionExt = (*Provider)(nil)
