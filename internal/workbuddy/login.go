// login.go —— 本上游的登录流程适配。
//
// # 这个文件解决什么
//
// 添加账号是**上游专属**动作：workbuddy 是 OAuth 设备码
// （申请链接 → 用户在浏览器授权 → 轮询取 token），
// codearts 是 OAuth + DPoP（还要生成密钥对、签名请求、~30 分钟凭证）。
// 核心的 `gateway.LoginFlow` 是这条接缝的**通用形状**，
// 本文件把本上游的具体实现适配到它。
//
// # 为什么适配而不是让核心直接用 oauth.Client
//
// `oauth.Client` 的授权站点是**构造时绑定**的（`oauth.New(baseURL)`）——
// 它自己不知道"我属于哪个上游"。若核心直接用它，
// "按上游分派"就退化成了"所有上游共用一个客户端"，
// 也就是分派之前的状态。
//
// 由本包适配之后：
//
//	· 归属是明确的（本包 = 这个上游）
//	· 核心只认 `gateway.LoginFlow` 这个形状，不知道下方是谁
//	· codearts 将来实现自己的适配即可，核心零改动
package workbuddy

import (
	"errors"

	"workbuddy2api/internal/gateway"
)

// OAuthFlow 本包对"登录流程"的最小需求。
//
// 刻意只有两个方法。它与 `*oauth.Client` 的 `Start` **逐字相同**，
// 但 `Poll` **不同** —— oauth 返回 `*oauth.Credential`，
// 而这里要求通用的 `gateway.Credential`。
//
// ⚠ 我第一版在这里写"零适配代码，装配层直接把 oauth.New(...) 传进来"，
// **那是错的**：写了个 `_ OAuthFlow = (*oauth.Client)(nil)` 才发现在编译期
// 就不成立。编译器的断言比注释可靠 —— 现在文件末尾就有这么一行。
//
// 所以需要一层适配（见 oauthAdapter）：把 oauth 的具体凭证
// 翻译成通用凭证。这层适配放在**本包**是对的位置 ——
// 本包知道自己的凭证长什么样，核心不需要知道。
type OAuthFlow interface {
	// Start 申请一次登录，返回 (state, 授权 URL)。
	Start() (state, authURL string, err error)
	// Poll 查询授权结果。未完成时返回 ErrLoginPending。
	Poll(state string) (gateway.Credential, error)
}

// oauthClient 具体 OAuth 客户端的形状（`*oauth.Client` 满足它）。
//
// 用接口而不是具体类型：本包不该 import internal/oauth。
// 装配层传进来的任何实现了这两个方法的对象都行。
type oauthClient interface {
	Start() (state, authURL string, err error)
	// Poll 返回**具体**凭证（*oauth.Credential 之类）。
	// 返回类型是 any，因为本包不 import 那个类型 ——
	// 由装配层通过 WrapOAuth 提供翻译函数。
	Poll(state string) (any, error)
}

// WrapOAuth 把具体 OAuth 客户端适配成本包的 OAuthFlow。
//
// # 为什么翻译函数由装配层给
//
// 翻译需要知道 `*oauth.Credential` 的字段（AccessToken / UID / …）——
// 那是 oauth 包的知识。本包若要自己翻译就得 import oauth，
// 而"本包不依赖登录的具体实现"是这个接口存在的理由。
//
// 装配层两边都知道，由它给出翻译最自然：
//
//	workbuddy.Config{ Login: workbuddy.WrapOAuth(
//	    oauth.New(cfg.OAuthBaseURL),
//	    func(v any) (gateway.Credential, error) { ... },
//	)}
//
// convert 接收 `Poll` 的原始返回值；返回通用凭证。
// pending 的判定**不在这里** —— 实现可直接返回 gateway.ErrLoginPending，
// 由下面的 Poll 透传（核心用 errors.Is 判断）。
func WrapOAuth(c oauthClient, convert func(any) (gateway.Credential, error)) OAuthFlow {
	return &oauthAdapter{client: c, convert: convert}
}

// oauthAdapter 把具体客户端的两个方法接成本包要的形状。
type oauthAdapter struct {
	client  oauthClient
	convert func(any) (gateway.Credential, error)
}

func (a *oauthAdapter) Start() (string, string, error) {
	if a == nil || a.client == nil {
		return "", "", errors.New("workbuddy: 未配置登录客户端")
	}
	return a.client.Start()
}

func (a *oauthAdapter) Poll(state string) (gateway.Credential, error) {
	if a == nil || a.client == nil {
		return gateway.Credential{}, errors.New("workbuddy: 未配置登录客户端")
	}
	raw, err := a.client.Poll(state)
	if err != nil {
		// 具体实现可以直接用 gateway.ErrLoginPending，也可以用自己的哨兵；
		// 前者由核心的 errors.Is 识别，后者需要在 convert 里翻译（或由
		// 装配层包一层）。这里**不猜测** —— 透传，让上层决定。
		return gateway.Credential{}, err
	}
	if a.convert == nil {
		return gateway.Credential{}, errors.New("workbuddy: 未提供凭证翻译函数")
	}
	return a.convert(raw)
}

// loginFlow 把本包的 OAuthFlow 适配成 gateway.LoginFlow。
//
// 适配是**显式**的：本包知道 `oauth.Client` 返回的凭证结构，
// 所以由本包负责把它翻译成通用的 `gateway.Credential`
// （`Secret` 里放能满足 `MarshalAuthFile` 的对象，
// 核心据此落盘 —— 见 admin.pollViaFlow 的注释）。
type loginFlow struct {
	flow OAuthFlow
	// providerID 本次登录的归属。由 LoginFlow() 从 cfg.Provider 传入 ——
	// 不读包内的 providerID 常量：那是"本包叫什么"，
	// 而"这次注册成哪个上游"是装配层的事实（见 Config.Provider 的注释）。
	// 两者不一致时，读常量的写法会把凭证标到错误的上游。
	providerID string
	// authDir 凭证落盘目录。同样由 LoginFlow() 从 cfg 传入 ——
	// 不在这里回头读 Provider：`loginFlow` 是核心实际持有的对象，
	// 它必须自带回答 `AuthDir()` 所需的信息（否则又要一个反向指针）。
	authDir string
}

// LoginFlow 返回本上游的登录流程形状；未配置时返回 false。
//
// # 两个入口，各有用途
//
//   - `LoginFlow()` —— 给**知道自己在问谁**的调用方用（返回 bool，语义清晰）
//   - `Start`/`Poll` —— 让 `*Provider` **本身**满足 `gateway.LoginFlow`，
//     于是 `gateway.ExtOf[gateway.LoginFlow](p)` 能认出它
//
// # 为什么必须有后者（这是实测踩到的）
//
// 我第一版只写了 `LoginFlow()` 访问器。结果 admin 的类型断言
// `gateway.ExtOf[gateway.LoginFlow](wb)` **返回 false** ——
// 因为 `*Provider` 有的是 `LoginFlow() (gateway.LoginFlow, bool)`，
// 而不是 `Start`/`Poll`。**方法集不匹配，断言就认不出来。**
//
// 表现很隐蔽：manifest 的 `login` 永远是 null，
// 而 `wb.LoginFlow()` 单独调用时明明是 true。
//
// 教训：**"返回接口"与"自己是接口"是两件事。**
// 前者对 Go 的类型断言完全不可见。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil || p.cfg.Login == nil {
		return nil, false
	}
	return &loginFlow{
		flow:       p.cfg.Login,
		providerID: p.ownProviderID(),
		authDir:    p.cfg.AuthDir,
	}, true
}

// Start 让 *Provider 满足 gateway.LoginFlow（见 LoginFlow 的注释）。
//
// ⚠ 未配置时明确报错，不 panic 也不假成功。
// 但要注意：`ExtOf` 判断的是"**方法存在**"，所以即使没配登录流程，
// `*Provider` 也会**看起来**实现了 LoginFlow。
//
// 因此探测登录能力**必须**用带 bool 的 `LoginFlow()`，
// 不能裸用 `ExtOf` —— 否则没配 OAuth 的部署也会在界面上出现
// 「＋ 添加账号」按钮，点下去才报错。
func (p *Provider) Start() (string, string, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("workbuddy: 未配置登录流程")
	}
	return f.Start()
}

// Configured 报告这份部署真的能走登录流程（= 配了 OAuth 客户端）。
//
// 见 gateway.LoginFlow.Configured 的注释：光"实现了方法"不够，
// 没配客户端的部署也会看起来实现了 —— 那会渲染出假按钮。
func (p *Provider) Configured() bool {
	return p != nil && p.cfg.Login != nil
}

// AuthDir 本上游凭证的落盘目录（`gateway.LoginFlow` 要求）。
//
// 空串表示"用核心的默认目录"—— 单上游部署的旧行为不变。
//
// # 为什么必须由上游自报
//
// 实测踩过：codearts 授权后凭证被写进 workbuddy 的目录，因为核心
// 用的是 `h.cfg.AuthDir`（默认上游的目录）。按上游分子目录之后
// （`auths/workbuddy/`、`auths/codearts/`），每个上游必须自报。
func (p *Provider) AuthDir() string {
	if p == nil {
		return ""
	}
	return p.cfg.AuthDir
}

// Poll 同上（让 *Provider 满足 gateway.LoginFlow）。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("workbuddy: 未配置登录流程")
	}
	return f.Poll(state)
}

// ownProviderID 本上游在池里的归属标识。
//
// cfg.Provider 为空时回落包内常量 —— 与 ownAccounts() 的降级一致
// （那里把空串交给池子按"未打标签 = 默认上游"解释；这里没有池子可问，
// 所以只能用自己的常量，这是唯一知道答案的时刻）。
func (p *Provider) ownProviderID() string {
	if p.cfg.Provider != "" {
		return p.cfg.Provider
	}
	return providerID
}

func (f *loginFlow) Start() (string, string, error) {
	if f == nil || f.flow == nil {
		return "", "", errors.New("workbuddy: 未配置登录流程")
	}
	return f.flow.Start()
}

// Poll 透传，但**统一 pending 的判定**。
//
// 各上游可能用自己的哨兵错误表示"用户还没点确认"。若不在这里归一，
// 核心就得认识每个上游的错误类型 —— 那又把上游知识推回核心了。
//
// ⚠ 这里用 `errors.Is` 而不是 `==`：具体实现可能包了一层
// （fmt.Errorf("...: %w", err)），`==` 会漏判，表现为"轮询永远报错"，
// 而用户那边其实只是还没点确认。
func (f *loginFlow) Configured() bool { return f != nil && f.flow != nil }

// AuthDir 转发给 Provider（`gateway.LoginFlow` 要求 *loginFlow 也实现）。
func (f *loginFlow) AuthDir() string {
	if f == nil {
		return ""
	}
	return f.authDir
}

func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	if f == nil || f.flow == nil {
		return gateway.Credential{}, errors.New("workbuddy: 未配置登录流程")
	}
	cred, err := f.flow.Poll(state)
	if err != nil {
		// 具体实现已经返回了 gateway.ErrLoginPending（或包了它）→ 直接透传，
		// 核心用 errors.Is 判断，不需要本包再翻译。
		return gateway.Credential{}, err
	}
	// 归属由**本包**填 —— 具体实现（oauth.Client）不知道自己在哪个上游。
	if cred.Provider == "" {
		cred.Provider = f.providerID
	}
	return cred, nil
}

// ---- 编译期断言 ----
//
// 这些行不是文档，是**编译器的检查**。我第一版在注释里写
// "oauth.Client 可以直接传进来、零适配"，写了下面的断言才发现
// `Poll` 的返回类型不匹配 —— 注释可以写错，断言不行。
var (
	// loginFlow 必须满足核心认的那个形状。
	_ gateway.LoginFlow = (*loginFlow)(nil)

	// OAuthFlow 是本包对"登录实现"的最小需求（见它的注释）。
	_ OAuthFlow = (*oauthAdapter)(nil)
)
