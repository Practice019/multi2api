package phanthywork

// sharedstore.go 读官方客户端的"共享凭证库"。
//
// # 这是哪一环，为什么必须有（D3）
//
// 官方桌面端 sidecar 的续期入口 `tH`（偏移 @99948835）在做任何网络动作**之前**，
// 先调一次 `o44()` 问一件事：
//
//	"我这个账号，是不是已经被**另一个端**刷新过了？"
//
// 判据（官方 `o44` @99948667 逐字）：
//
//	let z = Vd3(q, _);                                  // 从全部凭证里择优挑一条
//	if (!z || z.oauth.accessToken === K) return null;   // 还是同一个 ⇒ 没人刷过
//	let w = ay(K), O = ay(z.oauth.accessToken);         // ay() = 解 JWT 取 sub
//	if (!w || w !== O) return null;                     // ★ sub 不同 ⇒ 不是同一账号
//	return z.oauth.accessToken;                         // 直接采用，一个请求都不发
//
// 并且在**续期被拒之后**再问一次（`if (!J) return o44(await _(), q, O())`）。
//
// # 为什么本仓必须补这一环
//
// 本上游的 refresh_token 是**轮换**的：谁先刷，谁把对方手里那把作废。
// 网关与官方桌面端若共用同一账号，两边各自刷新就是**必然互踢**。
// 官方的解法不是"重试"，而是**先看对方有没有刷好、有就用对方的** ——
// 这样两边永远不会同时持有"已经作废的那把"。
//
// 缺这一环的后果是 `docs/agent2api-蒸馏报告.md` §L2 记下的那条：
//
//	网关与桌面端共用同一个 refresh_token，网关侧刷新会造成**轮换竞态**。
//
// 而"竞态"在旧实现里被表达成 `invalid_grant` ⇒ 终态 ⇒ 计一次失败 ⇒
// 连续三次**永久禁用**。⇒ 一次竞态就能把健康号打成死号。
//
// # 真源（官方 sidecar 的这四个函数）
//
//	S3()   @97912695  配置目录：(env[q.configDirEnv] || homedir + q.configDirName)
//	Nd3()  @99947975  读 <configDir>/.credentials.json
//	ay()   @99949600  解 JWT 的 sub
//	o44()  @99948667  择优采用（本文件的 adoptFromOfficialStore 就是它）
//
// ⚠ 官方还有一处 keyring 形态（`kd3()` @99947320：服务名
// `phanthy code-credentials-<sha256(configDir) 前 8 位>`）。本机实测
// `cmdkey /list` 里**没有**这些条目，官方实际用的是文件形态 ⇒ 本文件只读文件。
// 若将来在别的机器上发现 keyring 里有条目，再补一路读取。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// OfficialStoreCredential 从官方共享凭证库里读到的一份令牌。
type OfficialStoreCredential struct {
	// Source 读到的文件路径（日志用）。
	Source string
	// AccessToken 库里的 access token。
	AccessToken string
	// RefreshToken 库里的 refresh token（可能是对方刚轮换出来的那把）。
	RefreshToken string
	// ExpiresAtMS 到期时刻（毫秒，本机口径）。库里没写就按 JWT 的 exp 推。
	ExpiresAtMS int64
	// Scopes 库里的 scope 列表。
	Scopes []string
	// Sub 从 access token 解出来的账号标识。
	Sub string
}

// officialOAuthDoc 官方 `.credentials.json` 里 oauth 子对象的形状。
//
// 字段名是**驼峰**（accessToken / refreshToken / expiresAt / scopes），
// 与 OAuth 接口的蛇形（access_token…）**不是一套** —— 官方
// `Td3`/`t44` 读的是驼峰，别按接口那套写。
type officialOAuthDoc struct {
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken"`
	ExpiresAt    float64  `json:"expiresAt"`
	Scopes       []string `json:"scopes"`
}

// OfficialConfigDir 官方客户端的配置目录。
//
// 对齐官方 `S3()`：先看 env，再回落 `homedir + configDirName`。
// `configDirName` 取 `.phanthy-code`（本机实测该目录存在且含 .credentials.json）。
func OfficialConfigDir() string {
	for _, env := range []string{
		"PHANTHY_CODE_CONFIG_DIR", // 官方 q.configDirEnv
		"PHANTHY_CONFIG_DIR",      // 官方 opus_work 分支用的那个
		"PHANTHY_WORK_CONFIG_DIR",
	} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".phanthy-code")
}

// OfficialCredentialsPath 官方共享凭证库的文件路径（官方 `Nd3` 的那个 join）。
func OfficialCredentialsPath() string {
	d := OfficialConfigDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, ".credentials.json")
}

// ReadOfficialStore 读官方共享凭证库。
//
// 容错纪律（**读不到就当没有**，绝不报错、绝不因此让续期失败）：
//
//	文件不存在 / 权限不足 / 不是 JSON / 没有 accessToken ⇒ ok=false
//
// 理由：这是**别人的**文件，格式与生命周期都不由我们管；把它的读取失败
// 变成我们续期路径上的错误，等于让一个可选优化决定主流程成败。
//
// 官方 `Td3` 的优先级：claudeAiOauth → phanthyCodeOauth → 根对象本身。
func ReadOfficialStore() (OfficialStoreCredential, bool) {
	p := OfficialCredentialsPath()
	if p == "" {
		return OfficialStoreCredential{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return OfficialStoreCredential{}, false
	}
	// 匿名结构体里内嵌 officialOAuthDoc（无 tag）⇒ 它的字段被提升到顶层，
	// 于是"根对象本身就是 oauth"这一形态也能被同一个 Unmarshal 覆盖。
	var outer struct {
		ClaudeAiOauth    *officialOAuthDoc `json:"claudeAiOauth"`
		PhanthyCodeOauth *officialOAuthDoc `json:"phanthyCodeOauth"`
		officialOAuthDoc
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return OfficialStoreCredential{}, false
	}
	doc := outer.ClaudeAiOauth
	if doc == nil {
		doc = outer.PhanthyCodeOauth
	}
	if doc == nil {
		doc = &outer.officialOAuthDoc
	}
	at := strings.TrimSpace(doc.AccessToken)
	if at == "" {
		return OfficialStoreCredential{}, false
	}
	c := OfficialStoreCredential{
		Source:       p,
		AccessToken:  at,
		RefreshToken: strings.TrimSpace(doc.RefreshToken),
		ExpiresAtMS:  int64(doc.ExpiresAt),
		Scopes:       doc.Scopes,
		Sub:          JWTSub(at),
	}
	if c.ExpiresAtMS <= 0 {
		// 库里没写 expiresAt ⇒ 按 JWT 的 exp 推（官方 s44 就是这么干的）。
		c.ExpiresAtMS = JWTExpMS(at)
	}
	return c, true
}

// JWTSub 解 access token 的 sub（对应官方 `ay()`）。
//
// 解不开返回空串 —— 调用方必须把空串当成"身份不明"而**拒绝采用候选**，
// 不能当成"匹配"（那会让别的账号的令牌被错误采用）。
func JWTSub(token string) string {
	if c, ok := ParseJWTClaims(token); ok {
		return strings.TrimSpace(c.Sub)
	}
	return ""
}

// JWTExpMS 解 access token 的 exp（毫秒；对应官方 `s44()`）。
func JWTExpMS(token string) int64 {
	if c, ok := ParseJWTClaims(token); ok && c.Exp > 0 {
		return int64(c.Exp) * 1000
	}
	return 0
}

// adoptFromOfficialStore 对应官方 `o44()`：这个账号是不是**已经被别的端刷过了**？
//
// 返回 (可采用的令牌, true) 表示"采用它、本次不发任何续期请求"。
//
// 三条判据缺一不可（都是官方 o44 逐字要求的）：
//
//	① 库里的 accessToken 与现在的**不同** —— 相同说明没人刷过（`=== K` ⇒ null）
//	② 两边解出的 **sub 相同** —— 不同就是另一个账号，采用它等于串号
//	③ 库里那份**还没过期** —— 过期的采用它只会把过期时间往回拨
//
// ⚠ 判据 ② 是这里最危险的一条：漏了它，多账号环境下会把 A 号的令牌装到 B 号头上。
// 判据见 sharedstore_test.go 的 TestAdoptRejectsDifferentSub。
func adoptFromOfficialStore(a *Auth, nowMS int64) (TokenSet, bool) {
	if a == nil {
		return TokenSet{}, false
	}
	sc, ok := ReadOfficialStore()
	if !ok {
		return TokenSet{}, false
	}
	return adoptCandidate(a, sc, nowMS)
}

// adoptCandidate 是 adoptFromOfficialStore 的纯函数部分（把读库拆出去，便于用例注入）。
func adoptCandidate(a *Auth, sc OfficialStoreCredential, nowMS int64) (TokenSet, bool) {
	if a == nil || sc.AccessToken == "" {
		return TokenSet{}, false
	}
	// ① 与当前是同一个 ⇒ 没人刷过。
	if sc.AccessToken == a.OAuth.AccessToken {
		return TokenSet{}, false
	}
	// ② sub 必须相同。
	//
	// ⚠ 比的必须是**两个 token 各自的 JWT sub**，不能用 `AccountScope` 顶替：
	// 官方 `o44` 原文就是 `let w = ay(K), O = ay(z.oauth.accessToken);`
	// —— 两边都现解 JWT。`AccountScope` 是注册时记下的账号 uuid，正常情况
	// 与 sub 相等，但它是个**可能过期的快照**；身份判据用快照而不用事实，
	// 在换号/重登之后会判错（而且判错的方向是"误采用"，最危险的那种）。
	//
	// JWT 解不开（非 JWT 形态）时才退回 AccountScope —— 那种情况下
	// 至少还有一个可比的身份来源，总比放弃判据好。
	mine := JWTSub(a.OAuth.AccessToken)
	if mine == "" {
		mine = strings.TrimSpace(a.AccountScope)
	}
	if mine == "" || sc.Sub == "" || mine != sc.Sub {
		return TokenSet{}, false
	}
	// ③ 必须还没过期。
	if sc.ExpiresAtMS > 0 && sc.ExpiresAtMS <= nowMS {
		return TokenSet{}, false
	}
	ts := TokenSet{AccessToken: sc.AccessToken, ExpiresAtMS: sc.ExpiresAtMS}
	if sc.RefreshToken != "" {
		ts.RefreshToken = sc.RefreshToken
	}
	if len(sc.Scopes) > 0 {
		ts.Scopes = sc.Scopes
	}
	return ts, true
}
