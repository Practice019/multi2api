// lobsterai.go LobsterAI（有道龙虾）的协议常量与纯函数。
//
// # 事实来源
//
// 协议事实来自参照项目 dsh-codearts-auth（TypeScript，已实测跑通）的
// src/lobsterai*.ts 与 docs/lobsterai-integration-plan.md（1354 行集成计划）。
// 本包是它的 Go 移植，**判据照搬，不自行探究**。
//
// # 与其它上游的关系
//
// 参照项目的 AGENTS.md 明确写着：lobsterai 与 CodeBuddy 系**完全不同源** ——
// 登录方式、请求头、续期载荷、签到流程都不一样。所以它是**独立一套实现**，
// 只**共用架构模式**（账号池、限流切换、错误分类），**不共用类型**。
//
// # 统一信封（最容易搞错的一点）
//
// 除两个例外，全部是 `{code, msg, data}`，`code !== 0` 即失败。
//
//	例外 1：/api/proxy/v1/chat/completions 返回**裸 SSE，不套信封**
//	        （模型列表套信封、聊天不套）
//	例外 2：版本接口的 code/msg 在**外层**（与 data 同级）
package lobsterai

import (
	"crypto/rand"
	"encoding/base64"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// providerID 上游标识。
const providerID = "lobsterai"

// ProviderID 导出上游标识，供装配层（cmd/server）使用。
const ProviderID = providerID

// 基址（参照 lobsterai-product.ts）。
const (
	// DefaultAPIBase 业务 API 基址。
	DefaultAPIBase = "https://lobsterai-server.youdao.com"
	// DefaultPortalBase 登录门户基址。
	//
	// ⚠ 与 APIBase **同一 IP、同一 CNAME 目标**，但仍是两个独立字段
	//（Go 侧本就是两个独立 env）。
	DefaultPortalBase = "https://lobsterai.youdao.com"
	// DefaultClientVersionAPI 客户端版本号查询端点（第三方域名）。
	//
	// ⚠ 该域名与业务域名是**不同的服务**，且实测存在网络层/证书层差异
	//（直连曾失败、走工具才成功）→ 落地实现**必须保留 fallbackClientVersion 兜底**。
	DefaultClientVersionAPI = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"
	// DefaultClientVersion 版本号动态拉取失败时的兜底值。
	DefaultClientVersion = "2026.9.4"
)

// 端点路径（参照 lobsterai.ts:13-18、lobsterai-credits.ts:36-40）。
const (
	exchangePath = "/api/auth/exchange"
	refreshPath  = "/api/auth/refresh"
	modelsPath   = "/api/models/available"
	chatPath     = "/api/proxy/v1/chat/completions"
	callbackPath = "/auth/callback"

	activitySlotPath    = "/api/client-activities/slot"
	activityContextPath = "/api/client-activities"
	profileSummaryPath  = "/api/user/profile-summary"
)

// 超时与缓存（参照 lobsterai.ts:19-22）。
const (
	// requestTimeoutMS 单次请求超时。
	requestTimeoutMS = 30_000
	// loginTimeoutMS 登录整体超时。
	loginTimeoutMS = 10 * 60 * 1000
	// versionCacheTTLMS 版本号缓存有效期。
	versionCacheTTLMS = 12 * 60 * 60 * 1000
)

// 客户端能力声明（`X-LobsterAI-Client-Capabilities` 头）。
//
// # 两个能力**都必须声明**，各自解决一个具体问题（实测）
//
//   - `kimi-k3-agentic-v1`：**模型列表的准入条件**。不带该能力时
//     /api/models/available 只返回 25 个模型且**没有 kimi-k3**；带上才 26 个。
//   - `thinking-level-control-v1`：**思考档位协议的前提**。`reasoning_effort`
//     的常规档位不需要它，但 `"off"`（关闭思考）在**不带**该能力时服务端
//     直接 HTTP 500，带上则正常。
//
// 顺序无关（两种顺序都实测通过）。
const clientCapabilities = "kimi-k3-agentic-v1,thinking-level-control-v1"

// ClientCapabilities 导出（登录/模型端点都要用）。
const ClientCapabilities = clientCapabilities

// userAgent User-Agent。
//
// ⚠ **刻意不跟着真版本号改**：Go 侧用这个 UA 是实测可用的，
// 而 `X-LobsterAI-Client-Version` 头用动态真值。
// 若同时改两处，一旦服务端行为变化将无法归因是哪一个导致的。
const userAgent = "LobsterAI/0.1.0"

// 签到槽位参数（参照 lobsterai-credits.ts:45-52，用真实客户端观察到）。
const (
	slotPlacement          = "desktop_sidebar"
	slotContainerAPIVer    = "2"
	slotPlatform           = "win32"
	checkInActionName      = "check_in"
	creditBalanceUnitLabel = "积分"
)

// envelope 统一业务信封。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// envelopeError 把信封错误拼成可读文案。
func envelopeError(e envelope) error {
	msg := strings.TrimSpace(e.Msg)
	if msg == "" {
		msg = fmt.Sprintf("code=%d", e.Code)
	}
	return fmt.Errorf("lobsterai: %s", msg)
}

// parseEnvelope 解析信封并判成功。
//
// ⚠ code 用 *int 承接：缺 code 字段与 code=0 必须区分开
//（缺字段时参照实现回落 -1，即视为失败）。
func parseEnvelope(raw []byte) (envelope, error) {
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		return envelope{}, fmt.Errorf("lobsterai: 响应不是 JSON 对象")
	}
	code := -1
	if v, ok := rec["code"].(float64); ok {
		code = int(v)
	}
	msg := ""
	if s, ok := rec["msg"].(string); ok {
		msg = s
	} else if s, ok := rec["message"].(string); ok {
		msg = s
	}
	env := envelope{Code: code, Msg: msg}
	if code != 0 {
		return env, envelopeError(env)
	}
	// data 必须是对象（非对象视为 accessToken 可能已失效）
	data, ok := rec["data"].(map[string]any)
	if !ok || data == nil {
		if msg == "" {
			msg = "data 为空（accessToken 可能已失效）"
		}
		return envelope{Code: code, Msg: msg},
			fmt.Errorf("lobsterai: %s", msg)
	}
	b, _ := json.Marshal(data)
	env.Data = b
	return env, nil
}

// Auth 一份 LobsterAI 凭证（参照 lobsterai.ts 的 LobsteraiCredential）。
type Auth struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt 过期时间。字符串形态（毫秒或秒都接受，见 ExpiresAtMS）。
	ExpiresAt string `json:"expires_at,omitempty"`
	// UID 账号主键。
	UID string `json:"uid,omitempty"`
	// UserID 账号 userId（续期请求体要用）。
	UserID string `json:"user_id,omitempty"`
	// Nickname 展示名。
	Nickname string `json:"nickname,omitempty"`
	// UUID 安装 UUID。
	//
	// ⚠ 由**客户端**生成并贯穿整个账号生命周期：它不在服务端响应里，
	// 而是要在 exchange 时提交、并在之后**每次续期**时原样回传。
	// 因此必须随凭据持久化。
	UUID string `json:"uuid,omitempty"`
	// FirstKeyfrom 首次登录时间戳（毫秒字符串）。
	//
	// ⚠ 与 LatestKeyfrom 一样，**每次续期都用凭据里存储的原值**，
	// 不取当前时刻（严格对齐 Go 的 KeyfromBody()：它读的就是 a.LatestKeyfrom，
	// 而 RefreshToken 从不更新该字段）。
	FirstKeyfrom string `json:"first_keyfrom,omitempty"`
	// LatestKeyfrom 最近一次 keyfrom。
	LatestKeyfrom string `json:"latest_keyfrom,omitempty"`
	// FilePath 凭证文件落点。
	FilePath string `json:"-"`
}

// UIDValue 返回账号主键（缺失时由 accessToken 哈希派生）。
//
// 参照 resolveLobsteraiUid：user_id > uid > yid，最后回落 token 哈希。
func (a *Auth) UIDValue() string {
	if a == nil {
		return ""
	}
	for _, c := range []string{a.UID, a.UserID} {
		if strings.TrimSpace(c) != "" {
			return strings.TrimSpace(c)
		}
	}
	if a.AccessToken != "" {
		sum := sha256.Sum256([]byte(a.AccessToken))
		return hex.EncodeToString(sum[:8])
	}
	return ""
}

// jwtExpMS 从 JWT payload 取 exp（毫秒）；取不到返回 0。
//
// ⚠ 只解码、不验签。
func jwtExpMS(token string) int64 {
	if token == "" {
		return 0
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var payload struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	if payload.Exp <= 0 || payload.Exp != payload.Exp {
		return 0
	}
	return int64(payload.Exp * 1000)
}

// ExpiresAtMS 返回绝对过期时刻（毫秒），0 表示无过期信息。
//
// 取值顺序（参照 lobsteraiCredentialExpiresAtMs）：
//
//	1. expires_at 是纯数字串 → > 1e12 视为毫秒，否则视为秒（×1000）
//	2. expires_at 是日期串   → 按 RFC3339 解析
//	3. 都拿不到              → 回退 JWT exp
//
// ⚠ 第 3 步的回退是必需的：没有它，老凭据/手工导入的凭证过期判定恒为 false，
// 续期永远不触发 —— 表现为"凭据悄悄过期"，静默失效。
func (a *Auth) ExpiresAtMS() int64 {
	if a == nil {
		return 0
	}
	if raw := strings.TrimSpace(a.ExpiresAt); raw != "" {
		if isDigits(raw) {
			if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
				if v > 1_000_000_000_000 {
					return int64(v)
				}
				return int64(v * 1000)
			}
		} else if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			return ts.UnixMilli()
		}
	}
	return jwtExpMS(a.AccessToken)
}

// IsExpired 报告凭证是否已过期。
//
// ⚠ 无法解析过期时间时**不**判定过期（与 Go/Rust 侧一致）：
// 宁可用一个可能过期的凭据去试（服务端会回错，我们能识别并续期），
// 也不凭猜测阻止用户。
func (a *Auth) IsExpired(now time.Time) bool {
	ms := a.ExpiresAtMS()
	if ms <= 0 {
		return false
	}
	return now.UnixMilli() >= ms
}

// Renewable 报告凭证是否可续期。
func (a *Auth) Renewable() bool {
	return a != nil && strings.TrimSpace(a.RefreshToken) != ""
}

// needsRefresh 报告是否该提前续期。
func (a *Auth) needsRefresh(now time.Time, lead time.Duration) bool {
	if !a.Renewable() {
		return false
	}
	ms := a.ExpiresAtMS()
	if ms <= 0 {
		return false
	}
	return ms <= now.Add(lead).UnixMilli()
}

// keyfromBody 构造 keyfrom 身份载荷（exchange / refresh / models 共用）。
//
// ⚠ latestKeyfrom / firstKeyfrom 都用**凭据里存储的原值**，不取当前时刻。
func keyfromBody(a *Auth, clientVersion string) map[string]any {
	body := map[string]any{
		"firstKeyfrom":  a.FirstKeyfrom,
		"latestKeyfrom": a.LatestKeyfrom,
		"version":       clientVersion,
	}
	if a.UUID != "" {
		body["uuid"] = a.UUID
	}
	if a.UserID != "" {
		body["userId"] = a.UserID
	}
	return body
}

// refreshBody 构造续期请求体 = keyfrom 载荷 + refreshToken。
func refreshBody(a *Auth, clientVersion string) map[string]any {
	body := keyfromBody(a, clientVersion)
	body["refreshToken"] = a.RefreshToken
	return body
}

// modelListQuery 模型列表的 query（keyfrom body 的等价物，**不含 refreshToken**）。
func modelListQuery(a *Auth, clientVersion string) string {
	parts := []string{
		"firstKeyfrom=" + url.QueryEscape(a.FirstKeyfrom),
		"latestKeyfrom=" + url.QueryEscape(a.LatestKeyfrom),
		"version=" + url.QueryEscape(clientVersion),
	}
	if a.UUID != "" {
		parts = append(parts, "uuid="+url.QueryEscape(a.UUID))
	}
	if a.UserID != "" {
		parts = append(parts, "userId="+url.QueryEscape(a.UserID))
	}
	return strings.Join(parts, "&")
}

// versionRe 日期式版本号校验正则（对齐 sigin.py 的 version_key）。
//
// ⚠ 之所以要**校验**而不是直接采信：版本号是签到接口的必填 query 参数，
// 若上游返回 null / 空串 / HTML 错误页，把它拼进 URL 会让签到以一个
// 更费解的错误失败。提前拒绝能给出「版本格式异常」这种可读原因。
var versionRe = regexp.MustCompile(`^(\d+(?:\.\d+)*)(?:-[0-9A-Za-z.-]+)?$`)

// ParseClientVersion 校验并归一版本号；格式非法返回空串。
func ParseClientVersion(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

// ParseClientVersionFromUpdate 从版本接口响应里取版本号。
//
// ⚠ 该接口的 code/msg 在**外层**（与 data 同级），载荷在 data.value。
func ParseClientVersionFromUpdate(raw []byte) string {
	var rec struct {
		Data struct {
			Value struct {
				Version string `json:"version"`
			} `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return ""
	}
	return ParseClientVersion(rec.Data.Value.Version)
}

// NewUUID 生成 v4 UUID（用于登录会话与签到幂等键）。
func NewUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// shortUID 日志用的短 uid。
func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8] + "…"
}
