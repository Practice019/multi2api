// credential.go MiMo 凭证模型与文件读写。
//
// # 凭证形态（三态导入归一，评审报告 §2.3/§4.4）
//
// 我们自己的落盘格式是扁平 JSON（含 channel/type 判别字段）。
// Parse 额外兼容三种外部形态（批量导入/桌面端产物直接可吃）：
//
//	官方 auth.json 条目    {"type":"api","key":"…","metadata":{"uid":…,"base_url":…}}
//	官方 oauth 条目        {"refresh":"…","access":"…","expires":<秒>,…}
//	MiMo2API 桌面旧版      {"access_token":"…","refresh_token":"…","expires_at":<毫秒>}
//
// 毫秒/秒混用是实锤（expires_at 两种都出现），normalizeExpiresAt 统一。
package mimo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// filePrefix 凭证文件名前缀。LoadDir 按 `mimo*.json` 通配（与其余上游同律）。
const filePrefix = "mimo"

// 通道与凭证类型判别值。
const (
	ChannelPaid  = "paid"  // 开放平台（Bearer sk/tp）
	ChannelRoute = "route" // 桌面端主网关（Cookie serviceToken）—— 桌面配额，与平台余额两个池
	ChannelFree  = "free"  // CLI 免费通道（留位，默认关）

	TypeAPI   = "api"   // 长期 sk-/tp-
	TypeOAuth = "oauth" // 小米账号 OAuth（导入兼容位，孤证链路）
)

// Auth 归一化后的 MiMo 账号凭证。
//
// 并发模型与 trae 同构：mu 保护可变字段（AccessToken/RefreshToken/ExpiresAt），
// 刷新路径持写锁整段执行（oauth 轨的 refresh 若服务端轮换，语义未证——
// 持锁串行是唯一安全解）。其余字段加载后不变，直接读。
type Auth struct {
	mu sync.RWMutex

	Channel string // paid|free（空=paid，向后兼容）
	Type    string // api|oauth（空=api）

	Key string // sk-/tp- 长期 key（paid 主轨唯一鉴权材料）
	// ── route 通道专用（桌面端 mimo-server-cn 网关，Cookie 认证）──
	// 抓包实锤（2026-09-24 桌面端探测报告 §5.1/§6.3）：业务 API 一律
	// `Cookie: serviceToken=…; userId=…; mimopc_slh=…; mimopc_ph=…`，
	// **无 Authorization 头、无设备绑定校验**，令牌可独立复制调用。
	ServiceToken string // 会话级令牌（由 SSO 链现换，见 sso.go）
	// ── SSO 链的输入（用户探测报告 §3.3：一条 passToken 就能全自动换票）──
	PassToken string // 小米账号会话（30 天；serviceLogin 每次会续签 —— 网关同步滚动保存）
	CUserID   string // cUserId（serviceLogin 全套 account cookie 之一，缺一则 302 进 SPA 死路）

	Slh          string // mimopc_slh Cookie
	Ph           string // mimopc_ph Cookie
	ServiceAt    int64  // serviceToken 换到手时刻（Unix 秒）—— 主动预换的年龄依据
	DeviceD      string // 抓包时的设备指纹 d=pc_<hex32>（信息位，供审计/复现）
	RouteVersion string // x-client-version（桌面 app 版本，默认 26.923.232338）
	BaseURL      string // 账号专属网关（OAuth url 字段/区域网关）；空=用配置默认
	ClientID     string // oauth 刷新用的 client id（**随凭证保存**，TRAE 教训）
	AccountID    string // oauth accountId（可选）

	AccessToken      string // oauth：access token；free：bootstrap JWT
	RefreshToken     string // oauth：refresh_token（是否一次性轮换未证 → 持锁刷）
	ExpiresAt        int64  // 上述 AccessToken 的过期时刻（Unix 秒；0=无）
	RefreshExpiresAt int64  // refreshToken 自身过期（Unix 秒；0=未知——TRAE 事故的另一半教训：必须跟踪）

	Fingerprint string // free 轨的 64hex client 指纹

	UID        string // 池主键；sk 凭证缺 uid 时 deriveUID(key) 兜底
	Nickname   string
	LoggedInAt string // 同 uid 多文件取较新者（pickWinners 用）
	Source     string // oauth|import|authjson|manual
	FilePath   string // 落盘路径；刷新后原子写回
}

// KeyType 按前缀判 key 型（OmniProxy normalize 同款规则）：
// pay=按量 sk-，tokenplan=套餐 tp-，unknown=其它（不拒，只是不参与类型互备）。
func (a *Auth) KeyType() string {
	switch {
	case strings.HasPrefix(a.Key, "sk-"):
		return "pay"
	case strings.HasPrefix(a.Key, "tp-"):
		return "tokenplan"
	default:
		return "unknown"
	}
}

// CookieString 拼 route 通道的完整 Cookie 头（四项，实锤 §6.3）。
// 空值的辅助项跳过 —— 实测必需的是 serviceToken/userId，slh/ph 带上更像话。
func (a *Auth) CookieString() string {
	if a == nil {
		return ""
	}
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("serviceToken", a.ServiceToken)
	add("userId", a.UID)
	add("mimopc_slh", a.Slh)
	add("mimopc_ph", a.Ph)
	return strings.Join(parts, "; ")
}

// BearerToken 当前可用的鉴权材料快照（paid=Key；oauth/free=AccessToken）。
func (a *Auth) BearerToken() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Channel == ChannelFree || a.Type == TypeOAuth {
		return a.AccessToken
	}
	return a.Key
}

// NeedsRefresh 报告可刷凭证（oauth/free）是否将在 within 内过期。
//
// ⚠ 纯 sk 凭证**不该被问这个** —— 没有可刷材料（NeedsRenew 恒 false），
// 预检关闭由 RefreshSkew=(0,true) 在核心侧完成（见 extensions.go 事故档案）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a == nil || !a.Renewable() {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ExpiresAt <= 0 {
		return false // 无过期信息 ≠ 已过期（sk 语义）
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Renewable 报告这份凭证有没有"可刷/可重取"的材料。
//
//	free：有指纹（重 bootstrap 幂等）；
//	oauth：有 refreshToken；
//	纯 sk：false —— CredentialRefresher 对它是无害 no-op。
func (a *Auth) Renewable() bool {
	if a == nil {
		return false
	}
	switch a.Channel {
	case ChannelRoute:
		// 有 passToken 才能走 SSO 链自动换票；只贴了 serviceToken 的旧式凭证
		// 依旧不可续（过期显示「需重新登录」—— 不假装能刷，TRAE 教训）。
		return a.PassToken != "" && a.CUserID != ""
	case ChannelFree:
		return a.Fingerprint != ""
	case ChannelPaid:
		return a.Type == TypeOAuth && a.RefreshToken != ""
	}
	return false
}

// TokenDead 报告 refreshToken 是否已过期（不可再用于刷新）。
func (a *Auth) TokenDead() bool {
	if a == nil {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.RefreshToken == "" || a.RefreshExpiresAt <= 0 {
		return false
	}
	return time.Now().Unix() >= a.RefreshExpiresAt
}

// SaveAtomic 原子写回 FilePath（tmp+rename，0600）。加锁外壳防半更新。
func (a *Auth) SaveAtomic() error {
	if a == nil {
		return fmt.Errorf("mimo: 空凭证")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FilePath == "" {
		return fmt.Errorf("mimo: 没有 FilePath（凭证未从目录加载？）")
	}
	raw, err := marshalLocked(a)
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// Parse 解析凭证 JSON：自有扁平格式 + 三态外部形态归一。
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("mimo: 凭证文件为空")
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("mimo: 凭证不是合法 JSON: %w", err)
	}
	a := &Auth{}
	// snake_case 候选集（官方 oauth/MiMo2API 桌面旧版都用下划线）。
	Str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := doc[k]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					return s
				}
			}
		}
		return ""
	}
	Int := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := doc[k]; ok {
				var n json.Number
				dec := json.NewDecoder(strings.NewReader(string(v)))
				dec.UseNumber()
				if dec.Decode(&n) == nil {
					if iv, err := n.Int64(); err == nil && iv > 0 {
						return iv
					}
				}
			}
		}
		return 0
	}

	// metadata 子对象（官方 auth.json 的 api 条目形态）。
	var meta struct {
		UID     string `json:"uid"`
		BaseURL string `json:"base_url"`
	}
	if mv, ok := doc["metadata"]; ok {
		_ = json.Unmarshal(mv, &meta)
	}

	a.Channel = Str("channel")
	a.Type = Str("type")
	a.Key = Str("key", "api_key", "apiKey")
	a.BaseURL = firstNonEmpty(Str("baseUrl", "base_url"), meta.BaseURL)
	a.ClientID = Str("clientId", "client_id")
	a.AccountID = Str("accountId", "account_id")
	a.ServiceToken = Str("serviceToken", "service_token")
	a.PassToken = Str("passToken", "pass_token")
	a.CUserID = Str("cUserId", "c_user_id")
	if a.DeviceD == "" {
		a.DeviceD = Str("deviceId", "device_id") // 与抓包 d=pc_… 同值（§3.3）
	}
	a.ServiceAt = normalizeExpiresAt(Int("serviceAt", "service_at"))
	a.Slh = Str("mimopc_slh", "slh")
	a.Ph = Str("mimopc_ph", "ph")
	a.DeviceD = Str("deviceD", "device_d", "d")
	a.RouteVersion = Str("routeVersion", "client_version")
	a.AccessToken = Str("accessToken", "access_token", "access", "jwt")
	a.RefreshToken = Str("refreshToken", "refresh_token", "refresh")
	a.ExpiresAt = normalizeExpiresAt(Int("expiresAt", "expires_at", "expires", "jwtExpiresAt"))
	a.RefreshExpiresAt = normalizeExpiresAt(Int("refreshExpiresAt", "refresh_expires_at"))
	a.Fingerprint = Str("fingerprint", "client")
	a.UID = firstNonEmpty(Str("uid"), meta.UID, Str("accountId", "account_id"))
	a.Nickname = Str("nickname")
	a.LoggedInAt = Str("loggedInAt", "logged_in_at")
	a.Source = Str("source")

	if a.Channel == "" {
		switch {
		case a.ServiceToken != "" || (a.PassToken != "" && a.CUserID != ""):
			a.Channel = ChannelRoute
		case a.Fingerprint != "" && a.AccessToken != "" && a.Key == "":
			a.Channel = ChannelFree // 纯 free 档案
		default:
			a.Channel = ChannelPaid
		}
	}
	if a.Type == "" {
		if a.AccessToken != "" && a.RefreshToken != "" && a.Key == "" {
			a.Type = TypeOAuth
		} else {
			a.Type = TypeAPI
		}
	}
	// 判"有鉴权材料"：route 要 serviceToken+userId；paid 要 key；oauth 要 access；free 要 jwt/指纹。
	switch {
	case a.Channel == ChannelRoute && a.ServiceToken == "" && (a.PassToken == "" || a.CUserID == ""):
		return nil, fmt.Errorf("mimo: route 凭证既没有 serviceToken（即时可用），也没有 passToken+cUserId（SSO 可续）")
	case a.Channel == ChannelPaid && a.Type == TypeAPI && a.Key == "":
		return nil, fmt.Errorf("mimo: paid/api 凭证缺 key（sk-/tp-）")
	case a.Channel == ChannelPaid && a.Type == TypeOAuth && a.AccessToken == "" && a.RefreshToken == "":
		return nil, fmt.Errorf("mimo: oauth 凭证既无 access 也无 refresh")
	case a.Channel == ChannelFree && a.AccessToken == "" && a.Fingerprint == "":
		return nil, fmt.Errorf("mimo: free 凭证既无 jwt 也无指纹")
	}
	if a.UID == "" {
		// 稳定派生：key > access > refresh（同一份材料重载必得同一 uid，
		// 这是"重载/重启不产生重复账号"的前提）。
		if base := firstNonEmpty(a.Key, a.AccessToken, a.RefreshToken); base != "" {
			a.UID = DeriveUID(base)
		}
	}
	if a.UID == "" {
		// pass-only 的 route 凭证此刻可以没有 uid —— SSO 换票返回 userId 后
		// 由 import/sync 回填；其余形态仍必须有稳定主键。
		if !(a.Channel == ChannelRoute && a.PassToken != "") {
			return nil, fmt.Errorf("mimo: 凭证无法确定 uid（缺 uid 与可推导的 key）")
		}
	}
	if a.Nickname == "" && (a.Key != "" || a.AccessToken != "") {
		a.Nickname = MaskKey(firstNonEmpty(a.Key, a.AccessToken))
	}
	return a, nil
}

// DeriveUID 无 uid 的长期 key 的稳定兜底主键（loomy deriveUID 模板）：
// sha256(key) 前 12 位。**同一 key 永远映射到同一 uid** —— 这是"重载/重启
// 不产生重复账号"的前提。
func DeriveUID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "u-" + hex.EncodeToString(sum[:])[:12]
}

// MaskKey 掩码（前 7 + *** + 后 4，OmniProxy 同法）：任何回显路径都不给全 key。
func MaskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		return "***"
	}
	return k[:7] + "***" + k[len(k)-4:]
}

// LoadDir 扫描 dir 下 `mimo*.json`。目录不存在/无匹配 → (nil,nil) 非错误；
// 单文件坏 → 记日志跳过（与其余上游对齐）。
func LoadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, filePrefix+"*.json"))
	if err != nil {
		return nil, fmt.Errorf("mimo: 扫描凭证目录 %s 失败: %w", dir, err)
	}
	sort.Strings(files)
	var out []*Auth
	for _, fp := range files {
		raw, err := os.ReadFile(fp)
		if err != nil {
			log.Printf("mimo: 读取凭证文件失败，已跳过 %s: %v", filepath.Base(fp), err)
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			log.Printf("mimo: 解析凭证文件失败，已跳过 %s: %v", filepath.Base(fp), err)
			continue
		}
		a.FilePath = fp
		out = append(out, a)
	}
	return out, nil
}

// FileName 某凭证的落盘文件名（铁律 mimo-<uid>.json：glob 是 mimo*.json，
// 违反命名 = 重载扫不到）。
func FileName(a *Auth) string {
	if a == nil {
		return ""
	}
	return filePrefix + "-" + sanitizeFilePart(a.UID) + ".json"
}

// MarshalAuthFile 编成自有扁平格式（带缩进与结尾换行）。
func MarshalAuthFile(a *Auth) ([]byte, error) { return marshalLocked(a) }

func marshalLocked(a *Auth) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("mimo: 凭证为空")
	}
	doc := map[string]any{
		"channel": a.Channel,
		"type":    a.Type,
		"uid":     a.UID,
	}
	set := func(k, v string) {
		if v != "" {
			doc[k] = v
		}
	}
	set("key", a.Key)
	set("serviceToken", a.ServiceToken)
	set("passToken", a.PassToken)
	set("cUserId", a.CUserID)
	set("deviceId", a.DeviceD)
	set("mimopc_slh", a.Slh)
	set("mimopc_ph", a.Ph)
	set("deviceD", a.DeviceD)
	set("routeVersion", a.RouteVersion)
	set("baseUrl", a.BaseURL)
	set("clientId", a.ClientID)
	set("accountId", a.AccountID)
	set("accessToken", a.AccessToken)
	set("refreshToken", a.RefreshToken)
	set("fingerprint", a.Fingerprint)
	set("nickname", a.Nickname)
	set("loggedInAt", a.LoggedInAt)
	set("source", a.Source)
	setNum := func(k string, v int64) {
		if v > 0 {
			doc[k] = v
		}
	}
	setNum("serviceAt", a.ServiceAt)
	setNum("expiresAt", a.ExpiresAt)
	setNum("refreshExpiresAt", a.RefreshExpiresAt)
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// normalizeExpiresAt 毫秒/秒统一（>1e12 → 秒）。三来源（自有/oauth 秒/
// MiMo2API 毫秒）混用是实锤，不统一会造出"5 万年后过期"。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// sanitizeFilePart UID 里不适合做文件名的字符换掉。
func sanitizeFilePart(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
