// raccoon.go Raccoon Work（商汤小浣熊）的协议常量与纯函数。
//
// # 事实来源
//
// 协议事实来自参照项目 dsh-codearts-auth（TypeScript，已实测跑通）的
// src/raccoon*.ts 与 AGENTS.md。本包是它的 Go 移植，**判据照搬，不自行探究**。
//
// # 与其它上游的关键差异
//
//	统一信封      {code,message,details,data}，code===0 成功
//	              ⚠ 失败可能是 HTTP 200 + 非 0 code，**两种都要按 code 判**
//	登录          微信扫码（**code 由客户端本地随机生成，服务端接受任意自造 code**）
//	              或短信（依赖浏览器执行阿里云滑块 → 纯 Go 无法程序化完成）
//	手机号        AES-128-CFB 加密（key "senseraccoon2023"）
//	过期判定      expires_at → JWT exp **回退是必需的**（否则续期静默失效）
//	无每日签到    服务端按日自动发放，**没有端点**
package raccoon

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// providerID 上游标识。
const providerID = "raccoon"

// ProviderID 导出上游标识，供装配层（cmd/server）使用。
const ProviderID = providerID

// DefaultAPIBase 上游基址。四个前缀都挂它。
const DefaultAPIBase = "https://xiaohuanxiong.com"

// 四个路径前缀（参照 raccoon.ts:28-34）。
const (
	authPrefix    = "/api/web/auth/v1"
	llmPrefix     = "/api/web/llm/v2"
	pointsPrefix  = "/api/web/points/v1"
	desktopPrefix = "/api/web/desktop/v1"
)

// 端点路径。
const (
	// qrcodeLoginPath 轮询扫码状态（唯一必需头：Content-Type，**不带 Authorization**）。
	qrcodeLoginPath = authPrefix + "/login_with_qrcode_code"
	// authorizationCodePath 用授权码换凭证（浏览器登录链路的第二跳）。
	//
	// 实测（2026-10-05，真实账号）：POST {authorization_code} → {"code":0}
	// + access_token / refresh_token；失效码 → HTTP 400 + code 200035。
	authorizationCodePath = authPrefix + "/login_with_authorization_code"
	// refreshPath 续期。
	refreshPath = authPrefix + "/refresh"
	// userInfoPath 用户信息。
	userInfoPath = authPrefix + "/user_info"
	// chatPath 标准 OpenAI 兼容 + 标准 SSE。
	chatPath = llmPrefix + "/chat/completions"
	// modelCatalogPath 模型目录。
	modelCatalogPath = llmPrefix + "/model_catalog"
	// balancePath 积分余额。
	balancePath = pointsPrefix + "/balance"
	// loginGrantPath 一次性登录奖励（**无 body**，需 X-Client-Platform）。
	loginGrantPath = desktopPrefix + "/login/points/grant"
)

// 常量（参照 raccoon.ts:43-69、raccoon-product.ts:148-162）。
const (
	// phoneCipherSecret 手机号加密密钥（16 字节 → AES-128）。
	//
	// ⚠ 这是**公开常量**（硬编码在前端 bundle 里），只用于防止手机号明文
	// 出现在日志/代理里，**不是安全边界**。
	phoneCipherSecret = "senseraccoon2023"

	// qrPollIntervalMS 扫码轮询间隔。
	qrPollIntervalMS = 2_000
	// loginTimeoutMS 登录整体超时。
	loginTimeoutMS = 5 * 60 * 1000
	// requestTimeoutMS 单次请求超时。
	requestTimeoutMS = 60_000
	// catalogTimeoutMS 模型目录超时（比其它端点短）。
	catalogTimeoutMS = 20_000

	// tokenRefreshWindowSeconds 提前续期窗口（照抄官方 scheduleAuth.js）。
	//
	// access_token 寿命约 3 小时（实测 exp - nbf = 10805s）。
	tokenRefreshWindowSeconds = 300

	// clientPlatform X-Client-Platform 取值。
	//
	// ⚠ 是 `desktop/v1/login/points/grant` 的**准入条件**：
	// 取值必须是 desktop-windows / desktop-macos / desktop-linux，猜错会被拒。
	clientPlatform = "desktop-windows"
	// clientVersion X-Client-Version 取值（带 v 前缀）。
	clientVersion = "v1.0.35"

	// loginRewardPoints 登录奖励兜底积分（popup.points 缺失时用）。
	loginRewardPoints = 3000

	// loginRewardEventName 登录奖励在账单里的 `event_name`。
	//
	// ⚠ 判「登录奖励是否已领」必须同时匹配 `biz_type == "reward_grant"`
	// **与**本常量 —— 「新人注册礼包」也是 reward_grant，
	// 只判 biz_type 会让**新用户一开始就显示「已领取」**。
	loginRewardEventName = "桌面端登录奖励"
)

// envelope 统一业务信封。
//
// ⚠ code === 0 为成功。**失败可能是 HTTP 200 + 非 0 code** ——
// 只看状态码会把业务失败当成功。
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Details string          `json:"details"`
	Data    json.RawMessage `json:"data"`
}

// envelopeError 把信封的错误信息拼成可读文案（服务端文案优先）。
func envelopeError(e envelope) error {
	msg := strings.TrimSpace(e.Message)
	det := strings.TrimSpace(e.Details)
	switch {
	case msg != "" && det != "":
		return fmt.Errorf("raccoon: %s: %s", msg, det)
	case msg != "":
		return fmt.Errorf("raccoon: %s", msg)
	case det != "":
		return fmt.Errorf("raccoon: %s", det)
	default:
		return fmt.Errorf("raccoon: 业务失败（code=%d）", e.Code)
	}
}

// Auth 一份 Raccoon 凭证（参照 raccoon.ts:78-106）。
type Auth struct {
	// AccessToken JWT（服务端下发）。
	AccessToken string `json:"access_token"`
	// RefreshToken 续期令牌。
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt 过期时间，**毫秒时间戳的字符串**。
	//
	// ⚠ 可选。老凭据/手工导入可能没有它 —— 此时过期判定必须回退到 JWT exp，
	// 否则判定恒为 false，续期静默失效。
	ExpiresAt string `json:"expires_at,omitempty"`
	// OfficeIdentity `personal` 或组织码。作为 X-Org-Code 发出（空串也照发）。
	OfficeIdentity string `json:"office_identity,omitempty"`
	// UserID 用户 id（远端 user_info.id）。
	UserID string `json:"user_id,omitempty"`
	// Nickname 展示名。
	//
	// ⚠ 它是服务端**自动生成**的默认名（实测形如 RaccoonAva），
	// 微信扫码不回传微信昵称 —— 故**不适合做多账号区分**。
	Nickname string `json:"nickname,omitempty"`
	// Phone 绑定/注册手机号。
	Phone string `json:"phone,omitempty"`
	// DeviceID 设备指纹（32 位 hex），用于 X-Client-Device-ID。
	DeviceID string `json:"device_id,omitempty"`
	// FilePath 凭证文件落点（核心删除账号时要连文件一起删）。
	FilePath string `json:"-"`
}

// UID 账号池主键。
//
// 优先级：UserID > Phone > AccessToken 哈希。
// UserID 是服务端给的稳定 id，换 token 不会变。
func (a *Auth) UID() string {
	if a == nil {
		return ""
	}
	if a.UserID != "" {
		return a.UserID
	}
	if a.Phone != "" {
		return a.Phone
	}
	if a.AccessToken != "" {
		sum := sha256.Sum256([]byte(a.AccessToken))
		return "raccoon-" + hex.EncodeToString(sum[:8])
	}
	return ""
}

// DisplayUID 展示用标识。
//
// 参照 jet-hub-rpc.ts:246-265 的昵称取法：
// `nickname (手机号后 4 位)` → `nickname (user_id)` → `nickname` → 空。
//
// ⚠ 加上后缀是必要的：nickname 是服务端自动生成的默认名，
// 多账号时很可能重名，只显示它会分不清谁是谁。
func (a *Auth) DisplayUID() string {
	if a == nil {
		return ""
	}
	switch {
	case a.Nickname != "" && len(a.Phone) >= 4:
		return fmt.Sprintf("%s (%s)", a.Nickname, a.Phone[len(a.Phone)-4:])
	case a.Nickname != "" && a.UserID != "":
		return fmt.Sprintf("%s (%s)", a.Nickname, a.UserID)
	default:
		return a.Nickname
	}
}

// ── 手机号加密（参照 raccoon.ts:175-201）─────────────────────────────────

// EncryptPhone 用 AES-128-CFB 加密手机号，输出 Base64(iv ‖ ciphertext)。
//
//	key  = UTF8("senseraccoon2023") → 16 字节 ⇒ AES-128
//	iv   = 随机 16 字节
//	mode = CFB, padding = NoPadding
//
// ⚠ **必须显式 aes-128**：写成 aes-256-cfb 会因密钥长度不足抛错（不自动补齐）。
// ⚠ CFB 是流密码，NoPadding 与 PKCS7 输出**完全一致**（实测 11 字节手机号
// 两种设置下密文都是 11 字节），Go 的 cipher.NewCFBEncrypter 天然不填充，
// 与 CryptoJS 的 NoPadding 对齐。
func EncryptPhone(phone string) (string, error) {
	key := []byte(phoneCipherSecret)
	if len(key) != 16 {
		return "", fmt.Errorf("raccoon: 手机号加密密钥长度 %d，应为 16（AES-128）", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	ct := make([]byte, len(phone))
	cipher.NewCFBEncrypter(block, iv).XORKeyStream(ct, []byte(phone))
	out := make([]byte, 0, len(iv)+len(ct))
	out = append(out, iv...)
	out = append(out, ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// ── 二维码 code 与承载 URL（参照 raccoon.ts:15、raccoon-oauth.ts:116-130）──

// GenerateQRCode 生成 32 位小写 hex 的扫码 code。
//
// # ⚠ 关键事实：code 由**客户端本地**随机生成，服务端接受任意自造 code
//
// 实测报文（AGENTS.md:3539-3541）：
//
//	POST /api/web/auth/v1/login_with_qrcode_code  {"qrcode_code":"1790405291292abcdef123456"}
//	→ 200 {"code":0,"message":"success","data":{"status":"pending"}}
//
// 对齐客户端 `CryptoJS.lib.WordArray.random(16).toString()`。
// 正因如此，我们**完全不需要**官方 `office-raccoon://auth/callback` 那条链路
// （那条链路也不可用：回调地址写死在 Web bundle 里，改不成 localhost）。
func GenerateQRCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// QRLoginURL 构造二维码要承载的页面 URL。
//
//	https://xiaohuanxiong.com/login/mp?code=<32位hex>&appname=商汤小浣熊官网
//
// ⚠ appname 必须 URL-encode（参照实现用 URLSearchParams）。
func QRLoginURL(code string) string {
	q := url.Values{}
	q.Set("code", code)
	q.Set("appname", "商汤小浣熊官网")
	return DefaultAPIBase + "/login/mp?" + q.Encode()
}

// QRStatus 扫码状态（参照 raccoon.ts:64-69）。
type QRStatus string

const (
	// QRStatusPending 未扫码。
	QRStatusPending QRStatus = "pending"
	// QRStatusLogging 已扫码待确认。
	QRStatusLogging QRStatus = "logging"
	// QRStatusCanceled 用户取消。
	QRStatusCanceled QRStatus = "canceled"
	// QRStatusSuccess 登录完成。
	QRStatusSuccess QRStatus = "success"
)

// NormalizeQRStatus 把未知状态**降级为 pending**。
//
// ⚠ 降级方向的理由（参照 raccoon-oauth.ts:135-139, 179）：
//
//	误判成 success  → 流程拿到空 token 后卡死
//	误判成 canceled → 用户正在扫的二维码被无故刷新
//
// 两者都比"多等一轮"糟，所以未知一律 pending。
func NormalizeQRStatus(s string) QRStatus {
	switch QRStatus(strings.ToLower(strings.TrimSpace(s))) {
	case QRStatusPending:
		return QRStatusPending
	case QRStatusLogging:
		return QRStatusLogging
	case QRStatusCanceled:
		return QRStatusCanceled
	case QRStatusSuccess:
		return QRStatusSuccess
	default:
		return QRStatusPending
	}
}

// ── 过期判定（参照 raccoon.ts:115-162）────────────────────────────────────

// decodeJWTExpMS 从 JWT payload 里取 exp（毫秒）。
//
// ⚠ **只解码、不验签**；任何异常都返回 0（表示"取不到"），不抛错。
func decodeJWTExpMS(token string) int64 {
	if token == "" {
		return 0
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	// base64url 解码（JWT 用 RawURLEncoding，无 padding）
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 宽容一点：有些实现带 padding
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return 0
		}
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

// ExpiresAtMS 返回凭证的绝对过期时刻（毫秒）。
//
// # ⚠ 回退到 JWT 是**必需的**，不是锦上添花
//
// expires_at 是可选的（老凭据/手工导入可能没有）。只读它会让过期判定
// **恒为 false**，于是 refreshAll 永远跳过这些账号 ——
// 表现为"凭据悄悄过期、续期从不触发"，**静默失效、无任何报错**。
//
// 返回 0 表示"完全没有过期信息"（调用方据此视为不过期）。
func (a *Auth) ExpiresAtMS() int64 {
	if a == nil {
		return 0
	}
	if s := strings.TrimSpace(a.ExpiresAt); s != "" {
		if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 && n == n {
			return int64(n)
		}
	}
	return decodeJWTExpMS(a.AccessToken)
}

// IsExpired 报告凭证是否已过期。
//
// ⚠ 无任何过期信息时**保守视为不过期**：宁可用一个可能过期的凭据去试
// （服务端会回 401，我们能识别并续期），也不凭猜测阻止用户。
func (a *Auth) IsExpired(now time.Time) bool {
	ms := a.ExpiresAtMS()
	if ms <= 0 {
		return false
	}
	return ms <= now.UnixMilli()
}

// Renewable 报告凭证是否可以续期。
//
// ⚠ 判据会 trim（与 Cline 的判据不同 —— 那边不 trim）：纯空白的
// refresh_token 在这里视为不可续期。
func (a *Auth) Renewable() bool {
	return a != nil && strings.TrimSpace(a.RefreshToken) != ""
}

// needsRefresh 报告是否该提前续期。
//
// lead 是提前量。参照实现用共享调度器的 1 小时前瞻；
// tokenRefreshWindowSeconds（5 分钟）是官方 scheduleAuth.js 的值。
func (a *Auth) needsRefresh(now time.Time, lead time.Duration) bool {
	if !a.Renewable() {
		return false
	}
	ms := a.ExpiresAtMS()
	if ms <= 0 {
		return false // 无过期信息：不主动续，交给 401 自愈
	}
	return ms <= now.Add(lead).UnixMilli()
}

// ── 模型展示名（参照 raccoon.ts:220-262）─────────────────────────────────

// formatMultiplier 最多 4 位小数并去掉尾随 0（参照 String(Number(v.toFixed(4)))）。
func formatMultiplier(v float64) string {
	rounded := roundTo(v, 4)
	// Go 的 %g 会去掉尾随 0；对 0.75 → "0.75"、1.0 → "1"
	return strconv.FormatFloat(rounded, 'g', -1, 64)
}

func roundTo(v float64, digits int) float64 {
	p := 1.0
	for i := 0; i < digits; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5*sign(v))) / p
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// DisplayName 生成模型展示名（倍率**必须拼进 name**，不是 description）。
//
//	name = description 非空 ? description : id
//	effective 非有限 或 < 0 → 不加后缀（真正"取不到"的情形）
//	effective === 0         → `${name} · 免费`
//	base > 0 且 base > effective → `${name} · x{base}→x{effective}`
//	否则（**含 1 倍**）      → `${name} · x{effective}`
//
// ⚠ **1 倍也要显示**（真实缺陷，用户报障过）：
// 早期有 `if effective == 1 { return name }`，结果用户无法区分
// "它就是 1 倍"与"我们没取到它的倍率"。
//
// ⚠ 但 NaN / 负数**仍然**不加后缀 —— 那才是真正取不到的情形。
// 这两条不要合并（合并会把"取不到"显示成倍率，比不显示更误导）。
func DisplayName(id, description string, baseMultiplier, effectiveMultiplier float64) string {
	name := strings.TrimSpace(description)
	if name == "" {
		name = id
	}
	eff := effectiveMultiplier
	if eff != eff || eff < 0 { // NaN 或负数
		return name
	}
	if eff == 0 {
		return name + " · 免费"
	}
	if baseMultiplier > 0 && baseMultiplier > eff {
		return fmt.Sprintf("%s · x%s→x%s", name, formatMultiplier(baseMultiplier), formatMultiplier(eff))
	}
	return fmt.Sprintf("%s · x%s", name, formatMultiplier(eff))
}

// ── 其它 ────────────────────────────────────────────────────────────────

// shortUID 日志用的短 uid。
func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8] + "…"
}
