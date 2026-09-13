// dailyaction_ext.go 上游自报"我有哪些每日动作"。
//
// # 为什么它是**独立文件**而不是 extension.go 里的第 6 个接口
//
// 本仓的扩展点已经有一组（AdminExt / JobExt / LoginFlow / AuthDirExt /
// CredentialLoader），它们住在 extension.go。但这个文件是**新加的**，
// 而同一时间另一个任务（T2 额度扩展点）也在改 extension.go ——
// 两个改动同时落在一个文件上，后写的会静默覆盖先写的，双方都以为自己的改动在。
//
// 一个扩展点一个文件本来也更清晰：扩展点之间**没有共享代码**，
// 它们的唯一共同点只是"都用 gateway.ExtOf[T] 被发现"。
//
// # 为什么需要这个扩展点（用户原话）
//
//	"签到也就是福利领取，就是签到。能不能统一一下呢，就是领取一下福利呗"
//	"那个按钮前端的签到按钮能不能和这个统一一下呢"
//
// 改动前的事实：前端**写死**了「签到」——
//
//	webui.html:530   <button id="btnAllCheckin">全部签到</button>
//	webui.html:1934  `<button data-act="checkin" data-uid="${U}">签到</button>`
//
// 于是**每个**上游的每个账号行都长着「签到」「积分」「保活」三个按钮，
// 与"这个上游到底有没有这个动作"完全无关。实测后果：
//
//	provider=codearts  uid=01a08fe00b8b   ← 它有 welfare，**没有** checkin
//	    前端照样渲染「签到」「保活」
//	    点下去 POST /admin/checkin → 那条路由是 workbuddy 挂的，
//	    codearts 声明里根本没有 checkin 能力位 → 404 或作用在别家账号上
//
// 两个上游的动作**名字与语义都不同**：
//
//	workbuddy  签到 / 保活 / 猫到站领奖（三个，见 workbuddy.AdminRoutes）
//	codearts   福利领取（一个，每天可领 —— 用户确认了它就是签到）
//
// 所以正确做法不是"把签到移植到 codearts"（它没有签到，原项目也没有），
// 而是**把动作本身变成上游自报的事实**，前端只负责渲染上报来的东西：
// 有就显示，没有就不显示 —— 正是用户要的"有什么显示什么"。
//
// # 与 AdminRoute.Capability 的关系（为什么不直接复用能力位）
//
// AdminRoute 已经有 Capability，前端也拿到 manifest 里的 admin_routes 了。
// 但**能力位表达不了"动作"**，两条独立理由：
//
//  1. 一个能力位对应**多条**路由（checkin 位下有 /admin/checkin 与
//     /admin/keepalive —— 一个是签到一个是保活，前端只有能力位就分不出
//     该渲染成几个按钮、每个叫什么）。能力位是**分类**，动作是**入口**。
//  2. 能力位是**稀疏的分类**（growth 只有 workbuddy、welfare 只有 codearts），
//     而"每日动作"这个**位置**是每个上游都有的槽位。用能力位当槽位，
//     前端就得写一张"哪个能力位算每日动作"的表 —— 那正是要避免的硬编码。
//
// 所以这里是"上游自报一个动作清单"，每条动作自己带：
// 稳定 ID、人类可读文案、单账号端点、可选的全量端点。
package gateway

import "strings"

// DailyActionExt 上游自报"我有哪些每日可执行的动作"。
//
// 实现方只报**自己真的有**的动作。返回 nil / 空切片表示"一个都没有"——
// 这是合法状态（一个纯 API Key 上游就是如此），前端据此渲染 0 个按钮，
// **不放假按钮**（点了会 404 的那种）。
//
// # 判据：报了的动作必须真的能执行
//
// 与 Caps() 的契约同一条：**声明了就必须实现**。
// 所以 OneURL 指向的端点必须真的挂在这条上游的 AdminRoutes 里
// （有测试守住这一点，见 dailyaction_contract_test.go）。
// 报一个不存在的端点，前端就会渲染出一个点下去报错的按钮 ——
// 与 LoginFlow.Configured 要避免的是同一类缺陷。
type DailyActionExt interface {
	// DailyActions 返回该上游自报的每日动作（顺序即前端按钮顺序）。
	//
	// 返回的切片会被核心直接下发给前端，**不得**包含上游的私有状态
	//（凭证、session、token…）—— 只有下面 DailyAction 里的那六个字段。
	DailyActions() []DailyAction
}

// DailyAction 一个每日动作在 UI 契约里的样子。
//
// # 全部字段都是**稳定契约**
//
// ID 会成为前端 button 的 data-act 值，OneURL 会成为它 POST 的路径。
// 两者都是"前端按数据渲染"的输入，不是实现细节 —— 改它们等于改协议。
type DailyAction struct {
	// ID 动作的稳定标识（如 "checkin" / "keepalive" / "welfare"）。
	//
	// # 它必须**跨上游唯一**，而不只是"在本上游内唯一"
	//
	// 因为前端要按它写 data-act，而同一张账号表里同时有多个上游的行；
	// 且前端还要用它去查"这个动作对应的全量端点叫什么"。
	// 用复用 ID 会让两个语义不同的动作在前端无法区分。
	//
	// 约束 ^[a-z][a-z0-9-]*$（与 Provider.ID 同一套字符集）：
	// 它会出现在 HTML 属性值与 URL 里，不能含引号/空格/斜杠。
	ID string `json:"id"`

	// Label 按钮上的中文文案（如 "签到" / "保活" / "领取福利"）。
	//
	// 由**上游**给而不是前端按 ID 翻译：同一个 ID 在不同上游可以叫不同名字，
	// 而前端硬编码一张 ID→中文的表就等于把上游的文案搬进前端
	//（加第三个上游时又要改前端）。这正是本项目一直在避免的。
	Label string `json:"label"`

	// Title 鼠标悬停提示（如 "单账号签到"）。空则前端用 Label。
	Title string `json:"title,omitempty"`

	// OneURL 单账号端点：对该动作作用在**一个**账号上时 POST 的路径。
	//
	// 请求体形状是核心的通用约定：`{"uid": "<账号 uid>"}`。
	// 这与改造前前端写死的三个端点**逐字一致**（/admin/checkin、
	// /admin/keepalive、/admin/credits/refresh 都收这个体），
	// 所以抽象之后 workbuddy 的既有调用一个字节都不变。
	//
	// 空串表示"该动作没有单账号入口"——此时前端不渲染行内按钮，
	// 只可能在顶部出现全量入口（见 AllURL）。
	OneURL string `json:"one_url,omitempty"`

	// AllURL 全量端点：对**本上游全部账号**执行该动作的路径。
	//
	// 空串表示"该动作没有全量端点"。
	//
	// ⚠ 它的作用域是**本上游**，不是整个账号池。
	// 改造前的前端把「全部签到」写成对整池调用 POST /admin/checkin，
	// 而那条路由是 workbuddy 挂的 —— 在有两个上游的部署里，
	// 那个按钮的语义是模糊的。现在每个上游各自报自己的全量端点，
	// 前端按上游生成「全部签到（workbuddy）」这样的入口。
	AllURL string `json:"all_url,omitempty"`

	// Batch 顶部**是否出现**该动作的全量入口。
	//
	// # ⚠ 它与 AllURL 是两件事，不要把判据合成一个
	//
	// 我第一版让 Batch 表达"有没有全量端点"，于是按 AllURL != "" 推断。
	// **实测立刻出错**：workbuddy 的保活有全量端点（/admin/keepalive 不带
	// uid 就是全量），于是界面上冒出一个「全部保活」按钮 ——
	// 而那个按钮**是用户明确要求删掉的**（理由：它与自动保活槽调同一个
	// 函数，token 提前几小时刷没有收益；webui.html 里有完整注释）。
	//
	// 所以这里必须区分**两个不同的判断**：
	//
	//	AllURL != ""   "上游有没有这个端点"     —— 上游的**事实**
	//	Batch          "界面要不要给它一个入口" —— 产品**决策**
	//
	// 把决策伪装成事实推断，就会把被删掉的功能**偷偷加回来**，
	// 而且没有任何测试会红（除非专门钉一条 —— 我们钉了：
	// internal/gateway 的 TestWorkbuddyKeepaliveHasNoBulkButton）。
	//
	// 反过来说：Batch=true 而 AllURL 为空是**无效组合**（按钮点了没端点），
	// 前端渲染时必须两个都判（见 webui.html 的 renderAllDailyButtons）。
	Batch bool `json:"batch,omitempty"`
}

// ValidDailyActionID 校验动作 ID 的字符集。
//
// 与 validProviderID 同一套规则、同一个理由：ID 会出现在 HTML 属性值
// 与查询参数里，含引号/空白/斜杠会破坏渲染或让选择器错位。
//
// 非法 ID 的动作会被核心**跳过并记日志**，而不是渲染出去 ——
// 渲染一个畸形 ID 的按钮会让 `button[data-act="..."]` 选择器静默失效，
// 表现是"按钮在但点了没反应"，极难定位。
func ValidDailyActionID(id string) bool {
	if id == "" {
		return false
	}
	if id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for i := 1; i < len(id); i++ {
		ch := id[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return false
	}
	return true
}

// SanitizeDailyActions 过滤掉畸形动作并去重。
//
// # 为什么过滤发生在**核心**而不是信任上游
//
// 核心是"下发前的最后一道"，前端的渲染管线对畸形输入没有任何防御
// （它拿到什么就渲染什么）。在这之前过一次，坏数据就变成"少一个按钮"
// 而不是"页面某处静默坏掉"。
//
// 三种被丢掉的情况：
//
//  1. ID 非法（含大写/引号/空格）—— 会让选择器与属性值失效
//  2. Label 为空 —— 会渲染出一个没有文字的按钮，用户不知道那是什么
//  3. 两个动作用了**同一个 ID** —— 前端按 ID 查端点时会拿到错的那个
//
// 保留顺序（上游报的顺序就是按钮顺序），因为顺序是上游的表达。
func SanitizeDailyActions(in []DailyAction) []DailyAction {
	if len(in) == 0 {
		return nil
	}
	out := make([]DailyAction, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, a := range in {
		a.ID = strings.TrimSpace(a.ID)
		a.Label = strings.TrimSpace(a.Label)
		if !ValidDailyActionID(a.ID) || a.Label == "" || seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		out = append(out, a)
	}
	if len(out) == 0 {
		// 返回 nil 而不是空切片：下发给前端时 nil 编成 null，
		// 前端的 `Array.isArray` 归一同样处理成空数组 —— 两种写法等价，
		// 但 nil 让"一个都没有"在 Go 侧就是可判断的。
		return nil
	}
	return out
}
