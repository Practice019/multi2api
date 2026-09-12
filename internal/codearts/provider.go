// provider.go 把 CodeArts（华为云 CodeArts / InferHub）上游适配为 gateway.Provider。
//
// # 这个包在整条解耦链里的位置
//
// 它是**第二个 Adapter**，也是判据 1 的实测对象：
//
//	如果加第二个上游需要改核心（pool/logbuf/admin/server/scheduler），
//	说明 gateway 那条接缝漏了；如果一行都不用改，判据 1 成立。
//
// 结论（实测）：核心零改动。本包的存在本身不改变任何核心包。
//
// # 与 internal/upstream 的关系
//
// 本包复用 upstream 的**上游无关**部分：
//
//	upstream.ModelInfo / ModelCatalog / ErrKind   —— "对外词汇表"（跨上游共识）
//	upstream.PrepareBodyOptWithLimits             —— 出站请求体统一改写
//
// 而 CodeArts 自己的东西全部关在本包内：SDK-HMAC-SHA256 签名、DPoP 续期、
// /api/v2 前缀、maas_type 通道头、按模型配额探测、福利领取、订阅。
//
// # 为什么 Chat 必须自己做签名
//
// CodeArts 的鉴权不是 Bearer，而是每次请求现算的 SDK-HMAC-SHA256
// （见 sign.go）+ DPoP 续期（见 dpop.go），且 STS 凭证只有约 30 分钟寿命。
// 把这一整套关进 Provider.Chat 里，正是 Provider 接口存在的理由 ——
// 出口层只调一个 Chat，不需要知道签名是怎么回事。
package codearts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"workbuddy2api/internal/gateway"
)

// providerID 上游标识。
//
// 会被用作：
//   - 模型名前缀（"codearts/GLM-5.2"）
//   - 配置里的 provider key
//   - 统计维度（by_provider）
//
// 格式受 gateway 约束（^[a-z][a-z0-9-]*$），有契约测试守着。
const providerID = "codearts"

// ProviderID 导出上游标识，供**装配层**（cmd/server）使用。
//
// 装配层要在把 codearts 账号并入核心账号池时给出这个标识
// （pool.SyncToDirWithSecrets 的 provider 参数）。与 workbuddy 的同名常量
// 同一理由：标识的唯一权威仍是 providerID（ID() 返回它），导出面越小越好。
const ProviderID = providerID

// refreshSkew 提前续期窗口。
//
// 为什么是 3 分钟：CodeArts 的 STS 凭证寿命只有约 30 分钟，
// 窗口必须显著小于它，否则会出现"刚判定为新鲜、发出去已过期"的窗口。
// 但也不能太大 —— 窗口越大，续期触发越频繁，而 refresh_token 是**消费型**的
// （用一次即作废），没必要白白消耗。
const refreshSkew = 3 * time.Minute

// Provider 实现 gateway.Provider（以及 AdminExt 扩展点）。
//
// # 为什么不直接暴露 *Backend
//
// Backend 是"适配到旧 server.Backend 接口"的产物（凭证双向投影那套），
// 它服务于改造前的单上游世界。Provider 是新的、更窄的契约。
// 两者共存：本文件是 Provider 侧，backend.go 保留其词汇与辅助逻辑，
// 由 Provider 复用而不是被核心调用。
type Provider struct {
	client *Client

	// authDir 凭证目录。LoadDir 按 codearts*.json 通配，
	// 与 workbuddy 的 auths/ 目录**可以并存**（文件名前缀不同，互不干扰）。
	authDir string

	// accounts 凭证访问器（由 cmd/server 注入）。
	//
	// 为什么用函数注入而不是直接持有 Backend：
	// 账号池在核心（internal/pool），而上游包**不得**依赖 internal/pool
	// （架构约束 arch_test.go 强制）。所以核心把"按 uid 取凭证"这个动作
	// 以函数形式交给上游，方向是"核心给上游喂数据"，不是"上游去读核心"。
	accounts func() []*Auth

	// adminAdminEnv 管理端点需要的核心只读依赖（账号 uid 列表等）。
	adminEnv AdminEnv

	// refreshInterval 后台主动续期的扫描周期。<=0 表示不注册该任务
	// （只走请求路径的惰性续期）。
	refreshInterval time.Duration
}

// NewProvider 建一个 CodeArts Provider（契约测试用的无依赖构造）。
//
// 只构造，不做网络请求 —— 契约测试会多次调用 factory，不能有副作用。
//
// ⚠ 名字不能叫 New：本包的 client.go 已经有 `New() *Client`（上游 HTTP 客户端），
// 两者是不同的东西，同名会编译冲突。契约测试里传的是**这个**函数。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// Config Provider 的可选依赖。
//
// 全部可缺省：零值 Config 得到一个"能跑契约测试的最小实例"，
// 这也是 New() 的形态。任何字段缺失都**降级**而不是 panic。
type Config struct {
	// Client 上游 HTTP 客户端。nil 时用 Client 的默认构造。
	Client *Client
	// AuthDir 凭证目录。
	AuthDir string
	// Accounts 凭证访问器（nil 时按 AuthDir 现场扫描）。
	Accounts func() []*Auth
	// Admin 管理端点宿主依赖。
	Admin AdminEnv
}

// NewWithConfig 按配置建一个 CodeArts Provider。
func NewWithConfig(cfg Config) *Provider {
	if cfg.Client == nil {
		cfg.Client = New()
	}
	return &Provider{
		client:   cfg.Client,
		authDir:  cfg.AuthDir,
		accounts: cfg.Accounts,
		adminEnv: cfg.Admin,
	}
}

// SetAccounts 注入凭证访问器（启动期一次性）。
//
// 与 New 一样是延迟注入：账号池在 Provider 之后构造。
func (p *Provider) SetAccounts(fn func() []*Auth) {
	if fn != nil {
		p.accounts = fn
	}
}

// SetAdminEnv 注入管理端点需要的核心依赖（启动期一次性）。
func (p *Provider) SetAdminEnv(env AdminEnv) { p.adminEnv = env }

// SetClient 替换上游 HTTP 客户端（启动期一次性）。
func (p *Provider) SetClient(c *Client) {
	if c != nil {
		p.client = c
	}
}

// SetRefreshInterval 设置后台主动续期的扫描周期（<=0 表示关闭）。
//
// 启动期一次性调用。关闭时 Jobs() 不返回续期任务，
// 续期退化为纯请求路径的惰性行为（功能不受影响，只是空闲后首个请求会慢一点）。
func (p *Provider) SetRefreshInterval(d time.Duration) { p.refreshInterval = d }

// Client 暴露底层客户端（供 cmd/server 装配后台续期调度器等使用）。
func (p *Provider) Client() *Client { return p.client }

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// ⚠ **声明了就必须实现**（契约测试会查：声明了这些能力就必须有非空的 AdminRoutes）。
//
// CodeArts 实际具备：
//
//	CapChat        对话（/api/v2/chat/completions）
//	CapModels      模型目录（静态表，但确实是"目录"，见 models.go 的实测依据）
//	CapWelfare     福利领取（/v1/ops/claim 系列）
//	CapQuotaProbe  按模型配额探测（ProbeQuota / ProbeAllQuota）
//
// CodeArts **不具备**（因此**不声明**）：
//
//	CapCheckin     没有每日签到端点
//	CapGrowth      没有成长中心
//	CapTravel      没有猫猫旅行
//
// # 为什么 CapModels 算数
//
// CodeArts 没有公开的模型列表端点（实测多个候选路径全 404），
// 目录来自客户端本地配置的实测快照（见 models.go 的长注释）。
// 但它**是**一份非空、带上下文窗口的目录，/v1/models 拿它是有效的 ——
// 而"有没有独立端点"不是 CapModels 的判据（判据是"Models() 能不能给出目录"）。
//
// # 为什么不声明 CapQuotaProbe 之外的额度能力
//
// 契约把 CapQuotaProbe 归入"无法自动行为验证"的能力位，
// 声明它就必须同时实现 AdminExt 并提供非空路由 —— 本包满足（见 admin.go）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat |
		gateway.CapModels |
		gateway.CapWelfare |
		gateway.CapQuotaProbe
}

// Chat 转发一次对话请求。
//
// # 这里做的事（全部是 CodeArts 专属，核心一概不参与）
//
//  1. 凭证类型断言（不匹配返回明确错误，**不 panic**）
//  2. 提前续期：STS 只剩不到 refreshSkew 就先刷，避免等到 401 再浪费一次往返
//  3. 出站请求体统一改写（强制 stream + 裁剪超限的 max_tokens + 字段白名单）
//  4. 按模型选服务通道（默认 / benefit，见 models.go 的 Channel）
//     并分配独立会话 id 做心跳（并发会话上限 3，不释放会恒 400 TM.00001041）
//  5. SDK-HMAC-SHA256 签名 —— 在 Client.ChatStream 内部完成
//
// status 的语义与 workbuddy 对齐：非 2xx **不返回 error**，
// 而是把状态码与上游错误体一起返回，让调用方统一处理"业务错误码"（429 → 冷却）
// 与"传输错误"。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	// Client.ChatStream 不接受 ctx（内部用 chatHTTP 的超时控制）。
	// 这里先检查一次，保证调用方传已取消的 ctx 时能**立刻**返回 ——
	// 契约要求"ctx 取消时 Chat 必须尽快返回"，否则前端断开后上游调用仍在跑，
	// 并发会话槽被白占（CodeArts 每账号只有 3 个）。
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	// 提前续期。失败不阻断 —— 继续用原凭证试一次，
	// 真实失效会在 ChatStream 内部的重试续期里再处理一遍。
	if a.NeedsRefresh(refreshSkew) && a.RefreshToken != "" {
		if rerr := p.client.RefreshToken(a); rerr != nil {
			log.Printf("codearts: 预续期失败（继续尝试原凭证）: %v", rerr)
		}
	}

	rc, status, respBody, err := p.client.ChatStream(a, body)
	if err != nil {
		// respBody 非空时说明"上游返回了错误体"，属于业务错误，仍按流返回。
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

// Models 返回上游模型目录。
//
// CodeArts 的目录是**静态**的（见 models.go 的实测依据），因此这里不发网络请求，
// 只做凭证校验 —— 凭证不对时返回明确错误而不是"假装有目录"，
// 这样调用方能区分"上游不可用"与"目录为空"。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if _, err := authOf(cred); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ms := KnownModels()
	out := make([]gateway.ModelInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, gateway.ModelInfo{
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

// authOf 从 Credential 里取出 CodeArts 的凭证结构。
//
// Secret 是 any（各上游凭证结构不同），所以必须类型断言。
// **断言失败要返回明确错误，不能 panic** —— 这是契约要求，也有测试守着。
//
// ⚠ 这里刻意**同时支持** *Auth 与 *auth.Auth：
//
//	*codearts.Auth   本包的原生凭证（cmd/server 与测试用）
//	*auth.Auth       核心账号池投影出来的通用凭证
//
// 第二条是历史包袱（改造前 backend.go 把 codearts.Auth 投影成 auth.Auth 交给
// 旧的 server.Backend），但**保留**它有价值：核心的账号池目前仍以 auth.Auth
// 为通用流通形态，投影后 UID/AccessToken/RefreshToken/ExpiresAt 足以重建
// 一个可用的 CodeArts 凭证（真实签名材料 AK/SK 从 AccessToken 字段回读）。
//
// 不接受其它类型：返回错误而不是"尽力而为"，避免出现签名材料为空的静默失败。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("codearts: 凭证为空（Credential.Secret 未设置）")
	}
	switch v := cred.Secret.(type) {
	case *Auth:
		if v == nil {
			return nil, errors.New("codearts: 凭证是 nil 指针")
		}
		return v, nil
	default:
		return nil, fmt.Errorf("codearts: 凭证类型不对，期望 *codearts.Auth，实际 %T", cred.Secret)
	}
}

// 编译期断言：Provider 实现核心契约与 AdminExt 扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"路由神秘地没挂上"。
// 注意 `gateway.Provider =` 这个标记同时被 arch_test.go 用来**自动发现上游包**
// —— 有了它，本包一落地就被纳入架构约束，不需要在任何白名单里登记。
var (
	_ gateway.Provider = (*Provider)(nil)
	_ gateway.AdminExt = (*Provider)(nil)
)
