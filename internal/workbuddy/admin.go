package workbuddy

import (
	"net/http"

	"workbuddy2api/internal/gateway"
)

// 管理端点（AdminExt 实现）。
//
// # 本文件在 Task 3a 阶段的定位
//
// 这一步先把 **22 个 workbuddy 专属端点的清单与能力归属**表达出来，
// 让核心能通过 AdminExt 发现它们。
//
// Task 3c 会把 handler 实现也搬进来（现在还在 internal/admin）。
// 分两步的理由：先让"清单"成为可验证的契约（能力位 ↔ 端点一一对应），
// 再搬实现 —— 一次做完的话，出问题时分不清是清单错了还是搬运错了。
//
// # 判据
//
// 这 22 个端点是**平台特殊**的：codearts 一个都没有。
// 它们之所以现在写在 core 的 admin.go 里，是历史原因，不是设计。
// 搬完之后 core 的 admin 只保留 23 个通用端点。
//
// 数量核对（与改造前的 admin.go 逐条比对）：
//
//	签到      /admin/checkin, /admin/checkin/history
//	保活      /admin/keepalive
//	额度      /admin/credits/refresh
//	成长      /admin/growth, /claim, /accept, /redeem, /makeup, /open, /draw,
//	          /tasks, /travel/config              （9 个）
//	旅行      /admin/travel, /status, /depart, /claim  （4 个）
//	调度      /admin/schedule, /admin/task          （2 个）
//	本机登录  /admin/client-login, /switch, /restore  （3 个）
//	                                              合计 22 个

// AdminRoutes 返回 workbuddy 专属的管理端点。
//
// 核心只负责遍历挂载；**加新上游时 core 的 admin 包零改动**。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		// ---- 签到 ----
		{Method: "POST", Path: "/admin/checkin", Handler: notMovedYet("checkin"), Capability: gateway.CapCheckin, Title: "立即签到"},
		{Method: "GET", Path: "/admin/checkin/history", Handler: notMovedYet("history"), Capability: gateway.CapCheckin, Title: "签到历史"},

		// ---- 保活 ----
		{Method: "POST", Path: "/admin/keepalive", Handler: notMovedYet("keepalive"), Capability: gateway.CapCheckin, Title: "立即保活"},

		// ---- 额度刷新 ----
		{Method: "POST", Path: "/admin/credits/refresh", Handler: notMovedYet("creditsRefresh"), Capability: gateway.CapQuotaProbe, Title: "刷新积分"},

		// ---- 成长中心 ----
		{Method: "GET", Path: "/admin/growth", Handler: notMovedYet("growthList"), Capability: gateway.CapGrowth, Title: "成长计划"},
		{Method: "GET", Path: "/admin/growth/tasks", Handler: notMovedYet("growthTasks"), Capability: gateway.CapGrowth, Title: "任务列表"},
		{Method: "POST", Path: "/admin/growth/claim", Handler: notMovedYet("growthClaim"), Capability: gateway.CapGrowth, Title: "领取奖励"},
		{Method: "POST", Path: "/admin/growth/accept", Handler: notMovedYet("growthAccept"), Capability: gateway.CapGrowth, Title: "接取任务"},
		{Method: "POST", Path: "/admin/growth/redeem", Handler: notMovedYet("growthRedeem"), Capability: gateway.CapGrowth, Title: "兑换"},
		{Method: "POST", Path: "/admin/growth/makeup", Handler: notMovedYet("growthMakeup"), Capability: gateway.CapGrowth, Title: "补签"},
		{Method: "POST", Path: "/admin/growth/open", Handler: notMovedYet("growthOpen"), Capability: gateway.CapGrowth, Title: "开启宝箱"},
		{Method: "POST", Path: "/admin/growth/draw", Handler: notMovedYet("growthDraw"), Capability: gateway.CapGrowth, Title: "抽奖"},
		{Method: "GET", Path: "/admin/growth/travel/config", Handler: notMovedYet("growthTravelConfig"), Capability: gateway.CapGrowth, Title: "旅行配置"},

		// ---- 猫猫旅行 ----
		{Method: "GET", Path: "/admin/travel", Handler: notMovedYet("travelList"), Capability: gateway.CapTravel, Title: "猫猫旅行"},
		{Method: "GET", Path: "/admin/travel/status", Handler: notMovedYet("travelStatus"), Capability: gateway.CapTravel, Title: "旅行状态"},
		{Method: "POST", Path: "/admin/travel/depart", Handler: notMovedYet("travelDepart"), Capability: gateway.CapTravel, Title: "派猫出行"},
		{Method: "POST", Path: "/admin/travel/claim", Handler: notMovedYet("travelClaim"), Capability: gateway.CapTravel, Title: "领取旅行奖励"},

		// ---- 调度（workbuddy 的守时任务）----
		{Method: "GET", Path: "/admin/schedule", Handler: notMovedYet("schedule"), Capability: gateway.CapCheckin, Title: "调度状态"},
		{Method: "GET", Path: "/admin/task", Handler: notMovedYet("taskStatus"), Capability: gateway.CapCheckin, Title: "任务状态"},

		// ---- 本机客户端登录（workbuddy 专属）----
		{Method: "GET", Path: "/admin/client-login", Handler: notMovedYet("clientLoginStatus"), Capability: gateway.CapChat, Title: "本机登录状态"},
		{Method: "POST", Path: "/admin/client-login/switch", Handler: notMovedYet("clientLoginSwitch"), Capability: gateway.CapChat, Title: "切换本机登录"},
		{Method: "POST", Path: "/admin/client-login/restore", Handler: notMovedYet("clientLoginRestore"), Capability: gateway.CapChat, Title: "回滚本机登录"},
	}
}

// notMovedYet 占位 handler，Task 3c 会替换成真实实现。
//
// 为什么现在就要有 handler 而不是 nil：
// 契约测试检查每个 AdminRoute 的 Handler 非 nil，nil 会让"清单不完整"这件事
// 必须等到 Task 3c 才能发现。现在放一个明确报错的占位，
// 既满足契约（结构完整），又不会让人误以为它已可用。
func notMovedYet(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w,
			"workbuddy: 端点 "+name+" 尚未迁移到 Provider（Task 3c 待完成）",
			http.StatusNotImplemented)
	}
}

// 编译期断言：Provider 实现了这些扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"路由神秘地没挂上"。
var (
	_ gateway.Provider = (*Provider)(nil)
	_ gateway.AdminExt = (*Provider)(nil)
	_ http.HandlerFunc = notMovedYet("x")
)
