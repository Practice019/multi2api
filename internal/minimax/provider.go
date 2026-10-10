// provider.go MiniMax 上游的 Provider 面。
package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"workbuddy2api/internal/anthroconv"
	"workbuddy2api/internal/gateway"
)

// ProviderID 上游标识（注册名 / 配置键 / 模型前缀）。
const ProviderID = "minimax"

// providerID 包内别名。
//
// 两者并存是刻意的（zcode 同款）：导出那个给 `cmd/server` 与测试用，
// 包内一律用小写，这样"哪里依赖了字符串字面量"一眼可见。
const providerID = ProviderID

const displayName = "MiniMax Code"

// caps 本上游声明的能力位。
//
//	CapChat        推理走 Anthropic Messages
//	CapModels      远端目录（实测 4 个模型）
//	CapQuotaProbe  积分余额（credit/details）
//	CapCheckin     每日签到（signin/status + signin/claim）
//	CapImport      粘贴导入凭证
//
// 刻意**不声明** CapImageGen：远端目录里只有 `modalities.input` 含 image
// （**输入**图片），那是"能识图"，与"能生图"是两件事 ——
// 声明了就得实现，而本上游没有生图端点。
const caps = gateway.CapChat | gateway.CapModels | gateway.CapQuotaProbe |
	gateway.CapCheckin | gateway.CapImport

// Provider MiniMax 上游实例。
type Provider struct {
	client  *Client
	authDir string

	mu    sync.RWMutex
	creds map[string]*Auth

	// credSrc 核心给的活凭证来源（账号池里的 Secret）。
	//
	// 与 zcode / raccoon 同款：池子暴露的只是**核心投影**（uid/nickname/
	// filePath），而本包要 `*minimax.Auth`（含 access_token）——
	// 那个东西在 `pool.SecretOf` 里。
	credSrc func(uid string) (gateway.Credential, bool)

	// uidEnumerator 核心给的 uid 枚举（本包自己的 creds 在生产里是空的）。
	//
	// ⚠ 为什么不能只读 `p.creds`：生产上凭证由注入的凭证源提供，
	// `p.creds` 只有测试会填。zcode 上踩过这个坑（端点返回
	// `200 + {"accounts":[]}`，看起来像"没有账号"）。
	uidEnumerator func() []string

	// logins 登录会话表（state → 会话）。
	//
	// 放在 Provider 上而不是全局：登录是**上游专属**的，两个上游的
	// device_code 空间互不相干；放全局还要加前缀防撞。
	logins map[string]*loginEntry
}

// New 构造 Provider。
func New(cfg Config) *Provider {
	return &Provider{
		client:  NewClient(cfg),
		authDir: strings.TrimSpace(cfg.AuthDir),
		creds:   map[string]*Auth{},
		logins:  map[string]*loginEntry{},
	}
}

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
		return nil, fmt.Errorf("minimax: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return nil, fmt.Errorf("minimax: 凭证类型不对（期望 *minimax.Auth，实际 %T）", cred.Secret)
	}
	if !a.Usable() {
		return nil, fmt.Errorf("minimax: 凭证缺少 access_token")
	}
	return a, nil
}

// DefaultAccountBase OAuth 主机（导出给 cmd/server 打日志用）。
func DefaultAccountBase() string { return accountHost }

// DefaultAPIBase 业务/推理主机（导出给 cmd/server 打日志用）。
func DefaultAPIBase() string { return apiHost }

// ---- 对话 ----

// Chat 发一次对话。
//
// # 本上游**一律**走 Anthropic 协议
//
// 与 zcode 不同（那个两条通道、只有 JWT 走 Anthropic），MiniMax 只有
// 一条通道，`/mavis/api/v1/llm/v1/messages` 就是 Anthropic Messages
// 形状。所以转换是**无条件**的。
//
// 转换走共享的 `internal/anthroconv`（与 zcode 同一份实现）——
// 两个 Anthropic 消费者共用一套判据，不会分叉。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := p.authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	stream := detectStream(body)
	anthroBody, err := anthroconv.OpenAIToAnthropic(body)
	if err != nil {
		return gateway.ChatStream{}, fmt.Errorf("minimax: 请求体转换失败: %w", err)
	}
	// 上游这个端点**只按 SSE 回**，所以恒发 stream:true。
	anthroBody, err = forceStream(anthroBody)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.client.apiBase+pathInferMessages, strings.NewReader(string(anthroBody)))
	if err != nil {
		return gateway.ChatStream{}, err
	}
	req.Header = inferHeaders(a)
	resp, err := p.client.http.Do(req)
	if err != nil {
		return gateway.ChatStream{}, err
	}

	if resp.StatusCode >= 400 {
		// 错误体原样交出去：出口层要按业务码/状态码判。
		// ⚠ 但 402 已经在这里被识别过（`inferStatusError`）——
		// 出口层拿不到那个分类，所以这里**保留原文**，不吞。
		return gateway.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
	}
	if !stream {
		return p.translateNonStream(resp)
	}
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		err := anthroconv.AnthropicSSEToOpenAI(resp.Body, pw)
		pw.CloseWithError(err)
	}()
	return gateway.ChatStream{Status: resp.StatusCode, Body: pr}, nil
}

// translateNonStream 非流式响应：Anthropic JSON → OpenAI JSON。
//
// ⚠ 上游端点**恒回 SSE**（我们恒发 stream:true），所以这条路径实际上
// 只在"客户端请求非流式"时被走到 —— 那时我们仍按 SSE 收，
// 但要把它压成一条 OpenAI JSON。做不到就**如实返回上游原文**，
// 不编一个空响应（伪造成功会让"转换器有 bug"伪装成"模型答了空话"）。
func (p *Provider) translateNonStream(resp *http.Response) (gateway.ChatStream, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return gateway.ChatStream{}, err
	}
	converted, err := anthroconv.AnthropicJSONToOpenAI(raw)
	if err != nil {
		return gateway.ChatStream{
			Status: resp.StatusCode,
			Body:   io.NopCloser(strings.NewReader(string(raw))),
		}, nil
	}
	return gateway.ChatStream{
		Status: resp.StatusCode,
		Body:   io.NopCloser(strings.NewReader(string(converted))),
	}, nil
}

// detectStream 请求体里是不是 `stream:true`。
func detectStream(body []byte) bool {
	var probe struct {
		Stream json.RawMessage `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		// 解不出来时按**非流式**处理：非流式路径对畸形 body 更宽容
		//（上游会直接报 400，而流式路径会先建立 SSE 连接再失败）。
		return false
	}
	return strings.TrimSpace(string(probe.Stream)) == "true"
}

// ---- 模型目录 ----

// Models 返回模型目录。
//
// ⚠ **无凭证也必须给目录**（本仓其它上游的同款教训）：
// 出口层调 Models 时**不带凭证**（只传 provider id），所以"先 authOf、
// 失败就 error"会让内置兜底清单成为**死代码** —— 新装的上游在
// `/v1/models` 里一个模型都不出现，直到有人加账号。
// 而那正是用户第一次配置上游的时刻，他会以为装坏了。
//
// 顺序：有凭证且远端取得到 → 远端（**权威**，4 个模型）；
// 否则 → 兜底表（同样是实测值，只是可能滞后）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a, err := p.authOf(cred); err == nil && a != nil {
		if list, ferr := p.client.fetchModels(ctx, a); ferr == nil && len(list) > 0 {
			return toModelInfo(list), nil
		}
	}
	return toModelInfo(fallbackModels()), nil
}

// toModelInfo 把归一后的条目转成核心的 ModelInfo。
//
// ⚠ 只带 `ID` / 窗口 / 输出上限：`gateway.ModelInfo` 就这三个字段，
// **没有**展示名（`name`）。上游那个短名（`M3.1-Flash-Preview`）在核心
// 这一层用不上，丢掉是有意的 —— 前端显示的是 ID。
func toModelInfo(list []modelEntry) []gateway.ModelInfo {
	out := make([]gateway.ModelInfo, 0, len(list))
	for _, e := range list {
		out = append(out, gateway.ModelInfo{
			ID:              e.ID,
			ContextWindow:   e.ContextWin,
			MaxOutputTokens: e.MaxTokens,
		})
	}
	return out
}

// fallbackModels 兜底模型目录（4 条，顺序照抄远端 `model_order`）。
//
// # 为什么必须有它、且必须含 M3.1
//
// 参照项目实测：客户端 `config.js` 的内置表**只有 3 个**
// （M3 / M2.7-highspeed / M2.7），而远端有 **4** 个 ——
// 恰好漏掉 `MiniMax-M3.1-Flash-Preview`，而它是客户端界面上被选中的那个。
// 若兜底表也漏掉它，远端一失败用户就**看不到自己在用的模型**。
//
// # 为什么只有 M3.1 有档位
//
// 其余三个远端条目**没有 `effort_options` 字段** —— 那是远端事实，
// 不是我们漏解析。给它们编档位就是凭空猜测。
//
// 窗口口径是**档位表最大档**（M3.1 / M3 → 1M；M2.7 系 → 200K），
// **不是** `limit.context`（M3.1 那个字段是 512K，会让客户端远早于
// 官方能力触发压缩）。
func fallbackModels() []modelEntry {
	return []modelEntry{
		{
			ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview",
			ContextWin: 1_000_000, MaxTokens: 128_000, SupportsImage: true,
			EffortOptions: []string{"default", "low", "medium", "high", "xhigh", "max"},
			DefaultEffort: "default",
			ThinkingMode:  thinkingModeForcedOn,
		},
		{
			ID: "MiniMax-M3", Name: "M3",
			ContextWin: 1_000_000, MaxTokens: 128_000, SupportsImage: true,
			ThinkingMode: thinkingModeSwitchable,
		},
		{
			ID: "MiniMax-M2.7-highspeed", Name: "M2.7-highspeed",
			ContextWin: 200_000, MaxTokens: 128_000,
			ThinkingMode: thinkingModeForcedOn,
		},
		{
			ID: "MiniMax-M2.7", Name: "M2.7",
			ContextWin: 200_000, MaxTokens: 128_000,
			ThinkingMode: thinkingModeForcedOn,
		},
	}
}
