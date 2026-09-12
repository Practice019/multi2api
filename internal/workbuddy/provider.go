// Package workbuddy 把 WorkBuddy（腾讯 CodeBuddy）上游适配为 gateway.Provider。
//
// # 这个包的角色
//
// 它是**第一个 Adapter**，也是 gateway 接口的第一个真实检验：
// 如果 4 个方法装不下 workbuddy，说明接口设计错了 —— 现在发现比搬完业务后再发现便宜。
//
// 本包（Task 3a）只做**薄适配**：把现有的 internal/upstream 包成 gateway.Provider。
// workbuddy 的业务逻辑（成长中心/猫猫旅行/签到）后续搬进来（Task 3b/3c）。
//
// # 与 internal/upstream 的关系
//
//	internal/upstream   HTTP 客户端封装（ChatStream / FetchModels / 签名 / header）
//	internal/workbuddy  上游的**身份**与**能力声明**（本包）
//
// 搬运时的原则：upstream 是与上游无关的 HTTP 细节，留在原地；
// 只有"CodeBuddy 专属的业务语义"才搬进本包。
package workbuddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// providerID 上游标识。
//
// 会被用作：
//   - 模型名前缀（"workbuddy/auto"）
//   - 配置里的 provider key
//   - 统计维度（by_provider）
//
// 格式受 gateway 约束（^[a-z][a-z0-9-]*$），有契约测试守着。
const providerID = "workbuddy"

// Provider 实现 gateway.Provider（以及 AdminExt / JobExt 两个扩展点）。
//
// # 它同时是 workbuddy 业务的宿主
//
// Task 3b 把原先长在 internal/scheduler 里的成长/旅行业务搬进了本包，
// 那些业务的可变状态（快照缓存、到期表、自动动作开关）就挂在下面这些字段上。
// 核心调度器通过 gateway.JobExt 拿到任务，**不认识它们具体是什么**。
type Provider struct {
	client *upstream.Client

	// cfg 业务配置与依赖（账号池、历史日志、守卫间隔、六个自动开关）。
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日
	// 不再重试，避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// travel 猫猫旅行守卫状态（快照缓存 + 到期表 + 自动领奖开关）。
	travel *travelWatchState

	// growth 成长中心守卫状态（快照缓存 + 到期表 + 六个自动动作开关）。
	growth *growthWatchState

	// travelLastRun/growthLastRun 各自上次守卫轮的执行时刻。
	//
	// 为什么本包要自己记：Due 里除了"有账号到期"还要叠加"不早于配置的守卫间隔"
	// （核心的轮询粒度是 30 秒，而生产配置的守卫间隔是 1 分钟/10 分钟）。
	// 没有它，守卫轮会被抬到 30 秒一轮，空转频率翻倍。
	// 与 mu 共用同一把锁。
	travelLastRun time.Time
	growthLastRun time.Time

	// probeSem 成长中心探测的**共享并发预算**：账号之间、以及单个账号内的
	// 多个上游调用，都从这里取令牌。见 GrowthProbeConcurrency 的注释。
	//
	// 同样是 nil 安全（acquire/release 都对 nil 直接返回）—— 与 travel/growth
	// 一致，这样测试里手搓 &Provider{} 不会崩，只是失去限流。
	probeSem *growthProbeSem

	// adminEnv 管理端点宿主的核心依赖（调度器视图 + 共享任务槽）。
	//
	// 为什么放在 Provider 而不是只放在 AdminHandler：AdminRoutes() 是
	// gateway.AdminExt 的接口方法，核心只拿得到 *Provider，拿不到 Handler。
	// 所以依赖必须能从 Provider 上读出来。用 SetAdminEnv 延迟注入
	// （调度器在 Provider 之后构造）。
	//
	// 与 mu 共用同一把锁。
	adminEnv AdminEnv
}

// New 建一个 workbuddy Provider（契约测试用的无依赖构造）。
//
// 只构造，不做网络请求 —— 契约测试会多次调用 factory，不能有副作用。
func New() gateway.Provider { return NewWithConfig(Config{}) }

// NewWithConfig 按配置建一个 workbuddy Provider。
//
// 与 New 的区别：New 给的是"只有对话能力"的最小实例（契约测试用），
// 本函数给的是接了账号池与历史日志的完整实例（cmd/server 用）。
// 池为 nil 时所有需要账号的操作都是空操作 —— 不会 panic。
func NewWithConfig(cfg Config) *Provider {
	if cfg.Client == nil {
		cfg.Client = upstream.New()
	}
	return &Provider{
		client:     cfg.Client,
		cfg:        cfg,
		adminEnv:   cfg.Admin,
		adoptTried: make(map[string]string),
		travel:     newTravelWatchState(!cfg.TravelAutoClaimDisabled),
		// 每个 Provider 一份独立预算：测试里会起多个实例，
		// 共用包级信号量会让它们互相阻塞。
		probeSem: newGrowthProbeSem(GrowthProbeConcurrency),
		growth: newGrowthWatchState(
			// accept 默认 true：见 growthWatchState.autoAccept 的注释。
			boolOrPtr(cfg.GrowthAutoAccept, true),
			boolOrPtr(cfg.GrowthAutoMakeup, true),
			boolOrPtr(cfg.GrowthAutoRedeem, false),
			boolOrPtr(cfg.GrowthAutoOpen, false),
			boolOrPtr(cfg.GrowthAutoDraw, false),
			boolOrPtr(cfg.GrowthAutoClaim, true),
		),
	}
}

// SetClient 替换上游 HTTP 客户端。
//
// cmd/server 需要在构造之后再注入它：客户端上挂着 config 里的超时
// （普通 RPC / 首字节 / 流中空闲），而那些值只有在配置加载完才知道。
// 非线程安全 —— 只在启动期调用一次。
func (p *Provider) SetClient(c *upstream.Client) {
	if c != nil {
		p.client = c
	}
}

// SetAdminEnv 注入管理端点宿主需要的核心依赖（调度器视图 + 共享任务槽）。
//
// 同样是启动期一次性注入：调度器在 Provider 之后构造，所以只能后补。
// 后补的意义在于 /admin/task 与签到/保活共用**同一个任务槽** ——
// 否则管理端点各自持有槽，"已有任务在执行中"的判断就会失效。
func (p *Provider) SetAdminEnv(env AdminEnv) {
	p.mu.Lock()
	p.adminEnv = env
	p.mu.Unlock()
}

// SetCheckinRunner 注入核心调度器的账号级动作（签到/保活）。
//
// 与 SetAdminEnv 一样后注入：调度器在 Provider 之后构造。
// 分成两个 setter 而不是一个：Checkin 是"业务动作"（会被 /admin/checkin 调用），
// AdminEnv 是"展示与任务槽"，两者虽然当前都由调度器满足，但语义不同 ——
// 合并会让将来出现"只有任务槽、没有调度器"的接线无处安放。
func (p *Provider) SetCheckinRunner(r CheckinRunner) {
	p.cfg.Checkin = r
}

// SetClientLogin 注入本机客户端登录态管理器（未配置时为 nil，面板降级 503）。
func (p *Provider) SetClientLogin(m ClientLoginManager) {
	p.cfg.ClientLogin = m
}

// clientLogin 取本机客户端登录态管理器（未接线返回 nil）。
//
// 返回的是消费方接口（见 clientlogin.go）：本包不 import internal/clientlogin 的
// 具体类型也可以调用，转换由 cmd/server 的适配器完成。
func (p *Provider) clientLogin() ClientLoginManager {
	if p == nil {
		return nil
	}
	return p.cfg.ClientLogin
}

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// ⚠ **声明了就必须实现**（契约测试会查）。
// workbuddy 实际具备：对话、动态模型目录、签到、成长中心、猫猫旅行、主动额度探测。
// 不具备：福利领取（codearts 专属）。
//
// # CapQuotaProbe 的判据（Task 3c 修正）
//
// 原先这里**没有**声明 CapQuotaProbe，但 AdminRoutes 已经把
// POST /admin/credits/refresh 标成了 CapQuotaProbe —— 两边矛盾：
// 前端据此渲染入口时，能力位为 0，那个入口会静默消失。
//
// 实测 workbuddy **确实**有主动探测：Provider.RefreshCredits 直接调上游
// /v2/billing/meter/get-user-resource 查余额并写回池，不依赖任何一次对话的
// 响应体推断。所以正确做法是把它声明出来，而不是把端点的能力位改掉
// （后者等于把已有功能从界面上藏起来）。
//
// 统一到 CapCheckin 曾经是一个诱人的选项，但它会让
// /admin/credits/refresh 在"签到能力被关闭"的部署里一起消失 —— 而刷新额度
// 与签到是两件事（前者只查询，后者会上报签到）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat |
		gateway.CapModels |
		gateway.CapCheckin |
		gateway.CapGrowth |
		gateway.CapTravel |
		gateway.CapQuotaProbe
}

// Chat 转发一次对话请求。
//
// 各上游的差异（/v2 前缀、header 拼法、UA）全部关在 upstream.Client 里。
//
// 注意 status 的语义：非 2xx **不返回 error**，而是把状态码与错误体一起返回，
// 让调用方统一处理"业务错误码"（如 429 触发冷却）与"传输错误"。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	// ChatStream 目前不接受 ctx（内部自建）。这里检查一次，
	// 保证调用方传已取消的 ctx 时能立刻返回而不是白跑一趟请求。
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	rc, status, respBody, err := p.client.ChatStream(a, body)
	if err != nil {
		// respBody 非空时说明是"上游返回了错误体"，属于业务错误，仍按流返回。
		if len(respBody) > 0 {
			return gateway.ChatStream{
				Status: status,
				Body:   io.NopCloser(bytes.NewReader(respBody)),
			}, nil
		}
		return gateway.ChatStream{}, err
	}
	return gateway.ChatStream{Status: status, Body: rc}, nil
}

// Models 返回上游动态模型目录。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ms, err := p.client.FetchModels(a)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.ModelInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, gateway.ModelInfo{
			// upstream.ModelInfo 的 ContextWindow/MaxTokens 就是
			// 上游的 maxInputTokens/maxOutputTokens（见其字段注释）。
			ID:              m.ID,
			ContextWindow:   clampInt64(m.ContextWindow),
			MaxOutputTokens: clampInt64(m.MaxTokens),
		})
	}
	return out, nil
}

// clampInt64 把上游给的 int64 收进 int（契约里是 int）。
//
// 上游字段是 int64、gateway.ModelInfo 用 int 是为了跨平台一致。
// 极端值（上游返回超大数据）会溢出成负数 —— 那会让下游算出负的窗口大小，
// 比"截断到上限"更糟。所以显式收窄。
func clampInt64(v int64) int {
	const maxInt = int64(^uint(0) >> 1)
	if v < 0 {
		return 0
	}
	if v > maxInt {
		return int(maxInt)
	}
	return int(v)
}

// authOf 从 Credential 里取出 workbuddy 的凭证结构。
//
// Secret 是 any（各上游凭证结构不同），所以必须类型断言。
// **断言失败要返回明确错误，不能 panic** —— 这是契约要求，也有测试守着。
func authOf(cred gateway.Credential) (*auth.Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("workbuddy: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*auth.Auth)
	if !ok {
		return nil, fmt.Errorf("workbuddy: 凭证类型不对，期望 *auth.Auth，实际 %T", cred.Secret)
	}
	if a == nil {
		return nil, errors.New("workbuddy: 凭证是 nil 指针")
	}
	return a, nil
}

var _ gateway.Provider = (*Provider)(nil)
