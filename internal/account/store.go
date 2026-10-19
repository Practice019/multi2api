package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrDisabled 账号已被管理员禁用。
//
// ⚠ 与 ErrCredentials 分开是刻意的取舍：这会告诉调用方"这个用户名**存在**
// 但被禁了"，属于轻微的信息泄露。换来的是被禁用户知道该去找管理员，
// 而不是反复试口令。new-api 同样如此（`AUTH_USER_DISABLED` / "User has
// been banned" 直接返回）。开放注册的部署里，用户名本来就能通过注册接口
// 探测出来，所以这个泄露面等于零。
var ErrDisabled = errors.New("account: 账号已被禁用")

// Store 账号的进程内权威 + JSON 持久化。
//
// # 为什么用 JSON 文件而不是数据库
//
// 单机控制台，账号是**人**的数量级（几个到几十个）；`data/state.json`、
// `data/apikeys.json` 已经是同款做法（见 internal/apikey 的 saveLocked）。
// 引一个数据库只为存几十行账号，会把"零依赖单二进制"这个部署优势换掉。
//
// # 并发
//
// 一把全局锁。管理端点（建号/改密）与请求路径（查会话）的频率都极低，
// 锁粒度不重要；重要的是**返回值必须是副本** —— 见 copyUser 的注释。
type Store struct {
	path string

	mu sync.Mutex
	// users 以 normalizeUsername(username) 为键：唯一性与查找用**同一处**
	// 归一化，不会出现"注册时按小写判重、登录时按原样查不到"这种不一致。
	//
	// ⚠ 用户名不支持改名（new-api 支持），所以键不会失效。真要改名时
	// 必须同时重建这张表 —— 注释留在这里，免得后来人只改了 Username
	// 字段而忘了键。
	users map[string]*User
}

// New 加载（或初始化）路径上的账号库。目录不存在时自动创建。
//
// 解析失败返回错误、**不静默重建**：账号文件损坏时把库清空，
// 表现是"所有人突然都登录不进去、而且原来的账号也没了"，比直接报错
// 难查得多（与 apikey.New 同一条取舍）。
func New(path string) (*Store, error) {
	s := &Store{path: path, users: make(map[string]*User)}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行：落一个空文件，让文件位置可见（用户备份时找得到）。
			if err := s.saveLocked(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, fmt.Errorf("读取账号库: %w", err)
	}
	if len(raw) == 0 {
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
		// 兼容手写的账号文件：角色/状态缺省时给最小权限，而不是给管理员。
		if !ValidRole(u.Role) {
			u.Role = RoleMember
		}
		if u.AuthVersion <= 0 {
			u.AuthVersion = 1
		}
		s.users[normalizeUsername(u.Username)] = u
	}
	return s, nil
}

// saveLocked 原子写盘（临时文件 + rename）。调用方必须持锁。
//
// 权限 0600：文件里有 bcrypt 哈希。它不是明文口令，但仍不该给同机其它用户
// 一个离线爆破的副本（与 apikeys.json 同权限）。
func (s *Store) saveLocked() error {
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

// copyUser 返回一份副本。
//
// ⚠ 所有读方法都必须返回副本，不能返回内部指针：调用方拿到指针后会在
// 锁外读取（拼响应、写日志），而另一个 goroutine 可能正在改这个账号
// （改密/登录时间）。那是典型的 data race —— `go test -race` 会红，
// 而且真出问题时的表现是"读到写了一半的结构"，比崩溃更难查。
func copyUser(u *User) *User {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}

// Count 账号总数（首启判定"要不要显示创建管理员"用）。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.users)
}

// Create 建号。role 必须是 ValidRole 的值。
//
// ⚠ role 由**服务端**决定：注册接口一律传 RoleAdmin=false 的 RoleMember，
// 绝不把请求体里的 role 透传进来（new-api controller/user.go:283 就是
// 显式写死普通用户，注释还专门写了一句"明确设置角色为普通用户"）。
func (s *Store) Create(username, password string, role int) (*User, error) {
	if !ValidRole(role) {
		return nil, ErrBadRole
	}
	name := strings.TrimSpace(username)
	if err := validateUsername(name); err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	key := normalizeUsername(name)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.users[key]; exists {
		return nil, ErrUsernameTaken
	}
	u := &User{
		ID:           newID(),
		Username:     name,
		DisplayName:  name,
		PasswordHash: hash,
		Role:         role,
		Enabled:      true,
		AuthVersion:  1,
		CreatedAt:    nowStamp(),
	}
	s.users[key] = u
	if err := s.saveLocked(); err != nil {
		// 落盘失败就把内存里的新账号撤回：否则会出现"这个进程里能登录、
		// 重启后账号不存在"的幽灵账号。
		delete(s.users, key)
		return nil, err
	}
	return copyUser(u), nil
}

// CreateBootstrap 建**首个**账号，并把它建成管理员。
//
// # 为什么必须是一个原子操作（而不是"先 Count()==0 再 Create(admin)"）
//
// 两步写法有一个真实竞态：两个请求同时看到 Count()==0，于是**两个都**成为
// 管理员。这不是理论问题 —— 首启的窗口（刚部署、还没建号）恰好是最可能在
// 公网被扫到的时刻，而"多出来的那个管理员"没有任何迹象。
//
// 所以在同一把锁里做"检查空 + 建号"，第二个到达者得到 ErrBootstrapDone。
//
// # 与开放注册的关系
//
// 首启建管理员的**唯一**入口就是这里（对应计划里"首次访问控制台显示
// 创建管理员"）。库非空之后，注册接口一律只建 RoleMember，绝不给管理员 ——
// 否则第一个注册的人之后还能再"注册"出一个管理员。
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
	return copyUser(s.users[normalizeUsername(username)])
}

// ByID 按 ID 取账号（含 PasswordHash）。
func (s *Store) ByID(id string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.ID == id {
			return copyUser(u)
		}
	}
	return nil
}

// Snapshot 取脱敏视图（对外用）。
func (s *Store) Snapshot(id string) (View, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.ID == id {
			return u.view(), true
		}
	}
	return View{}, false
}

// List 列出全部账号的脱敏视图（按创建时间升序）。
func (s *Store) List() []View {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// SetRole 改角色（只有管理员能调到；校验在 HTTP 层）。
func (s *Store) SetRole(id string, role int) error {
	if !ValidRole(role) {
		return ErrBadRole
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.findLocked(id)
	if u == nil {
		return ErrNotFound
	}
	u.Role = role
	return s.saveLocked()
}

// SetEnabled 启用/禁用账号。禁用**不**删号（保留其 key 与历史）。
func (s *Store) SetEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	u := s.findLocked(id)
	if u == nil {
		return
	}
	u.LastLoginAt = nowStamp()
	u.LastLoginIP = strings.TrimSpace(ip)
	_ = s.saveLocked()
}

// findLocked 按 ID 查内部指针（调用方必须持锁）。
func (s *Store) findLocked(id string) *User {
	for _, u := range s.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}
