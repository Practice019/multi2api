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
//
// # 第四个字段：Email + Password（2026-11-01 起，为了自动续期）
//
// 上面那把 session 是 **30 天硬过期**的，以前本仓的结论是"无续期接口，
// 过期只能重登/重导"（expiry.go 的旧注释、Auth.Session 的旧注释都这么写）。
// 2026-11-01 在注册机侧实测推翻了它：**有续期路径**，而且不需要有效的 session —
//
//	POST /api/v1/public/captcha/challenge   （公开端点，不需要登录态）
//	POST /api/v1/public/captcha/redeem      → captcha_token
//	POST /api/v1/users/password-login       {email, password, captcha_token}
//	  → Set-Cookie: monkeycode_ai_session=<新 uuid>; Max-Age=2592000
//
// 所以 Email + Password 从"多余的泄露面"变成了**唯一的自救钥匙**：
// session 完全失效（连 401 都拿不到）时，只要密码还在就能换一个新 session 回来。
// 实现见 renew.go。
//
// # 代价与边界（硬要求，改这两个字段前先读）
//
// 明文密码落盘 = 多一份泄露面。因此：
//
//	① 绝不进日志：任何 logf/错误信息里都不许拼 Password（连前几个字符都不给）
//	② 绝不进 admin 响应：admin 的账号表只渲染 gateway 的**投影**（uid/nickname/…），
//	   而 gateway.Credential.Secret 的标签是 `json:"-"` ⇒ 结构上就序列化不出去
//	③ 只活在 SaveFile 写出的凭据档里（0600 + 原子写）
//	④ 它们**不参与**任何判据：能不能用永远看 session 的真实请求结果
//
// 历史依据（为什么当初会丢）：这两个字段最早只有"展示/排查"价值，
// 而注册机导出侧曾把 password 归入「危险」并主动裁掉（见 regmachine.go 文件头）。
// 现在续期让它们有了用途，所以导入侧改成保留。
package monkeycode

import (
	"crypto/sha256"
	"encoding/hex"
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
	// Session `monkeycode_ai_session` 的 uuid。**必需**，30 天硬过期。
	//
	// ⚠ 「30 天硬过期」不再等于「过期就没救」：自 2026-11-01 起本仓能**自动续期**
	// （Email + Password + PoW → password-login 换新 session，见 renew.go），
	// 且新旧 session **并存**（旧的不会被吊销）⇒ 续期是安全操作。
	Session string `json:"session"`

	// Email 上游账号邮箱。**只用于自动续期**（与 Password 配对喂 password-login）。
	//
	// 缺它时续期会明确报错而**不发**任何网络请求（见 Provider.RenewSession）。
	Email string `json:"email,omitempty"`

	// Password 明文密码。**只用于自动续期**，绝不外传/不外显。
	//
	// # 为什么现在要存明文（安全上必须写清，别人会问）
	//
	// 因为密码是**唯一的自救钥匙**：session 一旦失效，只有 password-login 能换回新 session
	// （那个端点不需要有效 session）。不存它 ⇒ 每 30 天就要人工重新登录一次。
	//
	// # 边界（三条硬约束）
	//
	//	① 绝不进日志（logf / error 里连前缀都不给）
	//	② 绝不进 admin 响应（gateway.Credential.Secret 是 `json:"-"`，结构上就出不去）
	//	③ 只活在凭据档里（SaveFile，0600 + 原子写）
	Password string `json:"password,omitempty"`

	// SessionRenewedAt 最近一次**自动续期成功**的时刻。
	//
	// 它比 AddedAt 更可信：AddedAt 是"入库时刻"，而 session 的 30 天是从**签发时刻**算的。
	// 续期后真正该拿来推到期的是这个（见 expiry.go 的 SessionAnchor 优先级）。
	//
	// ⚠ 类型是 time.Time 而不是 string：admin 的"刚续过就跳过"窗口（24h）
	// 与 expiry 的锚点都要拿它做**时间运算**（IsZero/Sub/Equal）。
	// 它在 JSON 里以 RFC3339 输出；而输入侧**容忍四种形态**（见 UnmarshalJSON）——
	// 注册机写的是**毫秒时间戳数字**，Go 的默认解析会因此**整条 JSON 失败**
	// （regmachine.go 的 balance 踩过同一个坑：类型不符 = 连 session 都读不到）。
	SessionRenewedAt time.Time `json:"session_renewed_at,omitempty"`

	// SessionRenewedAtRaw 续期时刻的**原始字符串**形态（只读兼容槽）。
	//
	// # 为什么留这个字段（而不是把老写法删干净）
	//
	// 本语义的前身就是 string（早期写盘的档里存的是 RFC3339 文本）。
	// 用户机器上已经有那种档，删掉兼容路径会让他们一升级就"续期时刻失效"
	// → 到期列退回 AddedAt 估算 → 刚续过的号被误报成"即将过期"。
	// 所以留一个只读槽：UnmarshalJSON 把文本原样放这儿，
	// SessionAnchor 在 time.Time 为空时回落到它。
	//
	// ⚠ 只读：**新写的档一律只写 session_renewed_at**，不要再往这里写。
	SessionRenewedAtRaw string `json:"session_renewed_at_raw,omitempty"`

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

// UnmarshalJSON 容错解析一份凭证。
//
// # 为什么 Auth 需要自定义反序列化
//
// 字段里有一个 time.Time（SessionRenewedAt），而**它的来源有两种形态**：
//
//	本仓自己写的               RFC3339 字符串
//	注册机导出的 renewedAt     毫秒时间戳**数字**（1793545574222）
//
// Go 对 time.Time 的默认解析只认 RFC3339。若不接管，拿注册机导出的行
// 会在这个字段上类型不符 ⇒ **整条 Unmarshal 失败** ⇒ 现象是
// 「凭证文件里没有可用的 session」，而 session 明明在里面。
// （regmachine.go 文件头的 balance 就是同一类坑。）
//
// 做法：先把字段解进一个**影子结构**（时间字段用 string 接），
// 再用 ParseAuthTime 容错解析。其它字段完全按默认规则走，不受影响。
func (a *Auth) UnmarshalJSON(raw []byte) error {
	type shadow struct {
		Session             string          `json:"session"`
		Email               string          `json:"email"`
		Password            string          `json:"password"`
		APIKey              string          `json:"api_key"`
		SigningSecret       string          `json:"signing_secret"`
		Region              string          `json:"region"`
		Nickname            string          `json:"nickname"`
		UserID              string          `json:"userid"`
		Plan                string          `json:"plan"`
		Balance             string          `json:"balance"`
		DailyToken          string          `json:"daily_token"`
		DailyTokenLimit     string          `json:"daily_token_limit"`
		AddedAt             string          `json:"added_at"`
		SessionRenewedAt    json.RawMessage `json:"session_renewed_at"`
		SessionRenewedAtRaw string          `json:"session_renewed_at_raw"`
	}
	var s shadow
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	a.Session = s.Session
	a.Email = s.Email
	a.Password = s.Password
	a.APIKey = s.APIKey
	a.SigningSecret = s.SigningSecret
	a.Region = s.Region
	a.Nickname = s.Nickname
	a.UserID = s.UserID
	a.Plan = s.Plan
	a.Balance = s.Balance
	a.DailyToken = s.DailyToken
	a.DailyTokenLimit = s.DailyTokenLimit
	a.AddedAt = s.AddedAt
	if t, ok := ParseAuthTime(string(s.SessionRenewedAt)); ok {
		a.SessionRenewedAt = t
	}
	// 老写法（只有字符串槽）：原文留下，锚点由 SessionAnchor 回落读取。
	a.SessionRenewedAtRaw = strings.TrimSpace(s.SessionRenewedAtRaw)
	return nil
}

// authFile 落盘外层形态 `{"auth":{…},"remark":"…"}`（与其余上游一致）。
type authFile struct {
	A      *Auth  `json:"auth"`
	Remark string `json:"remark,omitempty"`
}

// MarshalJSON 序列化凭证。
//
// # 为什么 Auth 需要自定义序列化（与下面的 UnmarshalJSON 成对）
//
// `time.Time` 是 struct，`encoding/json` 的 **omitempty 对它无效** ——
// 零值时间照样被写成 `"session_renewed_at":"0001-01-01T00:00:00Z"`。
// 后果不只是难看：那个值**会被解析成一个合法时刻（公元 1 年）**，
// 于是 SessionAnchor 认为"这号续期于公元 1 年" ⇒ 到期列显示"早就过期了"。
// 这类"零值被当真值"的错误在界面上极难反推。
//
// 做法：先转成影子结构，零值续期章用 nil 指针（omitempty 对指针有效）。
//
// ⚠ 必须与 UnmarshalJSON 的字段清单**成对维护**：加字段时两边都要加，
// 否则会静默丢字段（现象：内存里有、重启后没了）。
func (a *Auth) MarshalJSON() ([]byte, error) {
	type shadow struct {
		Session             string     `json:"session"`
		Email               string     `json:"email,omitempty"`
		Password            string     `json:"password,omitempty"`
		APIKey              string     `json:"api_key,omitempty"`
		SigningSecret       string     `json:"signing_secret,omitempty"`
		Region              string     `json:"region,omitempty"`
		Nickname            string     `json:"nickname,omitempty"`
		UserID              string     `json:"userid,omitempty"`
		Plan                string     `json:"plan,omitempty"`
		Balance             string     `json:"balance,omitempty"`
		DailyToken          string     `json:"daily_token,omitempty"`
		DailyTokenLimit     string     `json:"daily_token_limit,omitempty"`
		AddedAt             string     `json:"added_at,omitempty"`
		SessionRenewedAt    *time.Time `json:"session_renewed_at,omitempty"`
		SessionRenewedAtRaw string     `json:"session_renewed_at_raw,omitempty"`
	}
	s := shadow{
		Session:             a.Session,
		Email:               a.Email,
		Password:            a.Password,
		APIKey:              a.APIKey,
		SigningSecret:       a.SigningSecret,
		Region:              a.Region,
		Nickname:            a.Nickname,
		UserID:              a.UserID,
		Plan:                a.Plan,
		Balance:             a.Balance,
		DailyToken:          a.DailyToken,
		DailyTokenLimit:     a.DailyTokenLimit,
		AddedAt:             a.AddedAt,
		SessionRenewedAtRaw: a.SessionRenewedAtRaw,
	}
	// 零值 ⇒ 指针留 nil ⇒ 被 omitempty 省掉（这正是本类型要自定义序列化的原因）。
	if !a.SessionRenewedAt.IsZero() {
		t := a.SessionRenewedAt
		s.SessionRenewedAt = &t
	}
	return json.Marshal(s)
}

// ParseAuthTime 容错解析一个"时刻"字符串/数字（各上游导出格式五花八门）。
//
// # 为什么要它（而不是直接用 time.Parse）
//
// 同一个语义的时刻在本仓有**两个来源**，形态不一样：
//
//	本仓自己写的           RFC3339（time.Time 的 MarshalJSON 产物）
//	注册机导出的 renewedAt 毫秒时间戳**数字**（如 1793545574222）
//
// 而 Go 的 time.Time.**默认**解析只认 RFC3339 —— 拿注册机导出的行导入时
// 会在那个字段上报类型错，**整条 json.Unmarshal 失败**，现象却是
// 「凭证文件里没有可用的 session」（regmachine.go 文件头那段 balance 就是同一类坑）。
//
// 所以：任何**非必需**的时间字段都该走这个函数，不把类型细节暴露给调用方的
// struct 标签。容忍四种形态，解析不出来就回零值 + ok=false（不报错）。
//
//	空 / null         → (零值, false)
//	纯数字            → >1e11 按毫秒，否则按秒（两条都实测见过）
//	RFC3339(Nano)     → 直接 Parse
//	"2006-01-02 15:04:05" / "2006-01-02"（手写凭证常见）→ 也收
func ParseAuthTime(raw string) (time.Time, bool) {
	s := strings.Trim(strings.TrimSpace(raw), `"`)
	if s == "" || s == "null" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n > 1e11 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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
	// ① 先认**注册机导出的格式**。
	//
	// ⚠ 必须排在原生格式之前：注册机的 balance 是数字而本仓是 string，
	// 直接 Unmarshal 会因类型冲突**整条失败**（连 session 都读不到）。
	// 见 regmachine.go 的字段对照表。
	if isRegMachineDoc(raw) {
		return fromRegMachine(raw)
	}
	// ② 本仓落盘形态 {"auth":{…},"remark":"…"}
	var f authFile
	if err := json.Unmarshal(raw, &f); err == nil && f.A != nil && f.A.Usable() {
		return f.A, nil
	}
	// ③ 扁平形态 {"session":"…","api_key":"…"}
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
	// ★ 2026-11-02：身份升级时把旧残档迁走（见 migrateLegacySessionFile 的注释）。
	//
	// ⚠ 放在"写完之后"而不是"写之前"：先写新文件 ⇒ 中间**没有**"旧的已改名、
	//   新的还没写"那种窗口（那种窗口里账号会从目录里消失，一次 reload 就掉出池子）。
	migrateLegacySessionFile(dir, a, path)
	return path, nil
}

// legacySessionFilePrefix 旧残档的文件名中段：UID 回退成 session 短哈希时产生
// （`monkeycode-session-<16位hex>.json`，见 UID() 与 FileName()）。
const legacySessionFilePrefix = filePrefix + "-session-"

// migrateLegacySessionFile 把"同一个 session 的旧残档"改名（**不删**）。
//
// # 它治的是什么（真机踩到的 bug，2026-11-02）
//
// 一份凭证如果没有 userId，`UID()` 会回退成 `"session-"+shortHash(Session)`，
// 于是落盘成 `monkeycode-session-<hash>.json`。等这个账号后来以**带 userId** 的形态
// 再落盘（页内粘贴探到 `user.id`、或再导一行带 userId 的注册机行），`FileName()`
// 会算出**另一个**文件名 ⇒ 目录里同一个账号**两份文件**：
//
//	monkeycode-session-b4cb99d….json   ← 残档（无 nickname/plan/balance/added_at）
//	monkeycode-002d3463-….json         ← 正主
//
// 两份的 UID 不同 ⇒ 上游侧按 UID 去重（dedupeMonkeyCodeByUID）**不会**合并它们
// ⇒ 账号池里同一账号两行、面板上也是两行（残档那行昵称长成 `session-xxxx…`）。
//
// # 判据为什么必须是"session 相等"
//
// 只看文件名前缀会误伤：`monkeycode-session-<另一个session的hash>.json` 是**别的**
// 账号的正当残档（比如只粘过 session、一直在用的号），不能顺手改名。
// session 是这份凭证的身份材料，相等才说明"这就是同一个账号的那份旧档"。
//
// # 为什么改名而不是删除
//
// 与 renew.go 处理 token 缓存同一条纪律：**改名留证、可回滚**。
// 删掉就再也没法回答"当时那份到底是什么、要不要用回来"。
//
// # 只对"身份升级"生效
//
// 新凭证**必须**有 userid（否则它自己就是残档），否则直接返回 ——
// 这样"纯 session 粘贴"（永远没有 userid）不会触发任何迁移，它的 UID 稳定不变。
func migrateLegacySessionFile(dir string, a *Auth, newPath string) {
	if dir == "" || a == nil {
		return
	}
	sess := strings.TrimSpace(a.Session)
	if sess == "" || strings.TrimSpace(a.UserID) == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // 读不动目录不该阻断落盘本身（凭证已经写成功了）
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, legacySessionFilePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		oldPath := filepath.Join(dir, name)
		if oldPath == newPath {
			continue
		}
		old, lerr := LoadFile(oldPath)
		if lerr != nil || old == nil {
			continue // 坏文件/读不动：不动它（改名不是清理坏文件的地方）
		}
		if strings.TrimSpace(old.Session) != sess {
			continue // 别的 session ⇒ 别的账号，别误伤（见上面"判据"）
		}
		dst := name + ".migrated-" + time.Now().Format("20060102-150405")
		if rerr := os.Rename(oldPath, filepath.Join(dir, dst)); rerr != nil {
			logf("monkeycode: 旧残档改名失败（保留原名）%s: %v", name, rerr)
			continue
		}
		logf("monkeycode: 身份升级，旧残档已迁移 → %s（账号 %s）", dst, a.DisplayName())
	}
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
