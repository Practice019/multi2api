// provider.go 把 Loomy（讯飞 Loomy 桌面客户端的模型服务）适配为 gateway.Provider。
//
// # 本包在整条解耦链里的位置
//
// 它是**第三个 Adapter**，也是判据 1 的第二次实测：
//
//	第一个（workbuddy）→ 接缝还没被抽象出来
//	第二个（codearts） → 证明"加一个上游 = 加一个目录"，但它自己很重
//	第三个（本包）     → 证明"轻上游"同样成立：契约的最小实现只有 4 个方法，
//	                     其余能力**一个都不用实现**
//
// 对比三个上游的实现量（这组对比本身就是判据 1 的度量）：
//
//	workbuddy → Client + 提示词/身份伪造/签到/成长/旅行/活动/看板 + 22 条管理端点
//	codearts  → SDK-HMAC 签名 + DPoP + STS 续期 + 单所有者 store + 福利/订阅
//	loomy     → 一个 HTTP 转发 + 一份静态模型表 + 一个分类器
//
// # 本包只实现 4 个**必需**扩展点里的 3 个，且刻意不实现另外几个
//
//	实现：AuthDirExt / CredentialLoader(+Secret) / ErrorClassifier / ResetPolicyExt
//	不实现：AdminExt / JobExt / LoginFlow / CredentialRefresher / RefreshSkewExt / SoftRateExt
//
// "不实现"不是"懒得做"，每一条都有具体理由：
//
//	AdminExt            Loomy 没有可用的管理端点（积分余额只在客户端本地缓存里，
//	                    手册没有给出查询端点，凭空造一条只会是个假按钮）
//	JobExt              没有签到、没有续期、没有活动 —— 没有任何要定时做的事
//	LoginFlow           登录发生在桌面客户端里（Electron 授权），
//	                    网关这一侧无法发起也无法轮询 → 没有"页内添加账号"
//	CredentialRefresher 没有可刷的凭证（无 refresh token，见 credential.go）
//	RefreshSkewExt      与上一条同因：没有续期就没有"多早算该刷"
//	SoftRateExt         上游没有给出"限流何时解除"的信息，硬冷却即可
//
// 这正是 gateway 把扩展点做成**可选类型断言**而不是 Provider 方法的价值：
// 一个纯转发的上游可以只写 100 行就接入，而不必为 6 个用不上的能力
// 各写一个空实现。
package loomy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"workbuddy2api/internal/gateway"
)

// providerID 上游标识。
//
// 会被用作：
//   - 模型名前缀（"loomy/deepseek-v4-flash-0731"）
//   - 配置里的 provider key
//   - 统计维度（by_provider）
//
// 格式受 gateway 约束（^[a-z][a-z0-9-]*$），有契约测试守着。
const providerID = "loomy"

// ProviderID 导出上游标识，供**装配层**（cmd/server）使用。
//
// 装配层要把 loomy 账号并入核心账号池（pool.SyncToDirWithSecrets 的
// provider 参数），并据此判断"这个号属于哪个上游"。
// 与另两个上游的同名常量同一理由：标识的唯一权威仍是 providerID，
// 导出面越小越好。
const ProviderID = providerID

// Provider 实现 gateway.Provider（以及四个扩展点，见包注释）。
type Provider struct {
	client *Client
	// authDir 凭证目录。供 AuthDirExt 使用 —— 核心据此按上游重载 auths。
	authDir string
}

// NewProvider 建一个 Loomy Provider（契约测试用的无依赖构造）。
//
// 只构造，不做网络请求 —— 契约测试会多次调用 factory，不能有副作用。
//
// ⚠ 名字不能叫 New：本包的 client.go 已经有 `New() *Client`（上游 HTTP 客户端），
// 两者是不同的东西，同名会编译冲突。契约测试里传的是**这个**函数。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// Config Provider 的可选依赖，全部可缺省。
//
// 零值 Config 得到一个"能跑契约测试的最小实例"：默认指向生产上游、
// 没有凭证目录。任何字段缺失都**降级**而不是 panic。
type Config struct {
	// Client 上游 HTTP 客户端。nil 时用 New()。
	Client *Client
	// BaseURL 上游基址。Client 为 nil 时用它构造客户端；
	// 两者都给时以 Client 为准（显式注入优先）。
	BaseURL string
	// AuthDir 凭证目录（如 `auths/loomy`）。
	AuthDir string
}

// NewWithConfig 按配置建一个 Loomy Provider。
func NewWithConfig(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = NewWithBase(cfg.BaseURL)
	}
	return &Provider{client: c, authDir: cfg.AuthDir}
}

// SetClient 替换上游 HTTP 客户端（启动期一次性注入）。
func (p *Provider) SetClient(c *Client) {
	if c != nil {
		p.client = c
	}
}

// SetAuthDir 替换凭证目录（启动期一次性注入）。
func (p *Provider) SetAuthDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		p.authDir = dir
	}
}

// Client 暴露底层客户端（供装配层与测试使用）。
func (p *Provider) Client() *Client { return p.client }

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// Loomy 实际具备的只有两项：
//
//	CapChat    对话（POST /chat/completions，实测流式/function_calling/视觉/推理全通）
//	CapModels  模型目录（GET /models，实测 12 条）
//
// 其余五项**不声明**，每一项都对应一个它确实没有的东西：
//
//	CapCheckin     没有每日签到端点（积分按自然日**自动**重置，不需要领取）
//	CapGrowth      没有成长中心/任务体系
//	CapTravel      没有猫猫旅行
//	CapWelfare     没有福利领取
//	CapQuotaProbe  没有"主动探测余额"的端点（余额只在客户端本地缓存）
//
// ⚠ 声明 CapModels 意味着 Models() 必须真的返回非空目录 ——
// 契约测试会验证这一条（见 contract.go 的 capabilityProbes）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels
}

// Chat 转发一次对话请求。
//
// 做的事很少，因为 Loomy 本身就是 OpenAI 兼容端点 —— 没有签名、没有会话槽、
// 没有通道选择。全部工作是：
//
//  1. 凭证类型断言（不匹配返回明确错误，**不 panic**）
//  2. ctx 已取消则立刻返回（契约要求；否则前端断开后上游调用仍在跑）
//  3. 出站请求体改写（强制 stream + 裁剪超限 max_tokens，见 client.prepareBody）
//  4. 装**对话那条轨**的鉴权头（Authorization: Bearer，见 client.go 的双轨说明）
//
// status 的语义与另两个上游对齐：非 2xx **不返回 error**，
// 而是把状态码与上游错误体一起返回，让调用方统一处理"业务错误"（换号/冷却）
// 与"传输错误"。这一条很重要 —— 它让 errorclassifier 能拿到完整正文。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	// 契约要求"ctx 取消时 Chat 必须尽快返回"。这里先检查一次，
	// 保证调用方传已取消的 ctx 时不必等一次网络往返。
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	rc, status, respBody, err := p.client.ChatStream(ctx, a, body)
	if err != nil {
		// respBody 非空 = 上游返回了错误体（业务错误），仍按流返回。
		if len(respBody) > 0 {
			return gateway.ChatStream{
				Status: status,
				Body:   io.NopCloser(bytes.NewReader(respBody)),
			}, nil
		}
		return gateway.ChatStream{}, err
	}
	// 契约要求"err==nil 时 Status 或 Body 至少有一个非零"。
	// 正常分支两者都有；rc 为 nil 只可能来自上游给了 2xx 却没有 body
	// （httptest 的某些构造），那种情况下用空流兜住，避免调用方
	// 拿到 nil 再去 Close 而 panic。
	if rc == nil {
		rc = io.NopCloser(bytes.NewReader(nil))
	}
	return gateway.ChatStream{Status: status, Body: rc}, nil
}

// Models 返回模型目录。
//
// # 实时优先、静态兜底（与 codearts 的"纯静态"不同）
//
//	实时拿得到 → 用实时的（上游新增/下架模型能自动跟随）
//	实时拿不到 → 用静态快照（保证目录不会因为一次抖动/session 失效而整片消失）
//
// 为什么不能只靠实时：Models() 是 /v1/models 的数据源，它一空，
// 客户端就判定"模型不存在"并报错 —— 而真相只是这一轮没拉到。
// 为什么不能只靠静态：那会让上游新增的模型永远不出现（手动改代码才能用）。
//
// # 为什么过滤掉"已知不可用"的模型
//
// 手册实测两个生图模型（doubao-seedream-5-lite / qwen-image-3.0-pro）
// 在 /models 里**列着**，但一用就 404「该模型暂未开放」。
// 把它们继续挂在目录里，等于给客户端一个必然失败的选择 ——
// 与 codearts 过滤"额度当天耗尽"的模型是同一条判据：
// **目录是客户端唯一的选型依据，挂在上面就得能用。**
//
// 过滤是安全的：它只是不显示，请求仍然能发（上游的错误会如实返回），
// 而且上游一旦真的开放，实时目录会把它带回来（filterUnavailable 只按
// 静态表里的 Unavailable 标记过滤，不按 ID 白名单）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if _, err := authOf(cred); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	live, lerr := p.client.ModelList(ctx, cred.Secret.(*Auth))
	if lerr == nil && len(live) > 0 {
		return toModelInfos(filterUnavailable(live)), nil
	}
	if lerr != nil {
		// 只记日志、不返回错误：静态快照是**设计好的**回落路径，
		// 不是失败。把它报成 error 会让出口层跳过该上游 → 目录反而空了。
		log.Printf("loomy: 实时模型目录拉取失败，回落到静态快照: %v", lerr)
	}
	return toModelInfos(AvailableModels()), nil
}

// filterUnavailable 剔除静态表里已标记"上游下架"的模型。
//
// 只在实时目录里做（静态表本身已经过滤过）。匹配用大小写不敏感的 ID 比较：
// 上游对同一模型的写法可能大小写不稳（`Kimi-k2.6` 这种混合形态很容易被改）。
func filterUnavailable(live []Model) []Model {
	out := make([]Model, 0, len(live))
	for _, m := range live {
		if known, _, ok := ResolveModel(m.ID); ok && known.Unavailable {
			continue
		}
		out = append(out, m)
	}
	return out
}

// toModelInfos 把本包的 Model 投影成中立契约 gateway.ModelInfo。
//
// 实时条目的上下文字段可能缺失（0）—— 那时用静态表里的值补上，
// 让"上游只回了 id"的极简目录也能带上窗口信息。
func toModelInfos(ms []Model) []gateway.ModelInfo {
	out := make([]gateway.ModelInfo, 0, len(ms))
	for _, m := range ms {
		cw, mo := m.ContextWindow, m.MaxOutputTokens
		if known, _, ok := ResolveModel(m.ID); ok {
			if cw <= 0 {
				cw = known.ContextWindow
			}
			if mo <= 0 {
				mo = known.MaxOutputTokens
			}
		}
		out = append(out, gateway.ModelInfo{
			ID:              m.ID,
			ContextWindow:   cw,
			MaxOutputTokens: mo,
		})
	}
	return out
}

// authOf 从 Credential 里取出 Loomy 的凭证结构。
//
// Secret 是 any（各上游凭证结构不同），所以必须类型断言。
// **断言失败要返回明确错误，不能 panic** —— 这是契约要求，也有测试守着。
//
// ⚠ 只接受 `*loomy.Auth`，**不接受** `*auth.Auth`。
//
// 这条与 codearts 的同名函数同源（那边为此留了一段长注释）：核心在拿不到
// 不透明 secret 时会回落成 `cred.Secret = e.a`（`*auth.Auth`），
// 于是这里会报"凭证类型不对"。那是**正确**的行为 —— 与其用一个
// session 为空的投影去发请求、拿到一句含糊的 401，不如立刻说清类型不对。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("loomy: 凭证为空（Credential.Secret 未设置）")
	}
	switch v := cred.Secret.(type) {
	case *Auth:
		if v == nil {
			return nil, errors.New("loomy: 凭证是 nil 指针")
		}
		if strings.TrimSpace(v.Session) == "" {
			return nil, errors.New("loomy: 凭证缺少 session（唯一鉴权材料，见 credential.go）")
		}
		return v, nil
	default:
		return nil, fmt.Errorf("loomy: 凭证类型不对，期望 *loomy.Auth，实际 %T", cred.Secret)
	}
}

// ── 扩展点实现 ──────────────────────────────────────────────────────────

// AuthDir 本上游的凭证落盘目录（gateway.AuthDirExt）。
//
// 核心据此做"按上游重载 auths"：它只问"你的目录在哪"，
// 不认识目录里文件长什么样 —— 那是 LoadCredentials 的事。
func (p *Provider) AuthDir() string { return p.authDir }

// LoadCredentials 读取 dir 下本上游的全部凭证（gateway.CredentialLoader）。
//
// 只把 uid / nickname 投影出去（核心要的就是这两个：池子主键与展示名），
// **核心不解释凭证内容**。凭证本身走下面的 WithSecrets 版本。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	auths, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(auths))
	for _, a := range auths {
		out = append(out, gateway.Credential{
			Provider: providerID,
			UID:      a.UID,
			Nickname: a.Nickname,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret
// （gateway.CredentialSecretLoader）。
//
// # 为什么必须实现它（而不是只用 LoadCredentials）
//
// 只用投影版会有个静默缺口：手工往 `auths/loomy/` 拷一份凭证再点「重载 auths」，
// 池里会出现该 uid 但 `e.secret == nil` → 选中它时 authOf 报"凭证为空"
// → **每次请求都失败，而界面上一切正常**。
//
// 这正是 codearts 那边评审 R2 修的缺口（见 gateway.CredentialSecretLoader
// 的注释）。loomy 凭证更简单，但这个口子一样会漏。
//
// ⚠ "同源"是硬要求：本函数与 LoadCredentials 都必须走同一个 LoadDir，
// 否则会造出两份 `*loomy.Auth`，池里那份与这里这份分叉。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	auths, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(auths))
	for _, a := range auths {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: providerID,
				UID:      a.UID,
				Nickname: a.Nickname,
			},
			Secret: a, // 不透明；核心只保管
		})
	}
	return out, nil
}

// effectiveDir 决定用哪个目录：调用方给的优先，否则用装配时注入的。
//
// 核心在"按上游重载"时传的是 AuthDir() 的返回值；直接调本函数的调用方
// （测试、cmd/login）可能传自己的。两者都支持，且**优先级明确**：
// 显式入参 > 装配注入，避免"传了参数却用了别处配置"这种难查的分叉。
func (p *Provider) effectiveDir(dir string) string {
	if strings.TrimSpace(dir) != "" {
		return dir
	}
	return p.authDir
}

// 编译期断言：Provider 实现核心契约与它声称支持的全部扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"路由神秘地没挂上"。
// ⚠ 这些断言**不是**架构约束的发现机制（那由 arch_test.go 的包级依赖图
// 给出，见它关于三次绕过教训的长注释）—— 它们的作用是编译期自检。
var (
	_ gateway.Provider               = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.ErrorClassifier        = (*Provider)(nil)
	_ gateway.ResetPolicyExt         = (*Provider)(nil)
)
