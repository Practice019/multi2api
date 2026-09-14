// client.go CodeArts 上游客户端：聊天 SSE / 凭证续期 / 模型目录。
//
// 与 workbuddy2api 的 upstream.Client 对应的能力：
//
//	ChatStream   -> POST /api/v2/chat/completions  (OpenAI 形态 SSE)
//	RefreshToken -> POST /v1/oauth2/tokens         (DPoP + refresh_token)
//	FetchModels  -> 无独立端点，从本地配置读取（见 models.go）
//
// 主要差异：
//   - 鉴权用 SDK-HMAC-SHA256 签名（非 Bearer）
//   - 上游响应**本身就是 OpenAI 形态**，因此无需 Aggregate() 聚合
//   - 续期需要 DPoP 私钥
package codearts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/upstream"
)

// 上游域名。与 product.json 的 commercialVersionDomain.newFramework.productDomain 对应。
const (
	// DefaultEngineBase 是 model 调用（inferhub）基址。
	// 内核日志实证：inferhubBaseUrl = https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/
	DefaultEngineBase = "https://snap-access.cn-north-4.myhuaweicloud.com"
	// DefaultSTSBase 是 OAuth token 端点基址。
	DefaultSTSBase = "https://sts.cn-north-4.myhuaweicloud.com"

	// ChatPath 是 OpenAI 兼容聊天路径。
	ChatPath = "/api/v2/chat/completions"
	// TokenPath 是 OAuth token 端点（authorization_code / refresh_token 共用）。
	TokenPath = "/v1/oauth2/tokens"
	// CallerIdentityPath 用于校验凭证是否有效（等价于"探活"）。
	CallerIdentityPath = "/v5/caller-identity"
	// HeartbeatPath 是会话心跳端点。
	//
	// 为什么必须调它：服务端按账号限制**并发会话数**（实测上限 3）。
	// 每次 POST /api/v2/chat/completions 会占一个会话槽，
	// 若不在结束时置回 idle，第 4 次请求起就会持续收到
	//
	//	400 TM.00001041 并发会话数已达上限(3个)
	//
	// 错误信息指向"会话"而非"模型"，排查时极易误判成模型不可用。
	// IDE 的做法是在每轮对话前后分别置 busy/idle（AgentKernel 的
	// ChatSessionHeartbeat 服务），本实现照做。
	HeartbeatPath = "/snap-manager/v1/chat-session/heartbeat"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
//
// 与 workbuddy 的语义保持一致，便于 pool 复用同一套冷却策略。
type ErrKind int

const (
	ErrNone       ErrKind = iota
	ErrHardCredit         // 额度/余额不足 -> 长冷却
	ErrSoftRate           // 限流 -> 短冷却
	ErrAuth               // 401/凭证失效 -> 触发续期
	ErrNotFound           // 404 上游偶发 -> 短冷却，不累计错误
	ErrServer             // 5xx
	ErrClient             // 其他 4xx
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrAuth:
		return "auth"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
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
	return fmt.Sprintf("codearts upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 额度不足关键词（CodeArts 的免费额度提示）。
var hardMarkers = []string{
	"insufficient", "quota exceeded", "quota exhaust", "no credit",
	"out of credit", "balance not enough", "payment required",
	"额度不足", "余额不足", "积分不足", "额度用尽", "配额不足",
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
	switch {
	case status == http.StatusUnauthorized:
		return ErrAuth
	case status == http.StatusForbidden:
		// 403 常见于 securityToken 过期
		return ErrAuth
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusNotFound:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	return ErrNone
}

// Client 是 CodeArts 上游客户端。
type Client struct {
	HTTP     *http.Client // 短 RPC
	ChatHTTP *http.Client // 聊天 SSE（无总超时）

	// HeaderTimeout 聊天首字节（响应头）上限。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天流空闲上限。
	IdleTimeout time.Duration

	EngineBase string // 默认 DefaultEngineBase
	STSBase    string // 默认 DefaultSTSBase

	// ClientID 是 OAuth client_id，来自扩展名。
	ClientID string

	// SanitizeFingerprints 出站请求体指纹脱敏开关。
	// 与 upstream.Client 同名字段语义一致，默认 true。
	SanitizeFingerprints bool

	// MaxAuthRetry 凭证失效时的重试次数（触发 RefreshToken 后重试）。
	MaxAuthRetry int
}

// New 构造客户端（默认域名与连接池）。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr},
		HeaderTimeout:        120 * time.Second,
		EngineBase:           DefaultEngineBase,
		STSBase:              DefaultSTSBase,
		ClientID:             "vscode-codebot",
		SanitizeFingerprints: true,
		MaxAuthRetry:         1,
	}
}

func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

func (c *Client) engineBase() string {
	if c.EngineBase == "" {
		return DefaultEngineBase
	}
	return strings.TrimRight(c.EngineBase, "/")
}

func (c *Client) stsBase() string {
	if c.STSBase == "" {
		return DefaultSTSBase
	}
	return strings.TrimRight(c.STSBase, "/")
}

// signedRequest 构造一个已签名的请求。
//
// 注意 Content-Type：JSON 走 application/json，表单走 x-www-form-urlencoded。
// 两者都参与签名（作为 SignedHeaders 的一部分）。
func (c *Client) signedRequest(ctx context.Context, method, rawURL, body, contentType string, a *Auth, extra map[string]string) (*http.Request, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}

	headers := map[string]string{}
	for k, v := range extra {
		headers[k] = v
	}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}

	signed, err := Sign(a.Cred(), method, rawURL, body, headers)
	if err != nil {
		return nil, err
	}
	for k, v := range signed {
		req.Header.Set(k, v)
	}
	if body != "" {
		req.ContentLength = int64(len(body))
	}
	// 调试转储：把**发往上游的最终报文**落盘（含签名头）。
	//
	// 为什么需要它：排查"上游 400 参数非法"这类问题时，
	// 光看客户端发来什么是不够的 —— 网关会改写请求体
	// （强制 stream、归一 tool_choice、裁剪 max_tokens…），
	// 真正该比对的是**改写后实际发出去的那一份**。
	// 只猜字段会绕远路：直接看报文，差异一目了然。
	if os.Getenv("DSH_DEBUG_DUMP") == "1" && strings.HasSuffix(rawURL, ChatPath) {
		dumpUpstream(method, rawURL, body, headers)
	}
	return req, nil
}

// dumpUpstream 把发往上游的请求落盘（仅调试用）。
func dumpUpstream(method, url, body string, headers map[string]string) {
	dir := "./data/dump-upstream"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "=== %s %s ===\n", method, url)
	for k, v := range headers {
		// 签名/令牌类不打全文，避免泄漏
		if strings.EqualFold(k, "Authorization") || strings.Contains(strings.ToLower(k), "token") {
			fmt.Fprintf(&b, "%s: <redacted len=%d>\n", k, len(v))
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	b.WriteString("=== BODY ===\n")
	b.WriteString(body)
	name := filepath.Join(dir, time.Now().Format("150405.000")+".txt")
	_ = os.WriteFile(name, []byte(b.String()), 0o644)
}

// modelOf 从请求体里取 model 字段。
//
// 解析失败返回空串 —— 调用方（ChannelFor）会回落到默认通道，
// 而后续请求本身会由上游返回明确的参数错误，不需要在这里拦。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// Heartbeat 上报会话状态（busy / idle）。
//
// 服务端按账号限制并发会话数（实测 3）。一次 chat 请求会占用一个会话槽，
// 请求结束后必须置回 idle 才会释放；否则第 4 次起会一直收到
// `400 TM.00001041 并发会话数已达上限(3个)`。
//
// 两个必需细节（均从 IDE 的实际请求抓包得到，缺一即 400）：
//   - **必须带 `user-session-id` 头**：会话是按该 id 计数的，
//     不带头服务端无法定位要释放哪个会话；
//   - **body 必须是 `{}`**（2 字节），空 body 会被拒。
//
// 失败不影响主流程：这只是配额管理，报错时记日志即可 ——
// 把心跳失败升级成请求失败会让"能出内容但配额没释放"变成"完全不可用"。
func (c *Client) Heartbeat(a *Auth, status, sessionID string) error {
	if status != "busy" && status != "idle" {
		return fmt.Errorf("heartbeat: 非法 status %q（只接受 busy/idle）", status)
	}
	if sessionID == "" {
		return fmt.Errorf("heartbeat: 缺 sessionID（服务端按会话计数，必须有）")
	}
	rawURL := c.engineBase() + HeartbeatPath + "?status=" + status
	extra := map[string]string{
		"user-session-id": sessionID,
		"x-client-type":   "kernel",
	}
	// body 固定 "{}"，与 IDE 一致
	req, err := c.signedRequest(context.Background(), http.MethodPut, rawURL, "{}", "application/json", a, extra)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("heartbeat %s: http %d %s", status, resp.StatusCode, truncate(string(raw), 120))
	}
	return nil
}

// NewSessionID 生成一个会话 id（形如 ses_<32hex>，与 IDE 的格式一致）。
func NewSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 退化到时间戳，宁可 id 不完美也不要让请求直接失败
		return fmt.Sprintf("ses_%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("ses_%x", b)
}

// ChatStream 发聊天请求并返回原始 SSE 响应体（调用方负责 Close）。
//
// 返回值语义与 workbuddy 的 upstream.ChatStream 对齐：
//   - 非 2xx：rc 为 nil，respBody 为上游响应体（供 Classify），err 为 nil
//
// ChatStream 发聊天请求，并按模型自动选择服务通道。
//
// 通道选择是必须的：7 个模型分属两个**互斥**通道，
// 走错会报 `InferHub.002002009 The model is not registered`
// （看起来像账号未开通，极易误判）：
//
//	默认通道：GLM-5.2 / glm-5.2-sft-harmony /
//	          openpangu-2.0-pro / openpangu-2.0-flash
//	benefit ：deepseek-v4-flash-0731 / deepseek-v4-pro-0813 / glm-5.3-flash
//
// 反向也成立：给默认通道的模型加 maas_type=benefit 会报
// `InferHub.4005.200 unsupported model`。
func (c *Client) ChatStream(a *Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamWith(a, body, nil)
}

// ChatStreamWith 与 ChatStream 相同，但允许附加请求头（会叠加在通道头之上）。
//
// 调用方通常不需要传 extraHeaders —— 通道头会按请求体里的 model 字段自动选择。
func (c *Client) ChatStreamWith(a *Auth, body []byte, extraHeaders map[string]string) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.engineBase() + ChatPath

	// 与 upstream 路径一致：出站前统一改写请求体。
	//
	// 为什么复用 upstream.PrepareBody 而不是本层重做：
	//   1. 它强制 stream:true —— CodeArts 上游同样只接受流式；
	//   2. 它按开关清洗出站请求体里的指纹字段。
	// 漏掉这一步会让「客户端要非流式」的请求原样打到上游被参数校验拒绝，
	// 而错误信息只会显示成上游参数错，很难定位到是网关少做了一次改写。
	// 注意这里传入 MaxTokensTable()：客户端（尤其 DSH）会带远超模型上限的
	// max_tokens（实测 DSH 发 384000，而 deepseek 上限仅 65536），
	// 上游对超限直接 400，且该错误会被误判成账号故障。
	// 传 limits 让改写层把超限值裁剪到上限。
	outBody := upstream.PrepareBodyOptWithLimits(body, c.SanitizeFingerprints, nil, MaxTokensTable())

	// 按模型挑通道。注意用 outBody 而非 body ——
	// 模型名以改写后的请求体为准（改写不会动 model 字段，但保持一致更稳妥）。
	channelHeaders := ChannelFor(modelOf(outBody)).Header()

	// 每次请求分配一个独立会话 id，并把它同时用于：
	//   1. heartbeat（占用/释放会话槽）
	//   2. chat 自身的 user-session-id / x-ot-session-id 头
	//
	// 两者必须是**同一个 id**。否则服务端会给 chat 另开一个会话，
	// heartbeat 释放的是我们自己编的那个空会话，真正占用的那个永不释放；
	// 症状是"每账号只能成功 3 次（并发上限），之后恒 400 TM.00001041"，
	// 而错误只说并发满，看不出是 id 没对上。
	sessionID := NewSessionID()
	extra := map[string]string{
		"user-session-id": sessionID,
		"x-ot-session-id": sessionID,
		"x-client-type":   "kernel",
	}
	// 通道头必须在签名前并入 —— 它们参与 SignedHeaders，
	// 签名后才加会导致签名与实际发送的头不一致而 401。
	for k, v := range channelHeaders {
		extra[k] = v
	}
	for k, v := range extraHeaders {
		extra[k] = v
	}

	// 占用一个会话槽。失败不阻断（上报配额而已）。
	if herr := c.Heartbeat(a, "busy", sessionID); herr != nil {
		log.Printf("codearts: heartbeat busy 失败（继续）: %v", herr)
	}

	// 无论走哪条返回路径，都要把会话槽还回去 ——
	// 否则连续 3 次请求后账号就会卡在 TM.00001041。
	release := func() {
		if herr := c.Heartbeat(a, "idle", sessionID); herr != nil {
			log.Printf("codearts: heartbeat idle 失败: %v", herr)
		}
	}

	for attempt := 0; ; attempt++ {
		req, rerr := c.signedRequest(context.Background(), http.MethodPost, url,
			string(outBody), "application/json", a, extra)
		if rerr != nil {
			release()
			return nil, 0, nil, rerr
		}
		resp, derr := c.chatHTTP().Do(req)
		if derr != nil {
			release()
			return nil, 0, nil, derr
		}

		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			kind := Classify(resp.StatusCode, string(raw))

			// 凭证失效：自动续期后重试一次（STS 只有约 2 小时，这是常见路径而非异常）。
			if kind == ErrAuth && attempt < c.MaxAuthRetry {
				if rerr := c.RefreshToken(a); rerr == nil {
					log.Printf("codearts: 凭证失效已续期，重试 chat (uid=%s)", a.UID)
					continue
				} else {
					log.Printf("codearts: 凭证续期失败: %v", rerr)
				}
			}
			log.Printf("codearts: chat upstream %d %s body=%s",
				resp.StatusCode, kind, truncate(string(raw), 200))
			release()
			return nil, resp.StatusCode, raw, nil
		}

		// 成功：把 idle 上报挂在 body 关闭时。
		//
		// 为什么不在 return 前直接调：此时流还没读完，
		// 提前标 idle 会让服务端认为会话空闲并可能回收它。
		// 用包装的 ReadCloser 保证"流真正结束才释放"。
		return &heartbeatReleasingBody{ReadCloser: resp.Body, release: release},
			resp.StatusCode, nil, nil
	}
}

// RefreshToken 用 refresh_token grant 换取新凭证（DPoP 签名）。
//
// 成功后原地更新 a 的凭证字段并原子写回磁盘。
//
// 全程持 Auth.refreshMu —— 这是防"双消费"的关键：
// refresh_token 是一次性的，后台调度器与请求路径若同时进来，
// 两边会拿同一个 token 去换，必然一成功一失败，且失败方可能覆盖掉成功结果。
// 锁必须在**读取 a.RefreshToken 之前**拿到（下面第一件事就是拿锁），
// 否则读到的仍是旧值，锁就白加了。
func (c *Client) RefreshToken(a *Auth) error {
	a.LockRefresh()
	defer a.UnlockRefresh()

	if a.RefreshToken == "" {
		return fmt.Errorf("refresh_failed: 无 refresh_token（需重新登录）")
	}
	if len(a.DPoPPrivateKeyJWK) == 0 {
		return fmt.Errorf("refresh_failed: 缺 DPoP 私钥（无法签名，需重新登录）")
	}
	kp, err := FromPrivateJWK(a.DPoPPrivateKeyJWK)
	if err != nil {
		return fmt.Errorf("refresh_failed: 恢复 DPoP 密钥: %w", err)
	}

	status, raw, err := c.refreshAttempt(a, kp)

	// ── 窄自愈：只在"这个 refresh_token 已被服务端消费"时读盘、重试一次 ──
	//
	// 为什么需要：refresh_token 是一次性的，而"内存里那份已被消费、
	// 磁盘上有一份更新的未用 token"是**已知会发生的状态**
	// （内存更新与写盘之间存在窗口，另见 Task 007）。修复前这里只会把
	// 错误原样返回，只能靠重启网关恢复。
	//
	// 判据为什么必须窄：client_id 错、DPoP 证明无效这类 400 与 token 本身
	// 无关 —— 磁盘上那份 token 一样过不去，读盘重试只会把一次失败放大成
	// 两次请求。所以只认服务端明确说"已被用过"这一种体（实测原文是
	// error_code=STS5.1806 与 error_msg 里的 'has been used'）。
	if err != nil && status >= 400 && isRefreshTokenConsumed(raw) {
		if c.adoptDiskRefreshToken(a) {
			log.Printf("codearts: 内存凭证已被消费，采用磁盘上更新的 refresh_token 重试 (uid=%s)", a.UID)
			// 只重试一次：不用递归、不进循环。
			_, rawRetry, retryErr := c.refreshAttempt(a, kp)
			if retryErr != nil {
				return retryErr
			}
			raw, err = rawRetry, nil
		}
	}
	if err != nil {
		// 磁盘读不出来 / token 与内存相同 / 重试仍失败 → 原样返回
		return err
	}

	var tok struct {
		Credentials *struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("refresh_failed: 解析响应: %w", err)
	}
	if tok.Credentials == nil || tok.Credentials.AccessKeyID == "" {
		return fmt.Errorf("refresh_failed: 响应缺 credentials: %s", truncate(string(raw), 300))
	}

	a.Lock()
	a.AccessKey = tok.Credentials.AccessKeyID
	a.SecretKey = tok.Credentials.SecretAccessKey
	a.SecurityToken = tok.Credentials.SecurityToken
	if tok.Credentials.Expiration != "" {
		if ts, perr := time.Parse(time.RFC3339, tok.Credentials.Expiration); perr == nil {
			a.ExpiresAt = ts.Unix()
		}
	}
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	a.Unlock()

	// 落盘采用"先备份、后覆盖、再清备份"两阶段。
	//
	// 为什么不能像以前那样"失败了只记条日志就算了"：
	// refresh_token 是**消费型**的 —— 上面这次请求已经让服务端把旧 token 作废
	// （实测 STS5.1806 'the refresh token has been used'）。
	// 若此刻写盘失败，内存里是新凭证、磁盘上还是已作废的旧凭证，
	// 进程一重启就永久失去这个账号 —— 只能重新走浏览器登录。
	//
	// 两阶段保证：写盘失败时**磁盘保持原样**（宁可这次没续上，也不写坏），
	// 且留下备份让下次启动能明确报告"上次续期停在哪一步"。
	if a.FilePath != "" {
		bakDir := BackupDir(filepath.Dir(a.FilePath))
		if err := a.BackupTo(bakDir); err != nil {
			// 备份失败不改内存状态？不 —— 服务端已消费旧 token，
			// 此时回退内存反而更糟（内存与磁盘都对不上服务端）。
			// 因此继续走保存，只是把备份失败记下来（少了诊断依据而已）。
			log.Printf("codearts: 续期前备份失败 (uid=%s): %v（继续保存）", a.UID, err)
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("codearts: 续期后写回失败 (uid=%s): %v —— "+
				"磁盘仍是已作废的旧凭证，请重跑 cmd/login 重新登录", a.UID, err)
			return fmt.Errorf("refresh 成功但写回失败（旧 refresh_token 已被服务端消费，需重新登录）: %w", err)
		}
		if err := a.ClearBackup(bakDir); err != nil {
			// 清不掉备份不影响正确性，只是下次启动会多一条 WARN
			log.Printf("codearts: 清理续期备份失败 (uid=%s): %v", a.UID, err)
		}
	}
	return nil
}

// refreshAttempt 发**一次** refresh_token grant 请求，返回状态码与响应体。
//
// 抽成独立方法只为一件事：让"已被消费 → 读盘 → 重试一次"不必把
// 请求构造 + DPoP 签名整段复制一遍（复制出来的第二份迟早会与第一份漂移）。
//
// 调用方必须已持有 a.refreshMu —— 它在这里直接读 a.RefreshToken。
func (c *Client) refreshAttempt(a *Auth, kp *DPoPKeyPair) (int, []byte, error) {
	tokenURL := c.stsBase() + TokenPath
	form := url.Values{}
	form.Set("client_id", c.clientID(a))
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", a.RefreshToken)
	body := form.Encode()

	proof, err := kp.DPoPProof(http.MethodPost, tokenURL)
	if err != nil {
		return 0, nil, fmt.Errorf("refresh_failed: 生成 DPoP 证明: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("refresh_failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return resp.StatusCode, raw,
			fmt.Errorf("refresh_failed: http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return resp.StatusCode, raw, nil
}

// isRefreshTokenConsumed 判断响应体是否明确表示"这个 refresh_token 已被用过"。
//
// 只认上游实测的两种原文：
//
//	error_code = STS5.1806
//	error_msg  = invalid refresh token: 'the refresh token has been used'
//
// 刻意不做宽泛匹配（例如任何含 "invalid refresh token" 的体）：
// 判据一宽，client_id / DPoP 类的 400 也会被当成"token 已消费"，
// 于是每次失败都多打一次请求，还会错误地采用磁盘凭证。
func isRefreshTokenConsumed(raw []byte) bool {
	s := string(raw)
	return strings.Contains(s, "STS5.1806") || strings.Contains(s, "has been used")
}

// adoptDiskRefreshToken 在"内存那份 token 已被消费"时，改采磁盘上那份更新的凭证。
//
// 返回 true 表示已采用（调用方可以重试一次）；false 表示无从自愈，
// 此时**不得**改动 a 的任何字段 —— 调用方应原样返回原错误。
//
// 为什么不无条件读盘比对：那会把"谁是权威"变成每次续期都要判一次，
// 并引入"磁盘比内存旧"的 TOCTOU —— 只有服务端已经明确作废内存这份时，
// 磁盘才必然更新，这个方向是单向的。
//
// 日志不打印 token / AK / SK（见 security-checklist）。
func (c *Client) adoptDiskRefreshToken(a *Auth) bool {
	if a.FilePath == "" {
		return false
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		log.Printf("codearts: 续期自愈读盘失败 (uid=%s): %v", a.UID, err)
		return false
	}
	disk, err := ParseCredential(raw)
	if err != nil {
		log.Printf("codearts: 续期自愈解析磁盘凭证失败 (uid=%s): %v", a.UID, err)
		return false
	}
	// 空 → 磁盘没有可用 token；相同 → 磁盘那份也是刚被消费的那份。
	if disk.RefreshToken == "" || disk.RefreshToken == a.RefreshToken {
		return false
	}

	a.Lock()
	a.RefreshToken = disk.RefreshToken
	a.AccessKey = disk.AccessKey
	a.SecretKey = disk.SecretKey
	a.SecurityToken = disk.SecurityToken
	a.ExpiresAt = disk.ExpiresAt
	a.Unlock()
	return true
}

func (c *Client) clientID(a *Auth) string {
	if a != nil && a.ClientID != "" {
		return a.ClientID
	}
	if c.ClientID != "" {
		return c.ClientID
	}
	return "vscode-codebot"
}

// SignedGet 发一个已签名的 GET，返回响应体与状态码。
//
// 供排查工具与未来的额外上游接口使用（与 ChatStream 共用同一套签名逻辑，
// 避免各处重复实现签名而漏掉 CanonicalURI 尾斜杠之类的坑）。
func SignedGet(c *Client, a *Auth, url string, extra map[string]string) (string, int, error) {
	return SignedCall(c, a, nil, http.MethodGet, url, "", extra)
}

// SignedCall 发一个已签名的任意方法请求（排查工具与扩展接口用）。
func SignedCall(c *Client, a *Auth, ctx context.Context, method, url, body string, extra map[string]string) (string, int, error) {
	req, err := SignedRequest(c, a, ctx, method, url, body, "application/json", extra)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(raw), resp.StatusCode, nil
}

// SignedRequest 构造已签名请求但不发送（需要自定义超时/客户端的调用方用）。
func SignedRequest(c *Client, a *Auth, ctx context.Context, method, url, body, contentType string, extra map[string]string) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.signedRequest(ctx, method, url, body, contentType, a, extra)
}

// Verify 调 caller-identity 校验凭证是否可用（等价于探活）。
func (c *Client) Verify(a *Auth) error {
	rawURL := c.stsBase() + CallerIdentityPath
	req, err := c.signedRequest(context.Background(), http.MethodGet, rawURL, "", "", a, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	return nil
}

// heartbeatReleasingBody 在 SSE 流被关闭时上报会话 idle。
//
// 用包装而不是 defer 的原因：ChatStream 返回的是**流**，
// 调用方读完才真正结束。在函数返回时释放会让服务端在流仍在传输时
// 就认为会话空闲，可能导致它被回收。
//
// release 有 sync.Once 保护，重复 Close 不会重复上报。
type heartbeatReleasingBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *heartbeatReleasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
