// import.go workbuddy 的**批量粘贴导入**端点（国内版与海外版 workbuddy-intl
// 是同一个实现注册的两个实例，端点对两者都可用）。
//
// # 它解决什么问题（本轮用户的要求）
//
// 用户手上有一批**别处跑出来的账号 JSON**（导出工具的形态，键是中文）：
//
//	{"源":"workbuddy","用户名":"perezsteven2","uid":"d0c45ed6-…",
//	 "sessionToken":"eyJ…","refreshToken":"eyJ…","可用额度":350,
//	 "是否健康":"健康","expiresAt":1821537224,"healthNote":"积分接口正常（鉴权有效）"}
//
// 需要**直接粘贴进网页**一次导入 —— 与 loomy 的批量导入（internal/loomy/
// import.go）同一诉求。整条链路照抄那条已验证过的判据：
//
//	前端按钮（adminRouteBySuffix 认 hidden POST …/import）
//	→ 粘贴 JSON（单条/数组）→ 本端点逐条落盘
//	→ 前端自动调 POST /admin/accounts/reload {provider}
//	→ 账号池对齐，无需重启。
//
// # 落盘为什么走 auth.Auth.SaveAtomic
//
// workbuddy 凭证的落盘格式只有**一个权威**：internal/auth（页内 OAuth 登录
// 产出的文件、token 刷新后的写回，都是它）。导入如果另写一份序列化，
// 三处格式迟早漂移（本项目吃过的亏）。所以这里构造 &auth.Auth 后直接调
// SaveAtomic() —— 落出来的文件与登录产出**逐字段同形**（嵌套形 + deviceToken
// 键），下游（auth.Parse / LoadDirCompat / SyncToDirWithSecrets）不用区分。
//
// # channel / domain 怎么定
//
// 请求路由（chat/billing base）由**凭证的 channel 字段**决定
// （auth.DeriveChannel / upstream.Client 的 base 选择）—— 所以"导入进哪个池"
// 就是渠道的权威：
//
//	workbuddy-intl 实例 → channel=intl + domain=www.workbuddy.ai
//	（与页内 OAuth 登录产出的海外版凭证同形，见 auths/workbuddy-intl/ 实测文件）
//	默认实例（workbuddy）→ channel=cn + domain=copilot.tencent.com
//
// 实例身份只有一个信号：Config.ID（装配层注册的事实）。
//
// # 可用额度 / 是否健康 / healthNote 为什么不落盘
//
// 它们是**导入那一刻的快照**，不是凭证材料。额度以网关自己查询为准
// （workbuddy-intl 保留 CapQuotaProbe，「刷新积分」会向真实上游核对并写回池）；
// 把一份可能过期的快照写进凭证，等于让假数据获得与真数据同样的地位。
// 与 loomy 导入的取舍一致：**只搬鉴权材料，状态类信息原样忽略**。
package workbuddy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"workbuddy2api/internal/auth"
)

// importPath 批量导入端点的路径（**相对**路径，与 Routes() 里其它端点一致）。
//
// prefixed() 会给非默认实例加 /<id> 前缀：
//
//	默认实例   POST /admin/import
//	intl 实例  POST /admin/workbuddy-intl/import
//
// 前端按钮的触发判据是"hidden POST 路由以 /import 结尾"（adminRouteBySuffix），
// 两个实例各自从 manifest 拿到自己的路径，前端零改动。
const importPath = "/admin/import"

// importItem 一条输入记录的形态。
//
// 键名与导出工具的 JSON **逐字对应**（含中文键 —— encoding/json 的 struct tag
// 对任意字符串成立）。状态类字段（源/可用额度/是否健康/healthNote）解析出来
// 但**不落盘**，见文件头注释。
type importItem struct {
	Source       string `json:"源"`
	Username     string `json:"用户名"`
	UID          string `json:"uid"`
	// AccessToken 是 workbuddy 的唯一鉴权材料（Bearer JWT）。
	// json 键保持 `sessionToken` 兼容既有导出工具格式；别名 accessToken 也接受。
	AccessToken string `json:"sessionToken"`
	RefreshToken string `json:"refreshToken"`
	Quota        int64  `json:"可用额度"`
	Health       string `json:"是否健康"`
	ExpiresAt    int64  `json:"expiresAt"`
	HealthNote   string `json:"healthNote"`
}

// fillAliases 容错：导出工具的键名变体（英文同义词）补到主字段上。
//
// 只补**主字段为空**的槽位 —— 用户给出的中文键永远优先，别名只是让
// "手抄一份、字段名打了个英文"的输入也不至于整条失败。
func fillAliases(it *importItem, m map[string]any) {
	if it == nil || m == nil {
		return
	}
	if it.UID == "" {
		it.UID = importStringField(m, "userid")
	}
	if it.Username == "" {
		it.Username = importFirstStringField(m, "username", "nickname")
	}
	if it.AccessToken == "" {
		it.AccessToken = importFirstStringField(m, "sessionToken", "session", "accessToken")
	}
	if it.RefreshToken == "" {
		it.RefreshToken = importStringField(m, "refresh_token")
	}
	if it.ExpiresAt <= 0 {
		if v, ok := importNumberField(m, "expires_at"); ok {
			it.ExpiresAt = v
		}
	}
}

func importStringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func importFirstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := importStringField(m, k); s != "" {
			return s
		}
	}
	return ""
}

func importNumberField(m map[string]any, key string) (int64, bool) {
	switch n := m[key].(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}

// jwtClaims accessToken（Keycloak 签发的 JWT）payload 里导入兜底用得上的字段。
//
// # 为什么要解码 JWT
//
// uid / 过期时刻 / 用户名在 token 里**本来就有**（sub / exp /
// preferred_username，样例输入里三者与明文字段一致）。导出工具漏列某一项时，
// 从 token 里补出来比报"字段缺失"更有用 —— token 是账号的权威载体。
// 只解码、**不验签**：验签需要上游的公钥与网络，而这里不根据这些字段做
// 任何安全决策，只做"落什么进凭证文件"的缺省推导。
type jwtClaims struct {
	Sub               string `json:"sub"`
	Exp               int64  `json:"exp"`
	PreferredUsername string `json:"preferred_username"`
	Username          string `json:"username"`
}

// decodeJWTClaims 解 JWT payload（base64url，不验签）。任何一步失败都返回
// false —— 兜底失败等价于"没有兜底"，调用方按字段缺失处理。
func decodeJWTClaims(token string) (jwtClaims, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return jwtClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 容忍带 padding 的变体。
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return jwtClaims{}, false
		}
	}
	var c jwtClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return jwtClaims{}, false
	}
	return c, true
}

// importUIDPattern uid 的白名单：字母或数字开头，后续可以是字母数字._-。
//
// # 为什么不接受任意字符串
//
// uid 有两个用途：**拼凭证文件名**（workbuddy-<uid>.json）与**当账号池主键**。
// 放进路径分隔符就是任意路径写文件的原语（导入端点会吃请求体里的 JSON）；
// 静默把非法字符改写成合法字符则会让文件名与主键对不上（重载后 pool 里的
// uid 与文件各说各话）。Keycloak 的 sub 是 UUID，样例输入也是 UUID ——
// 收紧到这个字符集不损失任何真实输入。
var importUIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// importSafeUID 校验并返回可安全用于文件名与主键的 uid。
func importSafeUID(uid string) (string, error) {
	u := strings.TrimSpace(uid)
	if u == "" {
		return "", errors.New("缺少 uid（输入里没有，sessionToken 的 JWT 里也没有 sub）")
	}
	if !importUIDPattern.MatchString(u) {
		return "", fmt.Errorf("uid %q 含不支持的字符（只允许字母数字与 . _ -，且以字母数字开头）", u)
	}
	return u, nil
}

// importChannelDefaults 按**实例身份**给出导入凭证的渠道与 domain
// （依据见文件头注释）。
func importChannelDefaults(instanceID string) (channel, domain string) {
	if strings.Contains(strings.ToLower(instanceID), "intl") {
		return auth.ChannelIntl, "www.workbuddy.ai"
	}
	return auth.ChannelCN, "copilot.tencent.com"
}

// handleImport POST /admin/import —— 批量粘贴导入（AdminHandler 的 HTTP 面）。
//
// 请求体两种形状都认（与 loomy 同一条判据）：
//
//	[{...}, {...}]        数组（多条）
//	{...}                 单个对象
//
// 逐条处理、逐条回报 —— 一条坏数据不拖累整批。回执：
//
//	{ok, imported, failed, results:[{index, ok, uid?, warn?, error?}]}
func (h *AdminHandler) handleImport(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.p == nil {
		writeError(w, http.StatusInternalServerError, "导入端点未接线（Provider 为空）")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}
	trim := strings.TrimSpace(string(raw))
	if trim == "" {
		writeError(w, http.StatusBadRequest, "请求体为空 —— 请粘贴账号 JSON（单个对象，或 [ ] 包起来的数组）")
		return
	}

	var list []importItem
	var rawItems []json.RawMessage
	switch trim[0] {
	case '[':
		if err := json.Unmarshal(raw, &list); err != nil {
			writeError(w, http.StatusBadRequest, "不是合法的 JSON 数组: "+err.Error())
			return
		}
		_ = json.Unmarshal(raw, &rawItems)
	default:
		var one importItem
		if err := json.Unmarshal(raw, &one); err != nil {
			writeError(w, http.StatusBadRequest, "不是合法的 JSON 对象: "+err.Error())
			return
		}
		list = []importItem{one}
		rawItems = []json.RawMessage{json.RawMessage(trim)}
	}
	if len(list) == 0 {
		writeError(w, http.StatusBadRequest, "数组里没有任何账号")
		return
	}

	// 目录先确保存在（与 SaveAtomic 的 0600 文件同套权限语义），
	// 避免逐条写时重复建。AuthDir 未配置时回落 auths/<实例ID> ——
	// 与"按上游分子目录"的既有约定同形（loomy 兜底 auths/loomy 同一思路）。
	dir := h.p.cfg.AuthDir
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join("auths", h.p.ID())
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "创建凭证目录失败: "+err.Error())
		return
	}

	results := make([]map[string]any, 0, len(list))
	okN := 0
	for i := range list {
		it := list[i]
		if i < len(rawItems) {
			var m map[string]any
			if json.Unmarshal(rawItems[i], &m) == nil {
				fillAliases(&it, m)
			}
		}
		res := map[string]any{"index": i + 1}
		uid, warn, ierr := importOne(dir, h.p.ID(), it)
		if ierr != nil {
			res["ok"] = false
			res["error"] = ierr.Error()
		} else {
			okN++
			res["ok"] = true
			res["uid"] = uid
			if warn != "" {
				res["warn"] = warn
			}
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       okN == len(list),
		"imported": okN,
		"failed":   len(list) - okN,
		"results":  results,
	})
}

// importOne 把一条输入记录落盘成一份 workbuddy 凭证。
//
// 返回 (uid, 警告, 错误)：warn 非空表示落盘成功但有值得知道的事
// （当前只有"缺 refreshToken"—— access token 到期后无法续期）。
func importOne(dir, instanceID string, it importItem) (string, string, error) {
	sess := strings.TrimSpace(it.AccessToken)
	if sess == "" {
		return "", "", errors.New("缺少 accessToken（它是唯一鉴权材料）")
	}
	claims, _ := decodeJWTClaims(sess)

	uid, err := importSafeUID(firstNonEmptyStr(strings.TrimSpace(it.UID), claims.Sub))
	if err != nil {
		return "", "", err
	}
	expiresAt := it.ExpiresAt
	if expiresAt <= 0 {
		expiresAt = claims.Exp
	}
	nickname := firstNonEmptyStr(strings.TrimSpace(it.Username), claims.PreferredUsername, claims.Username, uid)

	warn := ""
	if strings.TrimSpace(it.RefreshToken) == "" {
		warn = "缺少 refreshToken —— access token 到期后无法续期，账号只能用到本 token 过期为止"
	}

	channel, domain := importChannelDefaults(instanceID)
	a := &auth.Auth{
		AccessToken:  sess,
		RefreshToken: strings.TrimSpace(it.RefreshToken),
		ExpiresAt:    expiresAt,
		Domain:       domain,
		Channel:      channel,
		UID:          uid,
		Nickname:     nickname,
		// FilePath 决定 SaveAtomic 写回哪里；文件名与 auth.LoadDir 的
		// glob（workbuddy*.json）及现有落盘惯例（两个实例同前缀）一致。
		FilePath: filepath.Join(dir, "workbuddy-"+uid+".json"),
	}
	if err := a.SaveAtomic(); err != nil {
		return "", "", fmt.Errorf("落盘凭证失败: %w", err)
	}
	return uid, warn, nil
}

// firstNonEmptyStr 返回第一个非空（去空格后）的串。
//
// 本包原来没有这个工具（loomy 有自己的 firstNonEmpty）—— 导入的
// 字段取舍全靠它，单独放在这里而不是塞进某个不相干的文件。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
