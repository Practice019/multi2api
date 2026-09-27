// cline.go Cline（Cline 桌面端 / Cline API）的协议常量与纯函数。
//
// # 这个包在整条解耦链里的位置
//
// 它是 gateway.Provider 的第六个实现（workbuddy / codearts / loomy / trae / mimo 之后）。
// 与其他上游的**最大不同**：它没有任何特殊依赖 ——
//
//	无 WASM       （对比 qoder 的加密推理端点）
//	无请求签名     （对比 codearts 的 SDK-HMAC-SHA256）
//	无双向加密     （对比 mimo 的 X25519 + AES-GCM 回调）
//	无本地监听端口 （对比 codearts / lobsterai 的本地回调服务器）
//
// 登录走 **WorkOS 设备码轮询**：起一次 POST 拿设备码与授权 URL，用户在浏览器里
// 确认，网关轮询换 token。全程不需要回调地址 —— 也就没有"服务器部署下回调
// 打不进本机"这一类问题（那正是 manual 模式存在的原因）。
//
// # 事实来源
//
// 协议事实来自参照项目 dsh-codearts-auth（TypeScript，已实测跑通）的
// src/cline*.ts 与 docs。本包是它的 Go 移植，**判据照搬，不自行探究**。
// 每条硬事实在注释里标出它在参照项目里的出处，便于日后复核。
package cline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// providerID 上游标识。
//
// 会被用作：模型名前缀（"cline/xxx"）、配置里的 provider key、统计维度。
// 格式受 gateway 约束（^[a-z][a-z0-9-]*$），有契约测试守着。
const providerID = "cline"

// ProviderID 导出上游标识，供装配层（cmd/server）使用。
const ProviderID = providerID

// 三个基址（参照 cline-product.ts:250-266）。
const (
	// DefaultAPIBase 推理、模型目录、账号端点、auth/register、auth/refresh。
	DefaultAPIBase = "https://api.cline.bot"
	// DefaultWorkOSBase 设备码授权与 token 轮询走它，**不是** APIBase。
	//
	// ⚠ 这是最容易搞错的一处：把设备码请求发到 api.cline.bot 会 404。
	DefaultWorkOSBase = "https://api.workos.com"
	// DefaultAppBase Web 应用基址（登录引导页用）。
	DefaultAppBase = "https://app.cline.bot"
)

// workOSClientID WorkOS 客户端 id。
//
// 两处一致（实测）：CLINE_ENVIRONMENTS.production 与凭据 JWT 的 client_id claim。
const workOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"

// WorkOSClientID 导出（登录流程与测试都要用它）。
const WorkOSClientID = workOSClientID

// tokenPrefix 访问令牌前缀。
//
// ⚠ **不可剥离**（头号坑）：Authorization 必须是 `Bearer workos:<jwt>`。
// 剥掉前缀即 401，而服务端给的文案会误导成「版本过旧」——
// 参照项目 AGENTS.md:1802-1804。见 clineBearerValue 的幂等补齐。
const tokenPrefix = "workos:"

// 端点路径（参照 cline-product.ts:276-295）。
const (
	// deviceAuthorizationPath 申请设备码与授权 URL（挂 WorkOSBase）。
	deviceAuthorizationPath = "/user_management/authorize/device"
	// deviceAuthenticatePath 轮询换 token（挂 WorkOSBase）。
	deviceAuthenticatePath = "/user_management/authenticate"
	// registerPath 用 WorkOS token 换 Cline 自己的 token（挂 APIBase）。
	registerPath = "/api/v1/auth/register"
	// refreshPath 续期（挂 APIBase）。
	refreshPath = "/api/v1/auth/refresh"
	// chatPath 推理（挂 APIBase，标准 OpenAI 兼容，仅 SSE）。
	chatPath = "/api/v1/chat/completions"
	// modelsPath 全量模型 id（需认证，460 个，**不含** cline-free/*）。
	modelsPath = "/api/v1/models"
	// recommendedModelsPath 推荐/免费/ClinePass 模型（**不需要认证**）。
	recommendedModelsPath = "/api/v1/ai/cline/recommended-models"
	// mePath 验证令牌有效性（挂 APIBase）。
	mePath = "/api/v1/users/me"
)

// balancesPathFmt 余额查询路径模板（accountId 需 URL 编码）。
const balancesPathFmt = "/api/v1/users/%s/balance"

// normalizedBalance 把原始余额换算成展示量级（见 balanceScale 的注释）。
//
// 保留两位小数（与其它 provider 的积分展示精度一致）。
func normalizedBalance(raw float64) float64 {
	return float64(int64(raw/balanceScale*100+0.5)) / 100
}

// deviceGrantType 设备码轮询的 grant_type。
//
// ⚠ 逐字取自 RFC 8628，编码后是
// grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// 超时与轮询参数（参照 cline-oauth.ts:77-86、cline-models.ts:48、cline-credits.ts:63）。
const (
	// httpTimeoutMS 单次 HTTP 超时。
	httpTimeoutMS = 30_000
	// deviceAuthExpiresMS 设备码默认有效期（服务端未给时回落）。
	deviceAuthExpiresMS = 300_000
	// deviceAuthIntervalMS 默认轮询间隔（服务端未给时回落）。
	deviceAuthIntervalMS = 5_000
	// pollMaxFailures 连续网络失败上限（成功拿到响应即归零）。
	pollMaxFailures = 5
	// pollMinIntervalMS 轮询间隔下限。
	//
	// ⚠ 服务端可能下发 0 或负数，无节制轮询会被限流。
	pollMinIntervalMS = 1_000
	// slowDownStepMS slow_down 时的**累积**增量。
	//
	// ⚠ 是累积而不是重置：源码 intervalSeconds += 1。
	// 用固定间隔会在服务端要求降速后持续被限流。
	slowDownStepMS = 1_000
	// modelsTimeoutMS 模型目录超时。
	modelsTimeoutMS = 20_000
	// creditsTimeoutMS 余额查询超时。
	creditsTimeoutMS = 30_000
)

// maxOutputTokensUpperBound max_tokens 上界。
//
// 取自内嵌目录里最大的 maxTokens（muse-spark-1.3-contributor），
// **不自行编造更大的值**。上游网关对超大 max_tokens 会直接 4xx，
// 而 DSH 可能注入一个来自其它 provider 的大值。
const maxOutputTokensUpperBound = 943_718

// balanceScale 余额换算系数（**全模块唯一的不确定点**）。
//
// 实测 balance: 500000。按 1e-5 解释为 $5.00，与 Cline 公开的新账号赠额
// 量级一致 —— 这是选取本值的**独立锚点**，不是从 /usages 的 costUsd 反推
// （那两个字段口径不同，实测不可互推：见参照 cline-credits.ts:35-44）。
//
// ⚠ 源码里没有换算点（balance 只出现在 zod schema 与 redaction 关键词表），
// 所以这个系数是推断而非实测。若核对后发现单位不同，**只改这一个常数**。
const balanceScale = 100_000

// clientHeaders 客户端标识头。
//
// 推理与账号端点都需要。只带 Authorization 时实测虽可通，但这些头是官方
// 客户端的身份声明，缺失可能在某些网关策略下被拒或降级，故照官方原样下发。
var clientHeaders = map[string]string{
	"HTTP-Referer":   "https://cline.bot",
	"X-Title":        "Cline",
	"X-IS-MULTIROOT": "false",
	"X-CLIENT-TYPE":  "cline-sdk",
}

// CliReasoningEffort 一档思考强度。
//
// ⚠ id 与 name **刻意不同**（wire 值 ≠ 展示名）：
// Cline IDE 的档位菜单是 None/Low/Medium/High/**Extra**，
// 而上游认的 wire 值是 none/low/medium/high/max。
//
// 对应关系是**行为实测**出来的（实测 reasoning 字符数）：
//
//	不传/none → 0（不传 = 不思考）
//	low       → 67
//	medium    → 379
//	high      → 294
//	xhigh     → 259（与 high 无可辨差异 → **伪档位，跳过**）
//	max       → 1192（high 的 4 倍 → 真正的最高档）
//
// 若只按名字对齐（xhigh → 显示成 XHigh），会给用户一个实测无差异的档位，
// 而真正的最高档 max 反被跳过。
type CliReasoningEffort struct {
	ID   string
	Name string
}

// reasoningEfforts 档位表（**所有模型共用一张**）。
//
// 远端不下发档位：/api/v1/models 只有 {id,object,created,owned_by}，
// recommended-models 只有 {id,name,description,tags}，sidecar 里也没有任何
// 模型详情端点。档位只存在于客户端内嵌表，而那张表覆盖不了远端 460 个 id。
//
// ⚠ 已知局限：对不在内嵌目录里的模型，档位是猜的。但上游对不认识的档位
// **静默忽略而不报错**（实测 reasoning_effort: 'banana' 返回 200 且思考量为 0），
// 所以最坏情况是"开关无效"，不会是"请求失败"。
var reasoningEfforts = []CliReasoningEffort{
	{ID: "none", Name: "None"},
	{ID: "low", Name: "Low"},
	{ID: "medium", Name: "Medium"},
	{ID: "high", Name: "High"},
	{ID: "max", Name: "Extra"},
}

// defaultReasoningEffort 默认档位。
//
// ⚠ 声明默认档会改变行为：实测"不传 reasoning_effort → 模型完全不思考"，
// 而 DSH 在用户未手动选择时会采用 model.reasoning.defaultEffort。
// 即声明后从"默认不思考"变成"默认 High 思考"，与 IDE 一致，
// 代价是思考 token 计入 completion_tokens。这是用户明确要求的变更。
const defaultReasoningEffort = "high"

// ReasoningEfforts 导出档位表供装配层/测试使用。
func ReasoningEfforts() []CliReasoningEffort {
	out := make([]CliReasoningEffort, len(reasoningEfforts))
	copy(out, reasoningEfforts)
	return out
}

// DefaultReasoningEffort 导出默认档位。
func DefaultReasoningEffort() string { return defaultReasoningEffort }

// Auth 一份 Cline 凭证（对外统一命名，见参照 cline.ts:27-51）。
type Auth struct {
	// AccessToken 访问令牌，**必须保留服务端下发的 workos: 前缀**。
	//
	// 实测：剥掉前缀即 401。见 clineBearerValue 的幂等补齐。
	AccessToken string `json:"access_token"`
	// RefreshToken 续期令牌。空 = 不可续期。
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpireTime 访问令牌过期时刻（**毫秒**时间戳）。
	ExpireTime int64 `json:"expire_time,omitempty"`
	// AccountID Cline 账号 id，形如 usr-01M3BCV4FYCGJKAWD3MJG3DBQM。
	//
	// ⚠ 余额端点必须用它，**不是** JWT 的 sub（user_…）：
	// 实测传 sub 返回 400 {"error":"Invalid request format"}。
	// 两者形态完全不同（usr-… vs user_…），极易混用。
	AccountID string `json:"account_id,omitempty"`
	// Email 账号邮箱（展示用，也是昵称的首选来源）。
	Email string `json:"email,omitempty"`
	// Nickname 展示名。
	Nickname string `json:"nickname,omitempty"`
	// FilePath 凭证文件落点（核心删除账号时要连文件一起删）。
	FilePath string `json:"-"`
}

// UID 账号池主键。
//
// 优先级：AccountID > Email > AccessToken 哈希。
//
// 为什么 AccountID 优先：它是服务端给的稳定凭据（usr-…），
// 换 token 不会变，而 token 每次续期都会变。
func (a *Auth) UID() string {
	if a == nil {
		return ""
	}
	if a.AccountID != "" {
		return a.AccountID
	}
	if a.Email != "" {
		return a.Email
	}
	if a.AccessToken != "" {
		sum := sha256.Sum256([]byte(a.AccessToken))
		return "cline-" + hex.EncodeToString(sum[:8])
	}
	return ""
}

// clineBearerValue 给 token 幂等地补 `workos:` 前缀。
//
// 参照 cline.ts:178-182：
//
//	token = accessToken.trim()
//	空 → ''
//	已以 workos: 开头 → 原样
//	否则 → 'workos:' + token
//
// # 为什么必须幂等补齐
//
// 实测：**续期返回的是裸 JWT**（不带前缀），而登录返回的带前缀。
// 两个来源形态不同，若只在登录时补一次，续期后就会变成裸 token → 401。
// 幂等补齐让两种形态都能得到正确结果。
func clineBearerValue(token string) string {
	t := strings.TrimSpace(token)
	if t == "" {
		return ""
	}
	if strings.HasPrefix(t, tokenPrefix) {
		return t
	}
	return tokenPrefix + t
}

// parseTimestamp 把上游的时间值归一成毫秒时间戳。
//
// 参照 cline.ts:89-99：
//
//	number 且有限且 > 0：< 1e12 → 秒（×1000）；否则 → 已是毫秒
//	string 且非空    → 按 RFC3339 解析（实测上游给 ISO 8601 字符串）
//	其它             → 0（未知）
//
// ⚠ 同时接受数字（秒/毫秒）：上游实测给 ISO 字符串，但要为格式变更留鲁棒性。
func parseTimestamp(v any) int64 {
	switch t := v.(type) {
	case float64:
		if t <= 0 || t != t { // NaN 自比不相等
			return 0
		}
		if t < 1e12 {
			return int64(t * 1000)
		}
		return int64(t)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		// 先试 RFC3339（上游实测形态："2026-09-25T05:23:47.000Z"）
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.UnixMilli()
		}
		// 再宽容一点：不带时区的 ISO 形态
		if ts, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
			return ts.UnixMilli()
		}
		// 最后试纯数字字符串
		if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
			if n < 1e12 {
				return int64(n * 1000)
			}
			return int64(n)
		}
		return 0
	}
	return 0
}

// credentialPayload 从 token 响应里解析出的字段（参照 cline.ts:121-157 的
// parseClineTokenPayload）。
type credentialPayload struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // 毫秒；0 = 上游没给
	AccountID    string
	Email        string
	DisplayName  string
}

// readString 取第一个"是非空字符串"的值（返回 trim 后的）。
//
// 参照 cline.ts:73-79：字段名同时接受驼峰与下划线形态，以对上游变更鲁棒。
func readString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// parseTokenPayload 解析 token 响应（登录 register 与续期 refresh 同构）。
//
// 参照 cline.ts:114-157。判据要点：
//
//  1. **判据是 `success && data.accessToken`，不是裸 accessToken** ——
//     只看裸字段会把失败信封当成成功（参照 AGENTS.md:1864-1865）。
//     ⚠ `success` 字段缺失时不据此判失败：上游某些响应（如裸响应形态）
//     不带该字段，那种情况下只能靠 accessToken 是否为空来判。
//  2. 兼容 data 信封与裸响应（上游某天直接返回 {accessToken,...} 仍能解析）
//  3. 字段名同时接受驼峰与下划线
//  4. accountId / email 优先从 userInfo 取，回落到顶层
func parseTokenPayload(raw []byte) (credentialPayload, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return credentialPayload{}, fmt.Errorf("cline: token 响应不是 JSON: %w", err)
	}
	root, ok := v.(map[string]any)
	if !ok {
		return credentialPayload{}, fmt.Errorf("cline: token 响应不是对象")
	}

	// 显式 success:false → 失败信封，即使它带了 data 也不能当成功。
	if s, ok := root["success"].(bool); ok && !s {
		msg := readString(root, "error", "message")
		if msg == "" {
			msg = "服务端返回失败"
		}
		return credentialPayload{}, fmt.Errorf("cline: token 响应失败: %s", msg)
	}

	// 兼容 data 信封
	inner := root
	if d, ok := root["data"].(map[string]any); ok {
		inner = d
	}

	p := credentialPayload{
		AccessToken:  readString(inner, "accessToken", "access_token"),
		RefreshToken: readString(inner, "refreshToken", "refresh_token"),
	}
	// expiresAt / expires_at / expire_time 三种都认
	for _, k := range []string{"expiresAt", "expires_at", "expire_time"} {
		if v, ok := inner[k]; ok {
			if ts := parseTimestamp(v); ts > 0 {
				p.ExpiresAt = ts
				break
			}
		}
	}

	if ui, ok := inner["userInfo"].(map[string]any); ok {
		p.AccountID = readString(ui, "clineUserId", "accountId")
		p.Email = readString(ui, "email")
		first := readString(ui, "firstName")
		last := readString(ui, "lastName")
		p.DisplayName = strings.TrimSpace(first + " " + last)
	} else {
		p.AccountID = readString(inner, "accountId", "account_id")
		p.Email = readString(inner, "email")
	}
	// userInfo 里没给 accountId/email 时回落到顶层
	if p.AccountID == "" {
		p.AccountID = readString(inner, "accountId", "account_id")
	}
	if p.Email == "" {
		p.Email = readString(inner, "email")
	}
	return p, nil
}

// buildCredential 由 token 载荷合成一份凭证（参照 cline.ts:184-204）。
//
// fallback 用于补全响应里缺失的字段（续期响应不带 nickname 等）。
//
// 昵称优先级：email > displayName > accountId。
// 取邮箱的理由：它**唯一且稳定**（参照 cline.ts:192）。
func buildCredential(p credentialPayload, fallback *Auth) *Auth {
	a := &Auth{
		AccessToken: clineBearerValue(p.AccessToken),
	}
	if fallback != nil {
		a.RefreshToken = fallback.RefreshToken
		a.ExpireTime = fallback.ExpireTime
		a.AccountID = fallback.AccountID
		a.Email = fallback.Email
		a.Nickname = fallback.Nickname
		a.FilePath = fallback.FilePath
	}
	// 响应里的值覆盖 fallback（仅在有值时覆盖，避免把已有的清空）
	if p.RefreshToken != "" {
		a.RefreshToken = p.RefreshToken
	}
	if p.ExpiresAt > 0 {
		a.ExpireTime = p.ExpiresAt
	}
	if p.AccountID != "" {
		a.AccountID = p.AccountID
	}
	if p.Email != "" {
		a.Email = p.Email
	}
	// 昵称：优先邮箱（稳定），其次 displayName，最后 accountId
	// ⚠ 与续期合并规则一致：续期**不更新**已有的 nickname
	if a.Nickname == "" {
		switch {
		case a.Email != "":
			a.Nickname = a.Email
		case p.DisplayName != "":
			a.Nickname = p.DisplayName
		default:
			a.Nickname = a.AccountID
		}
	}
	return a
}

// isExpired 报告凭证是否已过期（参照 cline.ts:245-248）。
//
// ⚠ 只读 ExpireTime，没有 JWT 回退（与 raccoon 不同）。
// ⚠ 无过期时间时**保守视为未过期**，交给服务端 401 判定。
func (a *Auth) isExpired(nowMS int64) bool {
	if a == nil || a.ExpireTime <= 0 {
		return false
	}
	return a.ExpireTime <= nowMS
}

// Renewable 报告凭证是否可以续期（参照 cline.ts:240-242）。
//
// ⚠ 判据是"有 refresh_token"，**与过期与否无关** ——
// 未过期但无 refresh_token 的凭据同样无法续期。
// ⚠ 注意：**不做 trim**（与某些上游的判据不同）。
func (a *Auth) Renewable() bool {
	return a != nil && a.RefreshToken != ""
}

// needsRefresh 报告是否该提前续期。
//
// lead 是提前量：距过期不足 lead 就该续（对齐参照 refresh.ts 的 1 小时前瞻）。
func (a *Auth) needsRefresh(now time.Time, lead time.Duration) bool {
	if !a.Renewable() {
		return false
	}
	if a.ExpireTime <= 0 {
		// 无过期信息：不主动续（保守），交给 401 自愈路径。
		return false
	}
	return a.ExpireTime <= now.Add(lead).UnixMilli()
}

// shortUID 日志用的短 uid（避免把完整 token/邮箱打进日志）。
func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8] + "…"
}
