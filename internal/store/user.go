// user.go users / user_sessions 两张表的仓储层 —— account.Store 换 SQL 后端的接缝。
//
// # 为什么这两张表在一个文件里
//
// 它们是**同一条身份链**的两半：users 是"人"，user_sessions 是"这个人的登录态"。
// 校验一条会话要同时读两张表（account 的 SessionStore.Lookup 就是那个顺序），
// 分两个文件只会让"改一边忘了另一边"多一个发生机会。
//
// # 本层只做存取，不做身份判定
//
// 角色是什么、会话算不算有效、口令哈希怎么比 —— 全是 internal/account 的事。
// 这里只回答"这一行在不在、写进去了没有"。判据放进本层会让 store 反向依赖
// 业务语义，而 store 必须能被 cmd/migrate-sql 这类工具独立使用。
//
// # 时间列是 unix 秒，JSON 是 RFC3339 串
//
// 换算在 internal/account 侧（它才知道盘上的格式），本层只收/发 int64。
// 这条分层与 models.go 文件头的「时间字段为什么是 int64」是同一件事的两面。
//
// # 与 cmd/migrate-sql 的关系（同一份映射，两处实现，判据必须一致）
//
// cmd/migrate-sql/main.go:340 的 accountToUser 是**一次性迁移工具**的映射：
// 它用 OnConflict{UpdateAll} 覆盖写（用户显式跑迁移，语义就是"以 JSON 为准"）。
// 本文件的 ImportUsersIfAbsent 是**运行时首启导入**：只补缺、绝不覆盖
// （PLAN-hyxapi-alignment.md 6.0：切换后 JSON 只作为导出格式，不得再被应用写回；
// 每次启动都拿旧 JSON 覆盖，会把 SQL 侧改过的口令冲回去）。
//
// 两处共用的判据（坏行的处置必须一致，否则同一份 accounts.json 走两条路会得到
// 两个不同的库）：
//
//	id 为空 / username 全空      → 跳过该行（username 有 UNIQUE，空串会撞约束）
//	created_at 不可解析          → 跳过该行（它是导出排序键，坏了就做不出稳定导出）
//	last_login_at 不可解析       → 归零保留该行（审计信息，不该让账号整体丢失）
//
// ⚠ 本层**不做**大小写归一化的清洗：写入什么就存什么。用户名口径归 account
// （normalizeUsername 只用于比较），库里加了 uniqueIndex 兜底。
package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrUserExists 这个 ID 或用户名已经有行了。
//
// 单列出来是因为调用方要区分"建号失败"与"这个号已经在了"：前者是 500，
// 后者对 account.CreateBootstrap 是"创建入口已关闭"（幂等语义）。
var ErrUserExists = errors.New("store: 用户已存在")

// ErrUserNotFound 目标行不存在（更新类操作的判据）。
var ErrUserNotFound = errors.New("store: 用户不存在")

// ── users ───────────────────────────────────────────────────────────────

// CountUsers users 表行数。
//
// account.Store.Count 与 CreateBootstrap 的"库空不空"判据都靠它。
func CountUsers() (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var n int64
	if err := DB.Model(&User{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 数 users: %w", err)
	}
	return n, nil
}

// GetUserByID 按主键取一行；不存在返回 (nil, nil)。
//
// # 为什么"不存在"不是错误
//
// 调用点（account.Store.ByID / Snapshot / SessionStore.Lookup）都要把
// "查不到"当成一个**正常分支**（会话作废、界面显示未登录）。返回
// gorm.ErrRecordNotFound 会逼每个调用点写一遍 errors.Is，漏一处就把
// "账号没了"变成 500。
func GetUserByID(id string) (*User, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	var u User
	err := DB.Where("id = ?", id).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读 user %q: %w", id, err)
	}
	return &u, nil
}

// GetUserByUsername 按用户名取一行（**ASCII 大小写不敏感**）；不存在返回 (nil, nil)。
//
// # 为什么用 COLLATE NOCASE 而不是让调用方先小写
//
// account 的比较口径是 normalizeUsername（转小写 + 去空白），即 Alice 与 alice
// 必须是同一个人。若这里按 username = ? 精确比，调用方传小写名就查不到库里
// 存的 Alice —— 那正是 account.store.go 注释里警告过的"注册时按小写判重、
// 登录时按原样查不到"。库里存的仍是用户输入的原样（界面显示不被改写），
// 只在**比较**时折叠大小写。
//
// 用户名允许的字符集是 ASCII 白名单（account.validateUsername），所以 SQLite 的
// NOCASE 对这些字符是完备的；Unicode 大小写折叠不在契约内。
func GetUserByUsername(username string) (*User, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	name := strings.TrimSpace(username)
	if name == "" {
		return nil, nil
	}
	var u User
	err := DB.Where("username = ? COLLATE NOCASE", name).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读 user %q: %w", name, err)
	}
	return &u, nil
}

// ListUsers 列出全部行，按 (created_at, id) 升序。
//
// 排序与 account.Store.List 的展示顺序一致；加 id 是为了让同一秒创建的多行也有
// 确定顺序（否则两次读的顺序可能不同，测试与导出都会飘）。
func ListUsers() ([]User, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var rows []User
	if err := DB.Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: 列 users: %w", err)
	}
	return rows, nil
}

// CreateUser 插入一行。主键或用户名冲突 → ErrUserExists。
//
// # 为什么不用 OnConflict{DoNothing} 把冲突吞掉
//
// 建号是一条**身份创建**路径：冲突时必须让调用方知道"没建成"，而不是让它以为
// 建好了、返回一个库里根本不由它写入的账号。
func CreateUser(u *User) error {
	if DB == nil {
		return ErrNotOpen
	}
	if u == nil {
		return fmt.Errorf("store: 建 user: 传入 nil")
	}
	if err := DB.Create(u).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: id=%q username=%q", ErrUserExists, u.ID, u.Username)
		}
		return fmt.Errorf("store: 建 user %q: %w", u.ID, err)
	}
	return nil
}

// SaveUser 按主键整行更新（不存在 → ErrUserNotFound）。
//
// # 为什么不做"只更新变化列"
//
// 调用方（改口令 / 升版本 / 启用禁用）都是"读出来、改一个字段、写回去"。只更新
// 部分列需要调用方自己维护"哪些列改了"，而漏一列的后果是静默丢改动 —— 这正是
// account.saveLocked 当年那个"磁盘没写、内存生效"的翻版。整行写把这件事变成
// 一条 UPDATE，语义就是"库里这一行 = 我手上这一行"。
func SaveUser(u *User) error {
	if DB == nil {
		return ErrNotOpen
	}
	if u == nil || strings.TrimSpace(u.ID) == "" {
		return fmt.Errorf("%w: id 为空", ErrUserNotFound)
	}
	res := DB.Model(&User{}).Where("id = ?", u.ID).Select(
		"username", "display_name", "password_hash", "role", "enabled",
		"auth_version", "created_at", "last_login_at", "last_login_ip",
	).Updates(u)
	if res.Error != nil {
		return fmt.Errorf("store: 更新 user %q: %w", u.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		// 行不存在。
		return fmt.Errorf("%w: id=%q", ErrUserNotFound, u.ID)
	}
	return nil
}

// TouchUserLogin 只更新最近登录时间与来源 IP。
//
// 与 SaveUser 分开是因为它是**高频且不影响身份**的写：整行更新会把
// password_hash / auth_version 一起带上，万一调用方手上那份是旧的，就等于用旧
// 口令哈希覆盖新口令（用户报障过的那类"改完口令又变回去"）。
func TouchUserLogin(id string, at int64, ip string) error {
	if DB == nil {
		return ErrNotOpen
	}
	res := DB.Model(&User{}).Where("id = ?", id).
		Updates(map[string]any{"last_login_at": at, "last_login_ip": strings.TrimSpace(ip)})
	if res.Error != nil {
		return fmt.Errorf("store: 更新 user 登录时间 %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%q", ErrUserNotFound, id)
	}
	return nil
}

// ImportUsersIfAbsent 首启导入：**只补缺、不覆盖、不删除**，返回新插入的行数。
//
// 见文件头「与 cmd/migrate-sql 的关系」：与迁移工具的 UpdateAll 语义**刻意不同**。
// 一条 INSERT ... ON CONFLICT DO NOTHING 一行，RowsAffected 累加就是"补了几行"，
// 与 internal/store/option.go 的 ImportFromConfigJSON 同一手法（也让两个进程同时
// 首启时的竞争收敛成"库自己判冲突"，不依赖先 SELECT 再 INSERT）。
//
// 整批放在一个事务里：导入到一半失败时不会留下"一半账号"的库。
func ImportUsersIfAbsent(rows []User) (int, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if len(rows) == 0 {
		return 0, nil
	}
	// 顺序稳定：同一份文件重复导入时的插入顺序一致，排查时 diff 干净。
	sorted := make([]User, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	imported := 0
	err := DB.Transaction(func(tx *gorm.DB) error {
		for i := range sorted {
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&sorted[i])
			if res.Error != nil {
				return fmt.Errorf("store: 导入 user %q: %w", sorted[i].ID, res.Error)
			}
			imported += int(res.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return imported, nil
}

// isUniqueViolation 判断一个错误是不是唯一约束冲突。
//
// # 为什么靠错误文本
//
// GORM 的 ErrDuplicatedKey 需要 gorm.Config{TranslateError: true}，而那是 db.go 的
// gormConfig() —— 本次改造明令不得改 db.go（6 个切片共享它）。glebarez/sqlite 驱动
// 给出的文本里含 UNIQUE constraint failed，所以这里退化成文本判据，并**只**用于把
// 冲突翻译成 ErrUserExists；其它错误原样上抛（不会被这段判据吞掉）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

// ── user_sessions ───────────────────────────────────────────────────────
//
// ⚠ 这一半是给 internal/account 的 SessionStore 接线准备的仓储 API。接线本身要改
// internal/account/session.go（本次任务写范围之外，见 T04 回报），所以此刻**没有
// 生产调用点**。放在这里而不是等下一个任务新建文件，是因为 users 与 user_sessions
// 是同一张身份链（见文件头），分开写必然漂移。

// CreateUserSession 插入一条会话。主键冲突 → ErrUserExists。
func CreateUserSession(s *UserSession) error {
	if DB == nil {
		return ErrNotOpen
	}
	if s == nil {
		return fmt.Errorf("store: 建 session: 传入 nil")
	}
	if err := DB.Create(s).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: sid=%q", ErrUserExists, s.ID)
		}
		return fmt.Errorf("store: 建 session %q: %w", s.ID, err)
	}
	return nil
}

// GetUserSessionByID 按 sid 取一条；不存在返回 (nil, nil)。
func GetUserSessionByID(id string) (*UserSession, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	var s UserSession
	err := DB.Where("id = ?", id).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读 session %q: %w", id, err)
	}
	return &s, nil
}

// ListUserSessionsByUser 列出某账号的全部会话行（按 last_active_at 倒序）。
//
// 不做"是否过期"的过滤：过期判据（滑动窗口 + 绝对上限）活在 account 层，本层多
// 一个判据就多一处会与它分叉的地方。
func ListUserSessionsByUser(userID string) ([]UserSession, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var rows []UserSession
	if err := DB.Where("user_id = ?", userID).Order("last_active_at desc, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("store: 列 sessions(user=%q): %w", userID, err)
	}
	return rows, nil
}

// SaveUserSession 按主键整行更新（不存在 → ErrUserNotFound）。
func SaveUserSession(s *UserSession) error {
	if DB == nil {
		return ErrNotOpen
	}
	if s == nil || strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("%w: sid 为空", ErrUserNotFound)
	}
	res := DB.Model(&UserSession{}).Where("id = ?", s.ID).Select(
		"user_id", "auth_version", "created_at", "last_active_at",
		"expires_at", "ip", "user_agent", "status",
	).Updates(s)
	if res.Error != nil {
		return fmt.Errorf("store: 更新 session %q: %w", s.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: sid=%q", ErrUserNotFound, s.ID)
	}
	return nil
}

// TouchUserSession 只更新滑动续期的两个时间列（理由同 TouchUserLogin）。
func TouchUserSession(id string, lastActiveAt, expiresAt int64) error {
	if DB == nil {
		return ErrNotOpen
	}
	res := DB.Model(&UserSession{}).Where("id = ?", id).
		Updates(map[string]any{"last_active_at": lastActiveAt, "expires_at": expiresAt})
	if res.Error != nil {
		return fmt.Errorf("store: 续期 session %q: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: sid=%q", ErrUserNotFound, id)
	}
	return nil
}

// DeleteUserSession 删一条，返回删除行数（幂等：删不存在的返回 0, nil）。
func DeleteUserSession(id string) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	res := DB.Where("id = ?", id).Delete(&UserSession{})
	if res.Error != nil {
		return 0, fmt.Errorf("store: 删 session %q: %w", id, res.Error)
	}
	return res.RowsAffected, nil
}

// DeleteUserSessionsExcept 删掉某账号除 keepID 之外的全部会话（"退出其它设备"）。
func DeleteUserSessionsExcept(userID, keepID string) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	q := DB.Where("user_id = ?", userID)
	if strings.TrimSpace(keepID) != "" {
		q = q.Where("id <> ?", keepID)
	}
	res := q.Delete(&UserSession{})
	if res.Error != nil {
		return 0, fmt.Errorf("store: 删 sessions(user=%q): %w", userID, res.Error)
	}
	return res.RowsAffected, nil
}

// DeleteExpiredUserSessions 删掉已过期的会话（滑动窗口），返回删除行数。
//
// ⚠ 本仓契约里 user_sessions **没有** hard_expires_at 列（models.go 的 9 列），
// 所以这里只能按 expires_at 判；绝对上限的判据留在 account 层（它手上有
// HardExpiresAt）。这条缺口已记在 cmd/migrate-sql/main.go:106-120。
func DeleteExpiredUserSessions(now int64) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	res := DB.Where("expires_at <= ?", now).Delete(&UserSession{})
	if res.Error != nil {
		return 0, fmt.Errorf("store: 清理过期 sessions: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// CountUserSessions 会话总行数（自检与测试用）。
func CountUserSessions() (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var n int64
	if err := DB.Model(&UserSession{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 数 user_sessions: %w", err)
	}
	return n, nil
}

// ImportUserSessionsIfAbsent 首启导入会话：**只补缺、不覆盖、不删除**，返回新插入的行数。
//
// 语义与 ImportUsersIfAbsent 完全同源（逐行 ON CONFLICT DO NOTHING、事务内、按 ID 排序
// 保证插入顺序稳定），分开只是为了让调用方的意图在名字上就看得见。
//
// ⚠ 导入**不筛过期**：调用方（account.NewSessionStore）负责把"盘上有几行就补几行"
// 这条判据做对，过期清理是 Cleanup 的事。
func ImportUserSessionsIfAbsent(rows []UserSession) (int, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if len(rows) == 0 {
		return 0, nil
	}
	sorted := make([]UserSession, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	imported := 0
	err := DB.Transaction(func(tx *gorm.DB) error {
		for i := range sorted {
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&sorted[i])
			if res.Error != nil {
				return fmt.Errorf("store: 导入 session %q: %w", sorted[i].ID, res.Error)
			}
			imported += int(res.RowsAffected)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return imported, nil
}

// DeleteHardExpiredUserSessions 删掉"超过绝对上限"的会话，返回删除行数。
//
// # 为什么参数是"签发时刻的上界"而不是 now
//
// user_sessions **没有** hard_expires_at 列（models.go 的 9 列契约，缺口记在
// cmd/migrate-sql/main.go:106-120）。绝对上限在 account 层是
// created_at + SessionMaxLifetime，所以这里只能按 created_at 反推：
// 调用方传 now - 绝对上限秒数，本函数删掉 created_at <= 该值的行。
//
// ⚠ created_at > 0 是刻意的：契约里 created_at=0 表示"没有时间"，
// 对应 HardExpiresAt=0（无绝对上限，见 account.expiredAt）。若把 0 也算进来，
// 一条没有时间的行会被这条清理无条件删掉 —— 与内存侧判据不一致。
func DeleteHardExpiredUserSessions(createdBefore int64) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	res := DB.Where("created_at > 0 AND created_at <= ?", createdBefore).Delete(&UserSession{})
	if res.Error != nil {
		return 0, fmt.Errorf("store: 清理超绝对上限的 sessions: %w", res.Error)
	}
	return res.RowsAffected, nil
}
