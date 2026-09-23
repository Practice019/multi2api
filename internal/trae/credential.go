// credential.go TRAE SOLO 的凭证模型与文件读写。
//
// # 凭证形态（实测自多个 trae 反代项目：traework2api / trae-api / trae-local-api）
//
// TRAE 账号登录后持有一对：
//
//	accessToken（JWT，用于所有 API 的 Cloud-IDE-JWT 鉴权头）
//	refreshToken（用于 ExchangeToken 轮换 accessToken，**每次消费即作废**）
//
// accessToken 有过期时间（ExpiresAt），refreshToken 轮换 —— 与 codearts 的
// 一次性 refresh_token 同构，所以刷新路径的并发约束照搬 codearts：
// 持写锁做整段 ExchangeToken（见 client.go 的 RefreshToken）。
//
// # 磁盘形态（两种都认）
//
//	嵌套形 {"auth":{...},"account":{...}}   ← 登录脚本/客户端导出
//	扁平形 {"accessToken":...,"uid":...}    ← 手写
//
// 文件名 `trae-<uid>.json`，与仓库惯例一致（workbuddy-*.json / codearts-*.json）。
package trae

import (
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

// filePrefix 凭证文件名前缀。LoadDir 按 `trae*.json` 通配。
const filePrefix = "trae"

// Auth 归一化后的 TRAE 账号凭证。
//
// 并发模型：mu 保护可变字段（AccessToken / RefreshToken / ExpiresAt）。
// 写路径（RefreshCredential）持写锁整段执行 ExchangeToken ——
// refreshToken 是**消费型**的，必须串行（与 codearts 同因）。
// 其余字段加载后不变，直接读。
type Auth struct {
	mu sync.RWMutex

	AccessToken  string // Cloud-IDE-JWT 头用
	RefreshToken string // 每次 ExchangeToken 轮换
	ExpiresAt    int64  // Unix 秒（accessToken 过期时刻）
	// RefreshExpiresAt refreshToken 自身的过期时刻（Unix 秒，可 0 = 未知/永不过期）。
	//
	// ⚠ 实测（trae-local-api / Trae2api-cn）：ExchangeToken 响应带 RefreshExpireAt，
	// refreshToken 不是永久的 —— 到期后 ExchangeToken 必然失败，且**没有预警**，
	// 账号表现为"突然过期"。跟踪它才能在到期前提示"需重新登录"。
	RefreshExpiresAt int64
	// ClientID 签发这份 refreshToken 的 OAuth client id。
	//
	// ⚠ 必须随凭证保存：ExchangeToken 的 ClientID 要与**签发** refreshToken 的
	// 那个一致（trae-local-api 用 ono9krqynydwx5、Trae2api-cn 从回调 userJwt
	// 里取 clientID）。硬编码一个值去刷所有账号，对"从桌面客户端导入"的账号
	// 会刷新失败 → 到期 → 突然过期。留空则回落 Client 默认值。
	ClientID     string
	Domain       string // "trae.cn"
	ApiHost      string // "https://api.trae.com.cn"（ExchangeToken host）
	MachineID    string // x-machine-id
	DeviceID     string // x-device-id
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 落盘路径；刷新后原子写回
}

// JWT 返回当前 accessToken 的读锁快照（防与刷新写并发竞态）。
func (a *Auth) JWT() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a == nil {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// RefreshTokenDead 报告 refreshToken 是否已过期（不可再用于 ExchangeToken）。
//
// RefreshExpiresAt<=0 视为未知 → 不死（沿用旧行为：能用就刷）。
// 到期后 ExchangeToken 必然失败，且此时 accessToken 也救不回来 ——
// 必须在到期前预警（见 runRefresh 的日志与失败计数）。
func (a *Auth) RefreshTokenDead() bool {
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

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename，0600）。
//
// 与 codearts 同一条：刷新成功后要落盘，否则下次启动读到的还是旧 token。
// 加锁外壳：防止与刷新并发写回半更新。
func (a *Auth) SaveAtomic() error {
	if a == nil {
		return fmt.Errorf("trae: 空凭证")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FilePath == "" {
		return fmt.Errorf("trae: 没有 FilePath（凭证未从目录加载？）")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":      a.AccessToken,
			"refreshToken":     a.RefreshToken,
			"expiresAt":        a.ExpiresAt,
			"refreshExpiresAt": a.RefreshExpiresAt,
			"clientId":         a.ClientID,
			"domain":           a.Domain,
			"apiHost":          a.ApiHost,
			"machineId":        a.MachineID,
			"deviceId":         a.DeviceID,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// Parse 兼容嵌套形与扁平形。
//
// 判据与 traework2api 一致：顶层有 "auth" 键 → 嵌套；否则扁平。
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("trae: 凭证文件为空")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("trae: 凭证不是合法 JSON: %w", err)
	}
	a := &Auth{}
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken      string `json:"accessToken"`
				RefreshToken     string `json:"refreshToken"`
				ExpiresAt        int64  `json:"expiresAt"`
				RefreshExpiresAt int64  `json:"refreshExpiresAt"`
				ClientID         string `json:"clientId"`
				Domain           string `json:"domain"`
				ApiHost          string `json:"apiHost"`
				MachineID        string `json:"machineId"`
				DeviceID         string `json:"deviceId"`
			} `json:"auth"`
			// 部分导出（Trae2api-cn / 手写）把 clientId / refreshExpiresAt 放在顶层。
			ClientID         string `json:"clientId"`
			ClientIDAlt      string `json:"clientID"`
			ClientIDAlt2     string `json:"client_id"`
			RefreshExpiresAt int64  `json:"refreshExpiresAt"`
			RefreshExpAlt    int64  `json:"refreshExpireAt"`
			Account          struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("trae: 解析嵌套凭证失败: %w", err)
		}
		a.AccessToken = n.Auth.AccessToken
		a.RefreshToken = n.Auth.RefreshToken
		a.ExpiresAt = n.Auth.ExpiresAt
		a.RefreshExpiresAt = n.Auth.RefreshExpiresAt
		a.Domain = n.Auth.Domain
		a.ApiHost = n.Auth.ApiHost
		a.MachineID = n.Auth.MachineID
		a.DeviceID = n.Auth.DeviceID
		a.UID = n.Account.UID
		a.EnterpriseID = n.Account.EnterpriseID
		a.Nickname = n.Account.Nickname
		a.ClientID = firstNonEmpty(n.Auth.ClientID, n.ClientID, n.ClientIDAlt, n.ClientIDAlt2)
		if a.RefreshExpiresAt <= 0 {
			a.RefreshExpiresAt = firstPositive(n.RefreshExpiresAt, n.RefreshExpAlt)
		}
		if a.RefreshExpiresAt > 0 {
			a.RefreshExpiresAt = normalizeExpiresAt(a.RefreshExpiresAt)
		}
	} else {
		var f struct {
			AccessToken      string `json:"accessToken"`
			RefreshToken     string `json:"refreshToken"`
			ExpiresAt        int64  `json:"expiresAt"`
			RefreshExpiresAt int64  `json:"refreshExpiresAt"`
			Domain           string `json:"domain"`
			ApiHost          string `json:"apiHost"`
			MachineID        string `json:"machineId"`
			DeviceID         string `json:"deviceId"`
			ClientID         string `json:"clientId"`
			ClientIDAlt      string `json:"clientID"`
			ClientIDAlt2     string `json:"client_id"`
			RefreshExpAlt    int64  `json:"refreshExpireAt"`
			UID              string `json:"uid"`
			EnterpriseID     string `json:"enterpriseId"`
			Nickname         string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("trae: 解析扁平凭证失败: %w", err)
		}
		a.AccessToken = f.AccessToken
		a.RefreshToken = f.RefreshToken
		a.ExpiresAt = f.ExpiresAt
		a.RefreshExpiresAt = f.RefreshExpiresAt
		a.Domain = f.Domain
		a.ApiHost = f.ApiHost
		a.MachineID = f.MachineID
		a.DeviceID = f.DeviceID
		a.UID = f.UID
		a.EnterpriseID = f.EnterpriseID
		a.Nickname = f.Nickname
		a.ClientID = firstNonEmpty(f.ClientID, f.ClientIDAlt, f.ClientIDAlt2)
		if a.RefreshExpiresAt <= 0 {
			a.RefreshExpiresAt = firstPositive(f.RefreshExpiresAt, f.RefreshExpAlt)
		}
		if a.RefreshExpiresAt > 0 {
			a.RefreshExpiresAt = normalizeExpiresAt(a.RefreshExpiresAt)
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("trae: 凭证缺少 accessToken（唯一鉴权材料）")
	}
	// 无 refreshToken 的凭证仍然可用（只是到期后无法自动续期），不拒绝。
	return a, nil
}

// LoadDir 扫描 dir 下 `trae*.json`，返回全部可用凭证。
//
// 语义与另两个上游的 LoadDir 对齐：
//
//   - 目录不存在 / 没有匹配文件 → (nil, nil)，**不是错误**
//   - 单个文件解析失败 → 记日志并跳过
//   - 每个 Auth 带 FilePath（刷新后原子写回要用）
func LoadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, filePrefix+"*.json"))
	if err != nil {
		return nil, fmt.Errorf("trae: 扫描凭证目录 %s 失败: %w", dir, err)
	}
	sort.Strings(files)
	var out []*Auth
	for _, fp := range files {
		raw, err := os.ReadFile(fp)
		if err != nil {
			log.Printf("trae: 读取凭证文件失败，已跳过 %s: %v", filepath.Base(fp), err)
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			log.Printf("trae: 解析凭证文件失败，已跳过 %s: %v", filepath.Base(fp), err)
			continue
		}
		a.FilePath = fp
		out = append(out, a)
	}
	return out, nil
}

// FileName 返回某凭证应落盘的文件名。
func FileName(a *Auth) string {
	if a == nil {
		return ""
	}
	return filePrefix + "-" + sanitizeFilePart(a.UID) + ".json"
}

// MarshalAuthFile 把凭证编成落盘形态（嵌套形，带缩进与结尾换行）。
func MarshalAuthFile(a *Auth) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("trae: 凭证为空")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":      a.AccessToken,
			"refreshToken":     a.RefreshToken,
			"expiresAt":        a.ExpiresAt,
			"refreshExpiresAt": a.RefreshExpiresAt,
			"clientId":         a.ClientID,
			"domain":           a.Domain,
			"apiHost":          a.ApiHost,
			"machineId":        a.MachineID,
			"deviceId":         a.DeviceID,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// sanitizeFilePart 把 UID 里不适合做文件名的字符换掉。
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
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

// shortUID 取 UID 前 8 位供日志与展示名使用（按字符，避免劈开多字节）。
func shortUID(uid string) string {
	rs := []rune(uid)
	if len(rs) <= 8 {
		return uid
	}
	return string(rs[:8])
}

// firstNonEmpty 返回第一个非空串（用于多来源字段的兼容读取）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstPositive 返回第一个正数（0 = 未提供，用于字段兜底）。
func firstPositive(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
