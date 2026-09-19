// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误
	// ErrContentBlocked 上游**内容策略**拦截（400 + 审核文案）→ 不罚账号。
	//
	// 追加在末尾（不插在中间）：ErrKind 的取值会被写进日志、也可能被按整数传递，
	// 插入会让所有既有取值的数字位移。有测试钉住（见 client_test.go 的
	// TestErrKindValuesAreStable）。
	//
	// 处置见 gateway.ErrKindContentBlocked：core 据此触发提示词降级重试，
	// 而**不是**换号 —— 换号对内容问题没有意义（每个号都被同一套策略拦）。
	ErrContentBlocked
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrContentBlocked:
		return "content_blocked"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// contentBlockedMarkers 上游**内容策略**拦截的文案标记（借鉴 workbuddy2api-panel）。
//
// # 为什么必须与 ErrClient 分开判
//
// 这类拦截返回的是 HTTP 400，形状与"客户端参数写错"完全一样。
// 若不单独识别，它会落到 ErrClient → core 反复换号重试：
//
//	换号对内容问题**没有意义** —— 每个账号背后是同一套内容策略，
//	换 3 个号就是白跑 3 次往返，最后返回"所有账号不可用"，
//	把一个内容问题误报成账号池故障。
//
// 单独识别之后，core 的动作变成"换提示词重试"（见 internal/prompt 的降级机制），
// 并且**不罚账号**（内容问题不是账号问题）。
//
// # 三个标记的来源
//
//	blocked by security policy  安全策略拦截
//	unapproved channel          未授权通道
//	illegal api invocation      非法 API 调用
//
// 三者都是上游在"识别到不该出现的指纹/来源"时给出的措辞，
// 与另一条 `code 11128`（裸数字反探测）是同一个拦截族的两种表现形态：
// 11128 是"请求体里有那串数字"，这三个是"请求体里有那些模板句"。
//
// ⚠ 大小写：上游的文案大小写不完全稳定，因此比较统一走 ToLower。
// 这是**上游自己的词汇表**，所以它必须留在本包（见 gateway 包注释的判据 1）——
// 放进 gateway 的通用兜底就等于让每个上游被这套词汇解释。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	// 内容策略拦截：必须在 `status >= 400 → ErrClient` 之前判。
	//
	// 它的 HTTP 形态与"客户端参数写错"完全一样（400），
	// 落到兜底就会被当成账号问题换号重试 —— 而内容问题换号没有意义。
	//
	// # 为什么 `11128` 刻意**不**归到这一类
	//
	// 11128 是上游的裸数字反探测，触发条件不止一种（见 sanitize.go 与
	// payload.go 的 normalizeRoles 注释：role 白名单违规也会回 11128）。
	// 把它一起归到"内容拦截"会掩盖真正的 role/参数问题 ——
	// 那些问题换号**确实**可能解决（不同账号的上游版本/策略可能不同）。
	//
	// 已知的 11128 触发源（裸数字、developer 角色）已分别由
	// sanitizeRewrites 与 normalizeRoles 在出站前消除，
	// 因此留它在 ErrClient 是安全的保守选择。
	lowerBody := strings.ToLower(body)
	for _, m := range contentBlockedMarkers {
		if strings.Contains(lowerBody, m) {
			return ErrContentBlocked
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	effortsMu sync.RWMutex
	efforts   map[string][]string

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	// PromptMode 系统提示词模式：prompt.ModeCustom（默认）/ prompt.ModePassthrough。
	//
	// 空串按 custom 处理（与配置缺省一致）：从源头消灭 system/developer 来源的
	// 指纹误报。见 internal/prompt 包注释里两层防护的分工。
	PromptMode string
	// PromptText 生效的系统提示词文本（custom 模式下替换客户端 system/developer）。
	//
	// 空串时回落 prompt.Default()，而不是"什么都不做" ——
	// 后者会让"装配漏了注入"变成一个完全静默的功能失效。
	PromptText string
	// PromptGate 内容拦截降级状态机（nil = 永不降级，恒用 PromptText）。
	//
	// 由 core 在观察到内容拦截时 Trigger()；本客户端在每次出站前读它，
	// 处于降级期则改用 prompt.Degraded 中性提示词。
	PromptGate *prompt.Gate

	// ── 出站身份（全部 opt-in，缺省与改造前逐字节一致）─────────────────
	//
	// 详见 headers.go 的包级注释：这些字段让出站请求头可以按部署环境
	// 对齐官方客户端形态，而不必改代码。**全部为空/false 时行为与改造前相同。**

	// UserAgent 出站 UA 的**逐字**覆盖（最高优先）。
	// 空 = 按 ClientVersion 决定（见 userAgent）。
	UserAgent string
	// ClientVersion WorkBuddy 客户端版本段（如 "5.5.4"）。
	//
	// ⚠ 非空会让聊天/billing 出站 UA 从 `CLI/2.63.2 CodeBuddy/2.63.2`
	// 切换成**桌面端三段式** `WorkBuddy/<v> WorkBuddy/<v> CLI/<cli>`。
	// 空 = 保持 CLI 形态（改造前行为）。
	ClientVersion string
	// CliVersion 三段式 UA 里的 CLI 段；空 = defaultCliVersion（2.137.1）。
	CliVersion string
	// ClientName 用量归属名（X-IDE-Name / X-IDE-Type / X-Product / X-Agent-Purpose）。
	//
	// 官方面板「使用端」列读它。空 = 只设 X-Product: "SaaS"（改造前行为）。
	// 配 "WorkBuddy" 即对齐官方桌面端身份。
	ClientName string
	// DeviceToken 全局设备风控令牌（X-Device-Token）。空 = 不使用全局值。
	DeviceToken string
	// DeviceTokenFile 设备令牌文件（带 5 分钟 TTL 缓存，见 device_token.go）。
	// 优先级低于 DeviceToken 与每号 auth.DeviceToken。
	DeviceTokenFile string
	// PassthroughIP 是否把入站客户端 IP 透传给上游（X-Forwarded-For 等三头）。
	//
	// 默认 false：XFF 是客户端可伪造的头，是否可信取决于部署链路。
	// 直连公网的部署**不应**打开它。
	PassthroughIP bool

	ChatBaseCN    string
	BillingBaseCN string

	// 渠道 base：海外版（channel=intl，WorkBuddy AI）走 ChatBaseIntl/BillingBaseIntl/WebBaseIntl。
	// 每个账号按凭证里的 channel 字段路由（auth.DeriveChannel 兜底），
	// 因此同一个网关进程可以同时服务 CN 与国际版账号池。
	ChatBaseIntl    string
	BillingBaseIntl string

	// WebBaseCN 官网（workbuddy.cn）域。
	//
	// # 为什么需要第三个基址（1:1 移植自 workbuddy2api-panel）
	//
	// 上游的端点分布在**三个**不同的域，用途各不相同：
	//
	//	copilot.tencent.com  聊天 / 模型 / 增长 / 专家市场 / 行为上报（桌面指纹）
	//	codebuddy.cn         签到 / 余额 / 活跃上报（CLI 指纹）
	//	workbuddy.cn         官网 Web 端行为上报 + 任务领奖
	//
	// Web 域的事件形状与另两个域**不同**：它是浏览器指纹
	// （os/osVersion/userAgent/machineId，带 x-client-platform: web），
	// 用于 Library_read 这类"页面行为"任务。发到错误域会被静默丢弃。
	WebBaseCN   string
	WebBaseIntl string
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
		WebBaseCN:            defaultWebBaseCN,
		ChatBaseIntl:         "https://www.workbuddy.ai",
		BillingBaseIntl:      "https://www.workbuddy.ai",
		WebBaseIntl:          "https://www.workbuddy.ai",
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

func (c *Client) chatBase(a *auth.Auth) string {
	if a != nil && a.Channel == auth.ChannelIntl {
		return c.ChatBaseIntl
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体。
//
// # 顺序是硬约束：先提示词替换，再协议改写与脱敏
//
//	applyPrompt        ← 删掉客户端的 system/developer，插一条我们自己的
//	PrepareBodyOpt...  ← 强制 stream / 归一 role 与 tool_choice / 注入 thinking /
//	                     sanitize 逐串擦指纹
//
// 先做提示词替换的理由：替换之后，**我们自己的那条 system 是干净的**
// （不含任何指纹），于是 sanitize 在它上面是零成本的空转，全部算力
// 都花在真正需要擦的 user/assistant/tool 消息上。
//
// 反过来的话，sanitize 要先在客户端的 system 上跑一遍正则，
// 随后那条消息又被整体删掉 —— 白做功，且顺序一乱就很容易让人以为
// "sanitize 负责 system"（它不负责，见 internal/prompt 包注释的分工表）。
func (c *Client) prepareBody(body []byte) []byte {
	body = c.applyPrompt(body)
	return PrepareBodyOptWithEfforts(body, c.SanitizeFingerprints, c.effortsSnapshot())
}

// applyPrompt 按 PromptMode 与降级状态改写请求体里的系统提示词。
//
// # 四种组合
//
//	mode=custom,      未降级 → 用 PromptText 替换 system/developer
//	mode=custom,      降级中 → 用 prompt.Degraded 替换（比自定义人格更"无特征"）
//	mode=passthrough, 未降级 → **原样返回**（保留客户端人格，一个字节都不动）
//	mode=passthrough, 降级中 → 用 prompt.Degraded 替换
//
// # 为什么降级对一个"透传"模式也能改写
//
// passthrough 的语义是"尊重客户端人格"，但**降级期的存在本身就说明**
// 那份人格刚刚撞了内容策略。此时继续透传 = 继续撞 400。
// 降级是用户通过配置**已经同意**的兜底路径（prompt 段落存在即代表同意），
// 而不是我们擅自改写：它只在"客户端人格已被上游拒绝"这个前提下生效。
//
// # custom 模式下 PromptText 为空时回落内置默认
//
// 见 Client.PromptText 的注释：让"装配漏了注入"表现为"功能仍按默认工作"，
// 而不是"功能完全不生效且无任何信号"。
func (c *Client) applyPrompt(body []byte) []byte {
	if c == nil {
		return body
	}
	mode := c.PromptMode
	if mode == "" {
		mode = prompt.ModeCustom // 配置缺省 = custom
	}
	degraded := c.PromptGate.Active()

	if mode == prompt.ModePassthrough && !degraded {
		return body
	}
	text := c.PromptText
	if text == "" {
		text = prompt.Default()
	}
	if degraded {
		text = prompt.Degraded
	}
	return prompt.Rewrite(body, text)
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot() map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	if len(c.efforts) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(c.efforts))
	for k, v := range c.efforts {
		cp[k] = v
	}
	return cp
}

func (c *Client) billingBase(a *auth.Auth) string {
	if a != nil && a.Channel == auth.ChannelIntl {
		return c.BillingBaseIntl
	}
	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖 + Web 行为上报用）。随账号渠道：intl → www.workbuddy.ai。
//
// 未注入时回落默认值 —— 与 B 同口径：测试里只注入 ChatBaseCN/BillingBaseCN
// 时不该让 Web 路径拿到空串（那会拼出 "/v2/report" 这种无 host 的 URL）。
func (c *Client) webBase(a *auth.Auth) string {
	if a != nil && a.Channel == auth.ChannelIntl {
		if c.WebBaseIntl != "" {
			return c.WebBaseIntl
		}
		return "https://www.workbuddy.ai"
	}
	if c != nil && c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return defaultWebBaseCN
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	c.RefreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamWithIP(a, body, "")
}

// ChatStreamWithIP 与 ChatStream 相同，但额外接受客户端 IP 用于透传。
//
// # 为什么是"新增一个带 IP 的入口"而不是给 ChatStream 加参数
//
// ChatStream 被大量测试直接调用（构造假上游断言出站行为）。
// 给它加第三个参数会让每个调用点都要改，而它们**都不关心** IP。
// 新增入口把改动收敛到真正需要透传的那两个调用方
// （workbuddy.Provider.Chat 与 server.chatVia 的单上游回落分支）。
//
// clientIP 为空 或 PassthroughIP=false 时不注入任何 IP 头 ——
// 这两种情况的行为与改造前逐字节一致。
//
// ⚠ 它**仍然不接受 ctx**（内部自建 context.WithCancel）：那是既有实现的事实，
// 不在本次改造范围内。需要取消传播的调用方走 gateway.Provider.Chat(ctx, ...)，
// 那里有显式的 ctx.Err() 前置检查（见 workbuddy/provider.go）。
func (c *Client) ChatStreamWithIP(a *auth.Auth, body []byte, clientIP string) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(c.prepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a, clientIP)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
	// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
	// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
}

// FetchModels 调上游动态模型接口。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
//
// # 路径随渠道（实测海外版与国内版不同）
//
//	CN（copilot.tencent.com）  /console/enterprises/personal/models
//	Intl（www.workbuddy.ai）   /v2/enterprises/personal/models
//
// 实测海外版走国内路径返回 500（APISIX 网关），`/v2/` 前缀路径返回 200。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	modelsPath := "/console/enterprises/personal/models"
	if a != nil && a.Channel == auth.ChannelIntl {
		modelsPath = "/v2/enterprises/personal/models"
	}
	url := c.chatBase(a) + modelsPath
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]struct {
		ID              string
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
		Efforts         []string
	}, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = struct {
			ID              string
			Name            string
			MaxInputTokens  int64
			MaxOutputTokens int64
			Disabled        bool
			Efforts         []string
		}{m.ID, m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled, m.Reasoning.SupportedEfforts}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Efforts,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 合并 /v3/config 里多出的模型（模型列表全面化）。
	//
	// # 为什么需要（实测：主列表不全面）
	//
	// 主列表（/console 或 /v2/enterprises/personal/models 的 cli agent 集合）
	// 对海外版只给 18 个 —— 而 /v3/config（模型目录/倍率表）有 21 个，
	// 多出 deepseek-v4.1-flash / deepseek-v4.1-flash-sg / gpt-6-astra /
	// kimi-k2.8-preview。这些模型**真实可用**（实测直接调用上游正常响应），
	// 只是不在 cli agent 的挂载集合里。合并后 /v1/models 列表全面，
	// 用户在界面上能选到 v4.1 等模型。
	//
	// 国内版同样受益：CN 主列表 16 个 vs /v3/config 更多模型。
	//
	// /v3/config 拉取失败不阻断（主列表照常返回）—— 它是锦上添花。
	if cat, cerr := c.FetchModelCatalog(a); cerr == nil && cat != nil && len(cat.Models) > 0 {
		seen := make(map[string]bool, len(out))
		for _, mi := range out {
			seen[mi.ID] = true
		}
		for _, ce := range cat.Models {
			if ce.ID == "" || seen[ce.ID] {
				continue
			}
			seen[ce.ID] = true
			// 主列表缺这些模型的元信息（ContextWindow/MaxTokens/Efforts 未知），
			// 只给 ID/Name —— 名称正确、可调用、倍率从目录表来，足够用户选择。
			out = append(out, ModelInfo{ID: ce.ID, Name: ce.ID})
		}
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortsMu.Lock()
	c.efforts = cache
	c.effortsMu.Unlock()
	return out, nil
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	url := c.billingBase(a) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	c.BillingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	url := c.billingBase(a) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	c.BillingHeaders(req, a)
	_, err = c.doJSON(req)
	return err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ModelQuota 是单个模型的额度状态。
//
// 定义在 upstream 而非 server：本包是"对外词汇表"，
// server 与各上游都依赖它。若定义在 server，各上游就得 import server
// 才能实现 Backend.QuotaStates —— 那会成环。
//
// 语义：Exhausted=true 表示**已确认**该模型当前因额度不足不可用。
// 未确认的模型不应出现在结果里（宁可不标记，也不要让客户端
// 因为"可能不可用"而避开一个实际能用的模型）。
//
// 为什么这个类型是**上游无关**的（因此放在共用层而非某个上游包里）：
// "某模型当前额度耗尽"是跨上游都成立的展示语义，与哪家上游无关。
// CodeBuddy 恒返回空表（它没有"按模型独立耗尽"这个机制），
// CodeArts 按模型探测后填充 —— 两者共用同一个类型，消费方无需分辨来源。
type ModelQuota struct {
	Exhausted bool      `json:"exhausted"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}
