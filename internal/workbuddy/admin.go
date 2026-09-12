package workbuddy

import (
	"sync"

	"workbuddy2api/internal/gateway"
)

// 管理端点（AdminExt 实现）。
//
// # 本文件在 Task 3c 阶段的定位
//
// Task 3a 先把 **22 个 workbuddy 专属端点的清单与能力归属**表达出来，
// 让核心能通过 AdminExt 发现它们；Task 3c（本步）把 handler 实现也搬进来。
// 分两步的理由：先让"清单"成为可验证的契约（能力位 ↔ 端点一一对应），
// 再搬实现 —— 一次做完的话，出问题时分不清是清单错了还是搬运错了。
//
// # 判据
//
// 这 22 个端点是**平台特殊**的：codearts 一个都没有。
// 它们之所以原先写在 core 的 admin.go 里，是历史原因，不是设计。
// 搬完之后 core 的 admin 只保留通用端点。
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
//
// # 一处刻意的例外：/admin/schedule 与 /admin/task
//
// 这两个端点读的是**核心调度器**的时点与任务槽状态，不是 workbuddy 业务。
// 它们在此注册只是为了"路由由上游自注册"这条结构不被破坏 ——
// 依赖通过 AdminEnv 的消费方接口注入（见 adminenv.go），本包不 import scheduler。
// 若将来出现第二个上游，把这两条移回 core 是更干净的做法（详见 adminenv.go 的注释）。

// AdminEnv 核心为上游管理端点提供的只读依赖（账号池之外的通用设施）。
//
// 它只被 Schedule/TaskStatus 两条端点使用；nil 时这两条端点降级为 501 / 空快照，
// 与改造前"未接线就报错而不是崩"的行为一致。
type AdminEnv struct {
	// Schedule 调度状态来源（见 SchedulerView）。
	Schedule SchedulerView
	// TaskSlot 全量任务的共用任务槽（见 TaskSlot）。
	TaskSlot TaskSlot
}

// AdminHandler workbuddy 22 个管理端点的宿主。
//
// 为什么不是直接在 *Provider 上挂方法：这些 handler 需要两份**只属于 HTTP 层**的状态
// （后台任务槽、以及将来可能的按请求状态），把它们塞进 Provider 会污染上游身份。
// Provider 只负责"业务怎么做"，Handler 负责"HTTP 怎么答"。
//
// 它**不持有账号池/上游客户端的独立副本** —— 全部经 p 转发，
// 保证管理台与守卫轮看到的是同一份业务状态。
type AdminHandler struct {
	p   *Provider
	env AdminEnv

	// task 后台任务槽：全量签到/保活/旅程/成长动作共用。
	// 同一时刻只允许一个（见 TaskSlot 的注释）。
	task TaskSlot

	// mu 保护 adminEnv 的延迟注入（见 SetEnv）。
	mu  sync.Mutex
	set bool
}

// NewAdminHandler 用 Provider 与核心依赖建一个管理端点宿主。
//
// p 为 nil 时不 panic：所有端点各自降级（与"未接线"语义一致），
// 这样测试里 NewAdminHandler(nil, ...) 也不会崩。
func NewAdminHandler(p *Provider, env AdminEnv) *AdminHandler {
	h := &AdminHandler{p: p, env: env, task: env.TaskSlot, set: true}
	if h.task == nil {
		h.task = newTaskSlot()
	}
	return h
}

// SetEnv 延迟注入核心依赖。
//
// 为什么需要它：Provider 在 cmd/server 里先构造（要拿到它注册进网关），
// 而调度器是后构造的 —— 管理端点宿主必须在调度器就绪后才知道 Schedule 从哪来。
// 非线程安全：只在启动期调用一次。
func (h *AdminHandler) SetEnv(env AdminEnv) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.env = env
	h.set = true
	if env.TaskSlot != nil {
		h.task = env.TaskSlot
	}
}

// AdminRoutes 返回 workbuddy 专属的管理端点。
//
// 核心只负责遍历挂载；**加新上游时 core 的 admin 包零改动**。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	h := NewAdminHandler(p, p.adminEnv)
	return h.Routes()
}

// Routes 返回挂载清单（供需要显式构造宿主的调用方使用，例如测试与 cmd/server）。
func (h *AdminHandler) Routes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		// ---- 签到 ----
		{Method: "POST", Path: "/admin/checkin", Handler: h.Checkin, Capability: gateway.CapCheckin, Title: "立即签到"},
		{Method: "GET", Path: "/admin/checkin/history", Handler: h.History, Capability: gateway.CapCheckin, Title: "签到历史"},

		// ---- 保活 ----
		{Method: "POST", Path: "/admin/keepalive", Handler: h.Keepalive, Capability: gateway.CapCheckin, Title: "立即保活"},

		// ---- 额度刷新 ----
		{Method: "POST", Path: "/admin/credits/refresh", Handler: h.CreditsRefresh, Capability: gateway.CapQuotaProbe, Title: "刷新积分"},

		// ---- 成长中心 ----
		{Method: "GET", Path: "/admin/growth", Handler: h.GrowthList, Capability: gateway.CapGrowth, Title: "成长计划"},
		{Method: "GET", Path: "/admin/growth/tasks", Handler: h.GrowthTasks, Capability: gateway.CapGrowth, Title: "任务列表"},
		{Method: "POST", Path: "/admin/growth/claim", Handler: h.GrowthClaim, Capability: gateway.CapGrowth, Title: "领取奖励"},
		{Method: "POST", Path: "/admin/growth/accept", Handler: h.GrowthAccept, Capability: gateway.CapGrowth, Title: "接取任务"},
		{Method: "POST", Path: "/admin/growth/redeem", Handler: h.GrowthRedeem, Capability: gateway.CapGrowth, Title: "兑换"},
		{Method: "POST", Path: "/admin/growth/makeup", Handler: h.GrowthMakeup, Capability: gateway.CapGrowth, Title: "补签"},
		{Method: "POST", Path: "/admin/growth/open", Handler: h.GrowthOpen, Capability: gateway.CapGrowth, Title: "开启宝箱"},
		{Method: "POST", Path: "/admin/growth/draw", Handler: h.GrowthDraw, Capability: gateway.CapGrowth, Title: "抽奖"},
		{Method: "GET", Path: "/admin/growth/travel/config", Handler: h.GrowthTravelConfig, Capability: gateway.CapGrowth, Title: "旅行配置"},

		// ---- 猫猫旅行 ----
		{Method: "GET", Path: "/admin/travel", Handler: h.TravelList, Capability: gateway.CapTravel, Title: "猫猫旅行"},
		{Method: "GET", Path: "/admin/travel/status", Handler: h.TravelStatus, Capability: gateway.CapTravel, Title: "旅行状态"},
		{Method: "POST", Path: "/admin/travel/depart", Handler: h.TravelDepart, Capability: gateway.CapTravel, Title: "派猫出行"},
		{Method: "POST", Path: "/admin/travel/claim", Handler: h.TravelClaim, Capability: gateway.CapTravel, Title: "领取旅行奖励"},

		// ---- 调度（workbuddy 的守时任务）----
		{Method: "GET", Path: "/admin/schedule", Handler: h.Schedule, Capability: gateway.CapCheckin, Title: "调度状态"},
		{Method: "GET", Path: "/admin/task", Handler: h.TaskStatus, Capability: gateway.CapCheckin, Title: "任务状态"},

		// ---- 本机客户端登录（workbuddy 专属）----
		{Method: "GET", Path: "/admin/client-login", Handler: h.ClientLoginStatus, Capability: gateway.CapChat, Title: "本机登录状态"},
		{Method: "POST", Path: "/admin/client-login/switch", Handler: h.ClientLoginSwitch, Capability: gateway.CapChat, Title: "切换本机登录"},
		{Method: "POST", Path: "/admin/client-login/restore", Handler: h.ClientLoginRestore, Capability: gateway.CapChat, Title: "回滚本机登录"},
	}
}

// 编译期断言：Provider 实现了这些扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"路由神秘地没挂上"。
var (
	_ gateway.Provider = (*Provider)(nil)
	_ gateway.AdminExt = (*Provider)(nil)
)
