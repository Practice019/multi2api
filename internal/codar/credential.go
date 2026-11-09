package codar

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 明确的错误（不 panic；断言失败时给"是什么不对"而不是空指针）。
var (
	errNoCredential  = errors.New("codar: 凭证为空（Credential.Secret 未设置）")
	errWrongCredType = errors.New("codar: 凭证类型不对（期望 *codar.Auth）")
	errNoToken       = errors.New("codar: 凭证缺少 token（唯一鉴权材料）")
)

// Auth 一份 Codar 凭证。
//
// 与 Go 侧网关约定的落盘结构（同时兼容 T-311 观察到的桌面端嵌套形态）：
//
//	{ "auth": { "token": "...", "username": "...", "expiresAt": 1794… },
//	  "account": { "uid": "...", "nickname": "..." } }
//
// 也接受**扁平**形态（{token,username,uid,expiresAt}）——导入别的工具时更省事。
type Auth struct {
	Token     string `json:"token"`
	Username  string `json:"username,omitempty"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	ExpiresAt int64  `json:"expiresAt,omitempty"` // Unix 秒；0 = 未知
	FilePath  string `json:"-"`                   // 该凭证来自哪个文件（不落盘）
}

type storedFile struct {
	Auth    *Auth `json:"auth"`
	Account *struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	} `json:"account"`
	// 扁平形态
	Token     string `json:"token"`
	Username  string `json:"username"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	ExpiresAt int64  `json:"expiresAt"`
}

// normalize 把两种落盘形态统一成 Auth（嵌套优先，缺项用扁平补）。
func (f *storedFile) normalize() *Auth {
	a := &Auth{}
	if f.Auth != nil {
		a.Token, a.Username, a.ExpiresAt = f.Auth.Token, f.Auth.Username, f.Auth.ExpiresAt
	}
	if f.Account != nil {
		a.UID, a.Nickname = f.Account.UID, f.Account.Nickname
	}
	if a.Token == "" {
		a.Token = f.Token
	}
	if a.Username == "" {
		a.Username = f.Username
	}
	if a.UID == "" {
		a.UID = f.UID
	}
	if a.Nickname == "" {
		a.Nickname = f.Nickname
	}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = f.ExpiresAt
	}
	a.fillUID()
	return a
}

// fillUID 补 UID：优先 username，其次 token 指纹。
//
// 为什么不用 token 当 UID：token 会轮换（重新登录就换），拿它当账号身份会让
// 同一个账号在池子里变成多个（codearts 当年踩过"同名多份凭证"的坑）。
func (a *Auth) fillUID() {
	if a.UID != "" {
		return
	}
	if a.Username != "" {
		a.UID = a.Username
		return
	}
	sum := sha256.Sum256([]byte(a.Token))
	a.UID = "tok-" + hex.EncodeToString(sum[:8])
}

// LoadDir 读取目录下的 *(codar)*.json（与其它上游同构：前缀写死、跳过子目录）。
func LoadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil // ⚠ 空目录守卫：filepath.Glob("") 会退化成"相对 CWD"的匹配
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "codar") || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		p := filepath.Join(dir, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var sf storedFile
		if err := json.Unmarshal(raw, &sf); err != nil {
			continue
		}
		a := sf.normalize()
		if a.Token == "" {
			continue
		}
		a.FilePath = p
		out = append(out, a)
	}
	return dedupeByUID(out), nil
}

// dedupeByUID 同一 UID 只保留**到期最晚**的那份；完全保序（与 codearts 的同名函数同语义）。
//
// 背景：重复登录会留下多份凭证；不去重会让"同账号多份"进池 ⇒ 调度与统计都被稀释。
// 文件名与时间无关 ⇒ 用 ExpiresAt 比较；ExpiresAt 为 0（未知）视为"最旧"。
func dedupeByUID(list []*Auth) []*Auth {
	best := map[string]*Auth{}
	order := []string{}
	for _, a := range list {
		if a == nil || a.UID == "" {
			continue
		}
		cur, ok := best[a.UID]
		if !ok {
			best[a.UID] = a
			order = append(order, a.UID)
			continue
		}
		if a.ExpiresAt > cur.ExpiresAt {
			best[a.UID] = a
		}
	}
	// 保序：按"首次出现顺序"输出（这样列表顺序稳定，便于人看）
	sort.SliceStable(order, func(i, j int) bool { return false })
	out := make([]*Auth, 0, len(order))
	for _, uid := range order {
		if a := best[uid]; a != nil {
			out = append(out, a)
		}
	}
	return out
}

// desktopConfig 桌面端 ~/.codar/config.json 的**最小**结构（只认识我们需要的键）。
type desktopConfig struct {
	Provider struct {
		AuthType string `json:"auth_type"`
		APIBase  string `json:"api_base"`
		APIType  string `json:"api_type"`
		Speedai  *struct {
			Token    string `json:"token"`
			Username string `json:"username"`
			LoginAt  int64  `json:"login_at"`
		} `json:"speedai"`
		Profiles map[string]struct {
			Speedai *struct {
				Token    string `json:"token"`
				Username string `json:"username"`
				LoginAt  int64  `json:"login_at"`
			} `json:"speedai"`
			APIBase string `json:"api_base"`
		} `json:"provider_profiles"`
	} `json:"provider"`
}

// ImportFromDesktopConfig 从桌面端配置里取出 token，转成 *Auth（**只读**，不修改该文件）。
//
// 依据 T-311 实测：`provider.api_base` 与 `provider.provider_profiles.speedai.api_base`
// 都为 https://api.claw.speedai.com/v1；token 在 `provider.speedai.token`（JWT，207 字符）。
// 返回的第 2 个值是该配置里声明的 api_base（供调用方覆盖 Config.APIBase）。
func ImportFromDesktopConfig(path string) (*Auth, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var dc desktopConfig
	if err := json.Unmarshal(raw, &dc); err != nil {
		return nil, "", fmt.Errorf("codar: 解析桌面端 config.json 失败: %w", err)
	}
	tok, user := "", ""
	if dc.Provider.Speedai != nil {
		tok, user = dc.Provider.Speedai.Token, dc.Provider.Speedai.Username
	}
	if tok == "" { // 回退到 profile
		for _, p := range dc.Provider.Profiles {
			if p.Speedai != nil && p.Speedai.Token != "" {
				tok, user = p.Speedai.Token, p.Speedai.Username
				break
			}
		}
	}
	if tok == "" {
		return nil, dc.Provider.APIBase, errors.New("codar: 桌面端配置里没有 provider.speedai.token")
	}
	a := &Auth{Token: tok, Username: user, ExpiresAt: jwtExpiry(tok), FilePath: path}
	a.fillUID()
	if a.Nickname == "" {
		a.Nickname = a.UID
	}
	return a, dc.Provider.APIBase, nil
}

// SaveOne 把一份 Auth 落盘到 dir（0600），文件名 codar-<uid>.json。
func SaveOne(dir string, a *Auth) (string, error) {
	if a == nil || a.Token == "" {
		return "", errNoToken
	}
	a.fillUID()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := "codar-" + safeName(a.UID) + ".json"
	p := filepath.Join(dir, name)
	// ★ 兜底：调用方给的 ExpiresAt 为 0 时，从 token 的 exp 解出来再落盘。
	//（2026-11-08 真机踩过：手工造的 Auth 没填 exp ⇒ 文件里 expiresAt=0 ⇒ 界面看不到到期时间，
	//  且 dedupeByUID 的"选到期最晚"会把这份当成最旧 —— 单测 TestSaveOneDerivesExpiryFromToken 钉住。）
	if a.ExpiresAt == 0 {
		a.ExpiresAt = jwtExpiry(a.Token)
	}
	doc := map[string]any{
		"auth":    map[string]any{"token": a.Token, "username": a.Username, "expiresAt": a.ExpiresAt},
		"account": map[string]any{"uid": a.UID, "nickname": a.Nickname},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil { // ★ 凭证落盘 0600
		return "", err
	}
	a.FilePath = p
	return p, nil
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// jwtExpiry 从 JWT 里取 exp（不校验签名 —— 我们只用它做展示/续期判断，真伪由上游决定）。
func jwtExpiry(token string) int64 {
	c, ok := jwtClaims(token)
	if !ok {
		return 0
	}
	if v, ok := c["exp"].(float64); ok {
		return int64(v)
	}
	return 0
}

// jwtClaims 解 JWT 的 payload 段（base64url，无填充）。
func jwtClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, false
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		return nil, false
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// ExpiresTime 把 ExpiresAt 转成 time.Time（0 ⇒ 零值）。
func (a *Auth) ExpiresTime() time.Time {
	if a == nil || a.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(a.ExpiresAt, 0)
}

// MarshalAuthFile 实现 core 要求的落盘契约（internal/admin 的 authFileWriter）。
//
// # 调用链（读 admin.go:2523-2557 得来）
//
//	LoginFlow.Poll 返回 Credential ⇒ core 断言 cred.Secret.(authFileWriter)
//	⇒ 调本方法拿 (文件名, 字节) ⇒ 写到**上游自报的 AuthDir()**（= auths/codar）⇒ 重扫目录并池
//
// ⚠ 文件名必须与 LoadDir 的前缀约束一致（"codar" 前缀），否则写下去也读不回来。
func (a *Auth) MarshalAuthFile() (string, []byte, error) {
	if a == nil || a.Token == "" {
		return "", nil, errNoToken
	}
	a.fillUID()
	if a.ExpiresAt == 0 {
		a.ExpiresAt = jwtExpiry(a.Token)
	}
	name := "codar-" + safeName(a.UID) + ".json"
	blob, err := json.MarshalIndent(map[string]any{
		"auth":    map[string]any{"token": a.Token, "username": a.Username, "expiresAt": a.ExpiresAt},
		"account": map[string]any{"uid": a.UID, "nickname": a.Nickname},
	}, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return name, blob, nil
}
