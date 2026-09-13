// 扩展点：把"平台特殊功能"从核心摘出去的唯一通道。
//
// # 为什么需要它们
//
// 实测两边的功能差异：
//
//	管理端点  通用 23 个 / **平台特殊 26 个**
//	调度任务  通用  0 个 / **平台特殊 17 个**
//	上游路径  零重叠
//
// 平台特殊的是**大多数**。如果把它们塞进 Provider 接口，
// Provider 会变成"什么都装"的浅接口，且第 3 个上游又要改它。
//
// # 机制：可选实现 + 类型断言
//
// 上游**只实现自己有的扩展**。核心用 gateway.ExtOf[AdminExt](p) 发现：
//
//	if ax, ok := gateway.ExtOf[gateway.AdminExt](p); ok {
//	    for _, r := range ax.AdminRoutes() { mux.Handle(r.Path, r.Handler) }
//	}
//
// 一个"纯 API Key、无账号生命周期"的上游可以只实现 Provider 就接入。
//
// # 加第四个扩展点时
//
// 只在本文件加接口，不改 Provider、不改核心 —— 这是"扩展性"的具体体现。
package gateway

import (
	"context"
	"net/http"
	"time"
)

// AdminExt 上游自己的管理端点。
//
// 实测 26 个平台特殊端点走这里（workbuddy 的成长/旅行/本机登录 22 个，
// codearts 的福利/订阅/配额 4 个）。
type AdminExt interface {
	// AdminRoutes 返回该上游的管理端点。没有就返回 nil / 空切片。
	//
	// 核心只做一件事：遍历所有 Provider 的 AdminRoutes 并挂载。
	// **加新上游时 core 的 admin 包零改动。**
	AdminRoutes() []AdminRoute
}

// AdminRoute 一条管理端点。
type AdminRoute struct {
	Method  string // GET / POST / PUT / DELETE
	Path    string // 完整路径，如 "/admin/growth"
	Handler http.HandlerFunc
	// Capability 该端点对应的能力位（0 表示通用端点）。
	// 前端据此决定是否显示入口 —— 能力由**后端下发**，前端不硬编码。
	Capability Capability
	// Title 面板上显示的中文名（如「成长计划」）。空则前端按 Path 猜。
	Title string
}

// JobExt 上游自己的定时任务。
//
// 实测 17 个平台特殊任务走这里（workbuddy 的签到/保活/成长/旅行，
// codearts 的福利/配额刷新）。
type JobExt interface {
	// Jobs 返回该上游要注册的定时任务。
	//
	// 核心的 scheduler 只剩框架：错峰执行已注册的 Job，**不认识任何具体任务名**。
	Jobs() []Job
}

// Job 一个定时任务。
type Job struct {
	// Name 任务名，用于日志与状态展示（如 "growth-watch"）。
	Name string
	// Interval 执行间隔。
	Interval time.Duration
	// Run 执行体。返回 error 只用于记录，不影响后续轮次。
	Run func(ctx context.Context) error
	// Due 可选的"是否到期"判断。nil 表示按 Interval 固定间隔。
	//
	// 为什么需要它：workbuddy 的守卫轮是按**每个账号的到期时刻**错峰的，
	// 不是全局固定间隔。有 Due 时核心就不再做间隔判断，交给上游自己决定。
	Due func(now time.Time) bool
}

// LoginFlow 上游自己的登录交互。
//
// 为什么登录不做成通用实现：
//
//	workbuddy → OAuth device flow（申请链接 → 用户在浏览器授权 → 轮询取 token）
//	codearts  → OAuth + DPoP（还要生成密钥对、签名请求、约 30 分钟凭证）
//
// 交互步骤数都不同，无法用一个通用实现覆盖。
// 但 **"/admin/login/start" 与 "/admin/login/poll" 这两个端点留在核心** ——
// 它们属于通用的 device flow 协议形状，核心只负责转发到上游的 LoginFlow。
type LoginFlow interface {
	// Start 申请一次登录，返回 (state, 授权URL)。
	Start() (state, authURL string, err error)
	// Poll 查询授权结果。未完成时返回 ErrLoginPending。
	Poll(state string) (Credential, error)
	// Configured 报告这份部署**真的能**走登录流程吗。
	//
	// # 为什么接口里要有这一条（而不是只看"实现了没有"）
	//
	// 上游为了让类型断言认出自己，必须把 Start/Poll 挂在 Provider 身上 ——
	// 那是**编译期**的事实，与"这次部署有没有配 OAuth 客户端"无关。
	//
	// 只看 `ExtOf[LoginFlow](p)` 会导致：**没配登录流程的部署也"实现了"**，
	// 于是 manifest 下发 login、前端渲染「＋ 添加账号」，
	// 用户点下去才报错。那是"假按钮"，正是这个字段要避免的。
	//
	// 加这一条之后，探测方可以 `ok && lf.Configured()` ——
	// 两道条件都满足才认为可用。实现方照实回答即可（通常是
	// "cfg 里那个客户端不是 nil"）。
	Configured() bool
}

// AuthDirExt 上游自报"我的凭证目录在哪"。
//
// # 为什么它是**独立**扩展点，而不是 LoginFlow 的一个方法
//
// 它原来是 `LoginFlow.AuthDir()`。那个位置是错的 ——
// **"凭证目录在哪"与"有没有登录流程"是两件无关的事**：
//
//	手工往 `auths/codearts/` 拷凭证再点「重载 auths」是常见路径，
//	那条路径**根本不需要登录流程**。
//
// 挂在 `LoginFlow` 上就等于说：不实现登录流程的上游，
// **连"我的凭证在哪"都答不出来**，于是按上游重载 auths 对它不成立。
//
// 拆出来之后，两件事各自独立：
//
//	只实现 AuthDirExt          → 可被按上游重载（哪怕没有任何登录交互）
//	再实现 LoginFlow           → 额外获得页内添加账号
//
// 按本仓已验证的模式（AdminExt / JobExt / LoginFlow 都是
// `gateway.ExtOf[T]` 类型断言）—— 加第五个扩展点同样不改核心。
type AuthDirExt interface {
	// AuthDir 本上游的凭证落盘目录。
	//
	// # 为什么目录必须由**上游自己**报（这是实测踩出来的）
	//
	// 我第一版让 `pollViaFlow` 用 `h.cfg.AuthDir` 落盘 —— 那是
	// **默认上游（workbuddy）的目录**。而 codearts 的凭证在
	// 另一个目录（多上游部署里各上游有自己的 auth_dir）。
	//
	// 实测后果：codearts 授权成功后，凭证被往 workbuddy 的 `./auths` 写。
	// 即使不报错，也是把号放错了地方 —— 而 `workbuddy` 那边会看到
	// 一个它不认识的 `codearts-*.json`。
	//
	// 更早一步的症状是 `rename auths.tmp auths: Access is denied.`
	//（那是文件名缺失导致的，见 codeartsAuthFile.MarshalAuthFile 的注释）。
	//
	// 让上游自报目录，核心只管"拿到目录 + 文件名就写" ——
	// 目录是**上游的事实**（它自己在哪读凭证），核心不该猜。
	//
	// 返回空串表示"上游没有独立目录，用核心的默认 AuthDir"
	//（单上游部署就是这个形态，行为与改造前一致）。
	//
	// ⚠ 这不是"落盘专用"的：**读**（按上游重载 auths）与**写**
	// 必须问同一个目录，否则会出现"写成功但池子里没有"，
	// 或者"重载扫不到刚写进去的凭证"。
	AuthDir() string
}

// ErrLoginPending 表示登录授权尚未完成（用户在浏览器里还没点确认）。
var ErrLoginPending = errLoginPending{}

type errLoginPending struct{}

func (errLoginPending) Error() string { return "登录授权尚未完成" }
