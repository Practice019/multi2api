// credential.go Cline 凭证的读写。
//
// # 落盘形态（与各上游一致）
//
// 网关的凭证文件是**嵌套形**（`{"auth":{...},"account":{...}}`），
// 由 internal/auth 的 Parse 读取；同时它也接受**扁平形**（顶层直接放字段），
// 让手写凭证的用户不必研究嵌套结构。
//
// 本文件负责三件事：
//
//	ParseCredential  单份凭证 JSON → *Auth（两种形态都认）
//	LoadDir          扫描目录下 cline*.json
//	MarshalAuthFile  网关落盘形态（供 core 写回）
//
// # 为什么 LoadDir 的 Glob 前缀必须是自己的
//
// 这条是实测踩出来的：核心曾经用 workbuddy 的解析器扫所有上游目录，
// 它的 Glob 写死 `workbuddy*.json`，于是扫 codearts/loomy 目录**一个都读不到**，
// 反而把根目录下遗留的 workbuddy 旧文件当成了结果（见 gateway.CredentialLoader
// 的注释）。所以每个上游必须自报"怎么读自己的凭证"。
package cline

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filePrefix 凭证文件名前缀。LoadDir 按 `cline*.json` 通配。
const filePrefix = "cline"

// rootDoc 嵌套形态的落盘结构。
type rootDoc struct {
	Auth    *Auth `json:"auth"`
	Account *struct {
		UID      string `json:"uid"`
		Email    string `json:"email"`
		Nickname string `json:"nickname"`
	} `json:"account"`
}

// ParseCredential 解析一份凭证 JSON（嵌套形与扁平形都认）。
//
// 判据：至少要有 access_token —— 没有它的凭证不可用。
// 这与参照项目"非 codearts 的 provider 凭据必须含 access_token"
// 的约定一致（那是账号池匹配账号身份的依据）。
func ParseCredential(raw []byte) (*Auth, error) {
	var doc rootDoc
	if err := json.Unmarshal(raw, &doc); err == nil && doc.Auth != nil && doc.Auth.AccessToken != "" {
		a := doc.Auth
		// 嵌套形把 UID/Nickname 放在 account 段，这里合并回去
		if doc.Account != nil {
			if a.AccountID == "" {
				a.AccountID = strings.TrimSpace(doc.Account.UID)
			}
			if a.Email == "" {
				a.Email = strings.TrimSpace(doc.Account.Email)
			}
			if a.Nickname == "" {
				a.Nickname = strings.TrimSpace(doc.Account.Nickname)
			}
		}
		a.normalize()
		return a, nil
	}

	// 扁平形
	var flat Auth
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("cline: 凭证 JSON 无法解析: %w", err)
	}
	if flat.AccessToken == "" {
		return nil, fmt.Errorf("cline: 凭证缺少 access_token")
	}
	flat.normalize()
	return &flat, nil
}

// normalize 补齐派生字段并统一令牌形态。
func (a *Auth) normalize() {
	if a == nil {
		return
	}
	// ⚠ 幂等补 workos: 前缀：手写的凭证可能带也可能不带
	a.AccessToken = clineBearerValue(a.AccessToken)
	a.RefreshToken = strings.TrimSpace(a.RefreshToken)
	a.AccountID = strings.TrimSpace(a.AccountID)
	a.Email = strings.TrimSpace(a.Email)
	a.Nickname = strings.TrimSpace(a.Nickname)
	if a.Nickname == "" {
		switch {
		case a.Email != "":
			a.Nickname = a.Email
		case a.AccountID != "":
			a.Nickname = a.AccountID
		}
	}
}

// LoadDir 扫描 dir 下 `cline*.json`，返回全部可用凭证。
//
// 语义与其它上游的 LoadDir 对齐：
//   - 目录不存在 / 为空 → 返回空切片，**不是错误**（用户可能还没放凭证）
//   - 单个文件解析失败 → 跳过并记日志（静默跳过会让"少了一个号"无法排查）
//   - 同一 AccountID 多份 → 取**过期时间更晚**的那份（更可能还能用）
func LoadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cline: 读凭证目录 %s 失败: %w", dir, err)
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
			log.Printf("cline: 读凭证文件失败，跳过 %s: %v", name, err)
			continue
		}
		a, err := ParseCredential(raw)
		if err != nil {
			log.Printf("cline: 解析凭证失败，跳过 %s: %v", name, err)
			continue
		}
		a.FilePath = p
		if a.UID() == "" {
			log.Printf("cline: 凭证 %s 无法派生 uid，跳过（放进池子会得到一个无法按 uid 寻址的账号）", name)
			continue
		}
		out = append(out, a)
	}
	return pickWinners(out), nil
}

// pickWinners 同 uid 多份凭证时按**可用性判据**选一份。
//
// 判据（逐级短路，全平则保留先出现的以保证稳定）：
//
//  1. ExpireTime 更大 —— 更晚过期 = 更可能还能用
//  2. 有 RefreshToken —— 能自动续期，过期只是暂时的
//
// ⚠ 不按文件名的字母序裁决：两份都是好凭证时字母序无害，但当一份已过期、
// 一份有效时，胜出的可能是过期那份 —— 而界面上一切正常（号在池里、昵称也对），
// 只有请求全部失败。这是最难查的一类故障。
func pickWinners(list []*Auth) []*Auth {
	byUID := map[string]*Auth{}
	order := make([]string, 0, len(list))
	for _, a := range list {
		uid := a.UID()
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
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID() < out[j].UID() })
	return out
}

// betterAuth 报告 cand 是否比 cur 更值得采用。
func betterAuth(cand, cur *Auth) bool {
	if cand.ExpireTime != cur.ExpireTime {
		return cand.ExpireTime > cur.ExpireTime
	}
	return cand.RefreshToken != "" && cur.RefreshToken == ""
}

// FileName 凭证的落盘文件名。
func FileName(a *Auth) string {
	return sanitizeFilePart(filePrefix+"-"+a.UID()) + ".json"
}

// MarshalAuthFile 把凭证编成网关的落盘形态（嵌套形）。
//
// 只写**必要**字段：access_token / refresh_token / expire_time 是续期要用的，
// account 段的 uid/nickname 是账号池主键与展示名。
func MarshalAuthFile(a *Auth) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("cline: 空凭证")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"access_token":  a.AccessToken,
			"refresh_token": a.RefreshToken,
			"expire_time":   a.ExpireTime,
			"account_id":    a.AccountID,
			"email":         a.Email,
		},
		"account": map[string]any{
			"uid":      a.UID(),
			"email":    a.Email,
			"nickname": a.Nickname,
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

// sanitizeFilePart 把 uid 里不适合做文件名的字符换掉。
//
// uid 可能来自邮箱（含 @ 与 .），直接用会造出带子目录语义或非法字符的名字。
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
