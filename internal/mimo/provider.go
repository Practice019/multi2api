// provider.go 把小米 MiMo 开放平台适配成 gateway.Provider（第五个上游）。
//
// 文件级契约实现见 extensions.go（全部扩展点集中 + 编译期断言群）。
// 本文件只留：结构体/Config/构造/ID/Caps/Chat/Models/authOf。
package mimo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// providerID 上游标识（模型前缀 `mimo/…`）。
const providerID = "mimo"

// ProviderID 导出标识，供装配层使用。
const ProviderID = providerID

// Provider gateway.Provider 实现。
type Provider struct {
	client  *Client
	authDir string
	log     *checkinlog.Log

	freeEnabled        bool
	refreshInterval    time.Duration
	reasoningBackfill  bool
	credentialPriority string // tp|sk|none（混池换类重试策略位）
	callbackPort       string
	importClientAuth   bool
	clientAuthDir      string

	onRefreshFailure func(uid string)
	onRefreshSuccess func(uid string)

	cache       *reasoningCache
	loginOnce   sync.Once
	loginCached *loginFlow
	cbOnce      sync.Once
	cbServer    *callbackServer
	cbErr       error
}

// Config NewWithConfig 入参（全部可缺省）。
type Config struct {
	Client *Client
	// AuthDir 凭证目录（如 `auths/mimo`）。
	AuthDir string
	// BaseURL / FreeBaseURL / OAuthHost / AuthHeader / ClientVersion 传输面覆盖。
	BaseURL       string
	FreeBaseURL   string
	OAuthHost     string
	AuthHeader    string
	ClientVersion string
	// FreeEnabled CLI 免费通道总开关（默认 false：通道实测已死，报告 §1）。
	FreeEnabled *bool
	// RefreshInterval oauth 轨后台刷新扫描间隔；<=0 不注册任务。
	RefreshInterval time.Duration
	// ReasoningBackfill 方言层开关（默认 true）。
	ReasoningBackfill *bool
	// CredentialPriority tp|sk|none，默认 tp（OmniProxy 同款：套餐 key 优先吃，
	// 按量 key 兜底；混池时 401/402/403 先换类重试）。
	CredentialPriority string
	// CallbackPort OAuth 回调监听端口（默认 18081；**勿撞 trae 的 18080**）。
	CallbackPort string
	// ImportClientAuth 允许读本机官方客户端 auth.json（默认 false：显式开）。
	ImportClientAuth bool
	// ClientAuthDir 官方 data 目录覆盖（探测候选见 oauth.go）。
	ClientAuthDir string

	Log              *checkinlog.Log
	OnRefreshFailure func(uid string)
	OnRefreshSuccess func(uid string)
}

// NewProvider 契约测试用的无依赖构造。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// NewWithConfig 建 Provider。
func NewWithConfig(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = New()
		if cfg.BaseURL != "" {
			c.PaidBase = strings.TrimRight(cfg.BaseURL, "/")
		}
		if cfg.FreeBaseURL != "" {
			c.FreeBase = strings.TrimRight(cfg.FreeBaseURL, "/")
		}
		if cfg.OAuthHost != "" {
			c.OAuthHost = strings.TrimRight(cfg.OAuthHost, "/")
		}
		if cfg.AuthHeader != "" {
			c.AuthHeader = cfg.AuthHeader
		}
		if cfg.ClientVersion != "" {
			c.ClientVer = cfg.ClientVersion
		}
	}
	free := false
	if cfg.FreeEnabled != nil {
		free = *cfg.FreeEnabled
	}
	c.FreeEnabled = free
	backfill := true
	if cfg.ReasoningBackfill != nil {
		backfill = *cfg.ReasoningBackfill
	}
	port := strings.TrimSpace(cfg.CallbackPort)
	if port == "" {
		port = "18081"
	}
	pri := strings.ToLower(strings.TrimSpace(cfg.CredentialPriority))
	if pri == "" {
		pri = "tp"
	}
	return &Provider{
		client:             c,
		authDir:            cfg.AuthDir,
		log:                cfg.Log,
		freeEnabled:        free,
		refreshInterval:    cfg.RefreshInterval,
		reasoningBackfill:  backfill,
		credentialPriority: pri,
		callbackPort:       port,
		importClientAuth:   cfg.ImportClientAuth,
		clientAuthDir:      cfg.ClientAuthDir,
		onRefreshFailure:   cfg.OnRefreshFailure,
		onRefreshSuccess:   cfg.OnRefreshSuccess,
		cache:              newReasoningCache(),
	}
}

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// 只声明可验证的两项（对话/模型）。MiMo **没有**程序化签到/额度端点
// （三源一致的全仓 0 发现，报告 §2.1）→ CapCheckin/CapQuotaProbe 不声明，
// 前端相应入口自动消失 —— 不虚报（契约 probeCapabilities 会当场抓）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels
}

// Chat 转发一次对话。
//
// # 处理链（顺序即语义）
//
//  1. dialect 出站回注（reasoning_content 补齐）；
//  2. 上游 400-且-reasoning 方言错误 → 降级（剥无 reasoning 的 tool_calls 历史）
//     重发恰 1 次；仍败上交分类器（判 None，不罚号）；
//  3. 401 自愈（**只对有可刷材料的凭证**：oauth→RefreshOAuth；free→重
//     bootstrap）→ 原请求重试恰 1 次 → 仍 401 上交（SessionDead 可见禁用）。
//     纯 sk 凭证无可刷材料 → 直接上交 —— 401 的 sk 就是死了，刷也没用。
//  4. 2xx：SSE **原样透传** + 旁路聚合（回写 reasoning 缓存，供下轮回注）。
//
// ⚠ 每趟出站都保持 (rc,status,respBody) 三态原始数据到最后再组装流 ——
// 中途"读掉错误体再上交原流"会把 Body 掏空（上游 4xx 变成空响应体）。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}
	if a.Channel == ChannelFree && !p.freeEnabled {
		return gateway.ChatStream{}, fmt.Errorf("mimo: free 凭证存在但通道未启用（mimo.free_enabled=false，实测通道已死）")
	}

	anchor := sessionAnchorOf(body)
	outBody := body
	if p.reasoningBackfill {
		outBody, _, _ = backfillOutbound(body, anchor, p.cache)
	}

	rc, status, respBody, err := p.client.ChatStream(ctx, a, outBody, p.affinityFor(anchor))
	if err != nil {
		return gateway.ChatStream{}, err
	}

	// 方言 400：降级重发一次（仅当确有可剥的历史）。
	if status == 400 && isReasoningParamError(status, string(respBody)) {
		if rc != nil {
			_ = rc.Close()
		}
		down, stripped := downgradeToolHistory(outBody)
		if stripped > 0 {
			log.Printf("mimo: reasoning 方言 400 → 降级重发（剥离 %d 条无 reasoning 的 tool 历史）uid=%s",
				stripped, shortUID(a.UID))
			rc, status, respBody, err = p.client.ChatStream(ctx, a, down, p.affinityFor(anchor))
			if err != nil {
				return gateway.ChatStream{}, err
			}
		}
	}

	// 401 自愈：oauth/free 可刷凭证 → 续期 + 原 body 重试恰一次。
	if status == 401 && a.Renewable() && rc == nil {
		if rc != nil {
			_ = rc.Close()
		}
		if rerr := p.renewForChat(ctx, a); rerr == nil {
			log.Printf("mimo: 401 自愈：uid=%s 续期后重试", shortUID(a.UID))
			rc, status, respBody, err = p.client.ChatStream(ctx, a, outBody, p.affinityFor(anchor))
			if err != nil {
				return gateway.ChatStream{}, err
			}
		} else {
			log.Printf("mimo: 401 后自愈续期失败 uid=%s: %v", shortUID(a.UID), rerr)
		}
	}

	if status >= 400 {
		if rc != nil {
			_ = rc.Close()
		}
		return gateway.ChatStream{Status: status, Body: io.NopCloser(bytes.NewReader(respBody))}, nil
	}

	// 2xx：直通 + 旁路聚合（Close 收尾写缓存）。
	agg := &streamAggregator{}
	pr := &passThroughAggReader{src: rc, agg: agg}
	return gateway.ChatStream{Status: status, Body: &closeAggReader{
		ReadCloser: io.NopCloser(pr),
		closer: func() {
			agg.finish(anchor, p.cache)
			_ = rc.Close()
		},
	}}, nil
}

// renewForChat 按凭证轨续期（oauth→账号域刷新+落盘；free→作废旧票重 bootstrap+落盘）。
func (p *Provider) renewForChat(ctx context.Context, a *Auth) error {
	switch {
	case a.Channel == ChannelFree:
		p.client.DropCachedJWT(a.Fingerprint)
		jwt, err := p.client.Bootstrap(ctx, a.Fingerprint)
		if err != nil {
			return err
		}
		a.mu.Lock()
		a.AccessToken = jwt
		a.ExpiresAt = jwtExpiry(jwt)
		a.mu.Unlock()
		if err := a.SaveAtomic(); err != nil {
			log.Printf("mimo: free 重取成功但落盘失败 uid=%s: %v", shortUID(a.UID), err)
		}
		return nil
	default: // oauth
		if err := p.client.RefreshOAuth(a); err != nil {
			return err
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("mimo: oauth 续期成功但落盘失败 uid=%s: %v", shortUID(a.UID), err)
		}
		return nil
	}
}

// affinityFor 会话锚 → `ses_` 前缀稳定 id（官方语义：会话内稳定、跨会话不复用）。
func (p *Provider) affinityFor(anchor string) string {
	if anchor == "" {
		return ""
	}
	return "ses_" + anchor
}

// Models 模型目录（实时优先、静态兜底）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.Channel == ChannelFree {
		return toInfos(staticModels), nil // 免费面无 /models 端点（存档）
	}
	live, lerr := p.client.FetchModels(ctx, a)
	if lerr == nil && len(live) > 0 {
		return toInfos(live), nil
	}
	if lerr != nil {
		log.Printf("mimo: 实时模型目录拉取失败，回落静态快照: %v", lerr)
	}
	return toInfos(staticModels), nil
}

func toInfos(ms []ModelInfo) []gateway.ModelInfo {
	out := make([]gateway.ModelInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, gateway.ModelInfo{ID: aliasModel(m.ID)})
	}
	return out
}

// authOf 从 Credential 取本包凭证（错型明确报错，不 panic）。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("mimo: 凭证为空（Credential.Secret 未设置）")
	}
	switch v := cred.Secret.(type) {
	case *Auth:
		if v == nil {
			return nil, errors.New("mimo: 凭证是 nil 指针")
		}
		return v, nil
	default:
		return nil, fmt.Errorf("mimo: 凭证类型不对，期望 *mimo.Auth，实际 %T", cred.Secret)
	}
}

// closeAggReader Close 时先做旁路收尾再关上游流。
type closeAggReader struct {
	io.ReadCloser
	closer func()
	once   sync.Once
}

func (r *closeAggReader) Close() error {
	var err error
	r.once.Do(func() {
		r.closer()
	})
	return err
}
