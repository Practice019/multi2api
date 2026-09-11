// Package clientlogin 管理本机 WorkBuddy 桌面客户端的登录凭证，
// 让管理台能把客户端「就地切换」成账号池里的任意账号。
//
// 逆向结论（2026-09，WorkBuddy 桌面端 5.5.4）：
//
//	凭证库   %LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop.info
//	账号指针 ~/.workbuddy/storage/skeleton/account-snapshot.json
//
// 凭证库是**明文 JSON**，形态与管理台的 auth 文件同构（account + auth 两块）。
// 客户端自己切换账号时，会把旧文件轮转成
//
//	workbuddy-desktop.<ISO 时间戳>.<pid>.<guid>.info
//
// 也就是天然的历史凭证备份 —— 本包把它当作「客户端原生凭证」的来源。
//
// 关键可行性依据：客户端 token 与管理台 token 来自同一 Keycloak realm
// （iss=https://copilot.tencent.com/auth/realms/copilot、azp=console、app_type=codebuddy），
// JWT 的 sub 就是 uid，两者只有 jti/sid/iat/exp 不同。
// 因此**账号池里持有的 token 可以直接充当客户端凭证**，切换不需要重新走登录流程。
//
// 安全约束：切换是破坏性操作（会改写本机客户端登录态），
// 所以每次写入前都把当前文件完整备份到 data/client-login/，并额外保留 last.json 供一键回滚。
//
// 另一个关键约束：客户端**正在运行时**一律拒绝切换/回滚（ErrClientRunning）。
// 登录态活在客户端内存里，磁盘文件被换掉它不会察觉，下一次刷 token 会把旧会话写回去 ——
// 现场表现就是「切了但过一会儿自己变回去」。实测踩过：08:30 切过去，09:40 自己变回来了。
package clientlogin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// 客户端凭证文件的固定名，以及它默认所在的目录。
const (
	ClientFileName = "workbuddy-desktop.info"
	snapshotName   = "account-snapshot.json"

	// SourceClient 表示凭证取自客户端自己的文件（含轮转备份），字段最完整。
	SourceClient = "client"
	// SourceGateway 表示凭证由管理台账号池提供，需要补齐客户端专有字段。
	SourceGateway = "gateway"
)

// Manager 管理本机客户端登录态。零值不可用，请用 New 构造。
type Manager struct {
	clientDir  string // 客户端凭证目录
	home       string // 用户主目录（用于定位 ~/.workbuddy）
	gwAuthDir  string // 管理台 auths 目录
	archiveDir string // 本包自己的凭证存档目录

	// proc 探测客户端是否在运行。运行中改盘会被客户端原地刷 token 还原，
	// 所以 Switch/Restore 都要先问它。带短缓存，见 running.go。
	proc processChecker
}

// SetProcessProbe 覆盖进程探测实现，仅用于测试。
// 传 nil 恢复平台默认实现。
func (m *Manager) SetProcessProbe(fn func() bool) {
	m.proc.mu.Lock()
	m.proc.probe = fn
	m.proc.at = time.Time{} // 让缓存立即失效，注入结果马上生效
	m.proc.mu.Unlock()
}

// ClientRunning 报告 WorkBuddy 桌面客户端当前是否在运行。
// 管理台用它来决定切换按钮是否可用，避免用户点了才被拒。
func (m *Manager) ClientRunning() bool {
	if m == nil {
		return false
	}
	return m.proc.running()
}

// New 构造 Manager。archiveDir 为空的实例仍然可读，但切换会被拒绝（无法保证可回滚）。
func New(clientDir, gwAuthDir, archiveDir string) *Manager {
	home, _ := os.UserHomeDir()
	if v := strings.TrimSpace(os.Getenv("WB2API_WORKBUDDY_HOME")); v != "" {
		home = v
	}
	return &Manager{
		clientDir:  clientDir,
		home:       home,
		gwAuthDir:  gwAuthDir,
		archiveDir: archiveDir,
	}
}

// SetHome 覆盖 ~/.workbuddy 所在的用户主目录。
// 做成方法而不是构造参数：绝大多数部署用 UserHomeDir() 就够了，
// 只有测试与「客户端装在别的用户下」这两种场景需要改。
func (m *Manager) SetHome(home string) {
	if strings.TrimSpace(home) != "" {
		m.home = home
	}
}

// Home 返回当前使用的用户主目录。
func (m *Manager) Home() string { return m.home }

// DefaultClientDir 返回客户端凭证目录的默认位置。
// 找不到已存在的位置时返回空串，由调用方决定是否禁用该功能。
func DefaultClientDir() string {
	if v := strings.TrimSpace(os.Getenv("WB2API_CLIENT_AUTH_DIR")); v != "" {
		if st, err := os.Stat(v); err == nil && st.IsDir() {
			return v
		}
	}
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		home, _ := os.UserHomeDir()
		local = filepath.Join(home, "AppData", "Local")
	}
	p := filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}

// ClientDir 返回本实例使用的客户端凭证目录（可能为空）。
func (m *Manager) ClientDir() string { return m.clientDir }

// ArchiveDir 返回凭证存档目录（可能为空）。
func (m *Manager) ArchiveDir() string { return m.archiveDir }

// Enabled 报告功能是否可用（客户端目录与存档目录都已确定）。
func (m *Manager) Enabled() bool {
	return m != nil && m.clientDir != "" && m.archiveDir != ""
}

// ---------------------------------------------------------------------------
// 磁盘形态
// ---------------------------------------------------------------------------

// authBlock 是客户端凭证文件里的 auth 块。
// 字段比管理台的 auth 多：会话状态、刷新有效期、scope 等，写回时必须补齐。
type authBlock struct {
	AccessToken      string `json:"accessToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	RefreshToken     string `json:"refreshToken"`
	TokenType        string `json:"tokenType"`
	NotBeforePolicy  int64  `json:"notBeforePolicy"`
	SessionState     string `json:"sessionState"`
	Scope            string `json:"scope"`
	Domain           string `json:"domain"`
	LastRefreshTime  int64  `json:"lastRefreshTime"`
	ExpiresAt        int64  `json:"expiresAt"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
}

// ExpiresAtTime 把毫秒级 expiresAt 转成时间。
func (a authBlock) ExpiresAtTime() time.Time {
	if a.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(a.ExpiresAt)
}

type deployStatus struct {
	StatusCode int    `json:"statusCode"`
	StatusMsg  string `json:"statusMsg"`
	DetailMsg  string `json:"detailMsg"`
}

type ssoInfo struct {
	Domain             string `json:"domain"`
	DomainModifiedTime int    `json:"domainModifiedTimes"`
}

// accountBlock 是客户端凭证文件里的 account 块。
type accountBlock struct {
	UID                      string       `json:"uid"`
	Nickname                 string       `json:"nickname"`
	Uin                      string       `json:"uin"`
	Type                     string       `json:"type"`
	LastLogin                bool         `json:"lastLogin"`
	IsCreator                bool         `json:"isCreator"`
	IsAdmin                  bool         `json:"isAdmin"`
	PluginEnabled            bool         `json:"pluginEnabled"`
	DeployStatus             deployStatus `json:"deployStatus"`
	AccountType              string       `json:"accountType"`
	SSO                      ssoInfo      `json:"sso"`
	Idp                      string       `json:"idp"`
	AreaInfoComplete         bool         `json:"areaInfoComplete"`
	OneIDAccountID           string       `json:"oneidAccountId"`
	IsCurrentOneIDEnterprise bool         `json:"isCurrentOneIdEnterprise"`
	IsCurrentOneIDPersonal   bool         `json:"isCurrentOneIdPersonal"`
	IsFirstLogin             bool         `json:"isFirstLogin"`
	PhoneNumber              string       `json:"phoneNumber"`
	// MpOpenID 故意不加 omitempty：切换账号时必须能把它显式清成空串，
	// 否则 mergeRaw 会把上一个账号的微信绑定留在文件里。
	MpOpenID string `json:"mpOpenId"`
}

// credential 是客户端凭证文件的保真表示。
// 只解析我们需要的两块，其余顶层键原样保留，写回时不会丢客户端自己的字段。
type credential struct {
	keys    map[string]json.RawMessage
	Account accountBlock
	Auth    authBlock
	path    string
}

// parseCredential 解析一份客户端凭证。auth 块缺失或 accessToken 为空时返回错误。
func parseCredential(raw []byte, path string) (*credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("凭据文件为空")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("凭据解析失败: %w", err)
	}
	c := &credential{keys: keys, path: path}
	if v, ok := keys["account"]; ok {
		if err := json.Unmarshal(v, &c.Account); err != nil {
			return nil, fmt.Errorf("account 块解析失败: %w", err)
		}
	}
	if v, ok := keys["auth"]; ok {
		if err := json.Unmarshal(v, &c.Auth); err != nil {
			return nil, fmt.Errorf("auth 块解析失败: %w", err)
		}
	}
	if strings.TrimSpace(c.Auth.AccessToken) == "" {
		return nil, errors.New("凭据缺少 accessToken")
	}
	return c, nil
}

// marshal 把凭证还原成带缩进的 JSON。
// account / auth / accounts / allAccounts 用当前值重写，其余顶层键原样搬过去。
//
// accountBlock 是「有名有姓的字段子集」，直接 Marshal 会丢掉客户端将来新增的键，
// 所以这里走一次覆盖合并：以原文 account 块为底，只把已知字段盖上去。
// auth 块同理 —— 我们认识的字段已覆盖客户端全部字段，但仍用同样的手法防止未来漂移。
func (c *credential) marshal() ([]byte, error) {
	acctRaw, err := mergeRaw(c.keys["account"], c.Account)
	if err != nil {
		return nil, fmt.Errorf("合并 account 块失败: %w", err)
	}
	authRaw, err := mergeRaw(c.keys["auth"], c.Auth)
	if err != nil {
		return nil, fmt.Errorf("合并 auth 块失败: %w", err)
	}
	// accounts / allAccounts 是账号数组，客户端切换后只留当前账号一条（观察值与客户端行为一致）。
	listRaw, err := json.Marshal([]json.RawMessage{acctRaw})
	if err != nil {
		return nil, err
	}

	out := make(map[string]json.RawMessage, len(c.keys)+4)
	for k, v := range c.keys {
		switch k {
		case "account", "auth", "accounts", "allAccounts":
			continue
		default:
			out[k] = v
		}
	}
	out["account"] = acctRaw
	out["auth"] = authRaw
	out["accounts"] = listRaw
	out["allAccounts"] = listRaw

	// Go 的 map 键序不可控，客户端也不依赖顺序，这里只管缩进可读。
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// mergeRaw 以 base（可能为 nil）为底，用 override 的 JSON 字段逐个覆盖，返回合并结果。
// base 不是 JSON 对象时直接返回 override —— 宁可丢掉无法解析的旧内容，也不能让写入失败。
func mergeRaw(base json.RawMessage, override any) (json.RawMessage, error) {
	merged := map[string]json.RawMessage{}
	if len(base) > 0 {
		// 解析失败（旧文件损坏）时忽略 base，仅用 override。
		_ = json.Unmarshal(base, &merged)
	}
	over, err := json.Marshal(override)
	if err != nil {
		return nil, err
	}
	var oMap map[string]json.RawMessage
	if err := json.Unmarshal(over, &oMap); err != nil {
		return nil, err
	}
	for k, v := range oMap {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// ---------------------------------------------------------------------------
// 状态视图
// ---------------------------------------------------------------------------

// Candidate 是一个可以切换过去的账号。
type Candidate struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	// Source 见 SourceClient / SourceGateway。
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

// Status 是客户端登录状态的完整快照。
type Status struct {
	Enabled    bool   `json:"enabled"`
	ClientDir  string `json:"client_dir"`
	ArchiveDir string `json:"archive_dir"`
	// ClientFile / SnapshotFile 是会被切换改写的两个文件，UI 明示出来。
	ClientFile   string `json:"client_file"`
	SnapshotFile string `json:"snapshot_file"`
	// Current 为 nil 表示客户端当前没有可用凭证（未登录或文件缺失）。
	Current *Candidate `json:"current"`
	// Candidates 按「当前 → 客户端原生 → 管理台账号池」排序。
	Candidates []Candidate `json:"candidates"`
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

// Status 计算当前客户端登录态与全部候选账号。
func (m *Manager) Status() (*Status, error) {
	st := &Status{Enabled: m.Enabled()}
	if !m.Enabled() {
		st.Error = "未找到 WorkBuddy 客户端凭证目录（%LOCALAPPDATA%\\CodeBuddyExtension\\Data\\Public\\auth）"
		return st, nil
	}
	// 客户端在跑时切换/回滚注定会被它覆盖，先把状态摆到界面上，
	// 让用户看到按钮为什么不可用，而不是点了才收到拒绝。
	st.ClientRunning = m.proc.running()
	st.ClientDir = m.clientDir
	st.ArchiveDir = m.archiveDir
	st.ClientFile = filepath.Join(m.clientDir, ClientFileName)
	st.SnapshotFile = m.snapshotPath()

	// 先把客户端目录里所有凭证（当前 + 轮转备份）归档，候选表才有原生来源。
	_ = m.syncArchive()

	cur, err := m.loadCurrent()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		st.Error = err.Error()
	}

	byUID := map[string]*Candidate{}
	order := []string{}
	put := func(c Candidate) {
		if c.UID == "" {
			return
		}
		if old, ok := byUID[c.UID]; ok {
			// 已有更强来源时不覆盖；原生凭证优先于账号池凭证。
			if old.Source == SourceClient || c.Source == SourceGateway {
				return
			}
		} else {
			order = append(order, c.UID)
		}
		cc := c
		byUID[c.UID] = &cc
	}

	if cur != nil {
		cc := m.candidate(cur, SourceClient, 0)
		cc.Current = true
		put(cc)
		st.Current = &cc
	}

	// 客户端原生存档（含轮转备份）。
	for _, c := range m.listArchive() {
		put(m.candidate(c, SourceClient, 0))
	}
	// 管理台账号池兜底。
	for _, a := range m.listGateway() {
		put(m.candidateGateway(a))
	}

	sort.SliceStable(order, func(i, j int) bool {
		a, b := byUID[order[i]], byUID[order[j]]
		if a.Current != b.Current {
			return a.Current
		}
		if (a.Source == SourceClient) != (b.Source == SourceClient) {
			return a.Source == SourceClient
		}
		if a.Valid != b.Valid {
			return a.Valid
		}
		return a.Nickname < b.Nickname
	})
	for _, uid := range order {
		st.Candidates = append(st.Candidates, *byUID[uid])
	}

	if bk, err := m.readLastBackup(); err == nil && bk != nil {
		// 备份与当前账号相同 => 回滚是无操作。这里就把 HasBackup 置否，
		// 让界面上的「回滚上一次」按钮同步禁用，而不是等用户点了再收 409。
		same := st.Current != nil && st.Current.UID == bk.Account.UID
		if !same {
			st.HasBackup = true
			st.BackupUID = bk.Account.UID
			st.BackupNick = firstNonEmpty(bk.Account.Nickname, bk.Account.PhoneNumber, bk.Account.UID)
			if fi, err := os.Stat(m.lastBackupPath()); err == nil {
				st.BackupAt = fi.ModTime().Format("2006-01-02 15:04:05")
			}
		}
	}
	return st, nil
}

// candidate 从一份客户端原生凭证生成候选视图。
func (m *Manager) candidate(c *credential, source string, gwExpiry int64) Candidate {
	exp := c.Auth.ExpiresAt / 1000
	if exp == 0 {
		exp = gwExpiry
	}
	nick := c.Account.Nickname
	if nick == "" {
		nick = c.Account.PhoneNumber
	}
	return Candidate{
		UID:           c.Account.UID,
		Nickname:      nick,
		Source:        source,
		ExpiresAt:     exp,
		ExpiresAtText: fmtTime(exp),
		Valid:         exp == 0 || time.Now().Unix() < exp,
		Restorable:    true,
		TokenHint:     tokenHint(c.Auth.AccessToken),
	}
}

// candidateGateway 从管理台账号凭证生成候选视图。
func (m *Manager) candidateGateway(a *auth.Auth) Candidate {
	nick := a.Nickname
	if nick == "" {
		nick = a.UID
	}
	return Candidate{
		UID:           a.UID,
		Nickname:      nick,
		Source:        SourceGateway,
		ExpiresAt:     a.ExpiresAt,
		ExpiresAtText: fmtTime(a.ExpiresAt),
		Valid:         a.ExpiresAt == 0 || time.Now().Unix() < a.ExpiresAt,
		Restorable:    false,
		TokenHint:     tokenHint(a.AccessToken),
	}
}

func fmtTime(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).Local().Format("2006-01-02 15:04")
}

// tokenHint 返回 token 的不可逆指纹：前 6 位 + 长度。
// 目的是让用户能肉眼比对「切过去的是不是同一个 token」，而不是只能相信后台。
func tokenHint(tok string) string {
	if len(tok) <= 6 {
		return ""
	}
	return tok[:6] + "…len" + fmt.Sprint(len(tok))
}

// ---------------------------------------------------------------------------
// 读写
// ---------------------------------------------------------------------------

func (m *Manager) clientPath() string { return filepath.Join(m.clientDir, ClientFileName) }
func (m *Manager) snapshotPath() string {
	return filepath.Join(m.home, ".workbuddy", "storage", "skeleton", snapshotName)
}

// loadCurrent 读取客户端当前凭证。
func (m *Manager) loadCurrent() (*credential, error) {
	raw, err := os.ReadFile(m.clientPath())
	if err != nil {
		return nil, err
	}
	return parseCredential(raw, m.clientPath())
}

// syncArchive 扫描客户端目录，把每份凭证按 uid 归档到 archiveDir/<uid>.json，
// 仅当来源比存档更新（expiresAt 更大）时覆盖。
// 轮转备份因此不会丢：客户端自己切换一次，我们就永久留住了那个账号的原生凭证。
func (m *Manager) syncArchive() error {
	if !m.Enabled() {
		return errors.New("clientlogin: 未启用")
	}
	entries, err := os.ReadDir(m.clientDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.archiveDir, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".info") {
			continue
		}
		p := filepath.Join(m.clientDir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		c, err := parseCredential(raw, p)
		if err != nil || c.Account.UID == "" {
			continue
		}
		dst := m.archivePath(c.Account.UID)
		if old, err := os.ReadFile(dst); err == nil {
			if oc, err := parseCredential(old, dst); err == nil && oc.Auth.ExpiresAt >= c.Auth.ExpiresAt {
				continue
			}
		}
		_ = os.WriteFile(dst, raw, 0o600)
	}
	return nil
}

func (m *Manager) archivePath(uid string) string {
	return filepath.Join(m.archiveDir, sanitizeUID(uid)+".json")
}

// sanitizeUID 只保留安全字符，避免 uid 被当成路径片段（目录穿越）。
func sanitizeUID(uid string) string {
	var b strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// listArchive 返回存档目录里所有可解析的凭证。
func (m *Manager) listArchive() []*credential {
	if !m.Enabled() {
		return nil
	}
	entries, err := os.ReadDir(m.archiveDir)
	if err != nil {
		return nil
	}
	var out []*credential
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(m.archiveDir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if c, err := parseCredential(raw, p); err == nil && c.Account.UID != "" {
			out = append(out, c)
		}
	}
	return out
}

// gatewayPath 返回管理台为 uid 保存的 auth 文件路径（不存在则空串）。
func (m *Manager) gatewayPath(uid string) string {
	if m.gwAuthDir == "" {
		return ""
	}
	p := filepath.Join(m.gwAuthDir, "workbuddy-"+sanitizeUID(uid)+".json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	// 兜底：文件名不一定严格等于 uid，扫描一遍匹配 account.uid。
	matches, _ := filepath.Glob(filepath.Join(m.gwAuthDir, "workbuddy*.json"))
	for _, f := range matches {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if a, err := auth.Parse(raw); err == nil && a.UID == uid {
			return f
		}
	}
	return ""
}

// listGateway 返回管理台账号池里的全部凭证。
func (m *Manager) listGateway() []*auth.Auth {
	if m.gwAuthDir == "" {
		return nil
	}
	list, err := auth.LoadDir(m.gwAuthDir)
	if err != nil {
		return nil
	}
	return list
}

// lastBackupPath 是最近一次切换前的完整登录态，用于一键回滚。
func (m *Manager) lastBackupPath() string {
	return filepath.Join(m.archiveDir, "last.json")
}

func (m *Manager) readLastBackup() (*credential, error) {
	raw, err := os.ReadFile(m.lastBackupPath())
	if err != nil {
		return nil, err
	}
	return parseCredential(raw, m.lastBackupPath())
}

// ---------------------------------------------------------------------------
// 切换 / 回滚
// ---------------------------------------------------------------------------

// SwitchResult 描述一次切换的结果，供 UI 直接展示。
type SwitchResult struct {
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

// ErrSameAccount 表示目标账号已经是当前登录账号。
var ErrSameAccount = errors.New("该账号已经是客户端当前登录账号")

// ErrAlreadyBackedUp 表示备份里的账号与当前登录账号一致，回滚是无操作。
var ErrAlreadyBackedUp = errors.New("当前已是备份中的登录态，无需回滚")

// ErrClientRunning 表示 WorkBuddy 客户端正在运行，此时改写磁盘登录态没有意义。
//
// 成因（实测）：登录态主要活在客户端内存里，我们换掉磁盘文件后它不会察觉，
// 下一次刷新 token 时仍按内���里的旧会话写回磁盘 —— 现场表现就是
// 「切了但过一会儿自己变回去」。所以运行中直接拒绝，让用户先退出客户端。
var ErrClientRunning = errors.New("WorkBuddy 客户端正在运行：它会把内存里的旧登录态写回磁盘，覆盖本次切换。" +
	"请先完全退出客户端（含托盘图标）再切换")

// Switch 把本机客户端登录态切换到 uid。
//
// 流程（每一步都可回滚）：
//  1. 归档客户端目录里的所有凭证；
//  2. 备份当前 workbuddy-desktop.info 到 archiveDir/last.json；
//  3. 选来源：客户端原生凭证优先（字段完整），否则用管理台账号池 token 合成；
//  4. 原子写入 workbuddy-desktop.info；
//  5. 同步改写 account-snapshot.json，让客户端启动时认到新账号。
//
// 客户端正在运行时直接返回 ErrClientRunning：登录态活在客户端内存里，
// 我们换掉磁盘文件后它不会察觉，下一次刷 token 会把旧会话写回去。
// 这一步在动任何文件之前完成，被拒的操作不留痕迹。
func (m *Manager) Switch(uid string) (*SwitchResult, error) {
	if !m.Enabled() {
		return nil, errors.New("clientlogin: 未找到客户端凭证目录，切换不可用")
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, errors.New("缺少 uid")
	}
	if err := m.syncArchive(); err != nil {
		return nil, fmt.Errorf("归档客户端凭证失败: %w", err)
	}

	cur, curErr := m.loadCurrent()
	if curErr == nil && cur != nil && cur.Account.UID == uid {
		return nil, ErrSameAccount
	}

	// 客户端在跑就直接拒绝：写进去也会被它按内存会话覆盖回去（见 ErrClientRunning）。
	// 这一步刻意放在备份之前 —— 被拒的操作不该留下任何痕迹。
	if m.proc.running() {
		return nil, ErrClientRunning
	}

	// 先把目标凭证解析出来，再动备份。
	// 顺序很重要：buildTarget 会失败（账号不存在、凭据解析不了），
	// 如果先写备份，一次失败就会覆盖掉上一次的有效备份 ——
	// 用户本来能回滚到 A，失败一次之后只能回滚到 B，回滚目标被静默换掉了。
	target, source, err := m.buildTarget(uid, cur)
	if err != nil {
		return nil, err
	}
	// 防御：绝不用空 token 覆盖有效凭据。
	if strings.TrimSpace(target.Auth.AccessToken) == "" {
		return nil, errors.New("拒绝写入空 accessToken")
	}
	out, err := target.marshal()
	if err != nil {
		return nil, fmt.Errorf("生成凭据失败: %w", err)
	}

	// 到这里才确认这次切换写得成，开始备份。
	// 备份失败就中止 —— 没有回滚能力时不做破坏性操作。
	backupPath := ""
	if curErr == nil && cur != nil {
		raw, err := os.ReadFile(m.clientPath())
		if err != nil {
			return nil, fmt.Errorf("读取当前凭据失败: %w", err)
		}
		if err := writeFileAtomic(m.lastBackupPath(), raw, 0o600); err != nil {
			return nil, fmt.Errorf("备份当前登录态失败，已中止切换: %w", err)
		}
		// 除 last.json 外再留一份带时间戳的副本：连点两次切换时，last.json 会被覆盖，
		// 而带时间戳的副本让每一次切换都留下独立痕迹。
		ts := time.Now().Format("20060102-150405")
		backupPath = filepath.Join(m.archiveDir, fmt.Sprintf("switch-backup-%s-%s.info", ts, sanitizeUID(uid)))
		_ = writeFileAtomic(backupPath, raw, 0o600)
	}

	if err := writeFileAtomic(m.clientPath(), out, 0o600); err != nil {
		return nil, fmt.Errorf("写入客户端凭据失败: %w", err)
	}
	if err := m.writeSnapshot(target.Account); err != nil {
		// 凭据已写入，账号指针没跟上：客户端可能仍按旧账号显示。
		// 不静默吞掉，明确告诉调用方。
		return nil, fmt.Errorf("凭据已切换，但账号指针写入失败（客户端可能仍显示旧账号）: %w", err)
	}

	nick := target.Account.Nickname
	if nick == "" {
		nick = uid
	}
	return &SwitchResult{
		UID:        uid,
		Nickname:   nick,
		Source:     source,
		Changed:    true,
		Message:    "已切换客户端登录态，请完全退出并重新启动 WorkBuddy 客户端生效",
		BackupPath: backupPath,
	}, nil
}

// buildTarget 选出目标凭证并补齐客户端专有字段。
func (m *Manager) buildTarget(uid string, cur *credential) (*credential, string, error) {
	// 先找客户端原生凭证（存档里最完整，含 sessionState/scope/uin/phoneNumber）。
	for _, c := range m.listArchive() {
		if c.Account.UID == uid {
			cp := *c
			cp.path = m.clientPath()
			cp.Account.LastLogin = true
			return &cp, SourceClient, nil
		}
	}

	// 回退到管理台账号池：token 同源，只需补齐客户端专有的元数据。
	p := m.gatewayPath(uid)
	if p == "" {
		return nil, "", fmt.Errorf("账号 %s 在客户端存档和账号池里都找不到凭证", uid)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, "", fmt.Errorf("读取账号池凭据失败: %w", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("解析账号池凭据失败: %w", err)
	}

	// 从当前文件继承「与账号身份无关」的字段，保留客户端自己写进去的其他顶层键。
	// 关键：account 块必须区分「可以继承的外观字段」与「属于某个账号的身份字段」——
	// 身份字段（uid/nickname/uin/phoneNumber/mpOpenId）一律重写，
	// 否则会出现「新账号带着旧账号手机号」这种比不切换更糟的状态。
	tmpl := map[string]json.RawMessage{}
	var acct accountBlock
	if cur != nil {
		for k, v := range cur.keys {
			tmpl[k] = v
		}
		acct = accountBlock{
			PluginEnabled: cur.Account.PluginEnabled,
			DeployStatus:  cur.Account.DeployStatus,
			AccountType:   cur.Account.AccountType,
			SSO:           cur.Account.SSO,
			Idp:           cur.Account.Idp,
			// EditionType 之类的展示字段不在本次写入范围内，交由 account-snapshot 保留。
		}
	} else {
		acct.PluginEnabled = true
	}
	acct.UID = a.UID
	acct.Nickname = a.Nickname
	acct.Type = "personal"
	acct.LastLogin = true
	acct.Uin = ""
	acct.MpOpenID = ""
	// 昵称在个人账号下就是手机号；账号池拿不到 phoneNumber，用昵称兜底比留空好。
	acct.PhoneNumber = a.Nickname

	ab := authBlock{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		TokenType:    "Bearer",
		Domain:       firstNonEmpty(a.Domain, "copilot.tencent.com"),
		Scope:        "profile offline_access email",
		ExpiresAt:    a.ExpiresAt * 1000, // 客户端用毫秒
	}
	// 从当前文件继承常量型字段（realm 的 notBeforePolicy 等），拿不到就用观察到的默认值。
	if cur != nil {
		ab.NotBeforePolicy = cur.Auth.NotBeforePolicy
		ab.SessionState = cur.Auth.SessionState
	}
	if ab.NotBeforePolicy == 0 {
		ab.NotBeforePolicy = 1724292326
	}
	now := time.Now()
	ab.LastRefreshTime = now.UnixMilli()
	if ab.ExpiresAt > 0 {
		// 观察值：accessToken 60 天、refreshToken 90 天，这里按此推算刷新窗口。
		ab.ExpiresIn = ab.ExpiresAt/1000 - now.Unix()
		if ab.ExpiresIn < 0 {
			ab.ExpiresIn = 0
		}
		ab.RefreshExpiresAt = ab.ExpiresAt + 30*24*3600*1000
		ab.RefreshExpiresIn = ab.RefreshExpiresAt/1000 - now.Unix()
	}

	return &credential{
		keys:    tmpl,
		Account: acct,
		Auth:    ab,
		path:    m.clientPath(),
	}, SourceGateway, nil
}

// snapshotDoc 对应 ~/.workbuddy/storage/skeleton/account-snapshot.json。
type snapshotDoc struct {
	Primary struct {
		Version        int    `json:"version"`
		UID            string `json:"uid"`
		Nickname       string `json:"nickname"`
		Type           string `json:"type"`
		EditionType    string `json:"editionType"`
		IsPro          bool   `json:"isPro"`
		IsAdmin        bool   `json:"isAdmin"`
		OneIDAccountID string `json:"oneidAccountId"`
		SavedAt        int64  `json:"savedAt"`
	} `json:"primary"`
}

// writeSnapshot 让客户端的「当前账号」指针指向刚切过去的账号。
// 文件不存在时按观察到的形态新建；存在时保留 editionType 之类的原值。
func (m *Manager) writeSnapshot(acct accountBlock) error {
	p := m.snapshotPath()
	if p == "" {
		return errors.New("无法定位 account-snapshot.json")
	}
	var doc snapshotDoc
	if raw, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(raw, &doc) // 解析失败就从零构造，不让旧文件阻断切换
	}
	if doc.Primary.Version == 0 {
		doc.Primary.Version = 1
	}
	if doc.Primary.EditionType == "" {
		doc.Primary.EditionType = "free"
	}
	doc.Primary.UID = acct.UID
	doc.Primary.Nickname = acct.Nickname
	doc.Primary.Type = firstNonEmpty(acct.Type, "personal")
	doc.Primary.IsAdmin = acct.IsAdmin
	doc.Primary.SavedAt = time.Now().UnixMilli()

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, out, 0o600)
}

// Restore 用最近一次备份回滚客户端登录态。
func (m *Manager) Restore() (*SwitchResult, error) {
	if !m.Enabled() {
		return nil, errors.New("clientlogin: 未找到客户端凭证目录，回滚不可用")
	}
	// 和 Switch 同一个理由：客户端在跑时改盘会被它原地覆盖（见 ErrClientRunning）。
	// 放在最前面 —— 被拒的操作不该改动任何文件。
	if m.proc.running() {
		return nil, ErrClientRunning
	}
	bk, err := m.readLastBackup()
	if err != nil {
		return nil, errors.New("没有可回滚的备份（尚未在本控制台执行过切换）")
	}
	raw, err := os.ReadFile(m.lastBackupPath())
	if err != nil {
		return nil, err
	}
	// 备份与当前是同一个账号时无事可做。这种情况出现在「切换后立刻回滚」之后再点一次回滚：
	// 直接报错比默默重写两个文件更诚实（重写还会平白刷新 savedAt）。
	if cur, cerr := m.loadCurrent(); cerr == nil && cur != nil && cur.Account.UID == bk.Account.UID {
		return nil, ErrAlreadyBackedUp
	}
	// 回滚本身也要可回滚：先把当前文件转存成一份带时间戳的副本。
	if cur, err := os.ReadFile(m.clientPath()); err == nil && len(cur) > 0 {
		ts := time.Now().Format("20060102-150405")
		_ = writeFileAtomic(filepath.Join(m.archiveDir, "restore-backup-"+ts+".info"), cur, 0o600)
	}
	if err := writeFileAtomic(m.clientPath(), raw, 0o600); err != nil {
		return nil, fmt.Errorf("回滚凭据失败: %w", err)
	}
	if err := m.writeSnapshot(bk.Account); err != nil {
		return nil, fmt.Errorf("凭据已回滚，但账号指针写入失败: %w", err)
	}
	nick := bk.Account.Nickname
	if nick == "" {
		nick = bk.Account.UID
	}
	return &SwitchResult{
		UID:      bk.Account.UID,
		Nickname: nick,
		Source:   SourceClient,
		Changed:  true,
		Message:  "已回滚到上一次登录态，请完全退出并重新启动 WorkBuddy 客户端生效",
	}, nil
}

// writeFileAtomic 以 tmp + rename 原子写文件，避免客户端读到半个文件。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
