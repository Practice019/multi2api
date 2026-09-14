// credential.go Loomy 的凭证模型与文件读写。
//
// # 本包为什么只有一份这么短的凭证
//
// 三个上游的凭证复杂度差距极大，这直接决定了各自的包有多重：
//
//	workbuddy → {accessToken, refreshToken, uid, domain}    + 刷新轮转
//	codearts  → {AK, SK, SessionToken, RefreshToken, DPoP私钥} + 一次性 refresh_token + 签名
//	loomy     → {session}                                     ← 就这一个字段
//
// 来源：Loomy 是 Electron 应用，登录后把登录态明文写在
// `%APPDATA%\Loomy\Local Storage\leveldb\*.log` 的 `loomy-auth-session` 键下，
// 里面的 `session` 字段（32 位小写 hex）就是它的 API 凭证
// —— 官方客户端自己就是用这个值调 `loomyad.xunfei.cn` 的
// （app.asar 里 `useSessionAuth: true` 出现 32 次），
// 所以"拿 session 调 API"是客户端的正常行为，不是我们发明的东西。
//
// # 为什么本包**刻意不实现** CredentialRefresher
//
// 手册第 2.2 / 2.3 节给了一组证据说明 session 没有 TTL、也没有 refresh token：
//
//	· 客户端重启 4 次（09-11/12/13/14），session 值始终未变
//	· Local Storage 里该键只有 1 条历史，从未轮换
//	· 源码里没有 /auth/refresh、checkSession、sessionExpired 之类的判断
//	· 服务端 /models 响应无 expires / ttl 字段，响应头无 Expires
//	· 无效 session 的报错是「登录已失效，请重新登录」——区分"无效"与"过期"，
//	  说明服务端只做有效性查询
//
// 也就是说：**没有可刷的东西**。实现一个空的 RefreshCredential 会让核心
// 以为"这个上游的凭证能自动恢复"，从而把「换号重试」当成有效处置 ——
// 而现实是只能人工重开客户端。所以失效只能走 ErrKindSessionDead（永久禁用），
// 见 errorclassifier.go 的说明。
package loomy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filePrefix 凭证文件名前缀。LoadDir 按 `loomy*.json` 通配。
//
// 与另两个上游同理：各上游在自己的目录（`auths/loomy/`）里按**自己的前缀**扫，
// 于是"按上游重载 auths"不会把别家的凭证读进来
// （这正是 gateway.CredentialLoader 存在的理由，见它的注释）。
const filePrefix = "loomy"

// Auth Loomy 的一份凭证。
//
// 字段分两组：
//
//	鉴权材料  Session                     ← 唯一必需
//	展示/排查 UID / Nickname / Phone / UserID / LoggedInAt
//
// 第 2 组全部来自客户端登录态的原样字段。把它们留下来是有实际用处的：
// 多账号部署时界面上要能区分"这是哪个号"，而 session 本身是不可读的 hex。
type Auth struct {
	// Session 32 位小写 hex。**唯一**的鉴权材料，双轨鉴权都用它
	//（一个端点走 `token:`、另一个走 `Authorization: Bearer`，见 client.go）。
	Session string `json:"session"`

	// UID 账号池主键。优先取客户端登录态里的 userid；
	// 缺失时由 session 派生（见 deriveUID）—— 必须是**确定性**的，
	// 否则同一个号每次启动都会被当成新账号，池子里堆一摞幽灵条目。
	UID string `json:"uid"`

	// Nickname 展示名：优先手机号掩码（客户端给的就是 `138****0000` 这种形态），
	// 其次 UID 后缀。两者都没有时留空，界面按 UID 前缀显示。
	Nickname string `json:"nickname,omitempty"`

	// Phone 客户端登录态里的手机号（可能已掩码）。
	Phone string `json:"phone,omitempty"`
	// UserID 客户端登录态里的 `userid`（原样保留；UID 就是它，除非缺失）。
	UserID string `json:"userId,omitempty"`
	// LoggedInAt 客户端记录的登录时刻（ISO8601 字符串）。
	//
	// 为什么保留字符串而不是解析成 time.Time：它只用于展示与"同一 uid 多份
	// 凭证取较新者"的排序，解析失败不该让整份凭证作废。字符串比较对 ISO8601
	// 恰好是时间序（同格式同精度），代价为零。
	LoggedInAt string `json:"loggedInAt,omitempty"`
}

// deriveUID 由 session 派生一个稳定 UID。
//
// 用在客户端登录态里没有 userid 的情形（手抄 session、或上游改了字段名）。
// 取 sha256 前 16 位十六进制而不是直接截 session：
// 一是避免把凭证本身当成账号标识到处出现在日志/界面里，二是长度可控。
func deriveUID(session string) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:8])
}

// looksLikeSession 报告 session 是否形如手册记录的 32 位小写 hex。
//
// ⚠ 它只用来**告警**，不作为拒绝条件。
//
// # 为什么不 fail fast
//
// 上游随时可能改成别的形态（加长、加前缀、换编码）。硬校验的后果是
// 一个**仍然可用**的凭证被网关自己拒之门外，而用户看到的是
// "启动就报错" —— 那种错误最难归因：明明是上游格式变了，却像用户抄错了。
// 所以这里只在加载时打一行告警，把判断留给上游：
// 真的不对，第一次请求就会拿到「登录已失效」，那条错误会被如实分类并展示。
func looksLikeSession(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// rawSession 是客户端登录态 JSON 的原样形态。
//
// `userid` 是全小写（手册第 2.2 节实录），但为了兼容手写凭证，
// 同时接受驼峰 `userId`。两个字段都在时以全小写为准（那是客户端的原样输入）。
type rawSession struct {
	Phone       string `json:"phone"`
	MaskedPhone string `json:"maskedPhone"`
	Session     string `json:"session"`
	UserID      string `json:"userid"`
	UserIDCamel string `json:"userId"`
	LoggedInAt  string `json:"loggedInAt"`

	// 下面两个是本网关写盘时补上的投影字段，重新读入时原样接受。
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
}

// ParseCredential 解析一份凭证 JSON。
//
// 同时认两种形态，这是刻意的：
//
//	① 客户端原样导出的登录态（只有 session/userid/phone/loggedInAt）
//	② 本网关写盘的形态（多出 uid/nickname）
//
// 用户从手册第 5 节抄出来的是 ①，粘进 `auths/loomy/loomy-<uid>.json` 就能用；
// 网关自己写出来的是 ②，重启后能原样读回。拒绝任何一种都会造成
// "手工能建、程序建的不认"这种别扭的不对称。
func ParseCredential(raw []byte) (*Auth, error) {
	var rs rawSession
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, fmt.Errorf("loomy: 凭证不是合法 JSON: %w", err)
	}
	// session 是唯一必需字段：没有它这份凭证什么也做不了。
	// 与其留一个空 session 让它在第一次请求时炸成"登录已失效"，
	// 不如在加载时就明确说"这份凭证没有 session"。
	session := strings.TrimSpace(rs.Session)
	if session == "" {
		return nil, fmt.Errorf("loomy: 凭证缺少 session 字段（它是唯一鉴权材料）")
	}

	a := &Auth{
		Session:    session,
		Phone:      firstNonEmpty(rs.MaskedPhone, rs.Phone),
		UserID:     firstNonEmpty(rs.UserID, rs.UserIDCamel),
		LoggedInAt: rs.LoggedInAt,
	}
	// UID：优先登录态的 userid；其次文件里已有的 uid（网关自己写的）；
	// 都没有才派生。顺序不能反 —— 同一个号在"先派生了 uid、后拿到 userid"
	// 之后必须收敛到 userid，否则池子里会出现同一份凭证的两个账号。
	a.UID = firstNonEmpty(a.UserID, strings.TrimSpace(rs.UID), deriveUID(session))
	a.Nickname = firstNonEmpty(strings.TrimSpace(rs.Nickname), a.Phone, shortUID(a.UID))

	if !looksLikeSession(session) {
		// 只告警（见 looksLikeSession 的注释）。
		log.Printf("loomy: 凭证 uid=%s 的 session 不是 32 位小写 hex（实测形态如此，"+
			"可能是上游改了格式或抄漏了字符）；仍按原样使用，若请求失败请核对来源",
			shortUID(a.UID))
	}
	return a, nil
}

// LoadDir 扫描 dir 下 `loomy*.json`，返回全部可用凭证。
//
// 语义与另两个上游的 LoadDir 对齐：
//
//   - 目录不存在 / 没有匹配文件 → 返回 (nil, nil)，**不是错误**
//     （用户还没添加过账号是正常状态，不是故障）
//   - 单个文件解析失败 → 记日志并跳过，不影响其余凭证
//     （静默跳过会让"少了一个号"变成无法排查的事）
//   - 同一 UID 出现多份 → 取 LoggedInAt 较新者（见 pickWinners）
//
// ⚠ dir 为空串时直接返回，不做 Glob。
//
// 这是从 codeartsCredStore 的注释里学来的坑：`filepath.Glob(filepath.Join("", "loomy*.json"))`
// 会退化成**相对进程 CWD** 的模式，把当前目录下任意 loomy*.json 当凭证读进来。
// 生产装配不会传空（config.go 保证至少是 "<auth_dir>/loomy"），但这条守卫
// 不该依赖调用方的自觉。
func LoadDir(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, filePrefix+"*.json"))
	if err != nil {
		return nil, fmt.Errorf("loomy: 扫描凭证目录 %s 失败: %w", dir, err)
	}
	// 排序让加载顺序稳定（Glob 本身已排序，但显式一次以免依赖它的实现细节）：
	// 顺序会影响同一 UID 多份凭证时"谁先被看到"，进而影响日志可复现性。
	sort.Strings(files)

	out := make([]*Auth, 0, len(files))
	for _, fp := range files {
		raw, err := os.ReadFile(fp)
		if err != nil {
			log.Printf("loomy: 读取凭证文件失败，已跳过 %s: %v", filepath.Base(fp), err)
			continue
		}
		a, err := ParseCredential(raw)
		if err != nil {
			log.Printf("loomy: 解析凭证文件失败，已跳过 %s: %v", filepath.Base(fp), err)
			continue
		}
		out = append(out, a)
	}
	return pickWinners(out), nil
}

// pickWinners 按 UID 去重，同一 UID 保留 LoggedInAt 较新的一份。
//
// 为什么需要：同一个号可能既有一份"手抄的 session"，又有一份"网关写回的"，
// 或者用户复制文件时留下了 `loomy-<uid> (1).json`。
// 两份都进池子会让同一个号被当成两个账号（并发上限被虚增、日志对不上）。
//
// 返回顺序 = UID 在入参里**首次出现**的顺序，不按 map 遍历
// —— map 顺序随机，会让池子内容与启动日志不可复现。
func pickWinners(list []*Auth) []*Auth {
	best := make(map[string]*Auth, len(list))
	order := make([]string, 0, len(list))
	for _, a := range list {
		if a == nil || a.UID == "" {
			continue
		}
		cur, seen := best[a.UID]
		if !seen {
			best[a.UID] = a
			order = append(order, a.UID)
			continue
		}
		// LoggedInAt 是 ISO8601，同格式下字符串比较即时间序（见 Auth.LoggedInAt）。
		// 空值视为最旧：有时间的比没时间的可信。
		if a.LoggedInAt > cur.LoggedInAt {
			best[a.UID] = a
		}
	}
	out := make([]*Auth, 0, len(order))
	for _, uid := range order {
		out = append(out, best[uid])
	}
	return out
}

// FileName 返回某凭证应落盘的文件名。
//
// 命名规则与 cmd/login 一致：`<prefix>-<uid>.json`。
// 用 UID 而不是 session 做文件名，避免凭证材料出现在文件系统路径里。
func FileName(a *Auth) string {
	return filePrefix + "-" + sanitizeFilePart(a.UID) + ".json"
}

// MarshalAuthFile 把凭证编成本网关的落盘形态（②）。
//
// 带缩进与结尾换行：这些文件是人会打开看的（排查 session 是不是抄错了），
// 压成一行会让 diff 与肉眼核对都变得难受。
func MarshalAuthFile(a *Auth) ([]byte, error) {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// sanitizeFilePart 把 UID 里不适合做文件名的字符换掉。
//
// 现在的 UID 只有十六进制与数字，但 UID 可能来自上游的 userid 字段
// （形态由上游决定）。让它带一个 `/` 或 `:` 进文件名，会在落盘时
// 变成"写到别的目录"或 Windows 上的非法名 —— 那是很难归因的失败。
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

// shortUID 取 UID 前 8 位供日志与展示名使用（与仓库其它地方的口径一致）。
//
// ⚠ 按**字符**截断，不按字节：UID 来自上游的 userid 字段（形态由上游决定），
// 一旦它不是纯 ASCII，按字节切会把一个多字节字符劈成两半 ——
// 日志里出现 `\ufffd`（本项目在 handler.go 的 contentBlockMsg 上踩过同一个坑）。
// 现在它还被用在额度那条日志里（短 uid 对不上时要打印两个 uid），
// 拼错一个汉字不会影响功能，但会让排错的人以为数据坏了。
func shortUID(uid string) string {
	rs := []rune(uid)
	if len(rs) <= 8 {
		return uid
	}
	return string(rs[:8])
}

// firstNonEmpty 返回第一个非空（去空白后）的字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
