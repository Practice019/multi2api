// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 渠道（channel）标识：账号来自哪个上游站点。
// 与「域名」解耦：域名随 Keycloak 发行方下发（auth 文件里的 domain 字段），
// 渠道是网关侧的路由维度（选 chat/billing base 与 Origin 头）。
const (
	// ChannelCN 国内版 WorkBuddy / CodeBuddy（copilot.tencent.com / www.codebuddy.cn）。
	ChannelCN = "cn"
	// ChannelIntl 海外版 WorkBuddy AI（www.workbuddy.ai）。
	ChannelIntl = "intl"
)

// DeriveChannel 从凭证 domain 推导渠道：含 workbuddy.ai 的域名视为海外版，
// 其余（含空值）回落国内版 —— 向后兼容旧凭证文件（无 channel 字段）。
func DeriveChannel(domain string) string {
	if strings.Contains(strings.ToLower(domain), "workbuddy.ai") {
		return ChannelIntl
	}
	return ChannelCN
}

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	Channel      string // cn / intl；空 = cn（DeriveChannel 兜底）
	UID          string
	EnterpriseID string
	Nickname     string
	// DeviceToken 该账号的设备风控令牌（X-Device-Token），可为空。
	//
	// # 为什么它属于账号而不是配置
	//
	// 官方客户端一个账号一份设备令牌。自建网关既支持"全局配一个"
	// （upstream.device_token），也支持**每号一份** —— 后者更贴近真实形态，
	// 且在多账号下能避免所有号共用一个设备指纹（那本身就是一个异常特征）。
	//
	// 优先级：本字段 > upstream.device_token > upstream.device_token_file。
	// 见 internal/upstream/headers.go 的 resolveDeviceToken。
	//
	// 磁盘形态：嵌套形放 `account.deviceToken`，扁平形放顶层 `deviceToken`。
	// 两种都认 —— 手写凭证的用户不必研究嵌套结构。
	DeviceToken string
	FilePath    string // 来源文件；refresh 后原子写回此处
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Channel      string `json:"channel"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
				DeviceToken  string `json:"deviceToken"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			Channel:      n.Auth.Channel,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.Account.DeviceToken,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Channel      string `json:"channel"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"deviceToken"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			Channel:      f.Channel,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	// 旧凭证无 channel 字段：按 domain 推导，保证老文件零迁移即可识别渠道。
	if a.Channel == "" {
		a.Channel = DeriveChannel(a.Domain)
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"channel":      a.Channel,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
			// DeviceToken 必须一起写回，否则 token 刷新会把用户手写的
			// 设备令牌**静默抹掉** —— 表现是"刷新一次之后风控头就没了"，
			// 而且下次刷新还是这样（文件里已经没有了）。
			//
			// 键名与 Parse 的 account.deviceToken 一致（两处必须同步）。
			"deviceToken": a.DeviceToken,
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

// UpstreamDir 返回某个上游在 base 下的专属凭证子目录。
//
// # 目录约定（本次引入）
//
//	auths/
//	├── workbuddy/     ← workbuddy-*.json
//	└── codearts/      ← codearts-*.json
//
// # 为什么按上游分子目录
//
// 改前两个上游的凭证**都往 `auths/` 根写**（实测：codearts 授权成功后
// 凭证被写进 workbuddy 的目录）。那会带来两个问题：
//
//  1. **各上游的 LoadDir 靠文件名前缀互相过滤**（`workbuddy*.json` /
//     `codearts*.json`）。前缀一旦不匹配（改名、换客户端），
//     对方的凭证就会被自己的扫描器当成"无法解析"而静默跳过。
//  2. 用户的**凭证目录权限/备份策略**通常按上游不同（codearts 的
//     还含 DPoP 私钥）。混在一起没法分别处理。
//
// 分子目录之后，"哪个文件属于谁"由**位置**表达，不再依赖文件名约定。
//
// # 为什么不递归扫描
//
// `filepath.Glob(dir + "/**")` 在 Go 里不递归，且**递归会破坏隔离** ——
// workbuddy 的扫描器会读到 codearts 子目录里的文件。
// 所以调用方要显式问"我要哪个上游的目录"。
func UpstreamDir(base, providerID string) string {
	if base == "" {
		return ""
	}
	if providerID == "" {
		return base // 无归属：保持旧行为，直接用 base
	}
	return filepath.Join(base, providerID)
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
//
// ⚠ 本函数**只扫 dir 这一层**，不递归。新部署应把 dir 指到
// `auths/workbuddy/`（见 UpstreamDir）。
//
// 为兼容"凭证还放在 auths/ 根"的既有部署，`LoadDirCompat` 会两处都扫 ——
// 迁移期用它，迁移完可以只用本函数。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}

// LoadDirCompat 迁移期用的加载器：**同时**扫 `base/<provider>` 与 `base` 根。
//
// # 为什么需要它
//
// 目录结构从"都放根下"改成"按上游分子目录"时，正在运行的部署里
// 凭证可能还在**旧位置**（根），也可能已经搬到**新位置**（子目录）。
// 若只读一处，迁移中途会看到"账号池突然空了"——
// 而那是很难判断的故障（看起来像凭证损坏）。
//
// # 优先级与去重
//
// 子目录优先：同一个 uid 两处都有时以**子目录**那份为准
// （新位置是权威），并在结果里只出现一次。
// 按 UID 去重而不是按文件名 —— 文件名可能因客户端不同而变，
// 但 uid 是账号池的主键（重复会让在途计数互相干扰）。
//
// # 什么时候可以不用它
//
// 所有部署都迁完之后。届时 `LoadDir(UpstreamDir(base, id))` 即可。
// 保留本函数没有害处：它是**只读**的，且子目录为空时自动退回根目录。
func LoadDirCompat(base, providerID string) ([]*Auth, error) {
	sub := UpstreamDir(base, providerID)

	var all []*Auth
	// 先子目录（权威），后根（兼容）
	for _, dir := range []string{sub, base} {
		if dir == "" {
			continue
		}
		list, err := LoadDir(dir)
		if err != nil {
			continue // 某一处读失败不该让整体失败（另一处可能有效）
		}
		all = append(all, list...)
	}

	// 按 UID 去重，**保留先出现的**（即子目录那份）
	seen := make(map[string]bool, len(all))
	out := make([]*Auth, 0, len(all))
	for _, a := range all {
		if a == nil || a.UID == "" {
			continue
		}
		if seen[a.UID] {
			continue
		}
		seen[a.UID] = true
		out = append(out, a)
	}
	return out, nil
}
