package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/store"
)

// ErrDisabled 账号已被管理员禁用。
//
// ⚠ 与 ErrCredentials 分开是刻意的取舍：这会告诉调用方"这个用户名**存在**
// 但被禁了"，属于轻微的信息泄露。换来的是被禁用户知道该去找管理员，
// 而不是反复试口令。new-api 同样如此（AUTH_USER_DISABLED / "User has
// been banned" 直接返回）。开放注册的部署里，用户名本来就能通过注册接口
// 探测出来，所以这个泄露面等于零。
var ErrDisabled = errors.New("account: 账号已被禁用")

// Store 账号的进程内索引 + 持久化后端。

// # 2026-10-26（T04）：后端由 JSON 换成 internal/store
//
// 裁定见 PLAN-hyxapi-alignment.md 6.0（数据层真相裁定）：
//
//	读路径   store.DB 打开时，SQL 是唯一权威
//	写路径   SQL；切换之后 JSON **不得再被应用写回**
//	JSON     仅用于 ① 首次启动导入（只导入不删除）② 导出备份 ③ 人工排查
//
// # 兼容期：双模而不是双写
//
// store.DB 是包级句柄（store/db.go:50），T11 之前没有任何进程内代码打开它
// （cmd/server 对 internal/store 零引用）。所以本 Store 是双模的：
//
//	store.DB == nil   → JSON 后端：与改造前逐字节同行为（本包测试也走这条）
//	store.DB != nil   → SQL 后端：启动时把 accounts.json **只补缺**地导入一次
//
// # 为什么不做"两边都写"
//
// 双写会在任一次写入失败时让两个真相源分叉，而分叉的表现是"界面上改了口令、
// 重启后回到旧口令"这类最难查的现象。所以只有一个权威：DB 打开就是 SQL，
// 没打开才是 JSON（见 sqlBackend）。
//
// # 公开方法签名一个都不动
//
// 这是本次改造的纪律（PLAN 4/T04）：调用方（cmd/server、internal/admin、
// internal/server、internal/account/http.go）一行都不用改。
//
// # 并发
//
// 一把全局锁（同改造前）。SQL 路径下这把锁仍然必要：改口令是读-改-写，
// 同进程内必须串行。跨进程写者只有 migrate-sql，而它按纪律在服务停掉后跑。
type Store struct {
	path string

	mu sync.Mutex
	// users 以 normalizeUsername(username) 为键：唯一性与查找用**同一处**
	// 归一化，不会出现"注册时按小写判重、登录时按原样查不到"这种不一致。
	//
	// ⚠ 用户名不支持改名（new-api 支持），所以键不会失效。
	//
	// SQL 模式下它只是 accounts.json 的**快照**（导入源），导入完成后不再被读
	// 也不被写；JSON 模式下它才是权威。
	users map[string]*User
	// imported 记录首启导入是否已成功完成（幂等标记，只增不减）。
	imported bool
}

// New 加载（或初始化）路径上的账号库。目录不存在时自动创建。
//
// 解析失败返回错误、**不静默重建**：账号文件损坏时把库清空，
// 表现是"所有人突然都登录不进去、而且原来的账号也没了"，比直接报错
// 难查得多（与 apikey.New 同一条取舍）。
//
// store.DB 已打开时，本函数顺带完成**首启导入**（只补缺、不覆盖、不删除），
// 导入失败**直接返回错误**：那意味着账号没能并入库，此时放行启动等于
// "所有人都登不进去"。
func New(path string) (*Store, error) {
	s := &Store{path: path, users: make(map[string]*User)}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行。JSON 权威期落一个空文件，让文件位置可见（用户备份时
			// 找得到）；SQL 权威期**不写** JSON —— 6.0 明说切换后 JSON 不得
			// 再被应用写回，而这里写一个空文件正是"应用写回"。
			if store.DB == nil {
				if err := s.saveLocked(); err != nil {
					return nil, err
				}
			}
			if err := s.activateSQL(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, fmt.Errorf("读取账号库: %w", err)
	}
	if len(raw) == 0 {
		if err := s.activateSQL(); err != nil {
			return nil, err
		}
		return s, nil
	}
	var arr []*User
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("解析账号库: %w", err)
	}
	for _, u := range arr {
		if u == nil || u.ID == "" || strings.TrimSpace(u.Username) == "" {
			continue // 跳过残缺行，不让一条坏记录卡住整个库
		}
		// 兼容手写的账号文件：角色/状态缺省时给**最小**权限，而不是给管理员。
		//
		// ⚠ 2026-10-19 单账号口径：这里落到 RoleMember 是**"认出来再拒掉"**，
		// 不是"给他一个成员身份"。所以这条分支的作用是：让畸形/旧记录
		// **可被准确命名**，且永远不会被当成管理员放进来。反过来（缺省给
		// 管理员）等于让一个手写坏了的文件凭空造出管理员。
		if !ValidRole(u.Role) {
			u.Role = RoleMember
		}
		if u.AuthVersion <= 0 {
			u.AuthVersion = 1
		}
		s.users[normalizeUsername(u.Username)] = u
	}

	// 非管理员的历史记录（改造前建出来的成员，或角色字段缺失/手写成别的值）。
	// 只在**启动时**记一条日志：这类记录无法登录（Authenticate 拒），但运维
	// 需要知道盘上有它们。刻意不逐条打、也不在每次登录尝试时重复打。
	if retired := retiredUsernames(s.users); len(retired) > 0 {
		log.Printf("account: 账号库 %s 里有 %d 条**非管理员**历史记录（%s）：单账号管理员模式下"+
			"它们不再构成身份，登录会被拒并提示「成员账号已取消，请用管理员账号」；"+
			"要彻底清掉可先备份、再删掉这些行。",
			s.path, len(retired), strings.Join(retired, ", "))
	}
	if err := s.activateSQL(); err != nil {
		return nil, err
	}
	return s, nil
}

// activateSQL 在 store.DB 已打开时完成一次性导入（幂等）。DB 没打开时什么都不做。
//
// 单独抽出来是因为 New 有三条返回路径（文件不存在 / 空文件 / 正常解析），
// 三条都必须走同一次导入。
func (s *Store) activateSQL() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if store.DB == nil {
		return nil
	}
	return s.importLocked()
}

// sqlBackend 报告本次调用该走哪个后端（调用方必须持锁）。
//
// store.DB 一旦打开就不再回头：导入失败**不**回落 JSON —— 回落会让写路径
// 重新写 accounts.json，而 6.0 明说切换后 JSON 不得再被应用写回。
// 失败只记日志，错在库上就地暴露（读失败 → 零值 + 日志；写失败 → 返回错误）。
func (s *Store) sqlBackend() bool {
	if store.DB == nil {
		return false
	}
	if !s.imported {
		if err := s.importLocked(); err != nil {
			log.Printf("[account] ⚠ 账号首启导入失败（仍按 SQL 后端工作，下次调用重试）: %v", err)
		}
	}
	return true
}

// importLocked 把 accounts.json 的账号**只补缺**地写进 users 表（调用方持锁）。
//
// # 为什么不是"表空才导入"
//
// 因为 setups 流程可能已经往 users 里写过一个 root 行（store.InitializeSetup）。
// 用"表空"当判据时，那种库里 accounts.json 的账号会**整批不导入**；
// 逐行补缺（INSERT ... ON CONFLICT DO NOTHING）对两种顺序都成立，且天然幂等。
func (s *Store) importLocked() error {
	if s.imported {
		return nil
	}
	rows := make([]store.User, 0, len(s.users))
	for _, u := range s.users {
		su, ok := storeUserFrom(u)
		if !ok {
			continue // created_at 不可解析：与 migrate-sql 同判据，跳过该行
		}
		rows = append(rows, su)
	}
	n, err := store.ImportUsersIfAbsent(rows)
	if err != nil {
		return fmt.Errorf("首启导入账号库: %w", err)
	}
	s.imported = true
	if n > 0 {
		log.Printf("account: 首启导入 %d 个账号 → users 表（只导入不删除，%s 原样保留）", n, s.path)
	}
	return nil
}

// retiredUsernames 列出库里的非管理员记录（仅用于启动日志）。
//
// 判据用 IsAdminRole（**唯一**一份"什么算能构成身份的角色"）。
func retiredUsernames(users map[string]*User) []string {
	out := make([]string, 0)
	for _, u := range users {
		if !IsAdminRole(u.Role) {
			out = append(out, fmt.Sprintf("%s(%s)", u.Username, RoleName(u.Role)))
		}
	}
	sort.Strings(out)
	return out
}

// saveLocked 原子写盘（临时文件 + rename）。调用方必须持锁。
//
// 权限 0600：文件里有 bcrypt 哈希。它不是明文口令，但仍不该给同机其它用户
// 一个离线爆破的副本（与 apikeys.json 同权限）。
//
// ⚠ SQL 权威期不许再调用它：那会把内存态写回 JSON，形成第二个真相源
// （6.0）。这里用一条显式 guard 把误用变成**响亮的错误**而不是静默写回。
func (s *Store) saveLocked() error {
	if store.DB != nil {
		return errors.New("account: SQL 权威期不允许写回 accounts.json（PLAN 6.0）")
	}
	arr := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		arr = append(arr, u)
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].CreatedAt < arr[j].CreatedAt })
	raw, err := json.MarshalIndent(arr, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// saveOrLog 落盘并**记录失败**，不把错误静默丢掉（2026-10-20，review 抓到）。
//
// 历史缺陷：原来 7 处调用一律 `_ = s.saveLocked()`，而 saveLocked 真的会失败
// （磁盘满、权限不足、Windows 下文件被占用）。吞掉错误的后果最严重的一处是
// **安全问题**：Revoke(sid) → 内存里删了 session，但磁盘没写 ⇒ 进程重启后
// 从磁盘恢复 ⇒ 刚被注销的 session 复活了。
//
// 这类修复的价值不在于"错误变少了"，而在于**失败不再无声**。
func (s *Store) saveOrLog(scene string) {
	if err := s.saveLocked(); err != nil {
		log.Printf("[account] ⚠ 落盘失败（%s）: %v —— 内存已生效但磁盘未写入，重启会丢失这次修改", scene, err)
	}
}

// copyUser 返回一份副本。
//
// ⚠ 所有读方法都必须返回副本，不能返回内部指针：调用方拿到指针后会在锁外
// 读取（拼响应、写日志），而另一个 goroutine 可能正在改这个账号。那是典型的
// data race —— 真出问题时的表现是"读到写了一半的结构"，比崩溃更难查。
func copyUser(u *User) *User {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}

// ── account.User ⇄ store.User 换算 ────────────────────────────────────────
//
// 判据与 cmd/migrate-sql/main.go:340 的 accountToUser **逐条一致**（两处实现、
// 同一份 accounts.json 必须得到同一个库）：
//
//	created_at 不可解析   → 跳过该行（导出排序键坏了，逐字节等价就无从谈起）
//	last_login_at 不可解析 → 归零保留该行（审计信息，不该让账号整体丢失）
//	角色数值原样搬运      → 不做 10↔100 映射（RoleRoot=100 是 setups 自愈判据，
//	                        见 store/role.go:31；本仓 account 侧没有 root 档）

// storeUserFrom 把账号投影成 users 表的一行；ok=false 表示该行应当跳过。
func storeUserFrom(u *User) (store.User, bool) {
	created, err := parseStamp(u.CreatedAt)
	if err != nil {
		log.Printf("account: 跳过账号 %s（%s）：created_at 无法解析 %q（%v）",
			u.ID, u.Username, u.CreatedAt, err)
		return store.User{}, false
	}
	last, err := parseStamp(u.LastLoginAt)
	if err != nil {
		last = 0 // 见上方判据：登录时间坏掉不该让账号迁不过去
	}
	return store.User{
		ID:           u.ID,
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		PasswordHash: u.PasswordHash,
		Role:         u.Role,
		Enabled:      u.Enabled,
		AuthVersion:  u.AuthVersion,
		CreatedAt:    created,
		LastLoginAt:  last,
		LastLoginIP:  u.LastLoginIP,
	}, true
}

// mapStoreRoleToAccountRole 是 SQL → account 的**唯一**角色映射处。
//
// # 为什么需要它
//
// store.RoleRoot(100) 是 setups 自愈判据用的超管档（internal/store/role.go:31），
// 而 account 侧只有 RoleAdmin(10) 能构成身份（IsAdminRole 是唯一判据）。不映射
// 的话，库里任何 role=100 的行都会被 Authenticate/Lookup 判成 ErrMemberRetired
// —— 一个真实存在的管理员被锁在门外。
//
// # 为什么只映射读方向
//
// 写方向**不动**：CreateBootstrap 仍写 RoleAdmin(10)，所以 migrate-sql export
// 导出的 accounts.json 里 role 数值与改造前一致（导出是逐字段搬运，见
// cmd/migrate-sql/main.go:738）。反过来（写 100）会让导出值与旧数据不同，且
// JSON 后端认不出 100 —— 那是把「JSON 仅作导出格式」这条裁定打破。
//
// ⚠ 全仓只有这一处做角色映射；新增转换点时必须调它，而不是各写一遍 if。
func mapStoreRoleToAccountRole(role int) int {
	if role == store.RoleRoot {
		return RoleAdmin
	}
	return role
}

// accountFromStoreUser 反向投影。时间戳 0 → 空串（与 omitempty 的语义一致）。
func accountFromStoreUser(su *store.User) *User {
	if su == nil {
		return nil
	}
	return &User{
		ID:           su.ID,
		Username:     su.Username,
		DisplayName:  su.DisplayName,
		PasswordHash: su.PasswordHash,
		Role:         mapStoreRoleToAccountRole(su.Role),
		Enabled:      su.Enabled,
		AuthVersion:  su.AuthVersion,
		CreatedAt:    formatStamp(su.CreatedAt),
		LastLoginAt:  formatStamp(su.LastLoginAt),
		LastLoginIP:  su.LastLoginIP,
	}
}

// parseStamp 把盘上的 RFC3339 串转成 unix 秒；空串 → 0（=「没有这个时间」）。
//
// 出处：cmd/migrate-sql/main.go:857 的 parseRFC3339（判据必须一致）。
func parseStamp(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// formatStamp 反向转换。**必须**用 UTC：盘上的账号文件是 UTC（Z 结尾），
// 用本地时区格式化会得到 +08:00 后缀，导出字节立刻不等。
//
// 出处：cmd/migrate-sql/main.go:875 的 formatRFC3339。
func formatStamp(n int64) string {
	if n <= 0 {
		return ""
	}
	return time.Unix(n, 0).UTC().Format(time.RFC3339)
}

// noteReadFail 读路径失败时的统一处置：**零值 + 一行日志**。
//
// 这些方法的签名不能返回 error（公开签名冻结），而"读不到"必须往安全的
// 方向倒：返回"没有这个账号"而不是 panic 或返回半截数据。
// 日志是这条路上唯一的信号 —— 没有它，库坏了的表现就是"界面显示未登录"。
func (s *Store) noteReadFail(scene string, err error) {
	log.Printf("[account] ⚠ SQL 读失败（%s）: %v —— 本次按「查不到」处理，请检查数据库", scene, err)
}

// Count 账号总数（首启判定"要不要显示创建管理员"用）。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlBackend() {
		n, err := store.CountUsers()
		if err != nil {
			s.noteReadFail("Count", err)
			return 0
		}
		return int(n)
	}
	return len(s.users)
}

// ⚠ 2026-10-19：Create(username, password, role) **已删除**。
//
// 它是"角色分裂"的残留：一个带 role 参数的建号口子，注册路径传 RoleMember。
// 单账号口径下它有两个不可接受之处 ——
//
//	① 它是一条**能建出非管理员账号**的代码路径（要求 1 明说不许有）；
//	② 它的存在让"库非空之后还能建号"成为可能，而那正是要求 2 要关掉的门。
//
// 现在**唯一**的建号路径是 CreateBootstrap（见下）。测试要造第二个账号时
// 不再有 API 可用 —— 那是对的：要造历史数据就直接写 accounts.json。

// CreateBootstrap 建**首个**账号，并把它建成管理员。
//
// # 为什么必须是一个原子操作（而不是"先 Count()==0 再 Create(admin)"）
//
// 两步写法有一个真实竞态：两个请求同时看到 Count()==0，于是**两个都**成为
// 管理员。首启的窗口（刚部署、还没建号）恰好是最可能在公网被扫到的时刻。
//
// SQL 后端的实现把"检查空 + 建号"放在同一把进程内锁里（本仓单进程），
// 并用 users.username 的 UNIQUE 兜底。
func (s *Store) CreateBootstrap(username, password string) (*User, error) {
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(username)
	key := normalizeUsername(name)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sqlBackend() {
		n, err := store.CountUsers()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, ErrBootstrapDone
		}
		u := &User{
			ID:           newID(),
			Username:     name,
			DisplayName:  name,
			PasswordHash: hash,
			Role:         RoleAdmin,
			Enabled:      true,
			AuthVersion:  1,
			CreatedAt:    nowStamp(),
		}
		su, ok := storeUserFrom(u)
		if !ok {
			return nil, fmt.Errorf("%w：创建时间无法写入（%q）", ErrBadUsername, u.CreatedAt)
		}
		if err := store.CreateUser(&su); err != nil {
			return nil, err
		}
		// 首启建号成功 ⇒ **显式**写下 setups 行（「初始化完成」的唯一标记）。
		//
		// ⚠ 刻意不把首号写成 store.RoleRoot(100) 再靠 CheckSetup 自愈：
		// migrate-sql export 会把 role 原样导回 accounts.json，而 JSON 后端的
		// IsAdminRole(100)=false ⇒ 回退到 JSON 模式时管理员会失效。所以库里仍是
		// RoleAdmin(10)（导出值不变），标记单独写（store.MarkSetupInitialized）。
		//
		// 失败**不**让建号失败：账号已经建成（这是成功的事实），把它报成错误只会
		// 让用户重试并撞上 ErrBootstrapDone。缺标记的后果有限（控制台的 setup 判定
		// 用的是「账号数为 0」，不读 setups 行），但仍要留下明确的错误日志。
		if err := store.MarkSetupInitialized(); err != nil {
			log.Printf("[account] ⚠ setups 标记未写下（账号已建，控制台不受影响）: %v", err)
		}
		return copyUser(u), nil
	}

	if len(s.users) > 0 {
		return nil, ErrBootstrapDone
	}
	u := &User{
		ID:           newID(),
		Username:     name,
		DisplayName:  name,
		PasswordHash: hash,
		Role:         RoleAdmin,
		Enabled:      true,
		AuthVersion:  1,
		CreatedAt:    nowStamp(),
	}
	s.users[key] = u
	if err := s.saveLocked(); err != nil {
		delete(s.users, key)
		return nil, err
	}
	return copyUser(u), nil
}

// ByUsername 按用户名取账号（含 PasswordHash，供登录校验）。
//
// 返回副本；调用方**不得**把它直接序列化给前端（用 Snapshot/List）。
func (s *Store) ByUsername(username string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlBackend() {
		su, err := store.GetUserByUsername(username)
		if err != nil {
			s.noteReadFail("ByUsername", err)
			return nil
		}
		return accountFromStoreUser(su)
	}
	return copyUser(s.users[normalizeUsername(username)])
}

// ByID 按 ID 取账号（含 PasswordHash）。
func (s *Store) ByID(id string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlBackend() {
		su, err := store.GetUserByID(id)
		if err != nil {
			s.noteReadFail("ByID", err)
			return nil
		}
		return accountFromStoreUser(su)
	}
	for _, u := range s.users {
		if u.ID == id {
			return copyUser(u)
		}
	}
	return nil
}

// Snapshot 取脱敏视图（对外用）。
func (s *Store) Snapshot(id string) (View, bool) {
	u := s.ByID(id)
	if u == nil {
		return View{}, false
	}
	return u.view(), true
}

// List 列出全部账号的脱敏视图（按创建时间升序）。
func (s *Store) List() []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlBackend() {
		rows, err := store.ListUsers()
		if err != nil {
			s.noteReadFail("List", err)
			return nil
		}
		out := make([]View, 0, len(rows))
		for i := range rows {
			out = append(out, accountFromStoreUser(&rows[i]).view())
		}
		return out
	}
	out := make([]View, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// Authenticate 校验用户名 + 口令。
//
// 成功返回账号副本（含 Role/AuthVersion，供签发会话用）。
// 失败一律 ErrCredentials；账号被禁用返回 ErrDisabled；账号不存在时
// **也走一遍 bcrypt**（对着占位哈希）以对齐耗时，见 VerifyPassword 的注释。
//
// # ⚠ 2026-10-19 单账号口径：非管理员一律拒（ErrMemberRetired）
//
// 判据是 IsAdminRole（唯一一份）。顺序刻意排成"**先验口令，再看角色**"：
// 角色判定若排在口令之前，"这个用户名存在吗"就有了一个**不需要口令**的探测
// 信号 ⇒ 用户名枚举。排在之后则拿不到口令的人区分不出来，而口令正确的本人会
// 拿到那句明确文案。
//
// 拒绝时**不**返回账号（连 Enabled 的答案都不给）。
//
// ⚠ SQL 后端的角色数值是**原样搬运**的（见 storeUserFrom 的注释）：若库里
// 有 store.RoleRoot=100 的行（InitializeSetup 建的超管），本判据**不认它**。
// 这是跨切片契约问题，已在 T04 回报里列为未决问题（不是本文件能单方面决定的）。
func (s *Store) Authenticate(username, password string) (*User, error) {
	u := s.ByUsername(username)
	if u == nil {
		// 占位校验：只为耗时对齐，结果必然为 false。
		_ = VerifyPassword(DummyHash(), password)
		return nil, ErrCredentials
	}
	if !VerifyPassword(u.PasswordHash, password) {
		return nil, ErrCredentials
	}
	if !u.Enabled {
		return nil, ErrDisabled
	}
	if !IsAdminRole(u.Role) {
		// 成员的旧记录（或手写成别的角色值的记录）：口令对也没用。
		return nil, ErrMemberRetired
	}
	return u, nil
}

// SetPassword 改口令，并把 AuthVersion +1（所有旧会话随之失效）。
//
// 返回新的 AuthVersion：调用方要把**当前这条会话**就地升到同一个值，
// 否则改口令的人自己会被踢下线（用户报障过的那类"改完必须重启"）。
func (s *Store) SetPassword(id, password string) (int64, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlBackend() {
		su, err := store.GetUserByID(id)
		if err != nil {
			return 0, err
		}
		if su == nil {
			return 0, ErrNotFound
		}
		su.PasswordHash = hash
		su.AuthVersion++
		if err := store.SaveUser(su); err != nil {
			return 0, mapStoreUserErr(err)
		}
		return su.AuthVersion, nil
	}
	u := s.findLocked(id)
	if u == nil {
		return 0, ErrNotFound
	}
	u.PasswordHash = hash
	u.AuthVersion++
	if err := s.saveLocked(); err != nil {
		return 0, err
	}
	return u.AuthVersion, nil
}

// BumpAuthVersion 只提升版本号（"退出所有设备"/强制下线），不连带改口令。
func (s *Store) BumpAuthVersion(id string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlBackend() {
		su, err := store.GetUserByID(id)
		if err != nil {
			return 0, err
		}
		if su == nil {
			return 0, ErrNotFound
		}
		su.AuthVersion++
		if err := store.SaveUser(su); err != nil {
			return 0, mapStoreUserErr(err)
		}
		return su.AuthVersion, nil
	}
	u := s.findLocked(id)
	if u == nil {
		return 0, ErrNotFound
	}
	u.AuthVersion++
	if err := s.saveLocked(); err != nil {
		return 0, err
	}
	return u.AuthVersion, nil
}

// mapStoreUserErr 把 store 层的"行不存在"翻成 account 的哨兵。
//
// HTTP 层按 ErrNotFound 出 404；若不翻，读-改-写撞上被并发删掉的行时会变成 500。
func mapStoreUserErr(err error) error {
	if errors.Is(err, store.ErrUserNotFound) {
		return ErrNotFound
	}
	return err
}

// ⚠ 2026-10-19：SetRole(id, role) **已删除**。
//
// 理由与 Create 的删除同源，而且更直接：它是**第四条**能造出非管理员账号的
// 路径（管理员被降级成成员 = 盘上多出一条非管理员记录），而单账号口径下
// 角色根本不是可配置项。

// SetEnabled 启用/禁用账号。禁用**不**删号（保留其 key 与历史）。
func (s *Store) SetEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlBackend() {
		su, err := store.GetUserByID(id)
		if err != nil {
			return err
		}
		if su == nil {
			return ErrNotFound
		}
		su.Enabled = enabled
		return mapStoreUserErr(store.SaveUser(su))
	}
	u := s.findLocked(id)
	if u == nil {
		return ErrNotFound
	}
	u.Enabled = enabled
	return s.saveLocked()
}

// TouchLogin 记录最近登录时间与来源 IP（登录成功后调）。
//
// 失败**不**影响登录：这只是审计信息，不该因为它让用户登不进来。
func (s *Store) TouchLogin(id, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlBackend() {
		if err := store.TouchUserLogin(id, time.Now().Unix(), ip); err != nil {
			log.Printf("[account] ⚠ 登录时间未落库（TouchLogin）: %v", err)
		}
		return
	}
	u := s.findLocked(id)
	if u == nil {
		return
	}
	u.LastLoginAt = nowStamp()
	u.LastLoginIP = strings.TrimSpace(ip)
	s.saveOrLog("TouchLogin")
}

// findLocked 按 ID 查内部指针（调用方必须持锁）。仅 JSON 后端使用。
func (s *Store) findLocked(id string) *User {
	for _, u := range s.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}
