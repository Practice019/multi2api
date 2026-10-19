// identify.go 请求身份的**单一**解析入口（会话 / 管理令牌）。
//
// # 为什么单独一个文件、而且必须在本包（而不是各自在 admin / server 里写一遍）
//
// 同一个端口上挂着两个入口：`/ui`（在 internal/server）与 `/admin/*`
// （在 internal/admin）。两者都要回答同一个问题——"这次请求是谁发的"。
//
// 各自写一份的后果是**漂移**，而漂移的形态恰好是我们刚修完的那类漏洞：
//
//	/ui 认令牌 cookie、/admin/* 不认 ⇒ 页面能打开，每个标签页 403
//	/ui 不认、/admin/* 认      ⇒ 页面打不开，但接口敞开（更难发现）
//	两边的 Cookie 名/判据有细微差别 ⇒ "登录成功但管理面永远说没登录"
//
// 所以判据写在**一处**，两个包都调它。cookie 名、Secure 策略、SameSite
// 也一起收在这里（那是同一份事实的另外几面）。
package account

import (
	"net/http"
	"strings"
	"time"
)

// AdminTokenCookie 管理令牌（admin_token）在浏览器里的载体。
//
// # 它为什么存在（`/ui?token=<admin_token>` 的一次性换手）
//
// 浏览器地址栏没法自定义请求头，所以"用管理令牌进控制台"只能靠查询参数
// —— 但把令牌留在 URL 里会顺着**历史记录 / Referer / 代理日志 / 截图**
// 一路漏出去。做法是：带 `?token=` 打开 `/ui` → 服务端校验通过后下发这个
// HttpOnly cookie 并 302 回干净的 `/ui`，URL 里不再有令牌。
//
// ⚠ 与**会话**（CookieName = wb2api_sid）是两件不同的事，刻意用两个名字：
//
//	wb2api_sid          人登录出来的会话（有账号、有角色、可撤销、可列出）
//	wb2api_admin_token  机器令牌的浏览器副本（无账号，等价管理员）
//
// 合成一个 cookie 的坏处：要么"退出登录"会把机器令牌一起清掉（脚本被打断），
// 要么令牌轮换后会话的表意变得含混（不知道该按哪条失效路径走）。
//
// ⚠ 轮换令牌会自动作废它：每次请求都拿 cookie 的值与**当前**生效的令牌
// 比，轮换改的是当前值 ⇒ 旧 cookie 立刻不再匹配，不需要额外清理动作。
const AdminTokenCookie = "wb2api_admin_token"

// AdminTokenCookieTTL 令牌 cookie 的有效期。
//
// 有界而不是"会话级"（浏览器关掉就没了）：这是**机器凭据**的副本，
// 不该像人登录的会话那样反复重来；12 小时足够覆盖一个工作日。
// 真正的失效保障是"轮换即作废"，不是这个时间。
const AdminTokenCookieTTL = 12 * time.Hour

// 身份来源（Principal.Kind）。
const (
	// KindSession 由人登录产生：有账号、有角色、可撤销。
	KindSession = "session"
	// KindToken 由管理令牌产生：无账号，**等价管理员**（脚本/CI 的机器凭据）。
	KindToken = "token"
)

// Principal 一次请求的身份。`ok=false`（零值）表示未认证。
//
// Role 用 RoleMember / RoleAdmin（抄 new-api common/constants.go:199-200
// 的两个值）。令牌来源恒为 RoleAdmin —— 机器凭据没有"成员"这种身份，
// 角色存在的意义是**区分人**。
type Principal struct {
	Kind     string // KindSession / KindToken
	UserID   string // 仅 KindSession
	Username string
	Role     int
	// User 仅 KindSession 非 nil。带 PasswordHash，**不要**序列化给前端
	// （对外用 View，见 store.go）。
	User *User
}

// IsAdmin 是否具备管理员角色。
func (p Principal) IsAdmin() bool { return p.Role >= RoleAdmin }

// bearerCred 取 Authorization: Bearer <cred> 里的凭据（无则空串）。
func bearerCred(r *http.Request) string {
	if r == nil {
		return ""
	}
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(v) < 7 || !strings.EqualFold(v[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(v[7:])
}

// TokenFromRequest 按优先级取请求里的**管理令牌**：
//
//	① Authorization: Bearer <admin_token>   脚本/CLI（首选，不进 URL）
//	② ?token=<admin_token>                  浏览器地址栏唯一能带凭据的地方
//	③ Cookie wb2api_admin_token             `/ui?token=` 换来的副本
//
// ⚠ 顺序有意义：Bearer 优先于查询参数，这样"脚本带对了头但 URL 里残留了
// 一个旧参数"时以头为准（参数可能是别人贴给他的残缺链接）。
//
// ⚠ 这里**只负责取**，不做判定 —— 判定必须由调用方用**当前**生效的令牌
// 做定长比较（见各包的 isToken 回调）。取与判分开是为了不让"哪个字段算
// 凭据"的知识散落两处。
func TokenFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if cred := bearerCred(r); cred != "" {
		return cred
	}
	if cred := strings.TrimSpace(r.URL.Query().Get("token")); cred != "" {
		return cred
	}
	c, err := r.Cookie(AdminTokenCookie)
	if err != nil || c == nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

// Identify 解析请求身份：① 管理令牌 → ② 浏览器会话 → 未认证。
//
// # 顺序与理由
//
// 令牌先判：一次定长字符串比较（无 IO、无锁），而会话要走 Lookup
// （查表 + 可能落盘续期）。绝大多数管理端点是脚本/探针在打，先命中便宜那条。
//
// # isToken 为什么是回调而不是令牌值
//
// 令牌支持**运行时轮换**（用户报障：轮换后要重启才生效 —— 根因就是内存里
// 存了构造期快照）。传回调让每次请求都读**当前**值，且调用方（admin / server）
// 各自持有可变的令牌字段。传值会把快照问题原样复制过来。
//
// # 会话这条路必须走 Lookup
//
// Lookup 里带着四道校验：会话存在 / 未过期 / 账号仍存在且启用 /
// AuthVersion 相等（改口令与强制下线靠它）。自己读 cookie 查表会漏掉全部四道，
// 表现成"口令改了、账号禁了，旧 cookie 照样能进管理面"。
func Identify(r *http.Request, isToken func(string) bool, accounts *Store, sessions *SessionStore) (Principal, bool) {
	if isToken != nil {
		if cred := TokenFromRequest(r); cred != "" && isToken(cred) {
			return Principal{Kind: KindToken, Username: "admin_token", Role: RoleAdmin}, true
		}
	}
	if accounts == nil || sessions == nil {
		return Principal{}, false
	}
	sid := SIDFromRequest(r)
	if sid == "" {
		return Principal{}, false
	}
	_, u, err := sessions.Lookup(sid, accounts)
	if err != nil || u == nil {
		return Principal{}, false
	}
	return Principal{Kind: KindSession, UserID: u.ID, Username: u.Username, Role: u.Role, User: u}, true
}

// SecureCookieFor 按配置决定 cookie 要不要加 Secure 属性。
//
// 三种取值对应三种部署：auto（本进程走 TLS 才加）/ always（反代或隧道在
// 前端终止 TLS，本进程看到的是明文，auto 判不出来）/ never（本机 http 调试）。
//
// 导出是为了让 `/ui` 下发的**令牌 cookie** 与登录端点下发的**会话 cookie**
// 遵循同一策略 —— 一个加 Secure 一个不加，会让"换到 https 之后就登不上"。
func SecureCookieFor(mode string, r *http.Request) bool {
	switch mode {
	case CookieSecureAlways:
		return true
	case CookieSecureNever:
		return false
	default:
		return r != nil && r.TLS != nil
	}
}

// SetAdminTokenCookie 下发令牌 cookie（`/ui?token=` 换手用）。
//
// 三个属性缺一不可，理由与会话 cookie 相同（见 http.go 的 setSIDCookie）：
//
//	HttpOnly    前端 JS 读不到 ⇒ XSS 偷不走令牌（这正是替换 localStorage 的关键）
//	SameSite=Lax 跨站请求不带 ⇒ 挡住绝大多数 CSRF 形态
//	Path=/      整站可用（/ui 与 /admin/* 都要带）
func SetAdminTokenCookie(w http.ResponseWriter, r *http.Request, secureMode, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminTokenCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(AdminTokenCookieTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   SecureCookieFor(secureMode, r),
	})
}

// ClearAdminTokenCookie 清掉令牌 cookie（MaxAge<0 让浏览器立刻删）。
//
// 用途：令牌轮换后浏览器还揣着旧副本，每次请求都白带一个不匹配的凭据；
// 未认证访问 `/ui` 时顺手清掉，用户下次打开就是干净的登录页。
func ClearAdminTokenCookie(w http.ResponseWriter, r *http.Request, secureMode string) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminTokenCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   SecureCookieFor(secureMode, r),
	})
}
