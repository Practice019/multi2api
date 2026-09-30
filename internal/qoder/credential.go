// credential.go Qoder 凭证的读写。
package qoder

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filePrefix 凭证文件名前缀。
const filePrefix = "qoder"

// rootDoc 嵌套形态。
type rootDoc struct {
	Auth    *Auth `json:"auth"`
	Account *struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	} `json:"account"`
}

// ParseCredential 解析一份凭证 JSON。
func ParseCredential(raw []byte) (*Auth, error) {
	var doc rootDoc
	if err := json.Unmarshal(raw, &doc); err == nil && doc.Auth != nil && doc.Auth.AccessToken != "" {
		a := doc.Auth
		if doc.Account != nil {
			if a.UID == "" {
				a.UID = strings.TrimSpace(doc.Account.UID)
			}
			if a.Nickname == "" {
				a.Nickname = strings.TrimSpace(doc.Account.Nickname)
			}
		}
		a.normalize()
		return a, nil
	}
	var flat Auth
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("qoder: 凭证 JSON 无法解析: %w", err)
	}
	if flat.AccessToken == "" {
		return nil, fmt.Errorf("qoder: 凭证缺少 access_token")
	}
	flat.normalize()
	return &flat, nil
}

func (a *Auth) normalize() {
	if a == nil {
		return
	}
	a.AccessToken = strings.TrimSpace(a.AccessToken)
	a.RefreshToken = strings.TrimSpace(a.RefreshToken)
	a.UID = strings.TrimSpace(a.UID)
	a.Nickname = strings.TrimSpace(a.Nickname)
	a.MachineID = strings.TrimSpace(a.MachineID)
	a.DeviceID = strings.TrimSpace(a.DeviceID)
	a.ProductID = strings.TrimSpace(a.ProductID)
	if a.ProductID == "" {
		a.ProductID = providerID
	}
}

// LoadDir 扫描 dir 下 `qoder*.json`。
//
// ⚠ 两个产品（qoder / qodercn）**共用同一个目录**：
// 文件名前缀相同，靠凭证里的 product_id 区分。
// 这与"按上游分子目录"的其它上游不同 —— Qoder 两版是同协议族，
// 分开目录会让同一账号在两处重复。
//
// filterProduct 非空时只返回该产品的凭证（装配层按实例过滤）。
func LoadDir(dir string) ([]*Auth, error) {
	return loadDirFiltered(dir, "")
}

// LoadDirFor 只返回指定产品的凭证。
func LoadDirFor(dir, productID string) ([]*Auth, error) {
	return loadDirFiltered(dir, productID)
}

func loadDirFiltered(dir, productID string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("qoder: 读凭证目录 %s 失败: %w", dir, err)
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		p := filepath.Join(dir, name)
		raw, err := os.ReadFile(p)
		if err != nil {
			log.Printf("qoder: 读凭证文件失败，跳过 %s: %v", name, err)
			continue
		}
		a, err := ParseCredential(raw)
		if err != nil {
			log.Printf("qoder: 解析凭证失败，跳过 %s: %v", name, err)
			continue
		}
		if productID != "" && a.ProductID != productID {
			continue
		}
		a.FilePath = p
		if a.UIDValue() == "" {
			log.Printf("qoder: 凭证 %s 无法派生 uid，跳过", name)
			continue
		}
		out = append(out, a)
	}
	return pickWinners(out), nil
}

// pickWinners 同 uid 多份时按可用性判据选一份。
func pickWinners(list []*Auth) []*Auth {
	byUID := map[string]*Auth{}
	order := make([]string, 0, len(list))
	for _, a := range list {
		uid := a.UIDValue()
		cur, ok := byUID[uid]
		if !ok {
			byUID[uid] = a
			order = append(order, uid)
			continue
		}
		if betterAuth(a, cur) {
			byUID[uid] = a
		}
	}
	out := make([]*Auth, 0, len(order))
	for _, uid := range order {
		out = append(out, byUID[uid])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UIDValue() < out[j].UIDValue() })
	return out
}

func betterAuth(cand, cur *Auth) bool {
	cm, cu := cand.ExpiresAtMS(), cur.ExpiresAtMS()
	if cm != cu {
		return cm > cu
	}
	return cand.Renewable() && !cur.Renewable()
}

// FileName 凭证落盘文件名（含产品后缀，避免两版互相覆盖）。
func FileName(a *Auth) string {
	base := filePrefix + "-" + sanitizeFilePart(a.UIDValue())
	if a.ProductID == ProviderIDCN {
		base += "-cn"
	}
	return base + ".json"
}

// MarshalAuthFile 编成网关落盘形态（嵌套形）。
//
// ⚠ 必须持久化 machine_id：**续期请求体要用它**。
// 丢了它续期会被服务端拒绝。
func MarshalAuthFile(a *Auth) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("qoder: 空凭证")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"access_token":  a.AccessToken,
			"refresh_token": a.RefreshToken,
			// ⚠ 绝对毫秒时刻（不是旧的相对秒 `expires_in`）——
			// 它是「Token」列与"要不要续期"的唯一权威，见 Auth.ExpiresAt。
			"expires_at":               a.ExpiresAt,
			"refresh_token_expires_at": a.RefreshTokenExpiresAt,
			"uid":                      a.UID,
			"nickname":                 a.Nickname,
			"machine_id":               a.MachineID,
			"device_id":                a.DeviceID,
			"product_id":               a.ProductID,
		},
		"account": map[string]any{
			"uid":      a.UIDValue(),
			"nickname": a.Nickname,
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

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

// saveAuthFile 原子写回。
func saveAuthFile(a *Auth) error {
	if a == nil || a.FilePath == "" {
		return nil
	}
	raw, err := MarshalAuthFile(a)
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}
