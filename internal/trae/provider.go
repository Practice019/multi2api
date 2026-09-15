// provider.go 把 TRAE SOLO（腾讯 TRAE 桌面端的 SOLO 免费对话通道）适配成 gateway.Provider。
//
// # 位置：本网关的第四个上游
//
//	workbuddy  CodeBuddy 桌面端
//	codearts   华为云 CodeArts
//	loomy      讯飞 Loomy
//	trae       TRAE SOLO（llm_utils_chat，function=solo_work_lite）
//
// 协议来自对多个 trae 反代项目的复现研究（traework2api / trae-api / trae-local-api /
// trae-solo-unlock），核心事实：
//
//	鉴权    Authorization: Cloud-IDE-JWT <accessToken>（对话侧还带 X-* 设备头）
//	凭证    accessToken(JWT) + refreshToken(消费型) + ExpiresAt
//	对话    POST {agent}/api/agent/v3/llm_utils_chat，自定义 SSE
//	模型    POST {agent}/api/ide/v1/get_detail_param（config_name 即模型 ID）
//	续期    POST {oauth}/cloudide/api/v3/trae/oauth/ExchangeToken（refreshToken 轮换）
//	签到    POST {ug}/trae/api/v2/ug/checkin_credits/{status,claim}
//	额度    POST {ug}/trae/api/v2/pay/ide_user_ent_usage（权益包 credits_limit 求和）
package trae

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// providerID 上游标识。
const providerID = "trae"

// ProviderID 导出上游标识，供装配层（cmd/server）使用。
const ProviderID = providerID

// Provider 实现 gateway.Provider（以及若干扩展点，见 extensions.go）。
type Provider struct {
	client *Client
	// authDir 凭证目录（AuthDirExt）。
	authDir string
	// refreshInterval 后台续期扫描间隔（<=0 不注册任务）。
	refreshInterval time.Duration
	// checkinEnabled 是否注册每日签到任务。
	checkinEnabled bool
	// callbackPort 本机 OAuth 回调端口（见 callback.go）。
	callbackPort string
	// log 任务历史落点（签到/续期结果写这里，「今日签到」列才有数据）。
	log *checkinlog.Log
	// fallbackEnabled / queueThreshold / maxAttempts 排队降级（见 tiers.go）。
	fallbackEnabled bool
	queueThreshold  int64
	maxAttempts     int

	// loginOnce / loginCached 缓存 LoginFlow 实例（见 login.go）。
	loginOnce   sync.Once
	loginCached *loginFlow
	// cbOnce / cbServer / cbErr 本机回调监听（见 callback.go）。
	cbOnce   sync.Once
	cbServer *http.Server
	cbErr    error
}

// Config Provider 的可选依赖，全部可缺省。
type Config struct {
	Client *Client
	// AuthDir 凭证目录（如 `auths/trae`）。
	AuthDir string
	// AgentBase / UgBase / OAuthBase 三个 host 覆盖（Client 为 nil 时生效）。
	AgentBase string
	UgBase    string
	OAuthBase string
	// RefreshInterval 后台续期扫描间隔；<=0 不注册任务。
	RefreshInterval time.Duration
	// CheckinEnabled 是否注册每日签到任务（默认 true）。
	CheckinEnabled bool
	// CallbackPort 本机 OAuth 回调端口（默认 18080）。
	CallbackPort string
	// Log 任务历史落点（签到结果写这里，「今日签到」列才有数据）。
	// 未注入则签到只执行不记历史（与 scheduler 的"静默丢弃"一致）。
	Log *checkinlog.Log
	// FallbackEnabled / QueueThreshold / MaxAttempts 排队检测自动降级（见 tiers.go）。
	FallbackEnabled bool
	QueueThreshold  int64
	MaxAttempts     int
}

// NewProvider 契约测试用的无依赖构造。
func NewProvider() gateway.Provider { return NewWithConfig(Config{}) }

// NewWithConfig 按配置建一个 TRAE Provider。
func NewWithConfig(cfg Config) *Provider {
	c := cfg.Client
	if c == nil {
		c = New()
		if cfg.AgentBase != "" {
			c.AgentHost = cfg.AgentBase
		}
		if cfg.UgBase != "" {
			c.UgHost = cfg.UgBase
		}
		if cfg.OAuthBase != "" {
			c.OAuthHost = cfg.OAuthBase
		}
	}
	checkin := cfg.CheckinEnabled
	return &Provider{
		client:          c,
		authDir:         cfg.AuthDir,
		refreshInterval: cfg.RefreshInterval,
		checkinEnabled:  checkin,
		callbackPort:    cfg.CallbackPort,
		log:             cfg.Log,
		fallbackEnabled: cfg.FallbackEnabled,
		queueThreshold:  cfg.QueueThreshold,
		maxAttempts:     cfg.MaxAttempts,
	}
}

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// TRAE SOLO 实际具备四项：
//
//	CapChat       对话（llm_utils_chat，流式/工具调用/思考链实测可用）
//	CapModels     模型目录（get_detail_param，config_name 即模型 ID）
//	CapCheckin    每日签到（checkin_credits status/claim，实测可领积分）
//	CapQuotaProbe 权益包额度（ide_user_ent_usage）
//
// 声明后三项会进 gateway.unverifiableCaps → 必须实现 AdminExt 且路由非空
// （本包有签到/额度/模型三条管理端点，见 admin.go）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels | gateway.CapCheckin | gateway.CapQuotaProbe
}

// Chat 转发一次对话请求。
//
// # 为什么返回的是"转换后的 OpenAI SSE"
//
// 上游返回自定义 SOLO SSE，而出口层只认 OpenAI SSE（见 sse.go 的文件头）。
// 所以这里把上游 body 包进一个管道：读上游 → 转 OpenAI chunk → 出口层读到标准形状。
//
// # 排队自动降级（用户本轮要求）
//
// 转换循环里监听 `request_wait_in_queue`：排队位置超过阈值、且还没产出
// 任何内容、且还有降级候选 → 放弃当前流、换同档/下一档模型重发（见
// tiers.go 的 fallbackChain）。客户端只看到一个连续的 OpenAI 流。
// 非 2xx 时 body 原样上交（供分类器用），不转换、不降级。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}
	rc, status, respBody, err := p.client.ChatStream(ctx, a, body)
	if err != nil {
		if len(respBody) > 0 {
			return gateway.ChatStream{Status: status, Body: io.NopCloser(strings.NewReader(string(respBody)))}, nil
		}
		return gateway.ChatStream{}, err
	}
	if status >= 400 {
		if rc != nil {
			_ = rc.Close()
		}
		return gateway.ChatStream{Status: status, Body: io.NopCloser(strings.NewReader(string(respBody)))}, nil
	}
	// 2xx：转换 SOLO SSE → OpenAI SSE（goroutine 内跑降级循环）。
	pr, pw := io.Pipe()
	go p.convertWithFallback(ctx, a, body, rc, pw)
	return gateway.ChatStream{Status: status, Body: pr}, nil
}

// convertWithFallback 转换循环 + 排队降级（在 Chat 的 goroutine 里跑）。
func (p *Provider) convertWithFallback(ctx context.Context, a *Auth, body []byte, rc io.ReadCloser, pw *io.PipeWriter) {
	maxAttempts := p.maxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	threshold := p.queueThreshold
	if threshold <= 0 {
		threshold = defaultQueueThreshold
	}
	chain := fallbackChain(modelOf(body), maxAttempts)
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		canRetry := p.fallbackEnabled && attempt+1 < len(chain)
		queuedPos, cerr := convertSOLOWithQueue(rc, pw, threshold, canRetry)
		if cerr != nil {
			_ = rc.Close()
			_ = pw.CloseWithError(cerr)
			return
		}
		if queuedPos <= 0 {
			// 正常结束（[DONE] 已写）。
			_ = rc.Close()
			_ = pw.Close()
			return
		}
		// 排队超阈值 → 换下一个候选模型重发。
		_ = rc.Close()
		attempt++
		next := chain[attempt]
		log.Printf("trae: %s 排队位置 %d 超过阈值 %d，降级到 %s", chain[attempt-1], queuedPos, threshold, next)
		nrc, nstatus, nresp, err := p.client.ChatStream(ctx, a, rewriteModelBody(body, next))
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if nstatus >= 400 {
			// 重试也非 2xx：客户端已拿到 200，只能写流内错误信封 + [DONE]。
			_ = nrc.Close()
			_ = writeOpenAIChunk(pw, map[string]any{
				"error": map[string]any{
					"code":    fmt.Sprintf("%d", nstatus),
					"message": truncate(string(nresp), 200),
				},
			})
			_, _ = io.WriteString(pw, "data: [DONE]\n\n")
			_ = pw.Close()
			return
		}
		rc = nrc
	}
}

// staticModels SOLO 免费模型静态快照（trae-solo-unlock 实测清单 + glm-5.2）。
//
// # 为什么需要静态兜底
//
// Models() 实时拉 get_detail_param，拉不到时（网络抖动/session 失效）若返回空，
// /v1/models 会整片消失、客户端判定"模型不存在"。静态快照保证目录不消失
// （与 loomy 的静态兜底同一条判据）。
var staticModels = []ModelInfo{
	{ID: "glm-5.2", Name: "GLM-5.2"},
	{ID: "glm-5.1", Name: "GLM-5.1"},
	{ID: "glm-5", Name: "GLM-5"},
	{ID: "glm-5v-turbo", Name: "GLM-5V Turbo"},
	{ID: "DeepSeek-V4-Pro", Name: "DeepSeek V4 Pro"},
	{ID: "DeepSeek-V4-Flash", Name: "DeepSeek V4 Flash"},
	{ID: "kimi-k2.6", Name: "Kimi K2.6"},
	{ID: "kimi-k2.5", Name: "Kimi K2.5"},
	{ID: "qwen-3.6-plus", Name: "通义千问 3.6 Plus"},
	{ID: "qwen-3.5", Name: "通义千问 3.5"},
	{ID: "Doubao_1_6", Name: "豆包 Seed Code"},
	{ID: "Doubao-Seed-2.0-Code", Name: "豆包 Seed 2.0"},
	{ID: "minimax-m2.7", Name: "MiniMax M2.7"},
	{ID: "minimax-m2.5", Name: "MiniMax M2.5"},
}

// Models 返回模型目录（实时优先、静态兜底）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	live, lerr := p.client.FetchModels(ctx, a)
	if lerr == nil && len(live) > 0 {
		return toModelInfos(live), nil
	}
	if lerr != nil {
		log.Printf("trae: 实时模型目录拉取失败，回落到静态快照: %v", lerr)
	}
	return toModelInfos(staticModels), nil
}

// ProbeHealth 探测一份凭证是否健康可用（gateway.HealthProbeExt，A2 移植）。
//
// 用 get_detail_param（模型目录）—— 便宜、真实带出鉴权头，401 即失败。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = p.client.FetchModels(ctx, a)
	return err
}

// toModelInfos 把本包 Model 投影成中立契约 gateway.ModelInfo。
func toModelInfos(ms []ModelInfo) []gateway.ModelInfo {
	out := make([]gateway.ModelInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, gateway.ModelInfo{ID: m.ID})
	}
	return out
}

// authOf 从 Credential 里取出 TRAE 的凭证结构。
//
// 只接受 `*trae.Auth`；断言失败返回明确错误，不 panic（契约要求）。
func authOf(cred gateway.Credential) (*Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("trae: 凭证为空（Credential.Secret 未设置）")
	}
	switch v := cred.Secret.(type) {
	case *Auth:
		if v == nil {
			return nil, errors.New("trae: 凭证是 nil 指针")
		}
		if strings.TrimSpace(v.AccessToken) == "" {
			return nil, errors.New("trae: 凭证缺少 accessToken（唯一鉴权材料）")
		}
		return v, nil
	default:
		return nil, fmt.Errorf("trae: 凭证类型不对，期望 *trae.Auth，实际 %T", cred.Secret)
	}
}
