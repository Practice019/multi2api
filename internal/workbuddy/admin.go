package workbuddy

import (
	"strings"
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
//	本机登录  /admin/client-login, /switch, /restore  （3 个）
//	                                              合计 20 个
//
// # 曾经有 22 个：/admin/schedule 与 /admin/task 已移回核心
//
// 这两条读的是**核心调度器**的时点与任务槽状态，不表达任何上游身份。
// Task 3c 曾把它们放这里（理由：写方 checkin/keepalive 在本包，读写不宜分家），
// 但阶段 0 评审指出这会**真出问题**：
//
//	第二个上游若不声明 CapCheckin → 这两条路由**没人服务**，
//	且前端按能力位把它们隐藏 —— 而它们本该对所有上游可见。
//
// 已移回 `internal/admin` 作为通用端点（见那里的注册处）。

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
	return h.allRoutes()
}

// Routes 返回挂载清单（供需要显式构造宿主的调用方使用，例如测试与 cmd/server）。
func (h *AdminHandler) Routes() []gateway.AdminRoute {
	return h.prefixed([]gateway.AdminRoute{
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

		// ---- 本机客户端登录（workbuddy 专属）----
		//
		// # 为什么 Capability 是 0（归 "core"）而不是 CapChat（评审 F2）
		//
		// 这三条路由原先声明 CapChat。那是错的，有两条独立理由：
		//
		//  1. **CapChat 描述的是"这个上游能不能对话"，不是"它有没有本机登录面板"。**
		//     这三条与对话能力毫无关系 —— 它们是本地客户端的登录态管理。
		//     用 CapChat 当占位符等于把"基础能力"和"专属面板"两个概念混在一起。
		//
		//  2. 前端的 hasAnyPanel 明确**排除** chat/models（理由是那两个能力
		//     每个上游都有，不构成"专属面板"判据）。所以声明 CapChat 的路由
		//     在导航里永远不会被当成面板入口 —— 声明与实际效果自相矛盾。
		//
		// 归 0 之后它们走 manifest 里已有的保留字 "core" 通道（见
		// uimanifest.go 的 routeCapName）。语义是"有端点，但没有对应能力位"，
		// 这正是事实。将来若真有人需要本机登录面板，正确做法是新增一个
		// 专属能力位，而不是借用 CapChat。
		{Method: "GET", Path: "/admin/client-login", Handler: h.ClientLoginStatus, Title: "本机登录状态"},
		{Method: "POST", Path: "/admin/client-login/switch", Handler: h.ClientLoginSwitch, Title: "切换本机登录"},
		{Method: "POST", Path: "/admin/client-login/restore", Handler: h.ClientLoginRestore, Title: "回滚本机登录"},
	})
}

// prefixed 按实例 ID 给管理端点路径加前缀。
//
// # 为什么需要它（海外版渠道支持）
//
// 海外版（workbuddy-intl）与国内版是**同一个实现**注册的第二个实例，
// 两者声明的 AdminRoutes 路径完全相同 —— 而 admin 核心挂载时对同 pattern
// 保留先注册者（见 admin.mountUpstreamRoutes 的冲突规则），后注册实例的
// 端点会整批被跳过。给非默认实例的路径加 `/<id>` 前缀后冲突消失，
// 且 manifest 下发的 admin_routes 路径同步带前缀，前端按 manifest 渲染自动适配。
//
// 默认实例（providerID "workbuddy"）不加前缀 —— 既有部署的端点路径
// 与前端行为逐字节不变。
//
// 同时按实例裁剪玩法类端点：DisableGrowthTravel（海外版）实例不挂
// 签到/成长/旅行/活动类端点 —— 上游没有这些玩法（product.json 显式禁用），
// 挂了只会得到恒失败。判定走双通道：能力位（CapCheckin/CapGrowth/CapTravel）
// + 路径前缀（autotask/school/blackcat 等玩法端点的 Capability 为 0，
// 能力位通道滤不到它们，见 autoTaskRoutes）。
func (h *AdminHandler) prefixed(routes []gateway.AdminRoute) []gateway.AdminRoute {
	prefix := h.pathPrefix()
	out := make([]gateway.AdminRoute, 0, len(routes))
	for _, r := range routes {
		if h.p != nil && h.p.cfg.DisableGrowthTravel &&
			(r.Capability&(gateway.CapCheckin|gateway.CapGrowth|gateway.CapTravel) != 0 ||
				isPlayFeaturePath(r.Path)) {
			continue
		}
		if prefix != "" {
			r.Path = prefix + r.Path
		}
		out = append(out, r)
	}
	return out
}

// isPlayFeaturePath 判定路径是否属于「玩法类」（签到/成长/旅行/活动）端点。
// 与 Caps()/Jobs() 的 DisableGrowthTravel 同一条判据，按路径前缀识别，
// 覆盖 Capability 为 0 的玩法端点（autotask/school/blackcat）。
func isPlayFeaturePath(path string) bool {
	for _, p := range []string{
		"/admin/checkin",
		"/admin/growth",
		"/admin/travel",
		"/admin/school",
		"/admin/blackcat",
	} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// pathPrefix 返回本实例的管理端点路径前缀；默认实例无前缀。
func (h *AdminHandler) pathPrefix() string {
	if h == nil || h.p == nil || h.p.ID() == providerID {
		return ""
	}
	return "/" + h.p.ID()
}

// allRoutes 在基础路由之上追加任务自动化端点（见 autotask_admin.go）。
//
// # 为什么单独一个方法而不是直接写进 Routes 的字面量
//
// 任务自动化是**成块移植**进来的（对应 B 的 panel/autotask.go + taskcenter.go），
// 单独一个切片让"这次移植加了哪些端点"一眼可见，也便于将来整块回退。
// 核心侧完全无感：AdminRoutes() 仍然只返回一个 []gateway.AdminRoute。
func (h *AdminHandler) allRoutes() []gateway.AdminRoute {
	return append(h.Routes(), h.autoTaskRoutes()...)
}

// 编译期断言：Provider 实现了这些扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"路由神秘地没挂上"。
var (
	_ gateway.Provider = (*Provider)(nil)
	_ gateway.AdminExt = (*Provider)(nil)
)
