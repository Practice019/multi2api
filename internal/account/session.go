package account

import (
	"crypto/rand"
	"encoding/hex"
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
)

// CookieName 承载会话 ID 的 Cookie 名。
//
// 属性（HttpOnly / SameSite=Lax / Path=/ / 可选 Secure）在 HTTP 层设置，
// 常量放这里是为了让"名字只有一处"，避免 Set 与 Delete 两侧写岔
// （写岔的表现是登出后 cookie 还在，浏览器继续带着旧 SID）。
const CookieName = "wb2api_sid"

// 会话时限。
//
//	SessionTTL       滑动窗口：每次带会话的请求把它续到 now+TTL。
//	SessionMaxLifetime 绝对上限：滑动续期**不能**无限延长 ——
//	                 否则一个被偷走的 cookie 只要持续被使用就永不失效。
//	                 安全清单要求"会话有过期"。
const (
	SessionTTL         = 24 * time.Hour
	SessionMaxLifetime = 30 * 24 * time.Hour
)

// 会话数量与签发频率上限（API 层的第 5 层反滥用，对应 new-api
// service/auth_session.go:468 的 AUTH_SESSION_ISSUANCE_LIMIT）。
//
// 两条不同的限制，别混：
//
//	MaxSessionsPerUser 同时**在线**几条（超了踢最久没动的），防止
//	                 "用正确口令刷出无限条会话"把内存/文件撑爆。
//	签发频率        一段时间内**新签发**几次（含已被踢掉的），
//	                 防止拿正确口令快速刷会话（每次签发都要落盘）。
const (
	MaxSessionsPerUser = 5
	issuanceWindow     = time.Hour
	maxIssuedPerWindow = 12
)

// 会话错误。HTTP 层按它映射状态码与 code（前端据此决定"跳登录页"还是"提示"）。
var (
	// ErrSessionNotFound 会话不存在（含已被清理）。
	ErrSessionNotFound = errors.New("account: 会话不存在")
	// ErrSessionExpired 会话已过期（滑动窗口或绝对上限到了）。
	ErrSessionExpired = errors.New("account: 会话已过期")
	// ErrSessionRevoked 会话已被撤销（登出/被踢/账号凭据变更）。
	ErrSessionRevoked = errors.New("account: 会话已失效")
	// ErrSessionLimit 会话数量或签发频率超限。
	ErrSessionLimit = errors.New("account: 会话数量或签发过于频繁")
)

// Session 一条浏览器登录态（持久化字段）。
//
// # 时间字段为什么是 unix 秒（而 User 用 RFC3339 字符串）
//
// 会话的过期判定在**每个受保护请求**上都跑一次，滑动续期还要做加减比较。
// 用 int64 秒可以直接比较，不用在热路径上解析 RFC3339；User 的时间是
// 给人看的审计信息，格式化成字符串更合适。两处的取舍不同，故不一致 ——
// 这条注释就是给"为什么同一个包里两种时间形态"留的答案。
type Session struct {
	SID    string `json:"sid"`     // 会话 ID（32 字节随机，十六进制）
	UserID string `json:"user_id"` // 归属账号
	// Username / Role 是签发时的**快照**，只用于日志与界面展示。
	//
	// ⚠ 权限判定**不能**用这里的 Role：账号被降级后，旧会话里的快照还是
	// "管理员"。真正的判据是每次校验时从账号库读的实时 Role（见 Lookup）。
	Username string `json:"username"`
	Role     int    `json:"role"`

	// AuthVersion 签发时账号的凭据版本。与账号当前值不等 ⇒ 会话失效。
	//
	// 这是"改口令/强制下线让所有旧会话一次性失效"的实现：不用遍历会话表，
	// 只要比对版本号。
	AuthVersion int64 `json:"auth_version"`
	// Version 本会话自己的版本，改口令时**就地升级**（AdvanceAuthVersion）用。
	//
	// new-api 用 (user_auth_version, session_version) 两个字段做乐观并发，
	// 我们单进程，只需要"知道自己的版本已升过"这一件事。
	Version int64 `json:"version"`

	CreatedAt    int64 `json:"created_at"`     // unix 秒
	LastActiveAt int64 `json:"last_active_at"` // unix 秒
	ExpiresAt    int64 `json:"expires_at"`     // unix 秒（滑动）
	// HardExpiresAt 绝对上限，签发时定死、续期不改。
	HardExpiresAt int64 `json:"hard_expires_at"`

	IP string `json:"ip,omitempty"`
	UA string `json:"ua,omitempty"`
}

// SessionView 会话的对外形态（"安全设置"里列出登录设备用）。
type SessionView struct {
	SID          string `json:"sid_masked"` // 掩码后的 SID（够识别，不可用于登录）
	CreatedAt    string `json:"created_at"`
	LastActiveAt string `json:"last_active_at"`
	ExpiresAt    string `json:"expires_at"`
	IP           string `json:"ip,omitempty"`
	UA           string `json:"ua,omitempty"`
	Current      bool   `json:"current"`
}

// sessionView 掩码 SID：SID 本身是登录凭据，列表里只给首尾各 4 位。
func sessionView(s *Session, current bool) SessionView {
	masked := s.SID
	if len(masked) > 10 {
		masked = masked[:4] + "…" + masked[len(masked)-4:]
	}
	return SessionView{
		SID:          masked,
		CreatedAt:    stampOf(s.CreatedAt),
		LastActiveAt: stampOf(s.LastActiveAt),
		ExpiresAt:    stampOf(s.ExpiresAt),
		IP:           s.IP,
		UA:           s.UA,
		Current:      current,
	}
}

func stampOf(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// SessionStore 会话的进程内权威 + JSON 持久化。
//
// # 为什么落盘（用户明确不想重启就掉线）
//
// 纯内存的话，进程一重启所有人的浏览器都回到登录页 —— 而"重启"在这台
// 机器上是很常见的动作（换二进制、改配置）。落盘让会话跨重启存活，
// 代价是磁盘上多一份会话文件（权限 0600，只含随机 SID，不含口令）。
type SessionStore struct {
	path string

	mu       sync.Mutex
	sessions map[string]*Session // sid → 会话
	// issued 每账号最近的签发时刻（进程内，不持久化）：签发频率限制用。
	issued map[string][]int64
}

// NewSessionStore 加载（或初始化）会话库。
//
// 启动时**顺手清理**已过期会话：不清理的话文件会随历史会话一直长，
// 而里面每一条都是"曾经有效的登录态"（虽然过期了，仍不该留着）。
func NewSessionStore(path string) (*SessionStore, error) {
	s := &SessionStore{
		path:     path,
		sessions: make(map[string]*Session),
		issued:   make(map[string][]int64),
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := s.saveLocked(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, fmt.Errorf("读取会话库: %w", err)
	}
	if len(raw) == 0 {
		return s, nil
	}
	var arr []*Session
	if err := json.Unmarshal(raw, &arr); err != nil {
		// 会话文件损坏**不**报错中断启动：它是可再生的（用户重新登录即可），
		// 与账号文件（损坏就没人能登录）的处置不同。删掉重建，并留下痕迹。
		fmt.Fprintf(os.Stderr, "警告：会话库解析失败，已重建（用户需重新登录）: %v\n", err)
		return s, s.saveLocked()
	}
	now := time.Now().Unix()
	for _, sess := range arr {
		if sess == nil || sess.SID == "" || sess.UserID == "" {
			continue
		}
		if sess.ExpiresAt <= now || (sess.HardExpiresAt > 0 && sess.HardExpiresAt <= now) {
			continue // 过期的不加载，等于顺手清理
		}
		s.sessions[sess.SID] = sess
	}
	return s, nil
}

// saveLocked 原子写盘。调用方必须持锁。
// saveOrLog 落盘并记录失败（2026-10-20，review 抓到）。
//
// ⚠ 这里最严重的一处是 Revoke：内存里删了 session、磁盘没写
// ⇒ 进程重启后从磁盘恢复 ⇒ **刚被注销的 session 复活了**。
// 用户看到"已退出登录"，实际上那个 token 还能用 —— 这是安全问题，不是可用性问题。
//
// ⚠ Revoke / RevokeOthers / Cleanup 的签名返回 bool/int/void，改签名会连带改所有
// 上游调用点；所以这里**至少留下明确的错误日志**，让"落盘失败"可查。
// 之前的形态是：磁盘满了半个月，用户注销了一次次失效，日志里一个字都没有。
func (s *SessionStore) saveOrLog(scene string) {
	if err := s.saveLocked(); err != nil {
		log.Printf("[account] ⚠ session 落盘失败（%s）: %v —— 内存已生效但磁盘未写入。"+
			"若这是 Revoke，重启后该 session 会复活，务必排查磁盘/权限", scene, err)
	}
}
func (s *SessionStore) saveLocked() error {
	arr := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		arr = append(arr, sess)
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

// copySession 返回副本（理由同 copyUser：锁外读 + 并发写 = data race）。
func copySession(s *Session) *Session {
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

func newSID() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("account: 无法生成会话 ID: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}

// Issue 给账号签发一条新会话。
//
// 顺序：先查签发频率与在线数量（超限直接拒），再生成 SID 落盘。
// 超限时**踢掉最久没活动的**那条（而不是拒绝新登录）：用户换了浏览器
// 想进去，最该牺牲的是那个早就没在用的会话，不是正在登录的这个。
func (s *SessionStore) Issue(u *User, ip, ua string) (*Session, error) {
	if u == nil || u.ID == "" {
		return nil, ErrNotFound
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	// 签发频率（含已失效的）：prune 掉窗口外的记录
	hist := s.issued[u.ID][:0]
	for _, at := range s.issued[u.ID] {
		if now-at < int64(issuanceWindow/time.Second) {
			hist = append(hist, at)
		}
	}
	if len(hist) >= maxIssuedPerWindow {
		s.issued[u.ID] = hist
		return nil, fmt.Errorf("%w：%s 在 %s 内签发次数过多",
			ErrSessionLimit, u.Username, issuanceWindow)
	}

	// 同时在线数量：超了先踢最久没活动的
	var mine []*Session
	for _, sess := range s.sessions {
		if sess.UserID == u.ID {
			mine = append(mine, sess)
		}
	}
	if len(mine) >= MaxSessionsPerUser {
		sort.Slice(mine, func(i, j int) bool { return mine[i].LastActiveAt < mine[j].LastActiveAt })
		for i := 0; i <= len(mine)-MaxSessionsPerUser; i++ {
			delete(s.sessions, mine[i].SID)
		}
	}

	sess := &Session{
		SID:           newSID(),
		UserID:        u.ID,
		Username:      u.Username,
		Role:          u.Role,
		AuthVersion:   u.AuthVersion,
		Version:       1,
		CreatedAt:     now,
		LastActiveAt:  now,
		ExpiresAt:     now + int64(SessionTTL/time.Second),
		HardExpiresAt: now + int64(SessionMaxLifetime/time.Second),
		IP:            strings.TrimSpace(ip),
		UA:            strings.TrimSpace(ua),
	}
	s.sessions[sess.SID] = sess
	s.issued[u.ID] = append(hist, now)
	if err := s.saveLocked(); err != nil {
		delete(s.sessions, sess.SID) // 落盘失败就撤回，避免"重启后会话不存在"的幽灵会话
		return nil, err
	}
	return copySession(sess), nil
}

// touchThreshold 滑动续期的落盘阈值。
//
// 每个请求都落盘代价太大（一次磁盘写），但完全不落盘会让"最后活动时间"
// 与过期时间在重启后回退。折中：内存里每次都续，**只有**续期把
// ExpiresAt 推进超过这个阈值时才落盘。用户的体感（不会莫名掉线）由内存
// 保证，重启后的精确度由阈值保证。
const touchThreshold = 5 * time.Minute

// Lookup 校验会话并返回（会话副本, 账号副本）。
//
// # 校验链（顺序即安全语义，别调换）
//
//	① SID 存在            → 否则 ErrSessionNotFound（前端跳登录页）
//	② 未过期（滑动 + 绝对） → 否则 ErrSessionExpired
//	③ 账号仍存在且启用      → 否则 ErrSessionRevoked / ErrDisabled
//	④ AuthVersion 相等     → 否则 ErrSessionRevoked（口令改过/被强制下线）
//
// ⚠ 第 ③ 步的"账号仍存在"不是多余的：删号之后旧 cookie 仍会到达服务端，
// 此时若只看会话表，那条会话还是"有效"的 —— 一个已删除账号的登录态。
func (s *SessionStore) Lookup(sid string, accounts *Store) (*Session, *User, error) {
	if strings.TrimSpace(sid) == "" {
		return nil, nil, ErrSessionNotFound
	}
	now := time.Now().Unix()

	s.mu.Lock()
	sess := s.sessions[sid]
	if sess == nil {
		s.mu.Unlock()
		return nil, nil, ErrSessionNotFound
	}
	if sess.ExpiresAt <= now || (sess.HardExpiresAt > 0 && sess.HardExpiresAt <= now) {
		delete(s.sessions, sid)
		s.saveOrLog("Lookup-expired")
		s.mu.Unlock()
		return nil, nil, ErrSessionExpired
	}
	// 滑动续期（绝对上限封顶）
	next := now + int64(SessionTTL/time.Second)
	if sess.HardExpiresAt > 0 && next > sess.HardExpiresAt {
		next = sess.HardExpiresAt
	}
	needSave := next-sess.ExpiresAt >= int64(touchThreshold/time.Second)
	sess.ExpiresAt = next
	sess.LastActiveAt = now
	if needSave {
		s.saveOrLog("Lookup")
	}
	snapshot := copySession(sess)
	s.mu.Unlock()

	u := accounts.ByID(snapshot.UserID)
	if u == nil {
		// 账号没了 ⇒ 这条会话作废（顺手删掉）
		s.Revoke(sid)
		return nil, nil, ErrSessionRevoked
	}
	if !u.Enabled {
		return nil, nil, ErrDisabled
	}
	if !IsAdminRole(u.Role) {
		// ⚠ 2026-10-19 单账号口径（第 ③ 步的一部分）：
		//
		// 成员角色已取消 ⇒ 它的会话**不构成身份**。放在这里而不是只放在
		// Authenticate：改造前签发的会话可能还躺在 data/sessions.json 里
		// （或者账号文件回滚、被人为编辑过），只堵登录口的话，那条 cookie
		// 仍能打开 `/ui`（`Identify` 走的就是 Lookup）—— 那就等于"成员面
		// 通过一条旧 cookie 又活了"。
		//
		// 顺手把它撤掉：让这条 cookie 下次不再被当成"还在试"，也不必等过期。
		s.Revoke(sid)
		return nil, nil, ErrMemberRetired
	}
	if u.AuthVersion != snapshot.AuthVersion {
		return nil, nil, ErrSessionRevoked
	}
	return snapshot, u, nil
}

// AdvanceAuthVersion 把会话**就地**升级到账号的新 AuthVersion（返回会话副本）。
//
// # 这是"改口令/轮换之后当前浏览器不掉线"的正解
//
// 直接抄 new-api `model/user_session.go` 的 `AdvanceUserSessionAuthVersion`：
// 账号版本号 +1 会让**所有**旧会话失效，但那把执行改动的会话必须同步升到
// 新版本，否则用户改完自己的口令就会立刻被自己踢下线（用户报障过：
// "轮换之后网页加载失败，必须重启"）。
//
// 只升自己那一条：别的设备按设计失效（那正是"改口令要把别人踢掉"的意图）。
func (s *SessionStore) AdvanceAuthVersion(sid string, nextAuthVersion int64) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sid]
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	now := time.Now().Unix()
	if sess.ExpiresAt <= now || (sess.HardExpiresAt > 0 && sess.HardExpiresAt <= now) {
		delete(s.sessions, sid)
		s.saveOrLog("AdvanceAuthVersion")
		return nil, ErrSessionExpired
	}
	if nextAuthVersion < sess.AuthVersion {
		// 版本回退不是我们能处理的场景（说明调用方拿错了值）。
		return nil, fmt.Errorf("account: 会话版本不能回退（%d → %d）", sess.AuthVersion, nextAuthVersion)
	}
	sess.AuthVersion = nextAuthVersion
	sess.Version++
	sess.LastActiveAt = now
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return copySession(sess), nil
}

// Revoke 撤销一条会话（登出 / 被踢 / 凭据变更）。不存在也算成功（幂等）。
func (s *SessionStore) Revoke(sid string) bool {
	if strings.TrimSpace(sid) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sid]; !ok {
		return false
	}
	delete(s.sessions, sid)
	s.saveOrLog("Revoke")
	return true
}

// RevokeOthers 撤销该账号除 keepSID 之外的全部会话（"退出其它设备"）。
func (s *SessionStore) RevokeOthers(userID, keepSID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for sid, sess := range s.sessions {
		if sess.UserID == userID && sid != keepSID {
			delete(s.sessions, sid)
			n++
		}
	}
	if n > 0 {
		s.saveOrLog("RevokeOthers")
	}
	return n
}

// RevokeAllForUser 撤销某账号的所有会话（删号/禁用时调）。
func (s *SessionStore) RevokeAllForUser(userID string) int {
	return s.RevokeOthers(userID, "")
}

// ListForUser 列出某账号的有效会话（"安全设置"页用），当前那条标记 Current。
func (s *SessionStore) ListForUser(userID, currentSID string) []SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	out := make([]SessionView, 0, 4)
	for _, sess := range s.sessions {
		if sess.UserID != userID || sess.ExpiresAt <= now {
			continue
		}
		out = append(out, sessionView(sess, sess.SID == currentSID))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActiveAt > out[j].LastActiveAt })
	return out
}

// Cleanup 删除已过期会话，返回删除条数（启动与定期调用）。
func (s *SessionStore) Cleanup() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	n := 0
	for sid, sess := range s.sessions {
		if sess.ExpiresAt <= now || (sess.HardExpiresAt > 0 && sess.HardExpiresAt <= now) {
			delete(s.sessions, sid)
			n++
		}
	}
	if n > 0 {
		s.saveOrLog("Cleanup")
	}
	return n
}

// Count 当前有效会话数（测试与状态展示用）。
func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// BySID 取会话副本（不校验，仅供测试与运维排查）。
func (s *SessionStore) BySID(sid string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copySession(s.sessions[sid])
}
