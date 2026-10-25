// auth.go Verdent 凭证的形态、落盘与读取。
//
// # 只有一把凭据：JWT
//
// Verdent 桌面端的 access token（JWT，三段式）**既是身份也是授权**：
// 请求上游时 `Authorization: Bearer <jwt>` 与 `Cookie: token=<jwt>` 双写（缺一可能被拒）。
// 它自带 `user_id` 与 `exp`，所以身份与到期时间**不用额外请求**就能解出来。
//
// # ⚠ 它不是我们能签发的（与 monkeycode 的 api_key 不同）
//
// 这个 JWT **只在 Verdent 桌面端的主进程内存里**，不落盘
// （Cookies / LocalStorage / vault / auth.json 全查过，都是空的）。
// 所以它**没法由本网关自动获取**，只能由人从别处拿来（DevTools Console 的
// `$c.getToken()`、或 mitmproxy 抓包），再粘进来 / 拷进 auths/verdent/。
// 没有登录协议可写 —— 这不是"还没实现"，是这个上游没有可自动化的登录面。
//
// # 两种磁盘形态都要能读
//
//	① 本仓规范形态  {"auth":{…},"remark":"…"}   —— 我们自己写的用它
//	② 扁平形态      {…}（字段直接在最外层）      —— register-machine 导出的是这种
//
// ② 是要支持的：用户手上那批账号就是从那边导过来的，要求他先改格式
// 等于把一次导入变成一次手工作业。读的时候两种都认，**写的时候只写 ①**
// （规范形态带 remark，且与其余上游同款，工具链认得）。
package verdent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// filePrefix 文件名前缀（`<dir>/verdent-<uid>.json`，与其余上游同款）。
const filePrefix = "verdent"

// Auth 一份 Verdent 凭证。
//
// 字段名刻意与 register-machine 导出的那份**逐字相同** —— 两边共用同一批文件，
// 字段名对不上就要在导入时做一次映射，而映射表是漂移的温床。
type Auth struct {
	Version  int    `json:"version,omitempty"`
	Platform string `json:"platform,omitempty"`
	// UserID 上游用户 id（JWT 的 user_id，也是文件名前缀来源）。
	UserID string `json:"userId,omitempty"`
	// UserIDLower 与 UserID 同值的另一种拼写（导出工具两种都写过）。
	UserIDLower string `json:"userid,omitempty"`
	// Name / Email 展示名（email 来自 agent.verdent.ai/user/center/info）。
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	// Token access JWT。**唯一真正必需**的字段。
	Token string `json:"token"`
	// TokenSource 来源标记（`github` 等），仅展示。
	TokenSource string `json:"tokenSource,omitempty"`
	// Stage 环境（`prod`），仅展示。
	Stage string `json:"stage,omitempty"`

	// ---- 额度（探测时写入，只作展示与选号参考）----
	IsSubscribed bool  `json:"isSubscribed,omitempty"`
	ExpiresAt    int64 `json:"expiresAt,omitempty"` // JWT exp（毫秒）
	AddedAt      int64 `json:"addedAt,omitempty"`
	UpdatedAt    int64 `json:"updatedAt,omitempty"`

	// FreeRatio5h / FreeRatio7d 免费档已用百分比（0~100）。
	FreeRatio5h   float64 `json:"freeRatio5h,omitempty"`
	FreeRatio7d   float64 `json:"freeRatio7d,omitempty"`
	FreeAvailable bool    `json:"freeAvailable,omitempty"`
	FreeResetAt5h int64   `json:"freeResetAt5h,omitempty"`
	FreeResetAt7d int64   `json:"freeResetAt7d,omitempty"`
	QuotaAt       int64   `json:"quotaAt,omitempty"`

	// ---- 付费档（eco）----
	EcoSubscribed bool    `json:"ecoSubscribed,omitempty"`
	EcoAvailable  bool    `json:"ecoAvailable,omitempty"`
	EcoRatio5h    float64 `json:"ecoRatio5h,omitempty"`
	EcoRatio7d    float64 `json:"ecoRatio7d,omitempty"`

	// Models 该账号可用的模型 key（探测时写入）。
	Models      []string `json:"models,omitempty"`
	ModelsCount int      `json:"modelsCount,omitempty"`
	Teams       []Team   `json:"teams,omitempty"`
	TeamCount   int      `json:"teamCount,omitempty"`

	// FilePath 落盘路径（读盘时填，**不序列化**）。
	FilePath string `json:"-"`
}

// Team 一个团队（展示用）。
type Team struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// authFile 本仓规范落盘形态 `{"auth":{…},"remark":"…"}`。
type authFile struct {
	A      *Auth  `json:"auth"`
	Remark string `json:"remark,omitempty"`
}

// jwtPayload JWT 里我们关心的字段（与客户端一致：只解 payload，不验签）。
//
// ⚠⚠ UserID 是 any 而不是 string —— 这是**实测踩出来的**（2026-10-26）。
//
// 上游的 payload 长这样（真的抓下来的）：
//
//	{"user_id": 749951105572520000, "version": 0, "token_type": "access",
//	 "exp": 1795509874, "nbf": ..., "iat": ...}
//
// **user_id 是 JSON 数字，不是字符串。** 声明成 string 时 json.Unmarshal 直接报
// "cannot unmarshal number into Go struct field"，于是 DecodeJWT 返回 error、
// Usable() 恒 false —— 表现是"账号文件明明在那儿，LoadDir 一条都读不出来"，
// 而日志只说"里没有可用的 token"。JS 客户端不会遇到这个（JS 数字/字符串自动混用），
// 所以从参考实现移植过来时这里是**唯一**会静默失配的地方。
//
// ⚠ 而且**必须用 json.Number**，不能用 float64 兜：
// 749951105572520000 有 18 位，超过 float64 的 53 位整数精度（该量级 ULP=128，
// 而 749951105572520000/128 不是整数）—— 用 float64 会把 uid **改掉**，
// 而 uid 是账号池的主键：改掉 = 每次重启多一条幽灵账号。
type jwtPayload struct {
	UserID    any    `json:"user_id"`
	Exp       int64  `json:"exp"`
	Iat       int64  `json:"iat"`
	TokenType string `json:"token_type"`
}

// UserIDString 把 user_id 归一成字符串（数字/字符串两种形态都认）。

// DecodeJWT 解出 JWT 的 payload（不验签）。
//
// ⚠ **不验签是刻意的，与客户端行为一致**：我们要的不是"这个 JWT 可信"，
// 而是"它的 user_id 与 exp 是什么" —— 真实性由上游在每次请求时判定。
// 在这里验签需要有公钥，而那个公钥我们拿不到，验签只是自欺。
func DecodeJWT(token string) (*jwtPayload, error) {
	t := strings.TrimSpace(token)
	t = strings.TrimPrefix(t, "Bearer ")
	t = strings.TrimSpace(t)
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("不是合法 JWT（应为三段式 a.b.c，实得 %d 段）", len(parts))
	}
	b64 := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	if m := len(b64) % 4; m != 0 {
		b64 += strings.Repeat("=", 4-m)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("JWT payload 不是合法 base64: %w", err)
	}
	var p jwtPayload
	// ★ UseNumber：保住大整数的**精确十进制**（见 jwtPayload 的注释）。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("JWT payload 不是合法 JSON: %w", err)
	}
	return &p, nil
}

// UserIDString 把 user_id 归一成字符串。
//
//	json.Number → 原样十进制（**精确**，UseNumber 保证）
//	string      → trim 后原样
//	float64     → 兜底（不该出现；出现说明有人把 UseNumber 去掉了）
func (p *jwtPayload) UserIDString() string {
	if p == nil {
		return ""
	}
	switch v := p.UserID.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

// UID 账号池主键。
//
// 优先文件里的 userId/userid，其次从 JWT 解，最后拿 token 哈希兜底。
//
// ⚠ **不能**拿 token 本身当主键：token 会过期重签，重签一次就多一条账号记录
// （monkeycode 的 api_key-hash 踩过这个坑，见那边 credential.go 的注释）。
func (a *Auth) UID() string {
	if a == nil {
		return ""
	}
	if s := strings.TrimSpace(a.UserID); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.UserIDLower); s != "" {
		return s
	}
	if p, err := DecodeJWT(a.Token); err == nil {
		if s := p.UserIDString(); s != "" {
			return s
		}
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

// DisplayName 展示名：邮箱 > name > 短 uid。
func (a *Auth) DisplayName() string {
	if a == nil {
		return ""
	}
	for _, s := range []string{a.Email, a.Name} {
		if v := strings.TrimSpace(s); v != "" {
			return v
		}
	}
	return a.DisplayUID()
}

// Usable 能不能拿去对话。
//
// 只判"token 非空且 JWT 能解出 user_id" —— **不判过期**：
// 过期判据放在 TokenExpired() 里单独给出，因为"过期"与"不可用"在本上游
// 不是一回事（上游会在 401 时明确告诉你，而本地时钟偏差可能误判）。
func (a *Auth) Usable() bool {
	if a == nil || strings.TrimSpace(a.Token) == "" {
		return false
	}
	p, err := DecodeJWT(a.Token)
	return err == nil && p.UserIDString() != ""
}

// TokenExpiredAt token 的到期时刻。零值 = 无 exp 声明。
func (a *Auth) TokenExpiredAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	if a.ExpiresAt > 0 {
		return time.UnixMilli(a.ExpiresAt)
	}
	if p, err := DecodeJWT(a.Token); err == nil && p.Exp > 0 {
		return time.Unix(p.Exp, 0)
	}
	return time.Time{}
}

// FileName 本凭证的规范文件名。
func (a *Auth) FileName() string {
	return filePrefix + "-" + a.UID() + ".json"
}

// LoadDir 读取 dir 下全部 Verdent 凭证。
//
// 两种磁盘形态都认（见文件头）。单个文件解析失败**跳过并记日志**，
// 不让一个坏文件带走整批账号 —— 静默跳过会让"少了一个号"变成无法排查的事。
func LoadDir(dir string) ([]*Auth, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, filePrefix+"*.json"))
	if err != nil {
		return nil, fmt.Errorf("verdent: 扫描 %s 失败: %w", dir, err)
	}
	sort.Strings(files)
	out := make([]*Auth, 0, len(files))
	seen := map[string]bool{}
	for _, fp := range files {
		raw, err := os.ReadFile(fp)
		if err != nil {
			log.Printf("verdent: 读 %s 失败，跳过: %v", fp, err)
			continue
		}
		a, err := parseAuthFile(raw)
		if err != nil {
			log.Printf("verdent: 解析 %s 失败，跳过: %v", fp, err)
			continue
		}
		if a == nil || !a.Usable() {
			log.Printf("verdent: %s 里没有可用的 token，跳过", fp)
			continue
		}
		uid := a.UID()
		if uid == "" {
			log.Printf("verdent: %s 解不出 uid，跳过", fp)
			continue
		}
		if seen[uid] {
			// 同名 uid 出现两次（多半是手工拷了两份）：保留先读到的那份。
			// 不合并 —— 合并规则要定义"哪个字段优先"，那是另一个坑。
			log.Printf("verdent: uid %s 在 %s 重复，跳过后者", uid, fp)
			continue
		}
		seen[uid] = true
		a.FilePath = fp
		out = append(out, a)
	}
	return out, nil
}

// parseAuthFile 认两种磁盘形态（见文件头）。
func parseAuthFile(raw []byte) (*Auth, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	if _, ok := probe["auth"]; ok {
		var f authFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		return f.A, nil
	}
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// MarshalAuthFile 把一份凭证编成**规范形态**（写盘用，永远写这一种）。
func MarshalAuthFile(a *Auth, remark string) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("verdent: 凭证为 nil")
	}
	return json.MarshalIndent(authFile{A: a, Remark: remark}, "", "  ")
}
