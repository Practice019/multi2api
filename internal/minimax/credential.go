// credential.go MiniMax 凭证的形态、落盘与读取。
//
// # 文件形状与本仓约定
//
// 与其余上游一致：`{"auth":{…},"remark":"…"}`（`admin` 的批量导入按键名统一解析）。
//
//	{"auth":{"access_token":"mmoat_…","refresh_token":"mmort_…",
//	         "token_type":"Bearer","expires_at":"1791258329000",
//	         "scope":"agent.default","nickname":"…"}}
//
// ⚠ `expires_at` 是**毫秒时间戳的字符串**（参照项目 `MinimaxCredential`
// 的原形状就是 `string`）—— 不是 RFC3339、不是秒。读的时候兼容秒级
// （见 `expiryMillis`），写的时候一律写毫秒串。
package minimax

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 文件名前缀（`<dir>/minimax-<uid>.json`，与其余上游同款）。
const filePrefix = "minimax"

// Auth 一份 MiniMax 凭证。
type Auth struct {
	// AccessToken 访问令牌（`mmoat_` 前缀）。
	//
	// ⚠ 它**不是 JWT** —— 参照项目实测：60 字符、0 个点。
	// 所以过期时刻**只能**来自 `expires_at` / `expires_in`，
	// 不能像 zcode 那样指望从 token 里解 `exp`。
	AccessToken string `json:"access_token"`
	// RefreshToken 续期令牌（`mmort_` 前缀）。
	RefreshToken string `json:"refresh_token,omitempty"`
	// TokenType 恒为 "Bearer"（**必须**，见 parseTokenGrant）。
	TokenType string `json:"token_type,omitempty"`
	// ExpiresAt 过期时刻，**毫秒时间戳的字符串**。
	ExpiresAt string `json:"expires_at,omitempty"`
	// IssuedAt 这份 token 的**发证时刻**（毫秒字符串，可选）。
	//
	// # 为什么需要它（用户提的问题逼出来的）
	//
	// 用户问：「Token 时间过了 50% 就会自动续期吗？」
	//
	// 要按比例算窗口就必须知道**总寿命**，而凭证里只有过期时刻。
	// ExpiresAt - IssuedAt 就是总寿命 ⇒ 窗口取一半即用户期望的语义。
	//
	// 手工导入的凭证没有这个字段（omitempty），那时回落到固定窗口。
	IssuedAt string `json:"issued_at,omitempty"`
	// Scope 授权范围（空格分隔）。必须含 `agent.default` 才算有效。
	Scope string `json:"scope,omitempty"`
	// AccountID 账号标识。
	//
	// ⚠ 参照项目里它由 JWT 的 `sub` 派生，而真实凭据的 access_token
	// **不是 JWT** ⇒ 实测恒为空。故本包**不拿它当账号主键** ——
	// 主键是派生出来的 uid（见 `UID`）。
	AccountID string `json:"account_id,omitempty"`
	// Identity **判重得到的既有账号 uid**（登录落盘前写入，见 accountkey.go）。
	//
	// # 为什么需要它（用户实测报的重复账号）
	//
	// 上游不提供账号身份，所以同一个号登录两次会得到两个不同的
	// access_token ⇒ 两个哈希 ⇒ 账号池里两条重复。
	//
	// 登录落盘前用账号指纹比对出「这就是已有的那个号」时，把它的 uid
	// 记在这里 —— `UID()` 优先返回它，于是**文件名与池记录都对齐到旧那条**，
	// 结果就是用户期望的「更新」而不是「新增」。
	//
	// ⚠ 只能由**判重证据**（指纹非空且相等）得出，绝不凭猜测填：
	// 覆盖旧凭证是**不可逆**的（旧 refresh_token 被替换就找不回来了）。
	Identity string `json:"identity,omitempty"`
	// Nickname 展示名（登录时取，可空）。
	Nickname string `json:"nickname,omitempty"`

	// FilePath 该凭证来自哪个文件（LoadDir 回填；非持久化字段）。
	// 删账号时要连文件一起删 —— 缺了会出现"删掉后重启又回来"。
	FilePath string `json:"-"`
}

// authFile 落盘包装：与其它上游一致，多一层 `auth` 键。
type authFile struct {
	A      *Auth  `json:"auth"`
	Remark string `json:"remark,omitempty"`
}

// MarshalAuthFile 返回 (文件名, 内容) —— 核心 pollViaFlow 的 authFileWriter 契约。
//
// ⚠ **不实现它就等于「登录不了」**：浏览器授权会成功，但核心落盘那一步
// 是类型断言 `cred.Secret.(authFileWriter)`，断言失败就 501
// 「该上游的凭证结构尚未接入落盘」。本仓在 raccoon 与 zcode 上各踩过一次
// —— 所以 `login_test.go` 里有一条专门的断言盯着它。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.A == nil {
		return "", nil, fmt.Errorf("minimax: 凭证为空")
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return FileName(f.A), raw, nil
}

// FileName 该凭证的落盘文件名。
func FileName(a *Auth) string {
	return filePrefix + "-" + sanitizeFilePart(a.UID()) + ".json"
}

// UID 账号池主键。
//
// # 优先用判重证据（Identity），其次上游 id，最后才是 token 哈希
//
// ⚠ 这条注释原来写的是「同一个 token 永远得到同一个 uid（幂等：重装/
// 重登不会产生两个账号）」—— **那句话是错的，而它就是重复账号的源头**。
// 用户实测：同一个号登录两次得到两条账号。
//
// 错在哪：重登拿到的**是一个新的 access_token**（实测两条令牌 60 字符
// 随机串、除前缀外无一相同），所以哈希必然不同。把哈希说成"重登幂等"
// 是概念错误，不是笔误。
//
// 现在：
//
//	Identity   判重（账号指纹）确认"这就是已有的那个号"时由登录流程写入
//	AccountID  上游若哪天给了真 id 就用它（实测恒为空，见 accountkey.go）
//	token-hash **兜底**：全新账号，或判不准的时候
//
// 兜底仍会产出新 uid —— 那是**故意的**：判不准时宁可多一条让用户删，
// 也不静默覆盖别人的凭证（覆盖不可逆）。
//
// ⚠ 续期（refresh）**不会**改 uid：它是就地更新同一份 Auth，
// Identity 与哈希基准都不动 ⇒ 池记录保持一条。
func (a *Auth) UID() string {
	if a == nil {
		return ""
	}
	if s := strings.TrimSpace(a.Identity); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.AccountID); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.AccessToken); s != "" {
		return "token-" + shortHash(s)
	}
	return ""
}

// DisplayUID 账号池里展示的 uid（截短，避免 36 位占满一列）。
func (a *Auth) DisplayUID() string {
	uid := a.UID()
	if len(uid) > 12 {
		return uid[:12] + "…"
	}
	return uid
}

// DisplayName 账号池里的展示名。
func (a *Auth) DisplayName() string {
	if a == nil {
		return ""
	}
	if s := strings.TrimSpace(a.Nickname); s != "" {
		return s
	}
	return a.DisplayUID()
}

// Usable 这份凭证能不能拿去发请求。
func (a *Auth) Usable() bool {
	return a != nil && strings.TrimSpace(a.AccessToken) != ""
}

// Token 请求头里用的令牌。
func (a *Auth) Token() string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.AccessToken)
}

// ExpiresAtMS 过期时刻（毫秒）。0 = 不知道。
//
// ⚠ 兼容**秒级**输入：参照项目实测 `expires_at` 可能是秒
// （`> 1e12` 判为毫秒，否则 ×1000）。写成只看毫秒会让"秒级那份"
// 被当成 1970 年（于是账号永远显示"已过期"）。
func (a *Auth) ExpiresAtMS() int64 {
	if a == nil {
		return 0
	}
	return parseExpiryMillis(a.ExpiresAt)
}

// Expired 是否已过期（不知道过期时间时保守视为**未**过期）。
func (a *Auth) Expired() bool {
	ms := a.ExpiresAtMS()
	return ms > 0 && time.Now().UnixMilli() >= ms
}

// Refreshable 是否有 refresh_token 可续期。
// LifetimeMillis 这份凭证的总寿命（毫秒）；未知时 0。
//
// 判据：IssuedAt 与 ExpiresAt 都在、且后者更大。
// 0 是**未知**哨兵（不是「寿命为零」）—— 调用方据此回落固定窗口。
func (a *Auth) LifetimeMillis() int64 {
	if a == nil {
		return 0
	}
	issued := parseExpiryMillis(a.IssuedAt)
	expires := a.ExpiresAtMS()
	if issued <= 0 || expires <= issued {
		return 0
	}
	return expires - issued
}
func (a *Auth) Refreshable() bool {
	return a != nil && strings.TrimSpace(a.RefreshToken) != ""
}

// parseExpiryMillis 解析过期时刻字符串 → 毫秒。
//
// 只认纯数字串（照抄参照项目的 `/^\d+$/` 判据）：
// 它在 `ToNumber` 之后还要乘 1000，所以"带小数/带单位"的输入
// 会让换算结果错得离谱 —— 直接判"读不到"更安全。
func parseExpiryMillis(s string) int64 {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	// > 1e12 视为毫秒（约 2001 年之后的毫秒时间戳），否则视为秒。
	if n > 1_000_000_000_000 {
		return n
	}
	return n * 1000
}

// shortHash 取字符串 sha256 的前 6 字节 hex（与 zcode 同款）。
func shortHash(s string) string {
	sum := sha256Sum(s)
	return hex6(sum)
}

// sanitizeFilePart 把 uid 变成安全的文件名片段。
func sanitizeFilePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

// LoadDir 读一个目录下全部 minimax 凭证。
//
// ⚠ 三条宽容度，都是本仓踩过的：
//
//	隐藏文件/非 .json 跳过（.tmp、.bak 不是凭证）
//	单个文件坏掉**不影响其余**（一个坏文件不该让 40 个号都读不出来）
//	坏文件**必须记日志**（静默跳过会让"少了一个号"无法排查）
//
// 读不到任何凭证**不是错误** —— 返回空切片（目录为空是正常状态）。
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
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		// 只认本上游前缀的文件（目录里可能有别家的残留）。
		if !strings.HasPrefix(name, filePrefix+"-") {
			continue
		}
		a, err := LoadFile(filepath.Join(dir, name))
		if err != nil {
			logf("minimax: 跳过无法解析的凭证文件 %s：%v", name, err)
			continue
		}
		if a == nil || !a.Usable() {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID() < out[j].UID() })
	return out, nil
}

// LoadFile 读单个凭证文件。
func LoadFile(path string) (*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a, err := parseAuth(raw)
	if err != nil {
		return nil, err
	}
	a.FilePath = path
	return a, nil
}

// parseAuth 解析凭证文件（落盘形态）。
//
// ⚠ 形态必须是 `{"auth":{…}}`。但**容忍**扁平形（直接把 Auth 的字段放顶层）：
// 用户手工拼的/从别处导来的文件常是扁平的，而"拒绝它"只会让人以为导入坏了。
func parseAuth(raw []byte) (*Auth, error) {
	var f authFile
	if err := json.Unmarshal(raw, &f); err == nil && f.A != nil && f.A.Usable() {
		return f.A, nil
	}
	var flat Auth
	if err := json.Unmarshal(raw, &flat); err == nil && flat.Usable() {
		return &flat, nil
	}
	return nil, fmt.Errorf("minimax: 凭证文件里没有可用的 access_token")
}

// SaveFile 把凭证写到 `<dir>/minimax-<uid>.json`（原子写 + 0600）。
func SaveFile(dir string, a *Auth) (string, error) {
	if a == nil || !a.Usable() {
		return "", fmt.Errorf("minimax: 凭证不可用（缺 access_token）")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, FileName(a))
	raw, err := json.MarshalIndent(authFile{A: a}, "", "  ")
	if err != nil {
		return "", err
	}
	// 原子写：先写 .tmp 再 rename —— 中途失败不会留下半个文件
	//（半个文件会被 LoadDir 当成"坏文件"跳过，表现为"账号随机消失"）。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	a.FilePath = path
	return path, nil
}

// saveInPlace 把凭证写回它**自己的来源文件**（原子写 + 0600）。
//
// # 为什么需要它（而不是复用 SaveFile）
//
// `SaveFile(dir, a)` 会按 uid **重新算文件名**并写到指定目录。
// 而续期要的是"写回它原来那个文件" —— 两者在正常情况下结果相同，
// 但有一个真实的分叉：用户手工放进来的凭证文件名可能不是
// `minimax-<uid>.json`（例如从别处导来的 `my-token.json`）。
// 用 SaveFile 会**多出一个文件**，而旧的那份还在 —— 重启后
// LoadDir 读到两份、uid 相同被去重，行为取决于读目录的顺序。
//
// 所以续期走"就地写回"，语义明确。
//
// FilePath 为空（内存里造的凭证，例如粘贴导入但还没落盘）时
// 返回 nil —— 那不是错误，调用方只记日志。
func saveInPlace(a *Auth) error {
	if a == nil || a.FilePath == "" {
		return nil
	}
	raw, err := json.MarshalIndent(authFile{A: a}, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.FilePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
