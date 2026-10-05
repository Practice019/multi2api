// credential.go ZCode 的凭证形态与加载/落盘（对齐本仓其它上游的约定）。
//
// # 一个上游两条凭证路径，本包都吃
//
//	api-key  管理员在控制台粘贴的 Z.ai / BigModel API Key
//	         （形态 `{apiKey}.{secretKey}`，或 bigmodel 的裸 apiKey）
//	         → 走 OpenAI 兼容端点，**不需要验证码**，零额外依赖
//	jwt      OAuth 登录得到的 Coding Plan JWT
//	         → 走 Anthropic 端点，**每请求需要新验证码**（见 captcha.go）
//
// 两者共用同一份凭证文件结构（`kind` 字段区分），因为它们描述的是
// "同一个账号的两种访问方式"——同一账号两种都可能有，谁可用谁上。
package zcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Auth 一份 ZCode 凭证。
//
// 字段名刻意贴近本仓其它上游（见 raccoon/credential.go）：
// 落盘 JSON 的键就是这些，改键名会破坏已有凭证文件的兼容性。
type Auth struct {
	// Kind 凭证类型：CredKindAPIKey / CredKindJWT。
	//
	// 空值按 api-key 处理（向后兼容手写的凭证文件）——
	// 猜错的代价是"用 api-key 去打 Anthropic 端点"，那会明确报鉴权失败，
	// 比直接拒绝加载更容易排障。
	Kind string `json:"kind,omitempty"`

	// APIKey Z.ai / BigModel 的 API Key。
	//
	// 形态：zai 是 `{apiKey}.{secretKey}` 两段；bigmodel 允许只有一段。
	// 原样保存、原样发送 —— 不做拆解（上游接受整串）。
	APIKey string `json:"api_key,omitempty"`

	// CodingPlan 该 Key 是否属于 Coding Plan 订阅。
	//
	// 为什么必须有这个字段：官方文档明示 Coding Plan 端点
	//（`/api/coding/paas/v4`）与通用端点（`/api/paas/v4`）
	// **不可互换、不互相消耗额度**。猜错会导致"有额度却报无额度"，
	// 而那种错误极难从现象反推原因 —— 所以让它显式。
	//
	// 零值 false = 按量付费的普通 Key（更保守的默认：普通 Key
	// 打错端点是明确的鉴权/额度错误，比订阅额度被静默消耗更安全）。
	CodingPlan bool `json:"coding_plan,omitempty"`

	// JWT Coding Plan 的访问令牌（OAuth 登录得到）。
	//
	// ⚠ 它**不可刷新**：官方 oauthService.ts 对不支持 refresh 的 adapter
	// 直接抛错，本地只解 payload.exp 判过期。所以过期后只能重新登录。
	JWT string `json:"jwt,omitempty"`

	// RefreshToken 上游若下发就存着（bigmodel 可能有）。
	//
	// ⚠ 但**不能假定存在**：官方明确"暂未提供 refresh token 交换接口"。
	// 所以续期逻辑必须以"JWT 过期 → 标记需重登"为主路径。
	RefreshToken string `json:"refresh_token,omitempty"`

	// UID 上游账号标识（OAuth 登录时的 user_id）。
	//
	// API Key 通道没有账号概念 → 用 key 的短哈希兜底（见 ensureUID）。
	UID string `json:"uid,omitempty"`

	// Nickname 展示名（邮箱/手机号，或 key 的尾部特征）。
	Nickname string `json:"nickname,omitempty"`

	// ExpiresAt 凭证过期时刻（Unix 秒，0 = 未知）。
	//
	// JWT 通道从 token 的 exp 解出；API Key 通道通常无过期概念（0）。
	ExpiresAt int64 `json:"expires_at,omitempty"`

	// DeviceMid 本账号的持久化设备指纹（uuid 形态）。
	//
	// ⚠ 抄自参照实现的关键约定：**每个账号一个自己的 device_mid**，
	// 而不是全局共用一个派生值。参照实现注释原话：
	//   "X-Device-Mid is the account's own persisted uuid4
	//    ('a fresh install on this machine' per account),
	//    not a shared/derived id."
	// 共用会让上游把多个账号看成同一台设备（风控上很可疑）。
	DeviceMid string `json:"device_mid,omitempty"`

	// CaptchaRegion 验证码区域（"cn" / "sg" 等）。
	//
	// ⚠ 参照实现注释："the captcha region rides with the minted token,
	// never hardcoded" —— 它必须**与签发验证码时用的 region 一致**，
	// 所以随凭证存下来，不写死。
	CaptchaRegion string `json:"captcha_region,omitempty"`

	// Origin 该 Key 属于哪个平台（`https://api.z.ai` / `https://open.bigmodel.cn`）。
	//
	// 为什么存在凭证里而不是全局配置：Z.ai（国际）与 BigModel（国内）是
	// **两套独立平台**，同一个 Key 不能跨用。一个网关同时有两边的 Key
	// 是常见情形，所以平台属性属于**账号**而不是上游实例。
	//
	// 空值 = 装配时探测（见 Client.ProbeOrigin）。
	Origin string `json:"origin,omitempty"`

	// FilePath 凭证在磁盘上的位置（加载时由 LoadDir 填，不落盘）。
	//
	// 核心只拿它做两件事：账号池"文件"列展示、删账号时连文件一起移除。
	// 与 Auth 同一个包所以不导出成 JSON —— 它是**运行时事实**，不是凭证内容。
	//
	// ⚠ 不填它的后果（本仓实测过的"删除后重启复活"）：
	// admin 的 accountDelete 拿不到 FilePath，purge_file 静默空转，
	// 文件还在 → 下次启动并池又把账号装回来。
	FilePath string `json:"-"`
}

// authFile 落盘包装：与其它上游一致，多一层 `auth` 键。
//
// 为什么不让 Auth 直接当文件顶层：本仓所有上游的凭证文件都是
// `{"auth": {...}}` 形态（见 raccoon/credential.go 的 authFile），
// admin 的"批量导入"按键名统一解析。破坏一致性会让导入功能对 zcode 失灵。
type authFile struct {
	A      *Auth  `json:"auth"`
	Remark string `json:"remark,omitempty"`
}

// Kind 归一化后的凭证类型（空值按 api-key）。
func (a *Auth) KindOf() string {
	k := strings.ToLower(strings.TrimSpace(a.Kind))
	switch k {
	case CredKindJWT, "oauth", "coding-plan":
		return CredKindJWT
	default:
		return CredKindAPIKey
	}
}

// UsesJWT 是否走 Coding Plan 的 JWT 通道。
func (a *Auth) UsesJWT() bool {
	return a.KindOf() == CredKindJWT
}

// Token 返回该凭证用于 `Authorization` 的值（不含 "Bearer " 前缀）。
//
// 两条通道的令牌不同，但**发送方式一样**（实测两条端点都接受 Bearer）：
//
//	api.z.ai/api/paas/v4/chat/completions  Bearer 与 x-api-key 都认
//	api.z.ai/api/anthropic/v1/messages     Bearer 与 x-api-key 都认
func (a *Auth) Token() string {
	if a.UsesJWT() {
		return a.JWT
	}
	return a.APIKey
}

// Usable 该凭证是否有可用的令牌（空凭证不并入账号池）。
func (a *Auth) Usable() bool {
	return strings.TrimSpace(a.Token()) != ""
}

// ensureUID 保证 UID 非空（账号池按 UID 去重，空 UID 会让多个账号互相覆盖）。
//
// API Key 通道没有账号概念，用 key 的短哈希兜底。
// 为什么用哈希而不是明文前 8 位：凭证文件可能被贴到 issue 里求助，
// 明文 key 前缀等于泄露一半密钥。
func (a *Auth) ensureUID() {
	if strings.TrimSpace(a.UID) != "" {
		return
	}
	a.UID = "key-" + shortHash(a.Token())
}

// shortHash 取 sha256 前 12 hex 字符（够避碰，又短到能放进文件名）。
//
// 与本仓其它上游一致：直接用 crypto/sha256，不额外抽公共包
// （见 cline/cline.go:257、oauth/oauth.go:85 的同款写法）。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// ExpiresAtMS 过期时刻的毫秒表示（0 = 未过期信息未知）。
//
// 为什么要有这个方法：账号池的"剩余时间"列读它，
// 而本仓其它上游用的是毫秒（见 gateway.CredentialExpiryExt 的约定）。
func (a *Auth) ExpiresAtMS() int64 {
	if a.ExpiresAt <= 0 {
		return 0
	}
	return a.ExpiresAt * 1000
}

// Expired 凭证是否已过期（无过期信息时返回 false —— 不能凭"不知道"就禁用账号）。
func (a *Auth) Expired(now time.Time) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return now.Unix() >= a.ExpiresAt
}

// LoadDir 读取凭证目录下所有 `*.json`。
//
// 与其它上游一致的行为：
//   - 目录不存在 → 返回空列表而非错误（首次运行是正常路径，不该刷红日志）
//   - 单个文件坏 → **跳过并继续**（一份坏凭证不该让整个上游起不来）
//   - `.rejected` 等隐藏子目录/文件跳过（本仓 admin 用它标记被拒凭证）
func LoadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// 跳过隐藏文件与非 json（备份文件 .bak-* 也在其中）。
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		a, err := LoadFile(filepath.Join(dir, name))
		if err != nil {
			// 不返回错误：一份坏文件不该拖垮整个上游。
			continue
		}
		if a != nil && a.Usable() {
			// 回填文件位置（投影要用它，删账号要连文件一起删）。
			a.FilePath = filepath.Join(dir, name)
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// DisplayUID 账号池的主键（投影与去重都用它）。
//
// 与 UID 分开的理由：UID 是上游给的原始标识（OAuth 的 user_id 可能是邮箱），
// 而**账号池主键必须是稳定的字符串**。当前实现直接返回 UID，
// 但这个函数是"将来要改时的唯一改动点"—— 调用方不直接读字段。
func DisplayUID(a *Auth) string {
	if a == nil {
		return ""
	}
	a.ensureUID()
	return a.UID
}

// DisplayNameOf 控制台显示名。
//
// 为什么不能直接返回 Nickname：API Key 通道**没有账号概念**，
// Nickname 常为空，而列表里显示空名会让人分不清哪个是哪个。
// 所以兜底显示"通道 + key 尾 4 位"—— 尾 4 位足够区分，又不泄露完整密钥。
func DisplayNameOf(a *Auth) string {
	if a == nil {
		return ""
	}
	if n := strings.TrimSpace(a.Nickname); n != "" {
		return n
	}
	token := a.Token()
	tail := token
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	if a.UsesJWT() {
		return "Coding Plan ·…" + tail
	}
	return "API Key ·…" + tail
}

// LoadFile 读一份凭证文件。
//
// 同时接受两种形态（本仓的历史包袱，必须都认）：
//
//	{"auth": {...}}   ← 本包落盘的标准形态
//	{...}             ← 直接是 Auth（手写的、或从别处抄来的）
func LoadFile(path string) (*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseAuth(raw)
}

// parseAuth 从字节里解出 Auth（两种形态都认，见 LoadFile）。
//
// ⚠ 两条分支都**必须**做 Usable 检查。早先只有平铺分支做了，
// 于是 `{"auth":{}}` 会被当成一份合法凭证收下 —— 它没令牌，
// 于是并池成一个"看着有、每个请求都 401"的账号。这类空账号
// 在账号池里的表现是**绿色的**（没错误），只有发请求才发现。
func parseAuth(raw []byte) (*Auth, error) {
	var f authFile
	if err := json.Unmarshal(raw, &f); err == nil && f.A != nil {
		if !f.A.Usable() {
			return nil, fmt.Errorf("zcode: 凭证里没有可用令牌（api_key / jwt 都为空）")
		}
		f.A.ensureUID()
		return f.A, nil
	}
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("zcode: 凭证文件既不是 {auth:{...}} 也不是平铺结构: %w", err)
	}
	if !a.Usable() {
		return nil, fmt.Errorf("zcode: 凭证文件里没有可用令牌（api_key / jwt 都为空）")
	}
	a.ensureUID()
	return &a, nil
}

// SaveFile 原子写一份凭证到 dir（文件名 `zcode-<uid>.json`）。
//
// 原子写的理由与其它上游相同：写到一半被杀会留下半个 JSON，
// 下次启动时那份凭证就"莫名失效"了。
func SaveFile(dir string, a *Auth) (string, error) {
	if a == nil {
		return "", fmt.Errorf("zcode: 凭证为空")
	}
	a.ensureUID()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(authFile{A: a}, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "zcode-"+sanitizeUID(a.UID)+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// sanitizeUID 把 UID 里不适合做文件名的字符换掉。
//
// 为什么必需：UID 来自上游（OAuth 的 user_id 可能是邮箱），
// 直接拼进文件名在 Windows 上会因 `@` `:` 等字符失败或被拒绝。
func sanitizeUID(uid string) string {
	var b strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		return "unknown"
	}
	// Windows 文件名上限 255，留出 "zcode-" 与 ".json" 的余量。
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
