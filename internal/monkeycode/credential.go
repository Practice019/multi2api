// credential.go MonkeyCode 凭证的形态、落盘与读取。
//
// # 三把凭据，缺一不可
//
// MonkeyCode 的模型接口要**两样东西同时在场**：
//
//	session         `monkeycode_ai_session` 的 uuid —— 用来查额度/模型目录/签到
//	api_key         `oma_…`                   —— 模型网关的 Bearer
//	signing_secret  `omas_…`                  —— `X-OhMyAgent-Signature` 的 HMAC 密钥
//
// 缺 api_key/signing_secret 时**不能对话**（网关会回
// `403 {"error":"invalid ohmyagent request"}`），但**仍可**查额度。
// 所以 Usable() 判的是 session（能探活），而 ChatUsable() 才判三把齐。
//
// # 三把是怎么来的
//
//	session        用户从浏览器 DevTools 复制（无登录协议，见 login.go 说明）
//	api_key+secret `POST /api/v1/users/ohmyagent/api-keys`（用 session 换）
//
// 第二把是**可无限签发**的（实测：同一账号可签任意多把，旧的不会失效），
// 所以这里存的是**我们签发的那一把**，不是用户已有的那一把。
package monkeycode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"log"
	"path/filepath"
	"sort"
	"strings"
)

// filePrefix 文件名前缀（`<dir>/monkeycode-<uid>.json`，与其余上游同款）。
const filePrefix = "monkeycode"

// 区域取值。
const (
	// RegionCN 国内站。基址 monkeycode-ai.com / proxy.monkeycode-ai.com。
	RegionCN = "cn"
	// RegionIntl 国际站。基址 monkeycode-ai.net / proxy.monkeycode-ai.net。
	RegionIntl = "intl"
)

// Auth 一份 MonkeyCode 凭证。
type Auth struct {
	// Session `monkeycode_ai_session` 的 uuid。**必需**，30 天硬过期，无续期接口。
	Session string `json:"session"`

	// APIKey 模型网关的 Bearer（`oma_` 前缀）。
	APIKey string `json:"api_key,omitempty"`

	// SigningSecret `X-OhMyAgent-Signature` 的 HMAC-SHA256 密钥（`omas_` 前缀）。
	SigningSecret string `json:"signing_secret,omitempty"`

	// Region `cn` / `intl`。空值按 cn 处理（国内站是主站）。
	//
	// ⚠ 不是"可选装饰"：两站的**模型目录、档位、额度单位都不同**
	// （国内站 45 个模型 / 国际站 11 个），选错基址会直接 404。
	Region string `json:"region,omitempty"`

	// Nickname 展示名。
	Nickname string `json:"nickname,omitempty"`

	// UserID 上游 `user.id`（uuid）。账号池主键的来源。
	UserID string `json:"userid,omitempty"`

	// Plan 订阅档位（`basic` / `pro` / `ultra`）。只作展示，不作判据。
	//
	// ⚠ 别拿它决定"能不能用某个模型"：真实权限在服务端，
	// 同一档位能用的模型集合随账号变化（实测 basic 号能跑
	// `monkeycode-basic/deepseek-v4-flash`，而它标着 is_hidden）。
	Plan string `json:"plan,omitempty"`

	// Balance 积分余额（探测时写入）。0 是合法值。
	Balance string `json:"balance,omitempty"`

	// DailyToken 每日 token 余量（探测时写入）。
	DailyToken string `json:"daily_token,omitempty"`

	// DailyTokenLimit 每日 token 上限（探测时写入）。
	DailyTokenLimit string `json:"daily_token_limit,omitempty"`

	// AddedAt 入库时刻（RFC3339，仅展示）。
	AddedAt string `json:"added_at,omitempty"`

	// FilePath 落盘路径（读盘时填，**不序列化**）。
	FilePath string `json:"-"`
}

// authFile 落盘外层形态 `{"auth":{…},"remark":"…"}`（与其余上游一致）。
type authFile struct {
	A      *Auth  `json:"auth"`
	Remark string `json:"remark,omitempty"`
}

// Provider 区域标识。
func (a *Auth) RegionOrDefault() string {
	if a == nil {
		return RegionCN
	}
	if strings.TrimSpace(a.Region) == RegionIntl {
		return RegionIntl
	}
	return RegionCN
}

// Host 区域对应的网页基址。
func (a *Auth) Host() string {
	if a.RegionOrDefault() == RegionIntl {
		return hostIntl
	}
	return hostCN
}

// ProxyBase 区域对应的模型网关基址。
func (a *Auth) ProxyBase() string {
	if a.RegionOrDefault() == RegionIntl {
		return proxyBaseIntl
	}
	return proxyBaseCN
}

// UID 账号池主键。
//
// 优先 UserID（上游真身份，稳定），其次 session 哈希（兜底）。
//
// ⚠ **不能**用 api_key 当主键：那个是**可重新签发**的，
// 重签一次就会多出一条账号记录（minimax 的 token-hash 踩过这个坑）。
// session 相对稳定（30 天内不变），拿它兜底是安全的。
func (a *Auth) UID() string {
	if a == nil {
		return ""
	}
	if s := strings.TrimSpace(a.UserID); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.Session); s != "" {
		return "session-" + shortHash(s)
	}
	return ""
}

// DisplayUID 账号池展示用的短 uid。
func (a *Auth) DisplayUID() string {
	uid := a.UID()
	if len(uid) > 12 {
		return uid[:12] + "…"
	}
	return uid
}

// DisplayName 展示名。
func (a *Auth) DisplayName() string {
	if a == nil {
		return ""
	}
	if s := strings.TrimSpace(a.Nickname); s != "" {
		return s
	}
	return a.DisplayUID()
}

// Usable 能不能拿去查额度/模型目录（只需 session）。
func (a *Auth) Usable() bool {
	return a != nil && strings.TrimSpace(a.Session) != ""
}

// ChatUsable 能不能拿去对话（三把齐）。
//
// ⚠ 与 Usable 分开是**必需的**：凭证刚导入时通常只有 session，
// api_key/signing_secret 要事后调一次签发接口才有。若把两者合成一个
// 判据，那批"只有 session"的凭证会被当成坏凭证**整条丢掉**，
// 用户看到的现象是"我明明粘贴了 session，账号却不见了"。
func (a *Auth) ChatUsable() bool {
	return a.Usable() &&
		strings.TrimSpace(a.APIKey) != "" &&
		strings.TrimSpace(a.SigningSecret) != ""
}

// FileName 该凭证的落盘文件名。
func FileName(a *Auth) string {
	return filePrefix + "-" + sanitizeFilePart(a.UID()) + ".json"
}

// MarshalAuthFile 导出落盘内容（供登录/导入流程写盘）。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.A == nil {
		return "", nil, fmt.Errorf("monkeycode: 凭证为空")
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return FileName(f.A), raw, nil
}

// MarshalAuthFile 把一份凭证包成落盘形态。
func MarshalAuthFile(a *Auth, remark string) (string, []byte, error) {
	return (&authFile{A: a, Remark: remark}).MarshalAuthFile()
}

// LoadDir 读一个目录下的全部凭证。
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
		// 只认本上游前缀（目录里可能有别家的残留）。
		if !strings.HasPrefix(name, filePrefix+"-") {
			continue
		}
		a, err := LoadFile(filepath.Join(dir, name))
		if err != nil {
			logf("monkeycode: 跳过无法解析的凭证文件 %s：%v", name, err)
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

// parseAuth 解析凭证文件。
//
// ⚠ 形态应当是 `{"auth":{…}}`，但**容忍**扁平形（直接把字段放顶层）：
// 用户手工拼的、或从别处导来的文件常是扁平的，拒绝它只会让人以为导入坏了。
func parseAuth(raw []byte) (*Auth, error) {
	var f authFile
	if err := json.Unmarshal(raw, &f); err == nil && f.A != nil && f.A.Usable() {
		return f.A, nil
	}
	var flat Auth
	if err := json.Unmarshal(raw, &flat); err == nil && flat.Usable() {
		return &flat, nil
	}
	return nil, fmt.Errorf("monkeycode: 凭证文件里没有可用的 session")
}

// SaveFile 把凭证写到 `<dir>/monkeycode-<uid>.json`（原子写 + 0600）。
func SaveFile(dir string, a *Auth) (string, error) {
	if a == nil || !a.Usable() {
		return "", fmt.Errorf("monkeycode: 凭证不可用（缺 session）")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, FileName(a))
	_, raw, err := MarshalAuthFile(a, "")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// sanitizeFilePart 文件名里只保留安全字符。
func sanitizeFilePart(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

// shortHash 取 SHA-256 的前 8 字节十六进制（16 字符），用于兜底 uid。
//
// ⚠ 不用完整 64 字符：文件名与列表列都放不下，而且这里的用途只是
// "同一份 session 稳定映射到同一个 uid"，16 字符的碰撞概率足够低。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// logf 打一条本上游的日志（前缀统一，便于 grep）。
func logf(format string, args ...any) {
	log.Printf(format, args...)
}
