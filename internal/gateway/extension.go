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
}

// ErrLoginPending 表示登录授权尚未完成（用户在浏览器里还没点确认）。
var ErrLoginPending = errLoginPending{}

type errLoginPending struct{}

func (errLoginPending) Error() string { return "登录授权尚未完成" }
