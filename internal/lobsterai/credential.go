// credential.go LobsterAI 凭证的读写（嵌套形与扁平形都认）。
package lobsterai

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filePrefix 凭证文件名前缀。LoadDir 按 `lobsterai*.json` 通配。
const filePrefix = "lobsterai"

// rootDoc 嵌套形态的落盘结构。
type rootDoc struct {
	Auth    *Auth `json:"auth"`
	Account *struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	} `json:"account"`
}

// ParseCredential 解析一份凭证 JSON。
//
// 判据：至少要有 access_token。
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
		return nil, fmt.Errorf("lobsterai: 凭证 JSON 无法解析: %w", err)
	}
	if flat.AccessToken == "" {
		return nil, fmt.Errorf("lobsterai: 凭证缺少 access_token")
	}
	flat.normalize()
	return &flat, nil
}

// normalize 统一字段形态。
func (a *Auth) normalize() {
	if a == nil {
		return
	}
	a.AccessToken = strings.TrimSpace(a.AccessToken)
	a.RefreshToken = strings.TrimSpace(a.RefreshToken)
	a.UID = strings.TrimSpace(a.UID)
	a.UserID = strings.TrimSpace(a.UserID)
	a.Nickname = strings.TrimSpace(a.Nickname)
	a.UUID = strings.TrimSpace(a.UUID)
	a.FirstKeyfrom = strings.TrimSpace(a.FirstKeyfrom)
	a.LatestKeyfrom = strings.TrimSpace(a.LatestKeyfrom)
	if s := strings.TrimSpace(a.ExpiresAt); s != "" {
		if ms := a.ExpiresAtMS(); ms > 0 {
			a.ExpiresAt = fmt.Sprintf("%d", ms)
		}
	}
}

// LoadDir 扫描 dir 下 `lobsterai*.json`。
func LoadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lobsterai: 读凭证目录 %s 失败: %w", dir, err)
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
			log.Printf("lobsterai: 读凭证文件失败，跳过 %s: %v", name, err)
			continue
		}
		a, err := ParseCredential(raw)
		if err != nil {
			log.Printf("lobsterai: 解析凭证失败，跳过 %s: %v", name, err)
			continue
		}
		a.FilePath = p
		if a.UIDValue() == "" {
			log.Printf("lobsterai: 凭证 %s 无法派生 uid，跳过", name)
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

// FileName 凭证的落盘文件名。
func FileName(a *Auth) string {
	return sanitizeFilePart(filePrefix+"-"+a.UIDValue()) + ".json"
}

// MarshalAuthFile 把凭证编成网关的落盘形态（嵌套形）。
//
// ⚠ **必须持久化 uuid / first_keyfrom / latest_keyfrom**：
// 它们不在服务端响应里，而是登录流程自己生成的状态，
// 且要在**每次续期**时原样回传。丢了它们续期会被服务端拒绝。
func MarshalAuthFile(a *Auth) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("lobsterai: 空凭证")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"access_token":   a.AccessToken,
			"refresh_token":  a.RefreshToken,
			"expires_at":     a.ExpiresAt,
			"uid":            a.UID,
			"user_id":        a.UserID,
			"nickname":       a.Nickname,
			"uuid":           a.UUID,
			"first_keyfrom":  a.FirstKeyfrom,
			"latest_keyfrom": a.LatestKeyfrom,
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

// saveAuthFile 原子写回磁盘。
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
