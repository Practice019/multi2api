// clientlogin.go 本机客户端登录态在管理端点里的**消费方视图**。
//
// # 为什么这里是一层接口而不是直接调 internal/clientlogin
//
// 架构约束禁止上游包依赖核心包，但 internal/clientlogin 是**通用设施**
// （在 gateway/arch_test.go 的 nonUpstreamPackages 白名单里），
// 本包语法上可以直接 import 它。之所以仍然走接口，有两个具体理由：
//
//  1. 方向一致性：本包已经有了 AccountPool（账号池）、CheckinRunner（调度器）、
//     SchedulerView（时点）三条消费方接口，客户端登录态没有理由例外 ——
//     一致的接线方式比"这条能直接 import 所以直接来"更好维护。
//  2. 可测性：接口只有三个方法，测试里桩掉它不需要建一个真的 Manager
//     及其文件系统布局。
//
// 接口由本包（消费方）声明，*clientlogin.Manager 已经满足它，
// 核心无需为它做任何改动。
//
// # 这里为什么把字段抄了两遍
//
// 下面两个结构体与 internal/clientlogin 的 Status / SwitchResult
// **逐字段同名同 tag** —— 因为它们的用途是把 clientlogin 的响应原样透传给
// 前端，任何字段名偏差用户都看得见。抄两遍是架构约束的代价，
// 转换只发生在 cmd/server 的 clientLoginAdapter 里（与 modelCatalogState
// 的处理方式一致）。
package workbuddy

import "errors"

// ClientLoginManager 本机客户端登录态管理。
//
// 三个方法的语义与 clientlogin.Manager 的对应方法逐字一致：
// Status 只读；Switch/Restore 是**破坏性**操作，失败原因由调用方翻译成 HTTP。
type ClientLoginManager interface {
	// Status 读客户端当前登录态与可切换候选人。
	Status() (*ClientLoginStatus, error)
	// Switch 把客户端登录态切到指定账号（先备份，失败即中止）。
	Switch(uid string) (*ClientLoginSwitchResult, error)
	// Restore 回滚到上一次切换前的登录态。
	Restore() (*ClientLoginSwitchResult, error)
}

// ClientLoginCandidate 一个可切换的登录态候选（与 clientlogin.Candidate 逐字段对应）。
//
// 字段在这里声明成具体类型而不是 any：前端会逐字段渲染
// （昵称、来源、有效期），用 any 会让"少传一个字段"变成静默的界面缺陷。
type ClientLoginCandidate struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	// Source 见 clientlogin 的 SourceClient / SourceGateway。
	Source string `json:"source"`
	// ExpiresAt 是 accessToken 到期时间（Unix 秒，0 表示未知）。
	ExpiresAt int64 `json:"expires_at"`
	// ExpiresAtText 是本地时区的可读到期时间。
	ExpiresAtText string `json:"expires_at_text"`
	// Valid 表示 accessToken 尚未过期。
	Valid bool `json:"valid"`
	// Current 表示这就是客户端当前登录的账号。
	Current bool `json:"current"`
	// Restorable 表示这个账号有一份客户端原生凭证存档（切换后字段最完整）。
	Restorable bool `json:"restorable"`
	// TokenHint 是 token 的脱敏指纹，便于人工核对而不是靠信任。
	TokenHint string `json:"token_hint"`
}

// ClientLoginStatus 客户端登录态视图（与 clientlogin.Status 逐字段对应）。
type ClientLoginStatus struct {
	Enabled    bool   `json:"enabled"`
	ClientDir  string `json:"client_dir"`
	ArchiveDir string `json:"archive_dir"`
	// ClientFile / SnapshotFile 是会被切换改写的两个文件，UI 明示出来。
	ClientFile   string `json:"client_file"`
	SnapshotFile string `json:"snapshot_file"`
	// Current 为 nil 表示客户端当前没有可用凭证（未登录或文件缺失）。
	Current *ClientLoginCandidate `json:"current"`
	// Candidates 按「当前 → 客户端原生 → 管理台账号池」排序。
	Candidates []ClientLoginCandidate `json:"candidates"`
	// ClientRunning 表示 WorkBuddy 桌面客户端当前在运行。
	// 在跑时切换/回滚都会被它原地覆盖，所以界面据此把按钮置灰并说明要先退出。
	ClientRunning bool `json:"client_running"`
	// HasBackup 表示存在可一键回滚的上一次登录态。
	HasBackup bool `json:"has_backup"`
	// BackupUID / BackupNick 是备份里那个账号（回滚后会变成这个账号），不能只给时间，
	// 否则用户无法判断点下去会回到谁。
	BackupUID  string `json:"backup_uid"`
	BackupNick string `json:"backup_nick"`
	BackupAt   string `json:"backup_at"`
	// Error 非空时说明读取过程中有失败（例如目录不存在），UI 直接展示。
	Error string `json:"error,omitempty"`
}

// ClientLoginSwitchResult 切换/回滚的结果（与 clientlogin.SwitchResult 逐字段对应）。
type ClientLoginSwitchResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Source   string `json:"source"`
	// Changed 为 false 表示目标账号本来就是当前登录账号，未做任何写入。
	Changed bool `json:"changed"`
	// Message 是面向用户的一句话结论。
	Message string `json:"message"`
	// BackupPath 非空表示可以在出问题时手动还原这个文件。
	BackupPath string `json:"backup_path,omitempty"`
}

// 客户端登录态的三种「预期失败」。它们与 internal/clientlogin 的同名哨兵
// 是**各自声明**的两组值（本包不得 import 核心包），语义一一对应；
// 由 cmd/server 的适配器负责翻译，保证 errors.Is 在两边都成立。
var (
	// ErrSameAccount 目标账号已经是客户端当前登录账号。
	ErrSameAccount = errors.New("该账号已经是客户端当前登录账号")
	// ErrAlreadyBackedUp 备份里的账号与当前登录账号一致，回滚是无操作。
	ErrAlreadyBackedUp = errors.New("当前已是备份中的登录态，无需回滚")
	// ErrClientRunning 客户端正在运行：写盘会被它用内存里的旧登录态覆盖。
	ErrClientRunning = errors.New("客户端正在运行")
)

// 别名：让 adminendpoints.go 里的判定读起来是"上游语义"而不是包名。
var (
	clientloginErrSameAccount     = ErrSameAccount
	clientloginErrClientRunning   = ErrClientRunning
	clientloginErrAlreadyBackedUp = ErrAlreadyBackedUp
)

// errorsIs 是 errors.Is 的本地别名（保持 adminendpoints.go 的 import 面最小）。
func errorsIs(err, target error) bool { return errors.Is(err, target) }
