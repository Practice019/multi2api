// expiry.go 本上游向核心报告「这份凭证什么时候到期」—— 账号列表的「Token 到期」列。
//
// # 用户报的问题（2026-11-01）
//
// 「我的 MonkeyCode 账号不显示每个账号的 Token 时间…我估计有一个账号就是 Token 过期了，
//
//	所以它现在用不了了。想办法在前端加一列信息，就是 Token 时间。」
//
// 事实：这一列**本来有**，是本上游把它从列集里删掉的（见 accountcolumns.go 的旧注释：
// 「本上游没有可展示的 bearer 到期时刻」）。那个判断对了一半 ——
// monkeycode 确实没有 bearer token（它是 session cookie + oma/omas 三元组），
// **但 session 有 30 天硬过期**（Auth.Session 的注释里就写着，无续期接口）。
// 「没有 bearer」不等于「没有到期时刻」；而到期恰恰是用户最需要看到的那个事实。
//
// # 这个时刻是怎么算出来的（诚实说明）
//
// 上游**没有**任何接口报告 session 的确切到期时刻，所以这里给出的是**推断值**：
//
//	锚点 + SessionLifetime(30 天)
//
// 锚点按可信度取第一个可得的：
//
//	① Auth.SessionRenewedAt   最近一次**自动续期成功**的时刻（2026-11-01 新增）
//	                           最可信 —— 新 session 就是那一刻签发的，30 天从那里算
//	② Auth.AddedAt            我们自己入库时盖的章（RFC3339）
//	③ 凭证文件的 mtime          ≈ 该凭证被写进本仓的时刻（登录/导入时写，探测不写回）
//	④ 都没有                    → 未知（返回 false，界面显示空/`—`）
//
// 偏差方向要说清楚：②比①**可能偏晚**（把老会话的文件复制进本仓时，mtime 是复制时刻）。
// 偏晚的含义是「显示还有几天，其实已经没了」—— 所以这一列**只用于预警**，
// 不能当判据：能不能用永远由真实请求决定（401 → SessionDead → 自动禁用），
// 那才是这个仓库里判「号还能不能用」的唯一权威。
//
// ★ 2026-11-01：本文件原来那条「无续期接口」的结论是**错的**（见 renew.go）。
// 上面加了 ① 这个锚点，就是为了让"续过期"的号显示**真续期时刻 + 30 天**，
// 而不是拿 AddedAt 估出来的假值（那种号会被这一列误报成"即将过期"，
// 用户于是白跑一次人工重登）。
//
// # 为什么不做成"探测一次问上游"
//
// 这是**账号列表渲染路径**上的调用（每个账号一次）。
// 在渲染路径上发网络请求会让列表变慢且把上游打热 —— 与 codearts 的 TokenExpiry 同一条纪律。
package monkeycode

import (
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// SessionLifetime 会话寿命：30 天。
//
// ⚠ 这是**上游的事实**（实测：password-login 的 `Set-Cookie` 带
// `Max-Age=2592000` = 30 天，服务端亲口给的），不是我们挑的窗口。
//
// ★ 2026-11-01 更正：本常量原来的注释写着「**无续期接口**，过期只能重新登录/重新注册」——
// **那条结论是错的**。真实情况：可以续期，且不需要有效 session：
//
//	邮箱 + 密码 + PoW → POST /api/v1/users/password-login → 新 session（实现见 renew.go）
//
// 所以"过期"不再等于"这个号废了"：有 Email/Password 的号能自动救回来，
// 只是**救的时候也要花一次 login**（所以到期预警仍然有意义）。
const SessionLifetime = 30 * 24 * time.Hour

// 编译期断言：与 AccountColumnsExt / CredentialTokenExt 并列。
var _ gateway.CredentialExpiryExt = (*Provider)(nil)

// TokenExpiry 报告这份凭证的**推断**到期时刻（Unix 秒）。
//
// 返回值语义与其它上游一致：0 或 false = 未知，**不能**当成「1970 年就过期了」。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return 0, false
	}
	at, ok := SessionAnchor(a)
	if !ok {
		return 0, false
	}
	return at.Add(SessionLifetime).Unix(), true
}

// SessionAnchor 取会话登记时刻（推断锚点）。
//
// 顺序与理由见文件头：续期章 > 入库章 > 文件 mtime > 未知。
func SessionAnchor(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	// ① 续期章：最高优先。续期过之后，session 的 30 天是从**那一刻**算的，
	//    拿 AddedAt 会严重低估剩余时间（老号续期后显示"还剩 2 天"，其实还有 30 天）。
	//
	//    解析容错（数字毫秒 / RFC3339 / 手写日期）在 Auth.UnmarshalJSON 里做掉，
	//    所以这里直接判零值即可 —— 手写形态不会被静默丢掉。
	if !a.SessionRenewedAt.IsZero() {
		return a.SessionRenewedAt, true
	}
	// 兼容路径：老档里续期时刻存的是字符串（见 Auth.SessionRenewedAtRaw 的说明）。
	// 少了这一步，老档升级后会静默退回 AddedAt 估算 —— 刚续过的号被误报"即将过期"。
	if raw := strings.TrimSpace(a.SessionRenewedAtRaw); raw != "" {
		if t, ok := ParseAuthTime(raw); ok {
			return t, true
		}
	}
	if raw := strings.TrimSpace(a.AddedAt); raw != "" {
		if t, err := parseAnchorTime(raw); err == nil {
			return t, true
		}
	}
	if path := strings.TrimSpace(a.FilePath); path != "" {
		if fi, err := os.Stat(path); err == nil {
			return fi.ModTime(), true
		}
	}
	return time.Time{}, false
}

// parseAnchorTime 解析锚点时刻。
//
// 三种布局都收：RFC3339（本仓自己写的）与两种手写常见形态
// （`2006-01-02 15:04:05` / 纯日期）—— 手写凭证是真实存在的用法，
// 为了"格式不认识"就把整个锚点丢掉会让那一列凭空变空。
func parseAnchorTime(raw string) (time.Time, error) {
	var lastErr error
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		t, err := time.Parse(layout, raw)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, lastErr
}
