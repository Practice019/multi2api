// credential.go CodeArts 凭证的磁盘形态与读写。
//
// 与 workbuddy2api 的 auth.Auth 的差异：
//   - CodeBuddy: accessToken + refreshToken（Bearer）
//   - CodeArts : accessKey + secretKey + securityToken（SDK-HMAC-SHA256）+ refreshToken（DPoP 续期）
//
// 落盘格式设计为**与 workbuddy 的 auths/ 目录共存**：
// 文件名前缀不同（workbuddy-*.json / codearts-*.json），互不干扰。
package codearts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 是 CodeArts 账号凭证（归一化后）。
type Auth struct {
	mu sync.Mutex // 串行化 refresh 写回，防止并发写半更新

	// refreshMu 串行化**整个续期过程**（请求 + 落盘）。
	//
	// 为什么需要它而不是复用 mu：mu 只保护字段读写，
	// 挡不住"两个 goroutine 同时拿同一个 refresh_token 去换"。
	// 而 refresh_token 是**消费型**的 —— 用一次即作废（实测 STS5.1806
	// 'the refresh token has been used'）。两个并发续期必然一个成功一个失败，
	// 更糟的是失败方可能把已作废的旧值写回，覆盖掉成功方拿到的新 token。
	//
	// 后台续期任务（jobs.go 注册的 Job）与请求路径（ChatStream 的惰性续期）
	// 是两条会同时触发续期的路径，这个锁就是它们之间的闸门。
	refreshMu sync.Mutex

	AccessKey     string `json:"accessKeyId"`
	SecretKey     string `json:"secretAccessKey"`
	SecurityToken string `json:"securityToken"`
	ExpiresAt     int64  `json:"expiresAt"` // Unix 秒

	RefreshToken string `json:"refresh_token"`
	// DPoPPrivateKeyJWK 是续期必需的 ES256 私钥（JWK JSON）。
	// 没有它就无法调 refresh_token grant —— 这是与 CodeBuddy 最大的不同：
	// 续期凭证不是一个 token，而是一对密钥 + refresh_token。
	DPoPPrivateKeyJWK json.RawMessage `json:"dpopPrivateKeyJwk"`

	ClientID string `json:"clientId"`

	UID      string `json:"uid"`
	Nickname string `json:"nickname"`

	FilePath string `json:"-"`
}

// Lock / Unlock 供其他包在改写字段期间加锁。
func (a *Auth) Lock()   { a.mu.Lock() }
func (a *Auth) Unlock() { a.mu.Unlock() }

// LockRefresh / UnlockRefresh 串行化续期全过程。
//
// 调用方（Client.RefreshToken）必须在**读取 refresh_token 之前**加锁，
// 并在写盘完成后才释放 —— 只保护写回是不够的，
// 因为真正会被重复消费的是"发出去的那个 refresh_token"。
func (a *Auth) LockRefresh()   { a.refreshMu.Lock() }
func (a *Auth) UnlockRefresh() { a.refreshMu.Unlock() }

// HasUsableDPoPKey 报告 raw 是不是一把**可用**的 DPoP 私钥 JWK。
//
// 三种"没有钥匙"的形态都算不可用：nil、全空白、JSON `null`。
//
// # 为什么必须把 null 也算进去（踩过的坑）
//
// `json.RawMessage` 在"字段缺失"与"显式 null"时会留下**不同**的字节
// （长度 0 vs 长度 4 的 `null`），所以只判 `len(raw) > 0` 会把后者当成"有钥匙"。
// 后果是双向的：
//
//	· 采纳磁盘凭证时 → 磁盘上丢了 dpop 段，反而**覆盖掉内存里可用的那把**
//	  → 对象从"能自愈"退化成"连发请求的资格都没有"
//	  （RefreshToken 开头就因缺 DPoP 私钥直接返回）
//	· 判定"凭证材料是否变化"时 → 空与 null 被当成两种不同的状态，
//	  于是每 60 秒的后台扫描都会判一次"有差异"并刷一行日志
//
// 判定"有没有钥匙"的口径必须只有这一处 —— 上面两件事都靠它。
func HasUsableDPoPKey(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// DPoPKeysDiffer 报告两份 DPoP 私钥是否**实质不同**。
//
// 「没有钥匙」（nil / 空白 / null）与「有钥匙」是**两种状态**：
// 一个空、一个非空 → 有差异；两个都空 → 无差异（不管空成哪种形态）。
//
// ⚠ 两边都有钥匙时按**字节**比较（去掉首尾空白）。这在生产里是安全的：
// 两侧都是同一个文件解析出来的 `json.RawMessage`，序列化形态一致。
// 但若一边是 `json.Marshal(PrivateJWK())` 的紧凑形态、另一边是磁盘上的缩进形态，
// **同一把钥匙也会被判成不同** —— 那只会导致一次多余的原地改写（值相同）
// 加一行日志，不会写坏数据。所以要断言"没有多余改写"时，
// 请按钥匙语义比较，不要按这个函数的字节口径去断言测试结果。
func DPoPKeysDiffer(a, b json.RawMessage) bool {
	ha, hb := HasUsableDPoPKey(a), HasUsableDPoPKey(b)
	if !ha || !hb {
		return ha != hb
	}
	return !bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
}

// NeedsRefresh 报告凭证是否将在 within 内过期。
// 注意：CodeArts 的 STS 凭证有效期**只有约 2 小时**，
// 因此 within 必须显著小于该值（推荐 5 分钟），否则会出现
// 「刚判定为新鲜，发出去已过期」的窗口。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Cred 转成签名所需的凭证三元组。
func (a *Auth) Cred() Credential {
	return Credential{
		AccessKey:     a.AccessKey,
		SecretKey:     a.SecretKey,
		SecurityToken: a.SecurityToken,
	}
}

// credFile 是磁盘格式：outer 包一层，与 OAuth 登录产物对齐。
//
// 注意：这里**不能**直接嵌入 Auth —— Auth 内含 sync.Mutex，
// 按值复制会触发 go vet 的 copylocks 告警，且可能复制出已加锁的互斥量。
// 因此磁盘结构用独立的 credBody 描述字段（无锁、无 FilePath）。
type credFile struct {
	Auth     credBody     `json:"auth"`
	Account  accountBlock `json:"account"`
	DPoP     dpopBlock    `json:"dpop"`
	ClientID string       `json:"clientId"`
}

// credBody 是 Auth 的磁盘投影。
type credBody struct {
	AccessKey     string `json:"accessKeyId"`
	SecretKey     string `json:"secretAccessKey"`
	SecurityToken string `json:"securityToken"`
	ExpiresAt     int64  `json:"expiresAt"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	ClientID      string `json:"clientId,omitempty"`
}

type accountBlock struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
}

type dpopBlock struct {
	PrivateKeyJWK json.RawMessage `json:"privateKeyJwk"`
}

// ParseCredential 解析磁盘凭证（支持两种形态）：
//
//	嵌套形（OAuth 登录产物）: {"auth":{...},"account":{...},"dpop":{...}}
//	扁平形（手写）:          {"accessKeyId":...,"secretAccessKey":...}
func ParseCredential(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty credential file")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}

	var a Auth
	if _, nested := probe["auth"]; nested {
		var n credFile
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a.AccessKey = n.Auth.AccessKey
		a.SecretKey = n.Auth.SecretKey
		a.SecurityToken = n.Auth.SecurityToken
		a.ExpiresAt = n.Auth.ExpiresAt
		a.RefreshToken = n.Auth.RefreshToken
		a.ClientID = n.Auth.ClientID
		a.UID = n.Account.UID
		a.Nickname = n.Account.Nickname
		a.DPoPPrivateKeyJWK = n.DPoP.PrivateKeyJWK
		if a.ClientID == "" {
			a.ClientID = n.ClientID
		}
	} else {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
	}

	if strings.TrimSpace(a.AccessKey) == "" || strings.TrimSpace(a.SecretKey) == "" {
		return nil, fmt.Errorf("parse_error: missing accessKeyId/secretAccessKey")
	}

	// UID 三级回落：JWT → account.uid → AK。
	//
	// 为什么需要回落链：uid 是账号池的主键 —— 空串会让所有账号塌成同一个键，
	// 在途计数（Acquire/Release）互相干扰，表现为
	// "连续 N 次成功后 in_flight 卡住，之后恒 503 no_healthy_account"。
	//
	// 优先级说明：
	//  1. refresh_token 的 JWT 里带权威 account_id（服务端给的，与华为云控制台一致）
	//  2. account.uid：旧版 cmd/login 可能没写，或与 JWT 不一致时以其为准会误导
	//  3. AK：一定存在且同账号内唯一，是最后的兜底键
	if info, jerr := ParseRefreshToken(a.RefreshToken); jerr == nil && info.ID != "" {
		a.UID = info.ID
		if info.Name != "" {
			a.Nickname = info.Name
		}
	}
	if strings.TrimSpace(a.UID) == "" {
		a.UID = a.AccessKey
	}
	return &a, nil
}

// BackupDir 返回凭证目录下存放续期备份的子目录。
//
// 用点开头的隐藏目录：它跟在 auths/ 旁边便于一起备份/迁移，
// 又不会被 LoadDir 的 `codearts*.json` 通配匹配到（避免把备份当账号加载）。
func BackupDir(authDir string) string {
	return filepath.Join(authDir, ".bak")
}

// BackupTo 把当前凭证整份备份到 bakDir/<原文件名>.json。
//
// 为什么需要"续期前备份"：refresh_token 是**消费型**的 ——
// 服务端把用过的 token 立即作废（实测报 STS5.1806 'the refresh token has been used'）。
// 如果续期请求成功但写盘失败，本地就只剩一个已作废的旧 token，
// 该凭证**永久报废**，只能重新走浏览器登录。
//
// 备份不能"救回"凭证（服务端已消费），它的价值是**诊断**：
// 启动时发现残留备份 = 明确告知用户"上次续期在写盘这一步失败了"，
// 而不是让用户对着一个静默失效的凭证猜原因。
func (a *Auth) BackupTo(bakDir string) error {
	if a.FilePath == "" {
		return fmt.Errorf("backup: FilePath 为空")
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		return fmt.Errorf("backup: 读原凭证: %w", err)
	}
	if err := os.MkdirAll(bakDir, 0o700); err != nil {
		return fmt.Errorf("backup: 建目录: %w", err)
	}
	dst := filepath.Join(bakDir, filepath.Base(a.FilePath))
	// 先写临时文件再 rename，保证备份本身也是原子的
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("backup: 写备份: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("backup: 落定备份: %w", err)
	}
	return nil
}

// ClearBackup 删除本账号的备份（续期成功落盘后调用）。
func (a *Auth) ClearBackup(bakDir string) error {
	if a.FilePath == "" {
		return nil
	}
	dst := filepath.Join(bakDir, filepath.Base(a.FilePath))
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear backup: %w", err)
	}
	return nil
}

// FindStaleBackups 返回残留的续期备份（启动时调用）。
//
// 非空意味着**上一次续期没能善终**：请求可能已消费掉 refresh_token，
// 但新凭证没落盘。调用方应打 WARN 并提示用户重新登录，
// 而不是拿一个可能已作废的 token 反复重试（浪费配额、刷屏日志）。
func FindStaleBackups(authDir string) ([]string, error) {
	bakDir := BackupDir(authDir)
	entries, err := os.ReadDir(bakDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取备份目录: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		out = append(out, filepath.Join(bakDir, e.Name()))
	}
	return out, nil
}

// SaveAtomic 原子写回 FilePath（tmp + rename），保持嵌套形。
// 全程持锁：防止与 Refresh 并发写出半更新凭证。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessKey) == "" || strings.TrimSpace(a.SecretKey) == "" {
		return fmt.Errorf("save refused: empty accessKey/secretKey (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := credFile{
		Auth: credBody{
			AccessKey:     a.AccessKey,
			SecretKey:     a.SecretKey,
			SecurityToken: a.SecurityToken,
			ExpiresAt:     a.ExpiresAt,
			RefreshToken:  a.RefreshToken,
			ClientID:      a.ClientID,
		},
		Account:  accountBlock{UID: a.UID, Nickname: a.Nickname},
		DPoP:     dpopBlock{PrivateKeyJWK: a.DPoPPrivateKeyJWK},
		ClientID: a.ClientID,
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

// LoadDir 扫描 dir 下 codearts*.json。
//
// 解析失败的文件**跳过但记日志**（早期版本是静默 continue）。
//
// 为什么必须打日志：用户往 auths/ 放了 3 个文件，只有 2 个进了账号池，
// 界面上没有任何提示 —— 这是"我入库了但不知道入哪里去了"的另一面：
// 文件没进池，也没人告诉他为什么。日志给出**文件名 + 原因**，
// 让"少了一个号"从猜测变成可查证的事实。
//
// 只打一层 Base：auths/ 目录的完整路径在日志里已经够长，
// 重复几十行会把真正的原因挤到看不见。
// 注意**解析成功的文件不产生任何日志** —— 否则账号多起来就是刷屏。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "codearts*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			log.Printf("codearts: 跳过 %s: %v", filepath.Base(f), err)
			continue
		}
		a, err := ParseCredential(raw)
		if err != nil {
			log.Printf("codearts: 跳过 %s: %v", filepath.Base(f), err)
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
