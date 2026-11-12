// client.go PhanthyWork 的 HTTP 出口层：OAuth 续期 / 设备注册 /
// activities/summary 换推理令牌 / v1/messages 调模型 / 模型目录。
//
// # 分层（报告 §7.2 推崇的那个分层）
//
//	crypto.go  纯函数：三条 canonical、sha256、ed25519、时钟规整
//	client.go  这一层：**只管把请求发出去**。它不认识账户、不认识账号池，
//	           唯一的输入是一份 *Auth 与它自己报的偏斜
//	provider.go 接缝：把上面的东西拼成 gateway.Provider
//
// 于是"哪个请求要带哪几个头"这件事只有一处真相（本文件的 applyV1 /
// proofFor），而"签名算得对不对"能在 crypto_test.go 里离线逐字节断言。
//
// # 端点清单（全部来自报告，逐字）
//
//	GET  /api/oauth/profile                          取 account.uuid（签名要用的 accountScope）
//	POST /oauth/token                                OAuth 换 token 与续期（**两处编码不同**，见下）
//	POST /api/oauth/desktop-installations/register    设备注册（**写操作**，一次性）
//	POST /api/oauth/activities/summary                换推理令牌（TTL 180s）+ 返回 server_time
//	GET  /api/phanthy_code/models                    模型目录（只带 Bearer）
//	POST /v1/messages                                 调模型（12 段 proof 签名）
//
// # 两处最容易搞错的编码差异（照抄会错）
//
//	登录换 token（authorization_code）：**form-urlencoded**，20 秒超时，不重试
//	续期（refresh_token）：**JSON body**，8 秒超时，3 次重试（仅 429/5xx）
//
// 另外两个 expires_in 缺省值**故意不同**（1800 / 3600），照抄会错。
package phanthywork

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/gateway"
)

// Client PhanthyWork 的 HTTP 客户端。
//
// 三个可注入字段（测试用 httptest 指向假上游）：HTTP / APIBase / UA。
type Client struct {
	HTTP *http.Client
	// APIBase 上游基址（默认 DefaultAPIBase）。
	//
	// ⚠ 出站 URL 只能拼在它下面 —— 报告 §1.2 引开源实现的 origin.mjs：
	// 「一次拼装错误就把账号送给了别的主机，而这类错**不会报错**：
	//   请求成功、日志干净、用户只是从此收不到额度。」
	APIBase string
	// UA User-Agent。**必须是 Bun/1.3.14**（坑②），否则 403 TM.00000001。
	UA string
	// skewMS 本机与「服务端」的偏移（毫秒，本机 - 服务端）。
	//
	// ⚠ 只在**登录流程**里用（那时还没有账号对象）；拿到账号之后一律走
	// Auth.serverNowMS（**按账号隔离**）。两条路径共用同一个口径。
	skewMS atomic.Int64
}

// New 生产默认值。
func New() *Client {
	return &Client{
		// 不设总超时：SSE 流不能被"总时长"掐断 —— 逐请求设超时。
		HTTP:    &http.Client{Timeout: 0},
		APIBase: DefaultAPIBase,
		UA:      DefaultUA,
	}
}

// NewWithBase 测试构造：把基址指向假上游（UA 保持官方值 ——
// 坑② 的断言需要它）。
func NewWithBase(base string) *Client {
	c := New()
	c.APIBase = base
	return c
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// apiBase 生效的上游基址（去尾斜杠）。
func (c *Client) apiBase() string {
	if c != nil && strings.TrimSpace(c.APIBase) != "" {
		return strings.TrimRight(strings.TrimSpace(c.APIBase), "/")
	}
	return DefaultAPIBase
}

// userAgent 生效的 UA。
//
// ⚠ 空串也要回落到 DefaultUA：UA 缺失同样命中"第三方客户端"判据
// （Node 默认 UA 实测 403），所以"没配"不能等于"不发"。
func (c *Client) userAgent() string {
	if c != nil && strings.TrimSpace(c.UA) != "" {
		return strings.TrimSpace(c.UA)
	}
	return DefaultUA
}

// skew 当前客户端口径的偏移（登录流程用）。
func (c *Client) skew() int64 {
	if c == nil {
		return 0
	}
	return NormalizeClockSkewMS(c.skewMS.Load())
}

// setSkew 设定客户端偏移（由 server_time 校准）。
func (c *Client) setSkew(v int64) {
	if c == nil {
		return
	}
	c.skewMS.Store(NormalizeClockSkewMS(v))
}

// serverNowMS 客户端口径的服务端毫秒时间（登录流程用）。
func (c *Client) serverNowMS() int64 { return serverNowMS(nowMS(), c.skew()) }

// baselineSkewMS 给新建的凭证用的时钟基线。
func (c *Client) baselineSkewMS() int64 { return c.skew() }

// 两个 expires_in 兜底值**故意不同**（报告 §4.2 的对照表）：
//
//	登录换 token（performExchange）：服务端没给时用 1800
//	续期（refresh_token）：服务端没给时用 3600
//
// 来自官方 sidecar 的逐字反编译 —— 照抄任一侧的兜底值都会错：
// 续期那条用的是 3600，而登录那条是 1800（刷出来的 token 通常更长）。
const (
	// exchangeDefaultExpiresIn 登录换 token 的兜底有效期（秒）。
	exchangeDefaultExpiresIn = 1800
	// refreshDefaultExpiresIn 续期的兜底有效期（秒）。
	refreshDefaultExpiresIn = 3600
)

// ── 通用请求 ─────────────────────────────────────────────────────────────

// doJSON 发一次请求并把响应读到内存，返回 (状态码, 响应体)。
//
// 非 2xx **不当作传输错误**：调用方要拿 (status, body) 去做分类与错误文案
// （与 gateway.Provider.Chat 的语义一致）。
func (c *Client) doJSON(req *http.Request, timeout time.Duration) (int, []byte, error) {
	ctx := req.Context()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	resp, err := c.httpClient().Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// 16 MiB 上限（与上游的 body 上限同量级，够任何 JSON 响应）。
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, body, nil
}

// applyCommonHeaders 注入每个上游请求都要带的头。
//
//	user-agent: Bun/1.3.14          —— 坑②（否则 403 TM.00000001「网络繁忙」）
//	x-phanthy-client-source: desktop —— 同一条判据的另一半（实测 200 的形态）
func (c *Client) applyCommonHeaders(req *http.Request) {
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("x-phanthy-client-source", clientSource)
	req.Header.Set("Accept", "application/json")
}

// newJSONRequest 造一个带 JSON body 的请求。
//
// body 必须是**已经序列化好的字节**（见 crypto.go 的 RequestBodyForHash）——
// 这里绝不重新序列化，那是坑⑧ 的全部内容。
func (c *Client) newJSONRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyCommonHeaders(req)
	return req, nil
}

// ── v1 签名（x-desktop-* 那 5 个头）───────────────────────────────────────

// applyV1 给请求挂上 v1 签名头（identity 私钥）。
//
// 头部形状（报告 §2.4 的 8 个头，去掉 content-type/accept/authorization）：
//
//	x-desktop-installation-id  di_…（**本地派生的那个**）
//	x-desktop-timestamp        与 canonical 里同一个值（毫秒字符串）
//	x-desktop-nonce            与 canonical 里同一个值
//	x-desktop-signature        identity 私钥签的 v1 canonical
//	idempotency-key            只在**有**的时候发（见 params.idempotencyKey）
//
// ⚠ 幂等键：v1 canonical 第 7 段带它，**且请求头也要带**。
// 但 activities/summary 那条路必须传空串且不发头（坑⑪）—— 判据由 params 给，
// 不在这里硬编码端点名。
type v1Params struct {
	// Method 用于 canonical 的 method（**保持原值**，坑⑫）。
	Method string
	// Path upstreamPath（canonical 第 3 段）。
	Path string
	// Body 最终发出的 body 原始字节。
	Body []byte
	// IdempotencyKey 空串 = 这条请求没有幂等键（canonical 第 7 段空串、不发头）。
	IdempotencyKey string
}

// signV1 生成 v1 签名头与用了的值（返回给调用方做断言/排查）。
func signV1(a *Auth, p v1Params, timestamp, nonce string) (sig string, err error) {
	canonical := V1Canonical(p.Method, p.Path, timestamp, nonce, RequestBodyForHash(p.Body), p.IdempotencyKey)
	return a.signV1Canonical(canonical)
}

// applyV1 把 v1 签名挂到请求上。
func applyV1(req *http.Request, a *Auth, p v1Params, timestamp, nonce string) error {
	sig, err := signV1(a, p, timestamp, nonce)
	if err != nil {
		return err
	}
	id, err := a.installationID()
	if err != nil {
		return err
	}
	req.Header.Set("x-desktop-installation-id", id)
	req.Header.Set("x-desktop-timestamp", timestamp)
	req.Header.Set("x-desktop-nonce", nonce)
	req.Header.Set("x-desktop-signature", sig)
	if p.IdempotencyKey != "" {
		req.Header.Set("idempotency-key", p.IdempotencyKey)
	}
	req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	return nil
}

// ── 账号档案（accountScope 的来源）──────────────────────────────────────

// profileResponse GET /api/oauth/profile 里本包要的两个字段。
type profileResponse struct {
	Account struct {
		UUID        string `json:"uuid"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	} `json:"account"`
}

// FetchAccountScope 取账号 uuid（= device canonical 的 accountScope 段）。
//
// ⚠ 报告两处都强调过这个值不能用 JWT 的 sub / 邮箱代替（用错回
// 400 desktop_account_scope_invalid），所以这里**真的去问服务端**，
// 而不是拿本地能猜的东西凑。
//
// 这条路只需要 Bearer（实测 5 个只读端点全部 200，不需要 proof 头）。
func (c *Client) FetchAccountScope(ctx context.Context, a *Auth) (string, string, error) {
	if a == nil || a.OAuth.AccessToken == "" {
		return "", "", errNoAccessToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase()+pathProfile, nil)
	if err != nil {
		return "", "", err
	}
	c.applyCommonHeaders(req)
	req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	status, body, err := c.doJSON(req, httpTimeout)
	if err != nil {
		return "", "", fmt.Errorf("phanthywork: 取账号档案失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", "", &UpstreamError{Status: status, Body: string(body), Op: "取账号档案"}
	}
	var pr profileResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", "", fmt.Errorf("phanthywork: 账号档案不是 JSON: %w", err)
	}
	scope := strings.TrimSpace(pr.Account.UUID)
	if scope == "" {
		return "", "", fmt.Errorf("phanthywork: 账号档案里没有 account.uuid（签名必须要它）")
	}
	return scope, strings.TrimSpace(pr.Account.DisplayName), nil
}

// ── 模型目录 ─────────────────────────────────────────────────────────────

// FetchModels 拉上游模型目录（**只带 Bearer** —— 实测 200 时不需要 proof）。
//
// ⚠ 返回的 id **不带 phanthy/ 前缀**（坑③）。这是本包最容易踩的一处：
// 桌面端本地网关给的是 phanthy/Phanthy-fast，云端目录给的是 Phanthy-fast，
// 打云端就必须用云端那份。
func (c *Client) FetchModels(ctx context.Context, a *Auth) ([]remoteModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase()+pathModels, nil)
	if err != nil {
		return nil, err
	}
	c.applyCommonHeaders(req)
	if a != nil && a.OAuth.AccessToken != "" {
		req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	}
	status, body, err := c.doJSON(req, modelsTimeout)
	if err != nil {
		return nil, fmt.Errorf("phanthywork: 拉模型目录失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, &UpstreamError{Status: status, Body: string(body), Op: "拉模型目录"}
	}
	var doc struct {
		SchemaVersion int           `json:"schema_version"`
		DefaultModel  string        `json:"default_model"`
		Models        []remoteModel `json:"models"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("phanthywork: 模型目录不是 JSON: %w", err)
	}
	// 报告 §5：schema_version 不是 1 就当失败，**不许猜字段**。
	// 猜字段的后果是"模型列表看起来正常，模型 id 全是错的"。
	if doc.SchemaVersion != modelsSchemaVersion {
		return nil, fmt.Errorf("phanthywork: 模型目录 schema_version=%d（期望 %d），字段形状可能已变，拒绝猜",
			doc.SchemaVersion, modelsSchemaVersion)
	}
	return doc.Models, nil
}

// ── OAuth 换 token 与续期 ────────────────────────────────────────────────

// TokenSet 一次 token 响应的归一结果。
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAtMS 本机口径的到期时刻（毫秒）。
	ExpiresAtMS int64
	Scopes      []string
	// ExpiresIn 服务端给的秒数（排查用；0 = 服务端没给，用了兜底值）。
	ExpiresIn int64
}

// ExchangeCode 用授权码换 token（**form-urlencoded**，20 秒超时，不重试）。
//
// ⚠ 与续期**编码不同**（报告 §4.2 的对照表）：这里 URLSearchParams，
// 续期 JSON。照抄任一侧都会错。
//
// 6 个字段：grant_type / client_id / code / state（**再带一次**）/
// code_verifier / redirect_uri（必须与 authorize 那次**逐字相同**）。
func (c *Client) ExchangeCode(ctx context.Context, code, state, verifier, redirectURI string) (TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", OAuthClientID)
	form.Set("code", code)
	form.Set("state", state)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+pathOAuthToken,
		strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.applyCommonHeaders(req)

	status, body, err := c.doJSON(req, exchangeTimeout)
	if err != nil {
		return TokenSet{}, fmt.Errorf("phanthywork: 换取登录凭据失败: %w", err)
	}
	if status == 429 {
		return TokenSet{}, &UpstreamError{Status: status, Body: brief(body), Op: "换取登录凭据"}
	}
	if status >= 500 {
		return TokenSet{}, fmt.Errorf("phanthywork: 登录服务暂时不可用（HTTP %d），稍后重试: %s", status, brief(body))
	}
	if status < 200 || status >= 300 {
		return TokenSet{}, fmt.Errorf("phanthywork: 授权码已失效或已使用（HTTP %d）: %s", status, brief(body))
	}
	return parseTokenResponse(body, exchangeDefaultExpiresIn, nowMS())
}

// RefreshTokens 用 refresh_token 续期（**JSON body**，8 秒超时，3 次重试）。
//
// 三条照抄的边界（全部来自报告 §4.2 的逐字反编译）：
//
//	① body 是 JSON（Content-Type: application/json），与登录那次 form 不同
//	② 只对 **429 或 >= 500** 重试；其它 4xx **立即失败**
//	③ 400 + invalid_grant ⇒ 返回 ErrRefreshExpired（凭据废了，终态）
//
// 返回的 TokenSet.RefreshToken 可能为空 —— 那表示服务端**轮换**了它
// （实测轮换），调用方按"没给新的就沿用旧的"处理（见 Auth.applyTokenSet）。
func (c *Client) RefreshTokens(ctx context.Context, a *Auth) (TokenSet, error) {
	if a == nil || strings.TrimSpace(a.OAuth.RefreshToken) == "" {
		return TokenSet{}, ErrRefreshExpired
	}
	payload := map[string]any{
		"grant_type":    "refresh_token",
		"client_id":     OAuthClientID,
		"refresh_token": a.OAuth.RefreshToken,
	}
	if len(a.OAuth.Scopes) > 0 {
		payload["scope"] = strings.Join(a.OAuth.Scopes, " ")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return TokenSet{}, err
	}

	var lastErr error
	for attempt := 0; attempt < refreshMaxAttempts; attempt++ {
		req, rerr := c.newJSONRequest(ctx, http.MethodPost, pathOAuthToken, raw)
		if rerr != nil {
			return TokenSet{}, rerr
		}
		status, body, derr := c.doJSON(req, refreshTimeout)
		if derr != nil {
			lastErr = fmt.Errorf("phanthywork: 续期请求失败: %w", derr)
			if ctx.Err() != nil {
				return TokenSet{}, ctx.Err()
			}
			continue
		}
		switch {
		case status >= 200 && status < 300:
			return parseTokenResponse(body, refreshDefaultExpiresIn, nowMS())
		case status == 400 && gateway.ContainsAny(string(body), authExpiredMarkers):
			// invalid_grant 的语义（报告 §4.2）：refreshToken 已失效
			//（过期/被吊销/被轮换）⇒ 终态，不要重试。
			return TokenSet{}, ErrRefreshExpired
		case status == 429 || status >= 500:
			wait := backoffFor(attempt, retryAfterMS(body))
			lastErr = &UpstreamError{Status: status, Body: brief(body), Op: "续期"}
			if attempt == refreshMaxAttempts-1 || wait > maxBackoff {
				return TokenSet{}, lastErr
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return TokenSet{}, ctx.Err()
			}
			continue
		default:
			// 其它 4xx：立即失败（不含 invalid_grant 的 400 都走这里）。
			return TokenSet{}, &UpstreamError{Status: status, Body: string(body), Op: "续期"}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("phanthywork: 续期失败（重试 %d 次后放弃）", refreshMaxAttempts)
	}
	return TokenSet{}, lastErr
}

// parseTokenResponse 解析 token 响应（登录与续期同构）。
func parseTokenResponse(body []byte, fallbackExpiresIn int64, localMS int64) (TokenSet, error) {
	var doc struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Scope        string          `json:"scope"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return TokenSet{}, fmt.Errorf("phanthywork: token 响应不是 JSON: %w", err)
	}
	at := strings.TrimSpace(doc.AccessToken)
	if at == "" {
		return TokenSet{}, fmt.Errorf("phanthywork: token 响应缺少 access_token（响应体不完整）")
	}
	expiresIn := fallbackExpiresIn
	if v := parseIntRaw(doc.ExpiresIn); v > 0 {
		expiresIn = v
	}
	ts := TokenSet{
		AccessToken:  at,
		RefreshToken: strings.TrimSpace(doc.RefreshToken),
		ExpiresIn:    expiresIn,
		ExpiresAtMS:  localMS + expiresIn*1000,
	}
	if s := strings.TrimSpace(doc.Scope); s != "" {
		ts.Scopes = strings.Fields(s)
	}
	return ts, nil
}

// ── 设备注册 ─────────────────────────────────────────────────────────────

// registerResponse 注册响应的关键字段（报告 §2.4 实测）。
type registerResponse struct {
	Installation struct {
		ID string `json:"id"`
	} `json:"installation"`
	RewardStatus string `json:"reward_status"`
	RewardReason string `json:"reward_reason"`
	Error        struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// RegisterDevice 设备注册（**写操作**，一次性）。
//
// body 7 个字段，**顺序即官方客户端的注入顺序**（JSON.stringify 保序，
// 签名覆盖这个串）—— 所以这里手写序列化，不用 map（map 的键序不保证）。
//
//	{
//	  "public_key":           identity 公钥
//	  "edition":              "phanthy_work"
//	  "platform":             windows/macos/linux
//	  "version":              **官方客户端版本号 2.48.0**（不是本包的版本！）
//	  "device_public_key":    device 公钥（**必须与 public_key 不同**）
//	  "device_signature":     device 私钥签的 device canonical
//	  "inference_public_key": 推理公钥
//	}
//
// 失败模式（报告 §2.4）：
//
//	409 desktop_installation_conflict → **终态**，返回 ErrRegistrationConflict
//	400/403 desktop_binding_invalid   → canonical 算错了（值得查代码）
//	400 desktop_account_scope_invalid → accountScope 不对
func (c *Client) RegisterDevice(ctx context.Context, a *Auth) (registerResponse, error) {
	var out registerResponse
	if a == nil {
		return out, errNilAuth
	}
	scope := a.accountScope()
	if scope == "" {
		return out, fmt.Errorf("phanthywork: 缺少 accountScope（设备注册的 canonical 必须要它）")
	}
	identityPub, err := a.identityPublicKey()
	if err != nil {
		return out, err
	}
	devicePub, err := a.devicePublicKey()
	if err != nil {
		return out, err
	}
	instID, err := a.installationID()
	if err != nil {
		return out, err
	}
	if a.Infer.PublicKey == "" {
		return out, fmt.Errorf("phanthywork: 缺少推理公钥（它是 device canonical 第 5 段）")
	}
	// ⚠ device 公钥必须与 identity 公钥不同（IDENTITY_POLICY 的
	// deviceKeyDistinct）。派生换域已经保证这一点，这里再挡一次：
	// 万一有人把两把密钥写成同一把，失败会是一条看不出原因的
	// desktop_binding_invalid。
	if devicePub == identityPub {
		return out, fmt.Errorf("phanthywork: device 公钥与 identity 公钥相同（IDENTITY_POLICY 要求两把必须不同）")
	}

	timestamp := msToString(a.serverNowMS())
	nonce := newNonce()
	deviceSig, err := a.signDeviceCanonical(
		DeviceCanonical(scope, instID, identityPub, a.Infer.PublicKey, timestamp, nonce))
	if err != nil {
		return out, err
	}

	// 手写序列化以保证**键序**（签名覆盖字节）。
	body := fmt.Sprintf(
		`{"public_key":%q,"edition":%q,"platform":%q,"version":%q,"device_public_key":%q,"device_signature":%q,"inference_public_key":%q}`,
		identityPub, Edition, platformName(), OfficialClientVersion, devicePub, deviceSig, a.Infer.PublicKey)

	req, err := c.newJSONRequest(ctx, http.MethodPost, pathRegister, []byte(body))
	if err != nil {
		return out, err
	}
	idem := registerIdempotencyKey(instID)
	if err := applyV1(req, a, v1Params{
		Method: http.MethodPost, Path: pathRegister, Body: []byte(body), IdempotencyKey: idem,
	}, timestamp, nonce); err != nil {
		return out, err
	}

	status, respBody, derr := c.doJSON(req, registerTimeout)
	if derr != nil {
		return out, fmt.Errorf("phanthywork: 设备注册请求失败: %w", derr)
	}
	if status < 200 || status >= 300 {
		ue := &UpstreamError{Status: status, Body: string(respBody), Op: "设备注册"}
		if status == 409 || strings.Contains(string(respBody), "desktop_installation_conflict") {
			// 终态：包一层，调用方用 errors.Is(err, ErrRegistrationConflict) 判。
			return out, fmt.Errorf("%w: %s", ErrRegistrationConflict, ue.Error())
		}
		return out, ue
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, fmt.Errorf("phanthywork: 注册响应不是 JSON: %w", err)
	}
	if out.Error.Code != "" {
		return out, &UpstreamError{Status: status, Body: string(respBody), Op: "设备注册"}
	}
	return out, nil
}

// ── 换推理令牌 ───────────────────────────────────────────────────────────

// summaryResponse activities/summary 的响应里本包要的字段。
//
// # 为什么一个结构体承载三种用途（这是报告 §4.7/full-platform §2.1 的结论）
//
// 实测这个端点是全平台**信息密度最高**的一个：一次调用同时给出
//
//	server_time                    → 时钟校准（坑①）
//	desktop_inference_token        → 换推理令牌（TTL 180s）
//	credits.available              → 钱包可用积分（额度）
//	daily.{status,server_date,...} → 签到状态 + **幂等键要用的服务端日期**
//	referral / feature_flags       → 邀请资格 / 今日签到是否开放
//
// 所以本包只发**这一条** v1 签名的请求就能同时满足续期、额度、签到三件事，
// 不必为每个功能各打一个端点（少一次往返，也少一处签名口径）。
//
// ⚠ 只声明用得上的字段：报告 §5 诚实说明过"逐条字段没有落盘"，所以
// **不编造**没采集到的字段（编了就会在解析时静默得到零值，而零值有语义）。
type summaryResponse struct {
	SchemaVersion int    `json:"schema_version"`
	ServerTime    string `json:"server_time"`
	// DesktopInferenceToken 推理令牌（JWT，实测 813 字符，exp-iat = 180s）。
	DesktopInferenceToken string `json:"desktop_inference_token"`
	// Credits 钱包（available = 可用积分，实测 3600）。
	Credits struct {
		Available int64 `json:"available"`
		Reserved  int64 `json:"reserved"`
	} `json:"credits"`
	// Daily 签到（**服务器自己的日键**在这里）。
	Daily struct {
		// Status 实测取值 granted_today（= 今天已领）。
		//
		// ⚠ 报告 §2.2 提醒：claim 端点的同义字段叫 already_granted ——
		// **两套词说同一件事**，判断时别只认一个。
		Status string `json:"status"`
		// ServerDate 服务端日键（UTC+8 换日）。幂等键**必须**用它，
		// 不许本地算日期（本地算在跨零点/时区差时会算出两个日键 ⇒ 发出两条）。
		ServerDate string `json:"server_date"`
		Points     int64  `json:"points"`
		StreakDay  int64  `json:"streak_day"`
		NextPoints int64  `json:"next_points"`
		// MaxStreakDay 连续上限（实测 5）。
		MaxStreakDay   int64  `json:"max_streak_day"`
		NextEligibleAt string `json:"next_eligible_at"`
	} `json:"daily"`
	// FeatureFlags 活动开关（daily_enabled=true 表示今天开放签到）。
	FeatureFlags struct {
		DailyEnabled bool `json:"daily_enabled"`
	} `json:"feature_flags"`
}

// FetchSummary 打一次 activities/summary（v1 签名，**无幂等键** —— 坑⑪）。
//
// 它是续期、额度、签到三条路的**唯一**上游读入口：调用方各取所需字段，
// 于是"签名怎么拼"只有一处实现（判据单源）。
func (c *Client) FetchSummary(ctx context.Context, a *Auth) (summaryResponse, int64, error) {
	var out summaryResponse
	localMS := nowMS()
	if a == nil {
		return out, localMS, errNilAuth
	}
	body := fmt.Sprintf(`{"inference_public_key":%q}`, a.Infer.PublicKey)
	timestamp := msToString(a.serverNowMS())
	nonce := newNonce()
	req, err := c.newJSONRequest(ctx, http.MethodPost, pathActivitiesSummary, []byte(body))
	if err != nil {
		return out, localMS, err
	}
	// ⚠ IdempotencyKey 留空（坑⑪：加了就签错）。
	if err := applyV1(req, a, v1Params{
		Method: http.MethodPost, Path: pathActivitiesSummary, Body: []byte(body),
	}, timestamp, nonce); err != nil {
		return out, localMS, err
	}
	status, respBody, derr := c.doJSON(req, summaryTimeout)
	localMS = nowMS() // 校准用"拿到响应"的时刻
	if derr != nil {
		return out, localMS, fmt.Errorf("phanthywork: 读账号状态请求失败: %w", derr)
	}
	if status < 200 || status >= 300 {
		return out, localMS, &UpstreamError{Status: status, Body: string(respBody), Op: "读账号状态"}
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, localMS, fmt.Errorf("phanthywork: 账号状态响应不是 JSON: %w", err)
	}
	return out, localMS, nil
}

// DailyLoginClaimResponse daily-login/claim 的响应（报告 §4.7 实测全字段）。
type DailyLoginClaimResponse struct {
	SchemaVersion int `json:"schema_version"`
	// Status 实测 already_granted = 今天已领（**也是 200**，不是错误）。
	//
	// ⚠ 首次领取的确切值是**未知量**（报告 §4.7 明说"没测到"：桌面端启动就自动领了）。
	// 所以判断"领到了没有"**不能**按某个固定字符串写死 —— 见 ClaimDailyLogin 的注释。
	Status string `json:"status"`
	// ServerDate 服务端日键（UTC+8）。**幂等键必须用它**。
	ServerDate string `json:"server_date"`
	Points     int64  `json:"points"`
	StreakDay  int64  `json:"streak_day"`
	// Reward 存在与否是"这次真的产生了奖励"的旁证（报告 §4.7 第 4 条判据：
	// already_granted 一定有 reward 对象；凭据问题一定没有）。
	Reward struct {
		Points     int64  `json:"points"`
		Status     string `json:"status"`
		RewardType string `json:"reward_type"`
	} `json:"reward"`
}

// ClaimDailyLogin 领取今日签到（POST daily-login/claim）。
//
// # 三条硬要求（报告 §4.7，逐条落在签名/幂等键/返回判定上）
//
//	① body 必须是**空对象** `{}` —— 不是空串（空串会让 canonical 第 6 段哈希对不上）
//	② 幂等键格式 `daily:<installationId>:<server_date>`，且**必须**用**响应里的**
//	   server_date（本地算日期会在跨零点/时区差时算出两个日键 ⇒ 发出两条）
//	③ "今日已领"是**幂等成功**（200 + already_granted），不是失败
func (c *Client) ClaimDailyLogin(ctx context.Context, a *Auth, serverDate string) (DailyLoginClaimResponse, error) {
	var out DailyLoginClaimResponse
	if a == nil {
		return out, errNilAuth
	}
	serverDate = strings.TrimSpace(serverDate)
	if serverDate == "" {
		// ⚠ 没有服务端日键就**不发**：本地算一个日期等于违反上面第 ② 条。
		return out, fmt.Errorf("phanthywork: 缺少服务端日键（server_date），拒绝用本地日期拼幂等键")
	}
	instID, err := a.installationID()
	if err != nil {
		return out, err
	}
	// ① body = 空对象（字面量 {}）
	const body = `{}`
	timestamp := msToString(a.serverNowMS())
	nonce := newNonce()
	req, err := c.newJSONRequest(ctx, http.MethodPost, pathDailyLoginClaim, []byte(body))
	if err != nil {
		return out, err
	}
	idem := DailyLoginIdempotencyKey(instID, serverDate)
	if err := applyV1(req, a, v1Params{
		Method: http.MethodPost, Path: pathDailyLoginClaim, Body: []byte(body), IdempotencyKey: idem,
	}, timestamp, nonce); err != nil {
		return out, err
	}
	status, respBody, derr := c.doJSON(req, summaryTimeout)
	if derr != nil {
		return out, fmt.Errorf("phanthywork: 签到请求失败: %w", derr)
	}
	if status < 200 || status >= 300 {
		return out, &UpstreamError{Status: status, Body: string(respBody), Op: "签到"}
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, fmt.Errorf("phanthywork: 签到响应不是 JSON: %w", err)
	}
	return out, nil
}

// DailyLoginIdempotencyKey 拼签到幂等键：`daily:<installationId>:<server_date>`。
//
// ⚠ 报告 §4.7 的实测值长这样：`daily:di_oit3noGRcRw6NNe4pz1NhPjrISfygoO7vTTkWmaib64:2026-11-12`。
// 开源实现写的是毫秒时间戳 —— **照抄会让幂等彻底失效**（每天发出多条，每条一个新键）。
func DailyLoginIdempotencyKey(installationID, serverDate string) string {
	return "daily:" + installationID + ":" + serverDate
}

// RequestInferenceToken 换推理令牌（POST /api/oauth/activities/summary）。
//
// body **只有 1 个字段**：
//
//	{ "inference_public_key": "<推理公钥 SPKI/b64url>" }
//
// ⚠ v1 canonical 第 7 段是**空串**，请求头**不发** idempotency-key（坑⑪）。
// 报告的原话：「别"顺手加一个幂等键" —— canonical 会变，签名立刻失败」。
// 所以这里 idempotencyKey 恒为空串（对比 RegisterDevice 用的是 register:<id>）。
//
// 响应同时带 server_time —— 本包在校准时钟时只认它（坑①）。
func (c *Client) RequestInferenceToken(ctx context.Context, a *Auth) (summaryResponse, int64, error) {
	if a == nil {
		return summaryResponse{}, nowMS(), errNilAuth
	}
	if a.Infer.PublicKey == "" {
		var zero summaryResponse
		return zero, nowMS(), fmt.Errorf("phanthywork: 缺少推理公钥")
	}
	// 与额度/签到共用同一次上游读（判据单源：签名怎么拼只有一处实现）。
	out, localMS, err := c.FetchSummary(ctx, a)
	if err != nil {
		return out, localMS, err
	}
	if strings.TrimSpace(out.DesktopInferenceToken) == "" {
		// 200 但没有令牌 = 结构变了。**明确失败**而不是静默继续
		//（静默继续的表现是随后 403 request_not_authorized，查不到根因）。
		return out, localMS, fmt.Errorf("phanthywork: 响应里没有 desktop_inference_token（上游字段形状可能已变）")
	}
	return out, localMS, nil
}

// ── 调模型 ───────────────────────────────────────────────────────────────

// ChatStream 发一次 /v1/messages，返回原始响应（**调用方负责 Close**）。
//
// 与 gateway.Provider.Chat 的契约一致：非 2xx 也算正常返回（把上游错误体
// 带回来给核心分类），只有网络层失败才返回 error。
func (c *Client) ChatStream(ctx context.Context, a *Auth, path string, body []byte, proof proofValues) (*http.Response, error) {
	if a == nil || a.OAuth.AccessToken == "" {
		return nil, errNoAccessToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyCommonHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	if proof.RequestID != "" {
		req.Header.Set("x-request-id", proof.RequestID)
		// ⚠ 这 4 段在 canonical 里有位置，但**头只在有值时才发**：
		// 报告列的 4 个 proof 头里没有它们（它们是别处的可选上下文头）。
	}
	req.Header.Set("x-phanthy-desktop-session", proof.InferenceToken)
	req.Header.Set("x-phanthy-proof-timestamp", proof.Timestamp)
	req.Header.Set("x-phanthy-proof-nonce", proof.Nonce)
	req.Header.Set("x-phanthy-proof-signature", proof.Signature)
	for k, v := range proof.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return c.httpClient().Do(req)
}

// proofValues 一次推理请求的 proof 头（由 buildProof 产出）。
type proofValues struct {
	// InferenceToken **推理令牌本身**（⚠ 不是 sessionId，坑⑨）。
	InferenceToken string
	Timestamp      string
	Nonce          string
	Signature      string
	// RequestID uuid v4（canonical 第 9 段、也作为 x-request-id 头发出去）。
	RequestID string
	// ExtraHeaders 可选上下文头（canonical 第 10~12 段对应的那三个）。
	ExtraHeaders map[string]string
}

// buildProof 构造一次推理请求的 12 段签名与 4 个 proof 头。
//
// ⚠ 4 个规则（报告 §2.6），每一条都有对应的测试：
//
//	① 段间 LF
//	② 第 6 段是**最终发出的 body 原始字节**的 sha256
//	③ 后 4 段缺项写**空串**但分隔符不能省
//	④ method 在这条串里大写（与 v1 串相反 —— 坑⑫）
func (c *Client) buildProof(a *Auth, method, path string, body []byte, token string, localMS int64) (proofValues, error) {
	if a == nil {
		return proofValues{}, errNilAuth
	}
	ts := msToString(serverNowMS(localMS, a.skew()))
	nonce := newNonce()
	reqID := randomUUID()
	authHeader := AuthorizationHeaderValue(a.OAuth.AccessToken)

	// 后 4 段的可选头：本包**不发**它们（头也没有对应值），
	// 但 canonical 里必须留**空串**占位。
	canonical := InferenceCanonical(
		method, path, ts, nonce, RequestBodyForHash(body),
		authHeader, token,
		reqID, "", "", "")

	sig, err := a.signInferenceCanonical(canonical)
	if err != nil {
		return proofValues{}, err
	}
	return proofValues{
		InferenceToken: token,
		Timestamp:      ts,
		Nonce:          nonce,
		Signature:      sig,
		RequestID:      reqID,
	}, nil
}

// ── 小工具 ───────────────────────────────────────────────────────────────

// brief 把响应体压成一行短串（日志/错误文案用）。
func brief(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// parseIntRaw 解析可能是数字也可能是字符串的数字字段。
func parseIntRaw(raw json.RawMessage) int64 {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return 0
	}
	if n, err := strconv.ParseInt(strings.Trim(t, `"`), 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(strings.Trim(t, `"`), 64); err == nil {
		return int64(f)
	}
	return 0
}

// retryAfterMS 从响应体里尽力取 retry-after（拿不到就 0）。
func retryAfterMS(body []byte) time.Duration {
	var doc struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(doc.RetryAfter * float64(time.Second))
}

// backoffFor 第 attempt 次重试前的等待（指数 + 上限）。
func backoffFor(attempt int, hint time.Duration) time.Duration {
	base := 500 * time.Millisecond * time.Duration(1<<uint(attempt))
	if hint > base {
		base = hint
	}
	if base > maxBackoff {
		base = maxBackoff
	}
	return base
}
