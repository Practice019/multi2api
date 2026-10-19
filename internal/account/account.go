// Package account 控制台**账号**（人）与**登录态**（浏览器会话）。
//
// # 为什么不放进 internal/auth（名字很像，语义完全不同）
//
// `internal/auth` 里的 `Auth` 是**上游凭证**（workbuddy/codearts 的
// accessToken/refreshToken 文件，含 refresh 与原子写回）。把控制台账号塞进去，
// 后来人会读错：把"上游 token 过期"当成"登录态过期"，或者反过来把登录口令
// 写进上游凭证文件。所以另起一个包，并且**不复用**那个类型的任何字段。
//
// # 抄自哪里（new-api）
//
// 出处：viking://resources/repos/QuantumNous/new-api（下同，逐条列行号）
//
//	角色常量      common/constants.go:199-200   RoleCommonUser=1 / RoleAdminUser=10
//	注册强制角色  controller/user.go:283        `Role: common.RoleCommonUser`
//	                                             （**不信客户端传上来的 role**）
//	用户字段约束  model/user.go:82              username/password 的 min/max
//	口令哈希      common/crypto.go:26/34        bcrypt
//	会话          model/user_session.go         SID / Version / UserAuthVersion /
//	                                             ExpiresAt / LastActiveAt / Status
//
// # 单机简化（用户要求"实现可以最简单实现"）
//
//	**单角色**（只有管理员，且只有一个账号）、JSON 落盘、进程内会话表；
//	不引数据库、不做多租户/RBAC/2FA。
//
// # ⚠ 2026-10-19 收口径：用户面取消
//
// 曾经的"管理员/成员两级"已收成**单账号管理员**：建号只剩首启那一次
// （CreateBootstrap），注册接口在库非空后关闭，成员留下的历史记录不构成身份。
// 详见 account.go 的角色常量注释、store.go 的 New/Authenticate 与 http.go 的 register。
package account

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 角色。数值照抄 new-api（common/constants.go:199-200）而不是自己编号：
// 将来若真要加"超管/只读"这类级别，不用做数据迁移，且与参考实现的判据
// `myRole > targetRole` 语义天然一致（见 canManageTargetRole 那类写法）。
//
// # ⚠ 2026-10-19（用户新口径）：登录系统收成**单账号管理员**
//
// 用户原话：「我觉得可以只要一个管理员就行了，只要一个账号。用户其实可以不用。」
// ⇒ 成员（RoleMember）这个角色**不再存在**：没有任何代码路径会**创建**出
// 非管理员账号（建号只有 Store.CreateBootstrap 一条路，它写死 RoleAdmin），
// 且任何非管理员的历史记录都**不构成身份**（Authenticate / Lookup 都拒）。
//
// 常量本身保留为**只读兼容值**，理由有二（都不是"以后可能还要用"）：
//
//	① 盘上可能有改造前建出来的成员行 —— 必须能**认出**它（拒绝时给出准确
//	   文案与日志），而不是把它当成"未知角色"糊过去（见 store.go 的 New）。
//	② 界面/日志里那句「成员账号已取消，请用管理员账号」需要一个名字。
const (
	// RoleMember 【历史值 · 已取消】普通成员。别再建它，也别把有它的记录
	// 当成可用账号 —— 它只是一个"我们认得的旧值"，不是一种权限。
	RoleMember = 1
	// RoleAdmin 管理员：唯一还能被创建、也是唯一能构成身份的角色。
	RoleAdmin = 10
)

// RoleName 给界面/日志用的中文名。未知值回一个明确的串，不猜。
func RoleName(role int) string {
	switch role {
	case RoleMember:
		return "成员（已取消）"
	case RoleAdmin:
		return "管理员"
	}
	return fmt.Sprintf("未知角色(%d)", role)
}

// ValidRole 报告 role 是不是我们**认识**的角色值。
//
// ⚠ 它现在是**只读判据**（唯一调用点是 store.go 读盘时的归一化），不再是
// "能不能写进去"的校验 —— 写入侧的角色参数已经不存在了：建号只有
// CreateBootstrap（写死管理员），没有 SetRole 这种改角色的口子。
//
// 认识 RoleMember 是刻意的：旧数据要被**认出来再拒掉**（明确文案 + 日志），
// 而不是落到"未知角色"那条含混的路上。
func ValidRole(role int) bool { return role == RoleMember || role == RoleAdmin }

// IsAdminRole 报告 role 是不是"能构成身份"的角色。
//
// 全仓只有这一个判据，且**必须**被身份链路（Authenticate / Lookup）调用：
// 单账号模式下"角色不是管理员"就等于"这个账号不存在"。
func IsAdminRole(role int) bool { return role == RoleAdmin }

// 用户名字符集与长度：与 new-api 的 `min=8,max=128` 只约束口令不同，
// 我们对用户名也给定界 —— 用户名会出现在日志、界面与 Cookie 之外的
// 每个地方，放任它是一段任意 Unicode 会让"同名不同形"（全角空格、
// 零宽字符）变成一类看不出来的冒充手法。
const (
	usernameMin = 3
	usernameMax = 32
	passwordMin = 8
	passwordMax = 128
)

// 错误哨兵：由 HTTP 层映射成状态码与文案。
var (
	// ErrUsernameTaken 用户名已被占用（比较口径是 normalizeUsername）。
	ErrUsernameTaken = errors.New("account: 用户名已被占用")
	// ErrNotFound 账号不存在。
	ErrNotFound = errors.New("account: 账号不存在")
	// ErrBadUsername 用户名不合法（长度/字符集）。
	ErrBadUsername = errors.New("account: 用户名不合法")
	// ErrBadPassword 口令不合法（长度）。
	ErrBadPassword = errors.New("account: 口令不合法")
	// ErrBadRole 角色值不认识。
	ErrBadRole = errors.New("account: 角色值不合法")
	// ErrCredentials 用户名或口令不正确。
	//
	// ⚠ 登录失败时**只有这一个**错误：不区分"用户不存在"与"口令错误"，
	// 否则错误文案本身就变成用户名枚举接口（new-api 的登录也没有区分，
	// 但它的注册接口会直说"用户名已存在" —— 那种取舍在开放注册下无法避免，
	// 登录这一侧则没必要再漏一次）。
	ErrCredentials = errors.New("account: 用户名或口令不正确")

	// ErrBootstrapDone 首个账号已存在，"创建管理员"这条路已经用过了。
	//
	// 它不是"错误"而是"状态"：首启建管理员的入口只允许成功一次。
	ErrBootstrapDone = errors.New("account: 管理员已存在，创建入口已关闭")

	// ErrMemberRetired 这条记录是**非管理员**（成员的旧数据，或角色字段
	// 缺失/手写成别的值）⇒ 单账号管理员模式下**不构成任何身份**。
	//
	// # 为什么它必须与 ErrCredentials 分开
	//
	// 用户新口径要的是"明确文案 + 一条日志"：告诉拿着成员账号的人
	// 「成员账号已取消，请用管理员账号」，而不是让他对着"用户名或口令不正确"
	// 反复重试（他会以为是自己记错了口令，而那个账号在改造前是好的）。
	//
	// ⚠ 但这**不是**用户名枚举口子：它只在**口令校验通过之后**才可能返回
	// （见 Store.Authenticate 的顺序），所以拿不到口令的人区分不出
	// "这个用户名被取消了"与"用户名不存在"。
	ErrMemberRetired = errors.New("account: 成员账号已取消")
)

// User 一个控制台账号（持久化字段）。
//
// ⚠ 这个结构体里带 `PasswordHash`。**不要把它直接序列化给前端**：
// 需要对外时用 View（见 store.go 的 List/Snapshot）。
type User struct {
	ID          string `json:"id"`           // u- 开头的随机标识
	Username    string `json:"username"`     // 原样保存（大小写保留），唯一性按小写比
	DisplayName string `json:"display_name"` // 界面显示名，默认取 Username
	// PasswordHash bcrypt 哈希（含 cost 与 salt）。**永不出现在日志/响应里**。
	PasswordHash string `json:"password_hash"`
	Role         int    `json:"role"`
	Enabled      bool   `json:"enabled"`

	// AuthVersion 账号级的"凭据版本"。改口令/强制下线时 +1。
	//
	// 语义照抄 new-api 的 user.auth_version：每条会话记下签发时的 AuthVersion，
	// 校验时两者必须相等 —— 于是"改了口令"能让**所有**旧会话一次性失效，
	// 不需要去遍历会话表挨个删。
	//
	// ⚠ 但"改口令的**这个人自己**"不该被踢下线（用户报障：轮换/改密后
	// 页面必须重启）。做法同样是抄来的：把当前这条会话的 AuthVersion
	// **就地升到新值**（new-api `model/user_session.go` 的
	// AdvanceUserSessionAuthVersion），见 session.go。
	AuthVersion int64 `json:"auth_version"`

	CreatedAt   string `json:"created_at"`              // RFC3339（UTC）
	LastLoginAt string `json:"last_login_at,omitempty"` // RFC3339（UTC）
	LastLoginIP string `json:"last_login_ip,omitempty"`
}

// View 账号的对外形态：**没有** PasswordHash，也没有 AuthVersion。
//
// AuthVersion 是内部判据，对外没有任何意义；少暴露一个"改过几次口令"
// 的侧信道。
type View struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        int    `json:"role"`
	RoleName    string `json:"role_name"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	LastLoginAt string `json:"last_login_at,omitempty"`
	LastLoginIP string `json:"last_login_ip,omitempty"`
}

// view 把内部结构体转成对外形态（脱敏）。
func (u *User) view() View {
	return View{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		RoleName:    RoleName(u.Role),
		Enabled:     u.Enabled,
		CreatedAt:   u.CreatedAt,
		LastLoginAt: u.LastLoginAt,
		LastLoginIP: u.LastLoginIP,
	}
}

// normalizeUsername 用户名比较口径：去首尾空白 + 转小写。
//
// 只用于**比较**（唯一性、查找），存盘仍存用户输入的原样（Username 字段），
// 界面按原样显示。这样 `Alice` 与 `alice` 是同一个人（避免"看起来一样却
// 是两个账号"的钓鱼空间），显示又不被强行改写成小写。
func normalizeUsername(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// validateUsername 校验用户名。
//
// 允许的字符集是**白名单**（不是黑名单）：字母/数字/`_`/`-`/`.`/`@`。
// 邮箱形态因此也能当用户名（`me@example.com`）。
// 之所以连 Unicode 都不放：用户名会进日志与界面，允许零宽/双向控制字符
// 会让"看起来一样的名字"变成一类看不见的冒充。
func validateUsername(name string) error {
	n := strings.TrimSpace(name)
	if len(n) < usernameMin || len(n) > usernameMax {
		return fmt.Errorf("%w：长度需在 %d–%d 之间", ErrBadUsername, usernameMin, usernameMax)
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '@':
		default:
			return fmt.Errorf("%w：只允许字母数字与 _ - . @", ErrBadUsername)
		}
	}
	return nil
}

// validatePassword 校验口令长度。
//
// 长度口径照抄 new-api 的 `validate:"min=8,max=128"`（model/user.go:82）。
// 不做"必须含大小写数字"的复杂度规则：那类规则在实践里产出的口令
// （`Password1!`）比长口令弱，而 128 的上界是为了挡住"10MB 口令"这种
// 拿 CPU 换血的 DoS（bcrypt 对超长输入的处理取决于实现，别赌）。
func validatePassword(password string) error {
	if len(password) < passwordMin || len(password) > passwordMax {
		return fmt.Errorf("%w：长度需在 %d–%d 之间", ErrBadPassword, passwordMin, passwordMax)
	}
	return nil
}

// newID 生成账号 ID（`u-` + 16 位十六进制）。
//
// 用 crypto/rand 而不是自增序号：ID 会出现在前端 URL 与日志里，
// 自增序号等于把"这台机器上有几个账号、谁先注册"广播出去，
// 而且是天然的遍历目标（IDOR 的入口）。
func newID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败意味着这台机器无法生成不可预测的随机数 ——
		// 此时**不能**退化成时间戳（那会让 ID 可预测）。直接 panic：
		// 账号系统在无法保证随机性的环境里继续跑，本身就是错的。
		panic("account: 无法生成随机 ID: " + err.Error())
	}
	return "u-" + hex.EncodeToString(buf[:])
}

// nowStamp 统一的时间戳形态（RFC3339，UTC）——与 internal/apikey 的
// CreatedAt 同一口径，避免同一份 JSON 里出现两种时间格式。
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }
