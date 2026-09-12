// /admin/ui/manifest —— 控制台 UI 的**唯一**渲染契约。
//
// # 为什么需要这条端点
//
// 后端早已完成多上游解耦：Provider 契约、能力位、AdminExt 扩展点齐备，
// 25 条上游路由也**已经**带着 Title（中文名）与 Capability 声明。
// 但前端拿不到这些事实 —— `/admin/providers` 只下发了能力名数组，
// 没有下发**路由清单**，于是前端只能把面板写死。
//
// 后果是三条实测出来的：
//
//  1. 加第 3 个上游时前端要手改 —— 判据 1（加新上游核心零改动）在**前端**上被打破
//  2. codearts 的福利/套餐/额度 4 条端点后端已挂载，前端**一个入口都没有**
//  3. 没有 workbuddy 的部署里，页面照样渲染「猫猫旅行」「成长计划」并持续报错
//
// 本端点把「有哪些上游、各自有什么能力、各自声明了哪些管理端点、有哪些定时任务」
// 一次性下发给前端。前端只认识**槽位与契约形状**，不认识任何上游名字。
//
// # 与前端的职责边界（重要）
//
// manifest 只用于**生成入口与标题**（导航分组、面板标题、面板归属）。
// manifest 里虽然带着 path，但前端**不得**据此自动发起写操作 ——
// 写操作一律走各面板自己显式的确认流程与 busyRun 反馈，
// 否则一条被误注册的写端点会被渲染成一个点一下就生效的按钮。
package admin

import (
	"net/http"
	"sort"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/scheduler"
)

// uiManifest 控制台渲染契约。
//
// 字段全部用**字符串能力名**而不是位掩码：位掩码的数值是内部表示，
// 前端不该依赖它；名字是稳定契约（沿用 /admin/providers 已有的决策）。
type uiManifest struct {
	// Service 网关身份标识。前端不再硬编码服务名（原先硬编码了 3 处）。
	Service string `json:"service"`
	// Providers 已注册上游（含能力位、是否默认、账号数）。
	Providers []providerInfo `json:"providers"`
	// Capabilities 全部已定义能力位的中文标题字典。
	//
	// 前端用它把能力位翻译成可读词；缺了这个字典，UI 只能显示英文位名。
	Capabilities []capabilityInfo `json:"capabilities"`
	// AdminRoutes 上游声明的管理端点（含归属上游、能力位、中文标题）。
	//
	// 这是让前端解耦的**关键字段** —— 在此之前它从未被下发过。
	AdminRoutes []uiAdminRoute `json:"admin_routes"`
	// Jobs 已注册的定时任务运行状态。
	Jobs []uiJob `json:"jobs"`
}

// capabilityInfo 一个能力位的对外描述。
type capabilityInfo struct {
	// ID 能力名（gateway.Capability.Names 的输出，如 "growth"）。
	ID string `json:"id"`
	// Title 中文标题（如「成长计划」）。
	Title string `json:"title"`
}

// uiAdminRoute 一条上游管理端点在 UI 契约里的样子。
type uiAdminRoute struct {
	// Provider 该端点属于哪个上游。
	Provider string `json:"provider"`
	// Method HTTP 方法。
	Method string `json:"method"`
	// Path 完整路径（如 "/admin/growth"）。
	Path string `json:"path"`
	// Capability 该端点对应的能力位名（核心通用端点为 "core"）。
	Capability string `json:"capability"`
	// Title 面板上显示的中文名。
	Title string `json:"title"`
}

// uiJob 一个定时任务在 UI 契约里的样子。
type uiJob struct {
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	IntervalMs int64  `json:"interval_ms"`
	LastRun    string `json:"last_run,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// capTitles 能力位的中文标题。
//
// # 为什么这张表住在 admin 而不是 gateway
//
// 标题是**展示层**的词，不是协议的一部分。gateway 只该知道能力叫什么
// （capNames 的英文 id 是路由与配置里的标识符），"成长计划"这种说法
// 属于控制台。放错层会让核心包背上一份随时要改的中文文案。
//
// # 加新能力位时
//
// 必须同时往这里加一行，否则 manifest 里该能力的 Title 会退回 id。
// 有测试守住这一点（见 uimanifest_test.go）。
//
// CoreCapability（"core"）不是真实能力位，而是 routeCapName 给
// "声明了端点但没声明能力位"的路由的保留名。也给它一个标题，
// 前端就能把它当普通条目渲染，不必特判空串。
var capTitles = map[string]string{
	CoreCapability: "通用端点",
	"chat":         "对话",
	"models":       "模型目录",
	"checkin":      "签到与保活",
	"growth":       "成长计划",
	"travel":       "猫猫旅行",
	"welfare":      "福利中心",
	"quota-probe":  "额度探测",
}

// CoreCapability 保留名：声明了管理端点但**没有**对应能力位的路由归到它。
//
// # 为什么用保留名而不是空串（评审 F2 相关）
//
// 空串在"该键存在但值为空"与"该键缺失"之间无法区分，而前端要按它做路由。
// 保留名的另一个好处是它**不可能是真实能力位**（gateway.capNames 里没有），
// 所以前端可以安全地拿它当"归通用区"的判据。
const CoreCapability = "core"

// capTitle 取能力位的中文标题；未登记时**退回 id 本身**而不是空串 ——
// 空串会让前端渲染出一个没有名字的导航项，比显示英文更难排查。
func capTitle(id string) string {
	if t, ok := capTitles[id]; ok {
		return t
	}
	return id
}

// uiManifest 组装并下发控制台渲染契约。
//
// 空注册表返回空数组而不是报错：前端要能在"未配置任何上游"的部署下
// 正常渲染空态，而不是拿到 500 后整页崩。
func (h *Handler) uiManifest(w http.ResponseWriter, r *http.Request) {
	m := uiManifest{
		Service:      h.cfg.ServiceName,
		Providers:    []providerInfo{},
		Capabilities: []capabilityInfo{},
		AdminRoutes:  []uiAdminRoute{},
		Jobs:         []uiJob{},
	}

	// ---- 能力位字典（全量下发，不只下发被用到的）----
	//
	// 下发全量的理由：前端要能显示"这个上游支持哪些能力"，
	// 而不仅是"当前哪些能力有面板"。只发被用到的话，
	// 一个还没有专属面板的能力位在前端就是不可见的。
	for _, c := range gateway.AllCapabilities() {
		m.Capabilities = append(m.Capabilities, capabilityInfo{ID: gateway.String(c), Title: capTitle(gateway.String(c))})
	}

	if h.cfg.Registry == nil {
		writeJSON(w, http.StatusOK, m)
		return
	}

	// ---- 上游 + 它们的管理端点 ----
	for _, p := range h.cfg.Registry.All() {
		info := providerInfo{
			ID:           p.ID(),
			Capabilities: p.Caps().Names(),
			Default:      p.ID() == h.cfg.DefaultProvider,
		}
		if h.cfg.Pool != nil {
			info.AccountCount = len(h.cfg.Pool.ListFor(p.ID()))
		}
		m.Providers = append(m.Providers, info)

		ext, ok := gateway.ExtOf[gateway.AdminExt](p)
		if !ok {
			continue
		}
		for _, rt := range ext.AdminRoutes() {
			m.AdminRoutes = append(m.AdminRoutes, uiAdminRoute{
				Provider:   p.ID(),
				Method:     rt.Method,
				Path:       rt.Path,
				Capability: routeCapName(rt.Capability),
				Title:      rt.Title,
			})
		}
	}

	// 排序：先按上游（默认上游在最前，其余字典序），再按路径。
	//
	// # 为什么必须显式排序
	//
	// Registry.All() 的顺序由注册顺序决定，而 map 遍历顺序随机 ——
	// 不排的话前端每次刷新导航项的顺序都可能跳，
	// 那种"点了刷新位置就变了"的体验比排序本身更让人困惑。
	sort.SliceStable(m.AdminRoutes, func(i, j int) bool {
		a, b := m.AdminRoutes[i], m.AdminRoutes[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Method < b.Method
	})

	// ---- 定时任务状态 ----
	for _, s := range h.jobStatuses() {
		m.Jobs = append(m.Jobs, uiJob{
			Provider:   s.Provider,
			Name:       s.Name,
			IntervalMs: s.IntervalMs,
			LastRun:    s.LastRun,
			LastError:  s.LastError,
		})
	}

	writeJSON(w, http.StatusOK, m)
}

// routeCapName 把能力位翻译成名字；未命名位返回 CoreCapability。
func routeCapName(c gateway.Capability) string {
	if c == 0 {
		return CoreCapability
	}
	return gateway.String(c)
}

// jobStatuses 取已注册任务的运行状态。
//
// # 为什么走 Scheduler 的接口而不是直接持有 *scheduler.Jobs
//
// cmd/server 已经有一个 adminSchedulerAdapter 包着 *scheduler.Scheduler，
// 再注入一个 Jobs 指针就是**第二份**接线。这里给 SchedulerView 加一个
// 可选方法，让已有适配器顺手把状态带出来即可。
//
// nil 安全：未接线（测试路径）时返回空切片，manifest 里的 jobs 就是空数组，
// 前端渲染空态 —— 而不是 panic。
func (h *Handler) jobStatuses() []scheduler.Status {
	if h.cfg.Scheduler == nil {
		return nil
	}
	jv, ok := h.cfg.Scheduler.(JobStatusView)
	if !ok {
		return nil
	}
	return jv.JobStatuses()
}
