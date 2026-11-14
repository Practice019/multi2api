// credential.go PhanthyWork 一份凭证的**全部本地状态**：落盘结构、解析、加载、
// 投影，以及"这份凭证现在能不能用"所依赖的那些方法。
//
// # 为什么把这么多东西放在一份凭证里（这是本包最关键的架构决定）
//
// 报告 §7.1 第 5 条与用户的硬要求指向同一件事：
//
//	多账号隔离：三把密钥 / installationId / 推理令牌**全部按账号隔离**
//
// 而"按账号隔离"有两种写法：
//
//	① Provider 上挂 map[uid]Xxx —— 要在每个写入口上记得加锁、记得按 uid 取、
//	   记得在账号被删时清理；三份状态就是三张表，任何一处漏了都表现为
//	   "A 号的令牌签了 B 号的请求"。
//	② 状态**长在凭证对象上** —— 凭证对象天然按账号一份，账号池持有的就是
//	   同一个指针（gateway.CredentialRefresher 的契约正是"原地更新
//	   cred.Secret 指向的那份"）。于是"隔离"不再是需要遵守的规矩，
//	   而是**类型的直接后果**。
//
// 本包选 ②：三把密钥、installationId 权威、推理令牌缓存、时钟偏移、
// 刷新并发闸，全部是 *Auth 的字段。
//
// ⚠ 唯一的例外是 HTTP 客户端的时钟（Client.skew）：登录流程发生在
// "还没有账号"的时候。但**签名用的时间口径**一律取 a.serverNowMS()。
package phanthywork

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/gateway"
)

// ── 常量 ─────────────────────────────────────────────────────────────────

const (
	// filePrefix 凭证文件名前缀。LoadDir 按 phanthywork-*.json 匹配。
	//
	// ⚠ 前缀**必须**是这个：LoadDir 会在同一目录里同时看到别的上游的凭证
	// （auths/ 根目录下混放是多上游部署的常态），前缀写宽了会把别家的文件
	// 当自己的解析 —— 那一类现象的形态是"少了一个号"或"多了一个幽灵号"，
	// 而两者都不报错。
	filePrefix = "phanthywork"

	// fileExt 凭证文件后缀。
	fileExt = ".json"

	// registrationCooldown 设备注册的熔断窗口（24 小时）。
	//
	// 报告 §2.4 引开源实现的 REGISTRATION_COOLDOWN_MS：409 之后如果每次启动都
	// 重发一次注册，等于持续触发服务端的设备风控，还可能把这个账号的**其它调用**
	// 一起拖下水。所以期内**零请求**。
	registrationCooldown = 24 * time.Hour

	// rootKeyOAuth / rootKeyAccount 落盘 JSON 的两个根键。
	//
	// 与其它上游同形（嵌套形），核心的通用凭证解析器也认这个形状。
	rootKeyOAuth   = "auth"
	rootKeyAccount = "account"
)

// ── 子结构（字段与 JSON 标签一一对应）────────────────────────────────────

// OAuthTokens OAuth 层（登录/续期拿到的两把令牌）。
type OAuthTokens struct {
	// AccessToken Bearer（RS256 JWT，实测 761 字符，exp-iat = 1800 秒）。
	AccessToken string `json:"access_token"`
	// RefreshToken 64 字符不透明串。**每次续期会轮换**（坑⑤），所以
	// 内存里这份永远是"当前唯一有效的那一个"。
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAtMS access_token 的到期时刻（**毫秒**）。
	//
	// ⚠ 单位是毫秒而不是秒：本包内部一律毫秒，转换只发生在 ExpiresAt()
	// 这一个**对外**的方法里（本仓踩过"按毫秒读秒字段 ⇒ 2027 年的号全被判成
	// 已过期"）。
	ExpiresAtMS int64 `json:"expires_at_ms,omitempty"`
	// Scopes 服务端下发的 scope 列表（6 个，逐字）。
	Scopes []string `json:"scopes,omitempty"`
}

// DeviceKey 设备绑定的两把**跨重启不变**的密钥。
//
// identity 进 body.public_key，device 进 body.device_public_key。
// 两者都由 identity 种子决定（device 是它换域派生出来的），所以只存种子。
type DeviceKey struct {
	// IdentitySeed B64 identity 私钥种子（32 字节 → base64url）。
	IdentitySeed string `json:"identity_seed,omitempty"`
	// InstallationID 服务端在注册响应里返回的 installation.id。
	//
	// ⚠ 它**可能**与本地派生的 di_… 不同（实测注册响应给的是 cmwctrcoe…
	// 那种 cuid），所以请求头 x-desktop-installation-id 一律用**本地派生的**
	// 那个（见 installationID）。这个字段只用于排查与展示，不参与签名。
	InstallationID string `json:"installation_id,omitempty"`
}

// RegistrationState 设备注册的状态机（终态与熔断都在这里）。
type RegistrationState struct {
	// RegisteredAtMS 注册成功（HTTP 200）的时刻。
	RegisteredAtMS int64 `json:"registered_at_ms,omitempty"`
	// LastAttemptMS 最后一次注册尝试的时刻（**成功失败都记**）。
	LastAttemptMS int64 `json:"last_attempt_ms,omitempty"`
	// ConflictAtMS 收到 409 desktop_installation_conflict 的时刻。
	//
	// 报告 §3⑥：409 是**终态**，换公式/换公钥/重跑注册都是同一条 409。
	ConflictAtMS int64 `json:"conflict_at_ms,omitempty"`
	// ReasonCode 服务端给的 reason（如 first_device_owner）。
	ReasonCode string `json:"reason_code,omitempty"`
}

// InferenceKey 推理面密钥。这一把**每次注册换新**（报告 §2.2）。
type InferenceKey struct {
	// Seed B64 推理私钥种子。
	Seed string `json:"seed,omitempty"`
	// PublicKey B64 推理公钥（SPKI/base64url）。
	//
	// ⚠ 它是 device canonical 第 5 段（坑⑩）与 activities/summary 请求体里
	// 唯一那个字段的值。
	PublicKey string `json:"public_key,omitempty"`
}

// InferenceTokenCache 180 秒推理令牌的内存缓存。
//
// ⚠ **刻意不落盘**：TTL 只有 180 秒（比 access_token 短一个数量级），
// 写进凭证文件只会让"文件里那份"在 3 分钟后变成误导。账号池拿到的是同一个
// *Auth 指针，进程内缓存足够。
//
// ⚠ 刻意**不带** JSON 标签：它不该出现在 MarshalAuthFile 的产物里；
// 带上标签就多一条"哪天有人顺手把整个 Auth 序列化出去"的路径。
type InferenceTokenCache struct {
	Token        string
	ExpAtMS      int64
	ObtainedAtMS int64
}

// ── 主结构 ───────────────────────────────────────────────────────────────

// Auth 一份 PhanthyWork 凭证。
//
// 它同时是：
//
//	gateway.Credential.Secret 里的那个"核心不许读"的不透明结构
//	LoadDir / ParseCredential 的产物
//
// 把落盘与内存合成一个类型是刻意的：任何第二份结构体都会带来"哪一份是权威"
// 的问题，而权威必须唯一（本仓反复吃过"续期写到另一个对象上"）。
type Auth struct {
	Provider string            `json:"-"` // 运行时归属，不进文件（目录已经说明了一切）
	OAuth    OAuthTokens       `json:"oauth"`
	Device   DeviceKey         `json:"device,omitempty"`
	Reg      RegistrationState `json:"registration,omitempty"`
	Infer    InferenceKey      `json:"inference,omitempty"`
	// UIDValue 账号主键的显式来源（手写凭证可以只写它）。
	UIDValue string `json:"uid,omitempty"`
	// AccountScope 账号 uuid —— **签名要用的那个**（= access token 的 sub）。
	AccountScope string `json:"account_scope,omitempty"`
	// NicknameValue 展示名（服务端给的是脱敏手机号 +86 150****3411）。
	NicknameValue string `json:"nickname,omitempty"`
	// FilePath 凭证文件落点（**不进 JSON**：它是运行时事实，不是内容）。
	FilePath string `json:"-"`

	// baseSkewMS 配置里给的时钟偏移基线（毫秒）。0 = 没给。
	baseSkewMS int64
	// skewMS **测出来的**时钟偏移（毫秒，本机 - 服务端）。
	// 用原子读写：签名发生在请求路径（并发），校准发生在换令牌之后。
	skewMS atomic.Int64
	// skewCalibrated 是否已经拿到过 server_time（没校准时用 baseSkewMS）。
	skewCalibrated atomic.Bool

	// dailyMu 保护下面两个"签到相关"的字段。
	dailyMu sync.Mutex
	// dailyDate **服务端**给的日键（UTC+8 换日）—— 签到幂等键必须用它，
	// 不许本地算日期（本地算在跨零点/时区差时会算出两个日键 ⇒ 发出两条）。
	dailyDate string
	// dailyStatus 服务端给的最后一次签到状态字面量（granted_today / already_granted …）。
	dailyStatus string

	// tokenMu 保护下面的推理令牌缓存。
	tokenMu sync.RWMutex
	token   InferenceTokenCache
	// gate 刷新并发闸（见 refreshGate）。**每个账号一个**。
	gate refreshGate
}

// EffectiveProvider 返回该凭证的上游 id。
func (a *Auth) EffectiveProvider() string {
	if a == nil || a.Provider == "" {
		return ProviderID
	}
	return a.Provider
}

// jwtClaims 本包从 access token 里读出来的字段。
//
// 只读**本地**时间戳与身份（不做验签 —— 我们只管用，不管验）。
type jwtClaims struct {
	Sub   string  `json:"sub"`
	Sid   string  `json:"sid"`
	Scope string  `json:"scope"`
	Iat   float64 `json:"iat"`
	Exp   float64 `json:"exp"`
}

// ParseJWTClaims 解出 access token 的 payload（解不开返回 ok=false，不报错）。
func ParseJWTClaims(token string) (jwtClaims, bool) {
	t := strings.TrimSpace(token)
	if i := strings.Index(t, ":"); i >= 0 && i < len(t)-1 {
		t = t[i+1:]
	}
	parts := strings.Split(t, ".")
	if len(parts) < 2 {
		return jwtClaims{}, false
	}
	raw, err := b64urlDecode(parts[1])
	if err != nil {
		return jwtClaims{}, false
	}
	var c jwtClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return jwtClaims{}, false
	}
	if c.Sub == "" && c.Exp == 0 {
		return jwtClaims{}, false
	}
	return c, true
}

// UID 账号池主键。
//
// 优先级：显式 UIDValue > JWT 的 sub（**身份 = JWT 的 sub**）
//
//	> AccountScope > access token 哈希（最后的兜底，保证"有号就能进池"）
//
// ⚠ **不读文件路径、不读昵称**：昵称是手机号（会变）、路径会随目录变，
// 拿它们当主键会造成"同一个号进池两次"。
func (a *Auth) UID() string {
	if a == nil {
		return ""
	}
	if v := strings.TrimSpace(a.UIDValue); v != "" {
		return v
	}
	if c, ok := ParseJWTClaims(a.OAuth.AccessToken); ok && strings.TrimSpace(c.Sub) != "" {
		return strings.TrimSpace(c.Sub)
	}
	if v := strings.TrimSpace(a.AccountScope); v != "" {
		return v
	}
	if a.OAuth.AccessToken != "" {
		return "phanthywork-" + sha256B64URL([]byte(a.OAuth.AccessToken))[:16]
	}
	return ""
}

// Nickname 展示名。优先显式值，其次账号 uuid，最后 uid。
func (a *Auth) Nickname() string {
	if a == nil {
		return ""
	}
	if v := strings.TrimSpace(a.NicknameValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.AccountScope); v != "" {
		return v
	}
	return a.UID()
}

// accountScope 返回签名要用的账号 uuid。
//
// 判据顺序：账号档案里的 account.uuid（= AccountScope，**权威**）优先，
// JWT 的 sub 兜底 —— 而**权威值由 ensureInstallation 去 profile 取**（见那里）。
//
// ⚠ 报告专门列过一条错写法：device canonical 的 accountScope 段不能用邮箱，
// 也不该用别的字段 —— 用错会回 400 desktop_account_scope_invalid。
func (a *Auth) accountScope() string {
	if a == nil {
		return ""
	}
	if v := strings.TrimSpace(a.AccountScope); v != "" {
		return v
	}
	if c, ok := ParseJWTClaims(a.OAuth.AccessToken); ok {
		return strings.TrimSpace(c.Sub)
	}
	return ""
}

// installationID 返回**本地派生**的 installationId（di_…）。
//
// ⚠ 请求头与三条 canonical 一律用这个，**不是**服务端注册响应里那个 id
// （那是安装记录 id，形态完全不同）。派生是确定性的，所以同一个 identity
// 公钥永远得到同一个值。
func (a *Auth) installationID() (string, error) {
	pub, err := publicKeyB64FromSeed(a.Device.IdentitySeed)
	if err != nil {
		return "", err
	}
	return DeriveInstallationID(pub)
}

// identityPublicKey 返回 identity 公钥（SPKI/base64url）。
func (a *Auth) identityPublicKey() (string, error) {
	return publicKeyB64FromSeed(a.Device.IdentitySeed)
}

// devicePublicKey 返回 device 公钥（SPKI/base64url）—— 由 identity 换域派生。
func (a *Auth) devicePublicKey() (string, error) {
	seed, err := deriveDeviceSeed(a.Device.IdentitySeed)
	if err != nil {
		return "", err
	}
	return publicKeyB64FromSeed(seed)
}

// signDeviceCanonical 用 **device 私钥** 签 device canonical。
//
// ⚠ IDENTITY_POLICY 实测：bindingSigner = device。用 identity 私钥签这条串会失败，
// 而错误是 desktop_binding_invalid —— 一个看不出原因的错误。
func (a *Auth) signDeviceCanonical(canonical string) (string, error) {
	seed, err := deriveDeviceSeed(a.Device.IdentitySeed)
	if err != nil {
		return "", err
	}
	return signEd25519(seed, []byte(canonical))
}

// signV1Canonical 用 **identity 私钥** 签 v1 canonical（x-desktop-signature）。
func (a *Auth) signV1Canonical(canonical string) (string, error) {
	return signEd25519(a.Device.IdentitySeed, []byte(canonical))
}

// signInferenceCanonical 用 **推理私钥** 签 12 段 canonical。
//
// ⚠ 报告 §2.6：签名私钥是 inference，**不是** identity、也不是 device。
func (a *Auth) signInferenceCanonical(canonical string) (string, error) {
	return signEd25519(a.Infer.Seed, []byte(canonical))
}

// ── 时钟（每账号一份）─────────────────────────────────────────────────────

// applyServerTime 用服务端时间校准本账号的时钟偏移。
//
// 调用点：**每次换令牌之后**（activities/summary 的响应带 server_time）。
// 报告 §3① 的原话是"每次启动都要重新校准，不能缓存一个固定值" ——
// 本机时钟一直在漂（两次实测 -66,996 与 -68,047）。
func (a *Auth) applyServerTime(serverTime string, localMS int64) bool {
	if a == nil {
		return false
	}
	t, err := parseServerTime(serverTime)
	if err != nil {
		return false
	}
	a.skewMS.Store(NormalizeClockSkewMS(localMS - t.UnixMilli()))
	a.skewCalibrated.Store(true)
	return true
}

// skew 返回当前生效的偏移（毫秒）。
//
// 还没校准过时回落到**配置基线**（Config.ClockSkewMS）—— 这让"本机确实有固定
// 偏移、且不想等第一次换令牌"的部署也能正确签名。
func (a *Auth) skew() int64 {
	if a == nil {
		return 0
	}
	if a.skewCalibrated.Load() {
		return a.skewMS.Load()
	}
	return NormalizeClockSkewMS(a.baseSkewMS)
}

// isClockCalibrated 报告本账号是否**已经拿到过服务端时间**。
//
// ⚠ 它区别于 skew()：skew() 在未校准时会回落到配置基线（可能是 0），
// 于是"看起来有偏移"和"真的校准过"这件事被混在一起。
// `ensureInferenceToken` 需要的是后者 —— 未校准就必须去换一次令牌
// （换令牌是唯一能拿 server_time 的常规路径），不能凭缓存跳过。
func (a *Auth) isClockCalibrated() bool {
	if a == nil {
		return false
	}
	return a.skewCalibrated.Load()
}

// serverNowMS 返回**服务端口径**的毫秒时间戳。
//
// ⚠ 所有签名（三条 canonical 的 timestamp 段、x-desktop-timestamp、
// x-phanthy-proof-timestamp）都**必须**用这个，而不是 nowMS()。
// 报告 §3① 实测：用本机时间 → 403 request_not_authorized；校准后 → 200。
// 而那句错误文案完全指不到时钟上（说的是"请更新或重新打开客户端"）。
func (a *Auth) serverNowMS() int64 { return serverNowMS(nowMS(), a.skew()) }

// ── 推理令牌缓存 ─────────────────────────────────────────────────────────

// cachedInferenceToken 返回当前缓存的推理令牌（可能已过期）。
func (a *Auth) cachedInferenceToken() InferenceTokenCache {
	a.tokenMu.RLock()
	defer a.tokenMu.RUnlock()
	return a.token
}

// storeInferenceToken 存下新换来的推理令牌。
//
// localMS 是**本机**时刻（用于算剩余寿命），令牌自身的 exp 是服务端口径 ——
// 两者相差一个 skew（量级是秒），而余量是 30 秒，故不互相换算。
func (a *Auth) storeInferenceToken(token string, expAtMS int64, localMS int64) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	a.token = InferenceTokenCache{Token: token, ExpAtMS: expAtMS, ObtainedAtMS: localMS}
}

// clearInferenceToken 清掉缓存（换安装身份/换密钥时要清）。
func (a *Auth) clearInferenceToken() {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	a.token = InferenceTokenCache{}
}

// inferenceTokenValid 报告缓存里的令牌是否还有**余量**。
//
// 判据抄报告 §3④ 给的那一行：
//
//	if payloadOf(token).exp * 1000 <= Date.now() + 30000 { token = await renewInference() }
//
// 即"余量不足 30 秒就重换"。用余量而不是"是否已过期"，是因为 180 秒的 TTL
// 配一次完整请求（含换令牌、签名、网络往返）足够把"刚好没过期"拖成"发出时已过期"。
func (a *Auth) inferenceTokenValid(localMS int64) bool {
	c := a.cachedInferenceToken()
	if c.Token == "" || c.ExpAtMS <= 0 {
		return false
	}
	return c.ExpAtMS-localMS > InferenceTokenMargin.Milliseconds()
}

// ── 续期 / 过期判定 ──────────────────────────────────────────────────────

// ExpiresAt 报告 access token 的到期时刻（**Unix 秒**）。
//
// ⚠ 对外一律秒（gateway.CredentialExpiryExt 的契约是 Unix 秒，而"按毫秒读秒"
// 是本仓实际踩过的坑：会把 2027 年的号全判成已过期）。
// 内部字段是毫秒，转换只在这里发生。
func (a *Auth) ExpiresAt() (int64, bool) {
	if a == nil {
		return 0, false
	}
	if a.OAuth.ExpiresAtMS > 0 {
		return a.OAuth.ExpiresAtMS / 1000, true
	}
	if c, ok := ParseJWTClaims(a.OAuth.AccessToken); ok && c.Exp > 0 {
		return int64(c.Exp), true
	}
	return 0, false
}

// AccessTokenExpired 报告 access token 是否已过期（本机时刻，含提前量）。
func (a *Auth) AccessTokenExpired(localMS int64, lead time.Duration) bool {
	at, ok := a.ExpiresAt()
	if !ok {
		return false // 没有过期信息 → 保守视为未过期，交给上游 401 自愈
	}
	return at*1000 <= localMS+lead.Milliseconds()
}

// Runnable 报告这份凭证是否至少具备"能发请求"的最低条件。
func (a *Auth) Runnable() bool {
	return a != nil && a.OAuth.AccessToken != "" && a.Device.IdentitySeed != ""
}

// refreshKey 返回**刷新并发闸的键**（坑⑤ 的落地）。
//
// 报告 §7.2 要求"键要跨轮换稳定"，本条实现取 **账号身份** 作键，理由：
//
//	① 轮换后键不变 ⇒ 刚刷完的那次成功结果仍在回放窗口里 ⇒ 紧随其后的并发请求
//	   **直接复用**，不会拿旧 token 再刷一次（互踢的根因）
//	② 同账号永远同键 ⇒ 不需要"轮换时记得迁移 gate 状态"这类会漏的动作
//	③ refreshToken 本身不进键 ⇒ 长期凭据不以明文出现在任何键/日志里
func (a *Auth) refreshKey() string {
	if a == nil {
		return "phanthywork:anon"
	}
	uid := a.UID()
	if uid == "" {
		uid = "anon"
	}
	return "phanthywork:" + uid
}

// ── 刷新并发闸（坑⑤）────────────────────────────────────────────────────

// refreshGate 「同键 in-flight 合并 + 成功结果短回放 + 失败短冷却」的并发闸。
//
// 抄的是报告 §7.2 推崇的 src/refresh-lock.mjs 语义（它自己那句注释：
// 「第二次会拿着已经被轮换掉的旧 token 去换 —— 结果不是「慢一点」，
//
//	而是「登录莫名失效」」）。
type refreshGate struct {
	mu       sync.Mutex
	inflight map[string]*gateCall
}

// gateCall 一次进行中的刷新。done 关闭后 value/err 可读。
type gateCall struct {
	done  chan struct{}
	value string
	atMS  int64
	err   error
}

// GateReplayWindow 成功后结果的回放窗口（开源实现的默认就是 60 秒）。
const GateReplayWindow = 60 * time.Second

// GateFailureCooldown 失败后的冷却（期内同键直接复用同一个失败，不再打上游）。
const GateFailureCooldown = 5 * time.Second

// GateMaxEntries 表容量上限：超了先清过期条目，再丢已完成的一条。
const GateMaxEntries = 128

// gateReady 惰性初始化（零值 refreshGate 必须可直接用 —— Auth 常常是
// 由 ParseCredential 直接 json.Unmarshal 出来的，不会经过构造器）。
func (g *refreshGate) gateReady() {
	if g.inflight == nil {
		g.inflight = make(map[string]*gateCall, 4)
	}
}

// acquire 在**一次临界区**里决定"复用还是发起"。
//
// # 为什么必须是一个方法（这是实测暴露的 TOCTOU）
//
// 第一版把它拆成 tryMerge + begin 两步：先查表、查到没有再去 begin。
// 于是 8 个并发调用可以有多个同时通过"表里没有"这一检查，然后各自 begin
// —— 实测结果是 **2 次**真实刷新（而目标是 1 次）。
//
// 对 refreshToken **轮换**的厂商，第 2 次刷新就是"拿着已作废的旧 token 去换"，
// 症状正是报告里那句"登录莫名失效"。所以"查表 + 登记"必须是同一个临界区。
//
// 返回 (call, initiator)：
//
//	initiator=true  调用方持有发起权，必须在自己结束时调 finish
//	initiator=false 调用方必须等 call.done 并直接读它的 value/err
//
// replayOK 控制"已完成的结果能不能复用"：
//
//	续期（refreshToken 轮换）→ true：刚刷完的结果在窗口内直接复用，
//	                            这正是"第二次不再拿旧 token 去换"的机制
//	换推理令牌（TTL 180 秒）→ false：只合并**进行中**的，
//	                            因为上一个令牌可能真的已经过期了，回放它会直接 403
func (g *refreshGate) acquire(key string, localMS int64, replayOK bool) (*gateCall, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gateReady()
	if c, ok := g.inflight[key]; ok {
		select {
		case <-c.done:
			if replayOK && c.err == nil && localMS-c.atMS <= GateReplayWindow.Milliseconds() {
				return c, false
			}
			if c.err != nil && localMS-c.atMS <= GateFailureCooldown.Milliseconds() {
				return c, false
			}
			// 超出窗口 ⇒ 这是一次**新的**刷新，往下走（仍在同一个临界区里）。
		default:
			// 仍在进行中 ⇒ 合并（这就是"N 个并发只打一次"的全部机制）。
			return c, false
		}
	}
	if len(g.inflight) >= GateMaxEntries {
		g.evictLocked(localMS)
	}
	c := &gateCall{done: make(chan struct{}), atMS: localMS}
	g.inflight[key] = c
	return c, true
}

// finish 结束一次刷新并广播结果（**只有发起者可以调**，且恰好一次）。
//
// 结果写回表里而不是删掉：成功留在窗口内供回放、失败留在冷却期内挡住重试。
func (g *refreshGate) finish(key string, c *gateCall, value string, err error) {
	g.mu.Lock()
	c.value, c.err = value, err
	g.gateReady()
	g.inflight[key] = c
	g.mu.Unlock()
	close(c.done)
}

// evictLocked 清理已完成且超出窗口的条目；仍超限则丢已完成的一条。
func (g *refreshGate) evictLocked(localMS int64) {
	for k, c := range g.inflight {
		select {
		case <-c.done:
			if localMS-c.atMS > GateReplayWindow.Milliseconds() {
				delete(g.inflight, k)
			}
		default:
		}
	}
	if len(g.inflight) < GateMaxEntries {
		return
	}
	for k, c := range g.inflight {
		select {
		case <-c.done:
			delete(g.inflight, k)
		default:
		}
		if len(g.inflight) < GateMaxEntries {
			return
		}
	}
}

// ── 官方形态 / 解析与加载 ────────────────────────────────────────────────

// rootDoc 网关的嵌套落盘形态。
type rootDoc struct {
	Auth    *Auth       `json:"auth"`
	Account *accountRef `json:"account"`
}

// accountRef 落盘形态里 account 段的三个字段。
type accountRef struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Email    string `json:"email,omitempty"`
}

// parseOfficialShape 认**官方客户端自己的**凭据文件形态。
//
// # 为什么必须认它（这是运维上最常发生的一件事）
//
// 报告 auth §3.1 实测：PhanthyCode / PhanthyWork 的凭据落在
// %USERPROFILE%/.phanthy-code/.credentials.json，形状是：
//
//	{ "claudeAiOauth": { "accessToken": "eyJ…", "refreshToken": "ju5W…",
//	                     "expiresAt": 1794407086720, "scopes": [ … ] } }
//
// ⚠ 三处与网关形态的差异，照抄字段名会全部落空：
//
//	根键是 claudeAiOauth（旧版/白牌是 phanthyCodeOauth，还支持没有根键的裸对象）
//	字段名是**驼峰** accessToken / refreshToken
//	expiresAt 是**毫秒**（不是秒）
//
// 认它的实际价值：用户把那个文件直接拷进 auths/phanthywork/ 就能用，
// 不必先跑一遍登录。少了这条，用户会得到一个"文件在、号不进池"的空转。
//
// 认不出来返回 (nil,false)（**不是错误**：这只是三种形态之一）。
func parseOfficialShape(raw []byte) (*Auth, bool) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	if len(root) == 0 {
		return nil, false
	}
	inner := root
	wrapped := false
	for _, key := range []string{"claudeAiOauth", "phanthyCodeOauth"} {
		if v, ok := root[key].(map[string]any); ok {
			inner = v
			wrapped = true
			break
		}
	}
	access := firstString(inner, "accessToken", "access_token")
	if access == "" {
		return nil, false
	}
	a := &Auth{
		Provider: providerID,
		OAuth: OAuthTokens{
			AccessToken:  access,
			RefreshToken: firstString(inner, "refreshToken", "refresh_token"),
			ExpiresAtMS:  firstMillis(inner, "expiresAt", "expires_at_ms", "expiresAtMs"),
			Scopes:       firstStrings(inner, "scopes", "scope"),
		},
	}
	// ⚠⚠ 第四种形态（2026-11-14 T-914）：**顶层驼峰**且带账号线索键
	//
	//	{"platform":"phanthy","userid":"cmwc…","phone":"150…","account":"+86 150****3411",
	//	 "accessToken":"eyJ…","refreshToken":"ZnGR…","expiresAt":1794545067558}
	//
	//	它走的就是上面那条"没有根键 ⇒ inner = root"的兜底路径（token/到期本来就读得到），
	//	**唯一没被读的是 `userid`** ⇒ uid 只能回落到 JWT 的 sub（sub 取不到时整份导入失败，
	//	用户看到的是"导不进去"）。所以这里补上 uid 与展示名。
	//
	// ⚠ 只在**没有根键**（顶层兜底那一档）时读这些键 —— 带 claudeAiOauth/phanthyCodeOauth 的
	//	旧输入**一字不变**（冲突时按旧形态解释；这条由 TestWrappedShapeWinsOverTopLevelCamel 钉住）。
	//
	// ⚠ `phone` / `account` **只作展示线索**（NicknameValue），**绝不**当 uid 的唯一来源 ——
	//	手机号是可能改的、也可能同一人多个号；uid 的唯一来源是 `userid`（缺失则不填，
	//	交给 UID() 的既有回落链）。
	if !wrapped {
		a.UIDValue = firstString(inner, "userid", "userId", "uid")
		a.NicknameValue = firstString(inner, "account", "nickname", "phone")
	}
	return a, true
}

// firstString 取第一个非空字符串字段（兼容驼峰与下划线两种拼法）。
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			if t := strings.TrimSpace(s); t != "" {
				return t
			}
		}
	}
	return ""
}

// firstMillis 取第一个时间字段并归一成**毫秒**。
//
// ⚠ 归一规则与 gateway.ExpiryFromMillisOrJWT 同源：小于 1e12 的按**秒**读，
// 否则按毫秒读（1794407086720 是毫秒；1794407086 是秒）。按毫秒读秒字段
// 会把 2027 年的号全判成已过期 —— 本仓实际踩过这个坑。
func firstMillis(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			if v <= 0 {
				continue
			}
			n := int64(v)
			if n < 1_000_000_000_000 {
				return n * 1000
			}
			return n
		case string:
			if n := parseIntRaw(json.RawMessage(v)); n > 0 {
				if n < 1_000_000_000_000 {
					return n * 1000
				}
				return n
			}
		}
	}
	return 0
}

// firstStrings 取第一个字符串数组字段（scopes / scope）。
func firstStrings(m map[string]any, keys ...string) []string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case []any:
			out := make([]string, 0, len(v))
			for _, x := range v {
				if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			if len(out) > 0 {
				return out
			}
		case string:
			if f := strings.Fields(v); len(f) > 0 {
				return f
			}
		}
	}
	return nil
}

// ParseCredential 解析一份凭证 JSON（**三种形态都认**）。
//
// 判据只有一条：**必须有 access_token**。没有它的凭证进池只会变成
// 一个"每次选中都失败"的幽灵号。
//
// 三种形态：
//
//	① 网关嵌套形 {"auth":{…},"account":{…}} —— 本包自己 MarshalAuthFile 的产物
//	② 官方客户端形 {"claudeAiOauth":{…}}（见 parseOfficialShape）
//	③ 本包扁平形 {"oauth":{…},"device":{…}} —— 手写凭证最省事的那种
func ParseCredential(raw []byte) (*Auth, error) {
	// ① 网关嵌套形
	var doc rootDoc
	if err := json.Unmarshal(raw, &doc); err == nil && doc.Auth != nil && doc.Auth.OAuth.AccessToken != "" {
		a := doc.Auth
		if doc.Account != nil {
			if a.UIDValue == "" {
				a.UIDValue = strings.TrimSpace(doc.Account.UID)
			}
			if a.NicknameValue == "" {
				a.NicknameValue = strings.TrimSpace(doc.Account.Nickname)
			}
		}
		if err := a.normalize(); err != nil {
			return nil, err
		}
		return a, nil
	}

	// ② 官方客户端自己的凭据文件形态
	if a, ok := parseOfficialShape(raw); ok {
		if err := a.normalize(); err != nil {
			return nil, err
		}
		return a, nil
	}

	// ③ 本包扁平形
	var flat Auth
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("phanthywork: 凭证 JSON 无法解析: %w", err)
	}
	if flat.OAuth.AccessToken == "" {
		return nil, errNoAccessToken
	}
	if err := flat.normalize(); err != nil {
		return nil, err
	}
	return &flat, nil
}

// normalize 补齐派生字段、重建缺失的密钥、统一形态。
//
// 设计取向：**能补齐就补齐，补不齐才报错**。
//
//	缺 identity 种子 → 造一把（等于换一个新 installationId）
//	缺推理密钥       → 造一对新的（下次调模型时会被注册流程换掉）
//	缺 AccountScope  → 留空（调模型前会去 profile 端点取**权威值**）
//
// 这样"用户手写了一份只有 access_token 的凭证"也能进池并可用，而不是变成
// 一个"看着有号、调不动"的死条目。
func (a *Auth) normalize() error {
	if a == nil {
		return errNilAuth
	}
	a.OAuth.AccessToken = strings.TrimSpace(a.OAuth.AccessToken)
	a.OAuth.RefreshToken = strings.TrimSpace(a.OAuth.RefreshToken)
	a.AccountScope = strings.TrimSpace(a.AccountScope)
	a.UIDValue = strings.TrimSpace(a.UIDValue)
	a.NicknameValue = strings.TrimSpace(a.NicknameValue)
	a.Device.IdentitySeed = strings.TrimSpace(a.Device.IdentitySeed)
	a.Device.InstallationID = strings.TrimSpace(a.Device.InstallationID)
	a.Infer.Seed = strings.TrimSpace(a.Infer.Seed)
	a.Infer.PublicKey = strings.TrimSpace(a.Infer.PublicKey)

	if a.Provider == "" {
		a.Provider = providerID
	}
	if a.OAuth.AccessToken == "" {
		return errNoAccessToken
	}
	// 密钥从缺到有的补齐（**只在缺的时候造**，绝不覆盖已有的 ——
	// 覆盖 identity 等于换设备，服务端会认为这是另一台机器）。
	if a.Device.IdentitySeed == "" {
		seed, _, err := generateEd25519()
		if err != nil {
			return fmt.Errorf("phanthywork: 生成 identity 密钥失败: %w", err)
		}
		a.Device.IdentitySeed = seed
	}
	if _, _, err := keyPairFromSeed(a.Device.IdentitySeed); err != nil {
		return fmt.Errorf("phanthywork: identity 种子不合法: %w", err)
	}
	if a.Infer.Seed == "" || a.Infer.PublicKey == "" {
		seed, pub, err := generateEd25519()
		if err != nil {
			return fmt.Errorf("phanthywork: 生成推理密钥失败: %w", err)
		}
		a.Infer.Seed, a.Infer.PublicKey = seed, pub
	}
	if _, _, err := keyPairFromSeed(a.Infer.Seed); err != nil {
		return fmt.Errorf("phanthywork: 推理种子不合法: %w", err)
	}
	// 公钥与种子必须对得上：对不上说明文件被手改过，签名会全部失败，
	// 而失败表现是"服务端说签名不对"（查不到本地这一层）。
	if want, err := publicKeyB64FromSeed(a.Infer.Seed); err == nil && want != a.Infer.PublicKey {
		a.Infer.PublicKey = want
	}
	if a.NicknameValue == "" {
		a.NicknameValue = a.AccountScope
	}
	return nil
}

// LoadDir 扫描 dir 下 phanthywork*.json，返回全部可用凭证。
//
// 语义与其它上游的 LoadDir 对齐（这几条都是实测踩出来的）：
//
//	目录为空 / 不存在 → 返回空切片，**不是错误**（用户还没放凭证）
//	单个文件解析失败   → 跳过并**记日志**（静默跳过会让"少了一个号"无法排查）
//	同一 uid 多份      → 取**可用性更强**的那份（见 betterAuth）
//
// ⚠ 空目录要先判空再扫：Glob 在 dir 为空时会退化成**相对当前工作目录**的模式，
// 把工作目录下的同名文件读进来（本仓实测踩过）。
func LoadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("phanthywork: 读凭证目录 %s 失败: %w", dir, err)
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileExt) {
			continue
		}
		p := filepath.Join(dir, name)
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			log.Printf("phanthywork: 读凭证文件失败，跳过 %s: %v", name, rerr)
			continue
		}
		a, perr := ParseCredential(raw)
		if perr != nil {
			log.Printf("phanthywork: 解析凭证失败，跳过 %s: %v", name, perr)
			continue
		}
		a.FilePath = p
		if a.UID() == "" {
			log.Printf("phanthywork: 凭证 %s 无法派生 uid，跳过（放进池子会得到一个无法按 uid 寻址的账号）", name)
			continue
		}
		out = append(out, a)
	}
	return pickWinners(out), nil
}

// pickWinners 同一 uid 多份凭证时按可用性判据选一份。
//
// 骨架复用 gateway.PickWinnersByUID（去重 + 稳定顺序 + 按 uid 升序），
// 本包只提供"uid 怎么取"与"哪份更好"。
func pickWinners(list []*Auth) []*Auth {
	return gateway.PickWinnersByUID(list, func(a *Auth) string { return a.UID() }, betterAuth)
}

// betterAuth 报告 cand 是否比 cur 更值得采用。
//
// 判据逐级短路：
//
//	① 注册成功过（有 RegisteredAtMS）优先 —— 没注册过的号还要走一次写操作
//	② access token 更晚过期优先
//	③ 有 refreshToken 优先（能自动续期，过期只是暂时的）
func betterAuth(cand, cur *Auth) bool {
	if (cand.Reg.RegisteredAtMS > 0) != (cur.Reg.RegisteredAtMS > 0) {
		return cand.Reg.RegisteredAtMS > 0
	}
	ca, _ := cand.ExpiresAt()
	cu, _ := cur.ExpiresAt()
	if ca != cu {
		return ca > cu
	}
	return cand.OAuth.RefreshToken != "" && cur.OAuth.RefreshToken == ""
}

// FileName 凭证的落盘文件名。
func FileName(a *Auth) string {
	key := a.UID()
	if key == "" {
		key = "unknown"
	}
	return sanitizeFilePart(filePrefix+"-"+key) + fileExt
}

// MarshalAuthFile 把凭证编成网关的落盘形态（嵌套形）。
//
// ⚠ 这个方法的签名**由核心决定**（见 internal/admin 的 authFileWriter）：
//
//	type authFileWriter interface {
//	    MarshalAuthFile() (name string, raw []byte, err error)
//	}
//
// 核心的 pollViaFlow 用 cred.Secret.(authFileWriter) 做类型断言 —— 断言不匹配
// **不报任何错**，只会回 501"该上游的凭证结构尚未接入落盘"。
// 所以方法名、参数个数、返回值顺序都不能改。
func (a *Auth) MarshalAuthFile() (string, []byte, error) {
	if a == nil {
		return "", nil, errNilAuth
	}
	if a.OAuth.AccessToken == "" {
		return "", nil, errNoAccessToken
	}
	doc := map[string]any{
		rootKeyOAuth: a,
		rootKeyAccount: map[string]any{
			"uid":      a.UID(),
			"nickname": a.Nickname(),
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("phanthywork: 凭证序列化失败: %w", err)
	}
	return FileName(a), raw, nil
}

// saveAuthFile 把凭证原子写回磁盘（先写 .tmp 再 rename）。
//
// FilePath 为空（凭据不是文件形态）时**跳过落盘**并返回 nil —— 那不是错误。
func saveAuthFile(a *Auth) error {
	if a == nil || a.FilePath == "" {
		return nil
	}
	name, raw, err := a.MarshalAuthFile()
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.FilePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if path != a.FilePath {
		log.Printf("phanthywork: 凭证 uid 变化，落盘到新文件 %s（旧文件 %s 未自动删除）", name, filepath.Base(a.FilePath))
		a.FilePath = path
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sanitizeFilePart 把 uid 里不适合做文件名的字符换掉。
func sanitizeFilePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return filePrefix
	}
	return out
}

// parseServerTime 解析 server_time（RFC3339，服务端给的是带 Z 的 UTC 形态）。
func parseServerTime(s string) (time.Time, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return time.Time{}, errEmptyServerTime
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if ts, err := time.Parse(layout, t); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("phanthywork: 无法解析 server_time %q", s)
}

// ── 本文件的哨兵 ─────────────────────────────────────────────────────────

var (
	errNoAccessToken   = cryptoErr("凭证缺少 access_token（唯一鉴权材料）")
	errNilAuth         = cryptoErr("凭证是 nil")
	errEmptyServerTime = cryptoErr("server_time 为空")
)

// ── 签到用的两个小访问器（服务端日键与状态）──────────────────────────────

// noteDaily 记下服务端给的日键与状态（签到路径专用）。
//
// ⚠ 空值不覆盖已有值：服务端偶尔不回某个字段时，保留上一次已知的日键比清空安全
// （清空会让下一次 Claim 又去读一次，而且"没有日键"本身是拒绝发送的理由）。
func (a *Auth) noteDaily(serverDate, status string) {
	if a == nil {
		return
	}
	a.dailyMu.Lock()
	defer a.dailyMu.Unlock()
	if s := strings.TrimSpace(serverDate); s != "" {
		a.dailyDate = s
	}
	if s := strings.TrimSpace(status); s != "" {
		a.dailyStatus = s
	}
}

// dailyServerDate 返回已知的服务端日键（空 = 还不知道）。
func (a *Auth) dailyServerDate() string {
	if a == nil {
		return ""
	}
	a.dailyMu.Lock()
	defer a.dailyMu.Unlock()
	return a.dailyDate
}

// dailyStatusValue 返回已知的最后一次签到状态（空 = 还不知道）。
func (a *Auth) dailyStatusValue() string {
	if a == nil {
		return ""
	}
	a.dailyMu.Lock()
	defer a.dailyMu.Unlock()
	return a.dailyStatus
}
