// provider.go MonkeyCode 上游的 Provider 面。
package monkeycode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/anthroconv"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// ProviderID 上游标识（注册名 / 配置键 / 模型前缀）。
const ProviderID = "monkeycode"

// providerID 包内别名。
//
// 两者并存是刻意的（与 zcode / minimax 同款）：导出那个给 cmd/server 与测试用，
// 包内一律用小写，这样"哪里依赖了字符串字面量"一眼可见。
const providerID = ProviderID

const displayName = "MonkeyCode"

// caps 本上游声明的能力位。
//
//	CapChat        双协议（openai_responses / anthropic）按模型的 interface_type 选
//	CapModels      远端目录（国内站 45 / 国际站 11）
//	CapQuotaProbe  /users/wallet（balance + daily_token）
//	CapImport      粘贴导入 session
//

const caps = gateway.CapChat | gateway.CapModels | gateway.CapQuotaProbe | gateway.CapImport |
	gateway.CapCheckin

// Provider MonkeyCode 上游实例。
type Provider struct {
	client  *Client
	authDir string

	mu    sync.RWMutex
	creds map[string]*Auth

	// credSrc 核心给的活凭证来源（账号池里的 Secret）。
	//
	// 池子暴露的只是**核心投影**（uid/nickname/filePath），而本包要
	// *monkeycode.Auth（含 api_key / signing_secret），那东西在 pool.SecretOf 里。
	credSrc func(uid string) (gateway.Credential, bool)

	// uidEnumerator 核心给的 uid 枚举。
	//
	// ⚠ 不能只读 p.creds：生产上凭证由注入的凭证源提供，p.creds 只有测试会填。
	// zcode 上踩过这个坑（端点返回 200 + 空列表，看起来像"没有账号"）。
	uidEnumerator func() []string

	// checkin 签到共享驱动（懒构造，见 initCheckin）。
	checkin *dailycheckin.Driver
	// checkinLog 签到历史（nil = 只打日志不落盘，界面上那列会空）。
	checkinLog *checkinlog.Log
	// checkinInterval 自动签到间隔（<=0 = 只给按钮，不自动跑）。
	checkinInterval time.Duration

	// logins 页内添加账号的 state 表（见 login.go）。
	//
	// 放在 Provider 上而不是全局：登录是**上游专属**的，
	// 两个上游的 state 空间互不相干；放全局还要加前缀防撞。
	logins map[string]*loginSession
}

// Config 构造参数。
type Config struct {
	// Client 可选。nil 时按 Proxy 新建。
	Client *Client
	// AuthDir 凭证目录（AuthDirExt 用）。
	AuthDir string
	// Proxy 出口。空 = 直连。
	Proxy string
	// CheckinLog 签到历史（nil = 不落盘，界面「今日签到」列会空）。
	CheckinLog *checkinlog.Log
	// CheckinInterval 自动签到间隔（<=0 = 只给按钮，不自动跑）。
	CheckinInterval time.Duration
}

// New 构造 Provider。
func New(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = NewClientWithProxy(cfg.Proxy)
	}
	p := &Provider{
		client:          c,
		authDir:         strings.TrimSpace(cfg.AuthDir),
		creds:           map[string]*Auth{},
		checkinLog:      cfg.CheckinLog,
		checkinInterval: cfg.CheckinInterval,
	}
	p.initCheckin()
	return p
}

// NewProvider 契约测试入口。
//
// ⚠ 不能叫 New：本包的 New 要 Config 参数。契约测试签的是无参构造。
func NewProvider() gateway.Provider { return New(Config{}) }

// SetCredentialSource 装配活凭证来源。
func (p *Provider) SetCredentialSource(fn func(uid string) (gateway.Credential, bool)) {
	p.mu.Lock()
	p.credSrc = fn
	p.mu.Unlock()
}

// SetUIDEnumerator 装配 uid 枚举来源。
func (p *Provider) SetUIDEnumerator(fn func() []string) {
	p.mu.Lock()
	p.uidEnumerator = fn
	p.mu.Unlock()
}

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力位。
func (p *Provider) Caps() gateway.Capability { return caps }

// DisplayName 控制台显示名。
func (p *Provider) DisplayName() string { return displayName }

// AuthDir 凭证目录。
func (p *Provider) AuthDir() string { return p.authDir }

// cred 取某 uid 的活凭证。
//
// 顺序：先问注入的凭证源（生产路径），再回落本实例的 creds（测试路径）。
// 反过来会让生产永远读到空 map。
func (p *Provider) cred(uid string) *Auth {
	p.mu.RLock()
	src := p.credSrc
	local := p.creds[uid]
	p.mu.RUnlock()

	if src != nil {
		if c, ok := src(uid); ok {
			if a, ok2 := c.Secret.(*Auth); ok2 && a != nil {
				return a
			}
		}
	}
	return local
}

// authOf 从核心凭证里取本包的 Auth。
func (p *Provider) authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, fmt.Errorf("monkeycode: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return nil, fmt.Errorf("monkeycode: 凭证类型不对（期望 *monkeycode.Auth，实际 %T）", cred.Secret)
	}
	if !a.Usable() {
		return nil, fmt.Errorf("monkeycode: 凭证缺少 session")
	}
	return a, nil
}

// DefaultHostCN 国内站基址（导出给 cmd/server 打日志用）。
func DefaultHostCN() string { return hostCN }

// DefaultHostIntl 国际站基址（导出给 cmd/server 打日志用）。
func DefaultHostIntl() string { return hostIntl }

// ---- 对话 ----

// defaultInstructions 发给上游的静态 system prompt，**同时**是签名对象。
//
// # 为什么必须是同一个常量
//
// 签名算的是"你在请求体里发出去的那段字符串"。两处各写一份 = 迟早漂移，
// 而漂移的表现是 403 `invalid ohmyagent request` —— 它**不会**告诉你差的是哪个字节。
// 所以这里收敛成唯一常量，签名与 payload 都引用它。
//
// # 为什么可以自定义
//
// 逆向确认签名只覆盖这段文本本身，不校验"是否是官方那段"。
// 所以不需要抄官方那 5170 字节，一段简洁的即可。
const defaultInstructions = "You are a helpful AI assistant accessed through an API gateway. " +
	"Answer the user's request directly and concisely. " +
	"Use the same language as the user unless asked otherwise. " +
	"When you need to call a tool, call it instead of describing it."

// Instructions 返回签名对象（导出，供测试与自检）。
func Instructions() string { return defaultInstructions }

// Chat 发一次对话。
//
// # 双协议路由
//
// 本上游的模型分两类，**协议不同且走错会 404**（不是 403）：
//
//	openai_responses  POST /v1/responses   {model, instructions, input}
//	anthropic         POST /v1/messages    {model, system, messages}
//
// 实测：qwen3.8-flash 走 responses，qwen3.5-plus / deepseek-flash 走 messages。
// 所以路由**必须**按模型名查表（见 interfaceOf），不能随便挑一个。
//
// # 404 的语义
//
// 走错协议的 404 `路由不存在` 是**本包的 bug**，不是账号问题。
// 用 interfaceOf 路由就是让这种 404 不该发生；万一发生，
// ErrorClassifier 会把 404 归 ErrKindNotFound（短冷却、不累计错误），
// 至少不会把好号禁掉。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := p.authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	if !a.ChatUsable() {
		return gateway.ChatStream{}, fmt.Errorf("monkeycode: 凭证缺 api_key/signing_secret（先调签发接口）")
	}

	model := modelOf(body)
	iface := interfaceOf(model)

	var payload []byte
	if iface == ifaceAnthropic {
		// OpenAI chat/completions → Anthropic Messages。
		// 走共享的 internal/anthroconv，与 zcode / minimax 同一份判据。
		anthro, cerr := anthroconv.OpenAIToAnthropic(body)
		if cerr != nil {
			// ⚠ 带 ErrConvertFailed 哨兵：出口层据此**不换号**直接回 400。
			// 换号对转换失败无效（每个号都会以同样方式失败）。
			return gateway.ChatStream{}, fmt.Errorf("monkeycode: 请求体转换失败: %w",
				errors.Join(cerr, gateway.ErrConvertFailed))
		}
		payload, err = ensureSystemSignature(anthro)
		if err != nil {
			return gateway.ChatStream{}, err
		}
		// ⚠ 恒发 stream:true —— 否则上游回 JSON，而转换链按 SSE 解析，
		//   结果是一个「HTTP 200 + 空 content」的静默错误（见 forceStream 注释）。
		payload, err = forceStream(payload)
		if err != nil {
			return gateway.ChatStream{}, err
		}
	} else {
		payload, err = toResponsesBody(body)
		if err != nil {
			return gateway.ChatStream{}, fmt.Errorf("monkeycode: 请求体转换失败: %w",
				errors.Join(err, gateway.ErrConvertFailed))
		}
	}

	// ★ 签名对象必须从**实际发出的 payload** 里取，不能直接用 defaultInstructions。
	//   anthropic 那条会把调用方的 system 拼在 defaultInstructions 后面，
	//   拼完的整段才是上游要签的那段；用常量签必然 403。
	var signed string
	if iface == ifaceAnthropic {
		signed = systemOf(payload)
	} else {
		signed = instructionsOf(payload)
	}

	resp, err := p.client.DoChat(ctx, a, ChatRequest{
		Interface:    iface,
		Payload:      payload,
		Instructions: signed,
	})
	if err != nil {
		return gateway.ChatStream{}, err
	}

	if resp.StatusCode >= 400 {
		// 错误体原样交出：出口层要按业务码/状态码判。
		return gateway.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
	}

	// 统一产出 OpenAI SSE —— 非流式由出口层用 wire.Aggregate 合成 JSON，
	// 上游侧不需要第二套转换（minimax 上踩过这个坑：给它一份好的 SSE 就够了）。
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		var cerr error
		if iface == ifaceAnthropic {
			cerr = anthroconv.AnthropicSSEToOpenAI(resp.Body, pw)
		} else {
			cerr = responsesSSEToOpenAI(resp.Body, pw)
		}
		pw.CloseWithError(cerr)
	}()
	return gateway.ChatStream{Status: resp.StatusCode, Body: pr}, nil
}

// ---- 模型目录 ----

// Models 返回模型目录。
//
// ⚠ **无凭证也必须给目录**（本仓其它上游的同款教训）：出口层调 Models 时
// 往往不带有效凭证，若"先 authOf、失败就 error"，兜底清单会变成死代码 ——
// 新装的上游在 /v1/models 里一个模型都不出现，而那正是用户第一次配置的时刻。
//
// 顺序：有凭证且远端取得到 → 远端（权威）；否则 → 兜底表。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a, err := p.authOf(cred); err == nil && a != nil {
		if list, ferr := p.client.FetchModels(ctx, a); ferr == nil && len(list) > 0 {
			// ★ 顺手把「模型名 → 协议」学到，供 Chat 路由用。
			//   远端接口类型是权威；兜底表只是它拿不到时的替代。
			learnInterfaces(list)
			return toModelInfo(list), nil
		}
	}
	return toModelInfo(fallbackModels()), nil
}

// toModelInfo 把上游条目转成核心的 ModelInfo。
//
// ⚠ 只带 ID / 窗口 / 输出上限：gateway.ModelInfo 就这三个字段。
// 上游的 access_level、is_hidden 那些**不进核心**（跨上游没有共识）。
func toModelInfo(list []ModelEntry) []gateway.ModelInfo {
	out := make([]gateway.ModelInfo, 0, len(list))
	for _, e := range list {
		if strings.TrimSpace(e.Model) == "" {
			continue
		}
		out = append(out, gateway.ModelInfo{
			ID:              strings.TrimSpace(e.Model),
			ContextWindow:   e.ContextLimit,
			MaxOutputTokens: e.OutputLimit,
		})
	}
	return out
}

var _ = http.StatusOK
