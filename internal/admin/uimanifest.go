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
	// DailyActions 上游自报的"每日动作"（签到 / 保活 / 领取福利…）。
	//
	// # 为什么它必须**单独**下发，而不是让前端从 AdminRoutes 里推
	//
	// AdminRoutes 是"这个上游挂了哪些端点"，DailyActions 是
	// "这个上游有哪几个**用户可点的每日动作、各自叫什么名字**"。
	// 两者不是一回事，三条理由：
	//
	//  1. **端点 ≠ 动作**：checkin 能力位下有 /admin/checkin 与 /admin/keepalive
	//     两条路由，名字分别是「立即签到」「立即保活」。光看端点是能凑出来，
	//     但要前端**猜**"哪些端点属于同一个每日动作槽位"——那正是硬编码。
	//  2. **一条路由两种语义**：/admin/checkin 带 uid 是单账号、不带是全量。
	//     AdminRoutes 里它只出现一次，表达不了"这个动作既有行内按钮
	//     又有全量按钮"。
	//  3. **顺序与文案是上游的表达**：上游报的顺序就是按钮顺序，
	//     Label 就是按钮文字。让前端按能力位反推，等于把上游的文案
	//     搬进前端 —— 加第三个上游时又要改前端。
	//
	// # 前端据此渲染的规则（用户要的"有什么显示什么"）
	//
	//	该上游报了某动作 → 它的每个账号行里有这个按钮
	//	没报             → **不渲染**（不放假按钮）
	//
	// 实测收益：codearts 不再出现「签到」「保活」（它没有这两个动作，
	// 点了会 404 或作用在别家账号上），而是出现它真有的「领取福利」。
	DailyActions []uiDailyAction `json:"daily_actions"`
}

// uiDailyAction 一个上游自报的每日动作在 UI 契约里的样子。
//
// 字段与 gateway.DailyAction **一一对应**，刻意不做改名：
// 这是同一条契约的两个投影，改名只会让"两边是不是同一个东西"
// 变成需要对照才能回答的问题。
type uiDailyAction struct {
	// Provider 该动作属于哪个上游（前端按行归属取它）。
	Provider string `json:"provider"`
	// ID 动作的稳定标识，前端写成 button 的 data-act。
	ID string `json:"id"`
	// Label 按钮文案（「签到」/「保活」/「领取福利」）。
	Label string `json:"label"`
	// Title 悬停提示。空则前端用 Label。
	Title string `json:"title,omitempty"`
	// OneURL 单账号端点（体为 {"uid": ...}）。空 = 没有行内入口。
	OneURL string `json:"one_url,omitempty"`
	// AllURL 全量端点。空 = 没有全量入口。
	AllURL string `json:"all_url,omitempty"`
	// Batch 是否支持批量（决定顶部是否出现该动作的全量按钮）。
	Batch bool `json:"batch,omitempty"`
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
	// Hidden 该端点存在但**不是面板入口**（上游自己声明，见 gateway.AdminRoute.Hidden）。
	//
	// # 为什么前端需要知道它
	//
	// 前端按"某能力位有没有 GET 入口"决定要不要生成一个通用面板。
	// 没有这个标志位时，唯一的排除办法是前端写死上游名 —— 那是硬编码。
	// 有它之后，前端只读数据：hidden 的 GET 路由**不算面板入口**，
	// 但路由本身与能力位都不受影响（账号行的每日动作仍靠能力位）。
	//
	// omitempty：false 是绝大多数路由的常态，不发出去让 manifest 保持精简；
	// 前端用 `!!r.hidden` 读，缺字段与 false 等价。
	Hidden bool `json:"hidden,omitempty"`
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
	// tasks / invite 是本轮为 loomy 的专属标签页加的（见 internal/loomy/adminroute.go）。
	// 标题刻意**不带上游名** —— 能力位是通用契约，任何上游声明它都会用这套文案，
	// 导航里显示成「新手任务 · Loomy」由渲染层拼上游名，不是写在这里。
	"tasks":  "新手任务",
	"invite": "邀请码",
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
		DailyActions: []uiDailyAction{},
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
		// ⚠ 登录能力**必须在这里也填一次**。
		//
		// `providerInfo` 有两个构造点：本文件（/admin/ui/manifest）与
		// schedule.go（/admin/providers）。我第一版只改了后者，
		// 于是 `/admin/providers` 有 login 而**前端真正读的 manifest 没有** ——
		// 界面上「＋ 添加账号」永远不出现，而接口调试看起来一切正常。
		//
		// 判据与那里**逐字相同**：ExtOf 认出 + 上游自报已配置。
		// 两条都要，缺一条会渲染出点了报错的假按钮。
		if lf, ok := gateway.ExtOf[gateway.LoginFlow](p); ok && lf.Configured() {
			info.Login = &providerLogin{Kind: "device", Label: "添加账号"}
		}
		// ⚠ 账号池列集**也必须在这里填一次** —— 与上面 login 完全同一个坑：
		// 前端真正读的是 manifest，而 providerInfo 有两个构造点
		//（本文件 + schedule.go 的 /admin/providers）。只改一处的结果是
		// "/admin/providers 有、前端读到的 manifest 没有" → 列集永远用不上，
		// 而接口调试看起来一切正常。
		info.AccountColumns = accountColumnsOf(p)
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
				Hidden:     rt.Hidden,
			})
		}
	}

	// ---- 每日动作（独立于 AdminExt：一个上游可以只报动作、不报端点）----
	//
	// # 为什么这个循环**不在**上面的 `if !ok { continue }` 里面
	//
	// 那个 continue 是为 AdminExt 写的。把每日动作塞进去会让
	// "实现了 DailyActionExt 但没实现 AdminExt"的上游整体被跳过 ——
	// 那种上游是合法的（比如一个只有全量动作、没有管理面板的上游）。
	//
	// # 为什么要过 SanitizeDailyActions
	//
	// 前端的渲染管线对畸形输入没有防御（拿到什么渲染什么）。
	// 在这里过一次，坏数据就变成"少一个按钮"而不是"页面某处静默坏掉"：
	// ID 非法会让 `button[data-act="..."]` 选择器失效（按钮在但点了没反应），
	// Label 为空会渲染出一个没有文字的按钮。
	for _, p := range h.cfg.Registry.All() {
		da, ok := gateway.ExtOf[gateway.DailyActionExt](p)
		if !ok {
			continue
		}
		for _, a := range gateway.SanitizeDailyActions(da.DailyActions()) {
			m.DailyActions = append(m.DailyActions, uiDailyAction{
				Provider: p.ID(),
				ID:       a.ID,
				Label:    a.Label,
				Title:    a.Title,
				OneURL:   a.OneURL,
				AllURL:   a.AllURL,
				Batch:    a.Batch,
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

	// 每日动作只按**上游**排序，上游**内部保持它自报的顺序**。
	//
	// # 为什么这里不能像 admin_routes 那样按字段排
	//
	// 上游报的顺序就是**按钮顺序**，那是上游的表达（"签到"在"保活"前面
	// 是产品决策，不是巧合）。按 ID 字典序排会把 workbuddy 的
	// 签到/保活排成 checkin/keepalive —— 这次恰好一样，但那是巧合；
	// 一旦上游报了 "daily-gift"，字典序就会把它排到最前面。
	//
	// 按 provider 排序的理由与 admin_routes 相同：Registry.All() 的顺序
	// 由 map 遍历决定，不排的话每次刷新按钮顺序都可能跳。
	// SliceStable 保证同一上游内的相对顺序不被破坏 —— 这正是这里要的。
	sort.SliceStable(m.DailyActions, func(i, j int) bool {
		return m.DailyActions[i].Provider < m.DailyActions[j].Provider
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
