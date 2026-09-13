// config.go workbuddy 上游自己的业务配置。
//
// # 为什么配置跟着上游走
//
// 改造前这些字段长在 scheduler.Config 上，于是核心调度器的配置结构里
// 出现了「猫猫旅行自动领奖」「成长中心六个开关」这类 CodeArts 永远不存在的项。
// 加第二个上游就得往同一结构里继续塞它专属的字段。
//
// 现在它们属于 workbuddy：核心只认识 gateway.Job，不关心某个上游有几个开关。
package workbuddy

import (
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/upstream"
)

// Config workbuddy 上游的业务依赖与开关。
//
// 命名沿用改造前的 scheduler.Config：任务开关用「禁用」而非「启用」，
// 于是零值 Config 即「成长/旅行守卫都开启」，与引入开关前的行为逐字一致。
type Config struct {
	// Pool 账号池的消费方视图（见 accounts.go）。
	//
	// 刻意不是 *pool.Pool：上游包不得依赖 pool（架构约束），
	// 且本包对池的全部需求就是 AccountPool 里那几个方法。
	// nil 时所有需要账号的操作都不做任何事（构造期可选）。
	Pool AccountPool

	// Provider 本上游在账号池里的归属标识（= ID()，装配层填 workbuddy.ProviderID）。
	//
	// # 为什么要有它，而不是直接用包内的 providerID 常量
	//
	// 因为"本包注册成哪个 ID"是**装配层的事实**：账号池按这个标签分域，
	// 装配层也可能用别的标识注册同一个实现（多实例场景）。
	// 本包读自己的常量就等于假定两者永远一致 —— 一旦不一致，
	// 按上游取号会取到空集，界面整片空白，且不会有任何报错。
	//
	// # 零值 = "" 时的行为
	//
	// 空串交给池子（ListFor("")）按"未打标签 = 默认上游"解释，
	// 与改造前"只有一个上游"的行为逐字一致 —— 既有测试与
	// NewWithConfig(Config{...}) 的手工构造都不需要改。
	Provider string
	// Client 上游 HTTP 客户端。nil 时退回 upstream.New()。
	Client *upstream.Client
	// Log 任务结果历史（可选；nil = 不记录）。管理台的「今日签到了吗」也读它。
	Log *checkinlog.Log

	// Login 本上游的登录流程（可选；nil = 不支持页内添加账号）。
	//
	// # 为什么是接口而不是具体的 *oauth.Client
	//
	// 与 Pool 同一个理由：本包不该依赖"登录是怎么实现的"。
	// 接口只声明本包真正要用的两个动作（申请授权链接、轮询取凭证），
	// 装配层把具体的 oauth 客户端适配进来。
	//
	// 这样做的直接收益：本包不需要 import internal/oauth ——
	// 而 oauth.Client 的授权站点是构造时绑定的（指向本上游的站点），
	// 那种"我知道自己是哪个上游"的知识不该散进上游包的逻辑里。
	//
	// nil 时 LoginFlow() 返回 false，管理台据此在 manifest 里
	// 把 login 置空 → 前端不渲染「＋ 添加账号」按钮。**那是正确降级**：
	// 部署方没配登录流程，就不该给一个点了会失败的按钮。
	Login OAuthFlow

	// ---- 以下供管理端点使用（Task 3c）----

	// Core 核心服务（历史落库）。nil 时退回本包自己的 record。
	//
	// 签到/保活的历史由核心统一落库（跨上游同一格式），本包把
	// "这一条是什么"交给它，Nickname 由核心补（它在账号池里）。
	Core coreService
	// Admin 管理端点宿主的依赖（调度器视图 + 共享任务槽）。
	//
	// 与 Core 分开：Core 是"业务动作的结果往哪写"，Admin 是"展示与任务槽"，
	// 两者的接线时机不同（后者要等调度器建好）。
	Admin AdminEnv
	// ClientLogin 本机客户端登录态管理（可选；nil = 关闭「本地登录」面板，
	// 相关端点降级为 503，与改造前一致）。
	ClientLogin ClientLoginManager

	// TravelAutoClaimDisabled 显式关闭「猫到站自动领奖」。零值 = 自动领奖开启。
	TravelAutoClaimDisabled bool
	// TravelWatchInterval 守卫轮间隔（同时是上游未给 arrive_at 时的兜底轮询周期）。
	// <=0 回落 1 分钟。
	TravelWatchInterval time.Duration

	// GrowthWatchInterval 成长中心扫描间隔。<=0 回落 10 分钟。
	GrowthWatchInterval time.Duration
	// 六个自动动作的初始开关。用 *bool 区分「未设置」与「显式 false」，
	// 默认值在 New 里给出：领奖 true、补签 true、接单 true，其余三个 false。
	// 这里刻意不用 Disabled/Enabled 命名：各开关默认值不一致，
	// 「零值即某一边」的约定必然让其中几个名字读起来是反的。
	//
	// GrowthAutoClaim 默认开：completed 只代表任务条件达成，不调 claim 奖励永远不到账
	// （实测对 completed 的 chat_5 调 claim 后积分 +100 且状态变 claimed）。
	GrowthAutoClaim  *bool
	GrowthAutoAccept *bool
	GrowthAutoMakeup *bool
	GrowthAutoRedeem *bool
	GrowthAutoOpen   *bool
	GrowthAutoDraw   *bool
}

// boolOrPtr 取 *bool 的值，nil（未设置）时返回默认值。
func boolOrPtr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
