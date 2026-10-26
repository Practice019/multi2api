package account

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/store"
)

// 本文件是 T04b（SessionStore 换 SQL 后端）的验收测试。
//
// 夹具复用 store_sql_test.go 的 openAccountSQLDB / writeAccountsFile / accountRow /
// countJSONRows（同一包、同一套 store.DB 纪律：自己开库、t.Cleanup 关库、不并行）。

// writeSessionsFile 写一份与盘上同形的 sessions.json，返回路径与原始字节。
//
// 字节留着是为了断言"只导入不删除"：导入前后必须逐字节相同。
func writeSessionsFile(t *testing.T, dir string, sessions ...*Session) (string, []byte) {
	t.Helper()
	raw, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写 sessions.json: %v", err)
	}
	return path, raw
}

// TestSQLSessionBackendImportsSessionsJSON 验收判据：
// store.DB 已打开 ⇒ user_sessions 行数 == sessions.json 元素数。
//
// 夹具里刻意放一条**已过期**的会话：导入的口径是"盘上有几行就补几行"
// （与 cmd/migrate-sql 的 importSessions 一致），清理是 Cleanup 的事。
// 这条用例把两步都钉住，免得后人把"导入"与"清理"混成一个动作。
func TestSQLSessionBackendImportsSessionsJSON(t *testing.T) {
	openAccountSQLDB(t)
	now := time.Now().Unix()
	hardCap := int64(SessionMaxLifetime / time.Second)
	dir := t.TempDir()
	path, before := writeSessionsFile(t, dir,
		&Session{SID: "s-live-1", UserID: "u-1", AuthVersion: 1, Version: 1,
			CreatedAt: now - 100, LastActiveAt: now - 50, ExpiresAt: now + 3600,
			HardExpiresAt: now - 100 + hardCap, IP: "10.0.0.1", UA: "ua-1"},
		&Session{SID: "s-live-2", UserID: "u-1", AuthVersion: 1, Version: 1,
			CreatedAt: now - 200, LastActiveAt: now - 100, ExpiresAt: now + 7200,
			HardExpiresAt: now - 200 + hardCap, IP: "10.0.0.2", UA: "ua-2"},
		&Session{SID: "s-expired", UserID: "u-2", AuthVersion: 1, Version: 1,
			CreatedAt: now - 90000, LastActiveAt: now - 80000, ExpiresAt: now - 10,
			HardExpiresAt: now - 90000 + hardCap},
	)
	elements := countJSONRows(t, path)

	s, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	rows, err := store.CountUserSessions()
	if err != nil {
		t.Fatalf("CountUserSessions: %v", err)
	}
	t.Logf("user_sessions 行数=%d，sessions.json 元素数=%d", rows, elements)
	if int(rows) != elements {
		t.Fatalf("user_sessions 行数 %d != sessions.json 元素数 %d", rows, elements)
	}

	// 只导入不删除：文件还在，且逐字节没变。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("导入后 sessions.json 不见了: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("导入改写了 sessions.json（不允许，只导入不删除）")
	}

	// 幂等：第二次 NewSessionStore（模拟重启）不重复导入。
	if _, err := NewSessionStore(path); err != nil {
		t.Fatalf("NewSessionStore(第二次): %v", err)
	}
	again, err := store.CountUserSessions()
	if err != nil {
		t.Fatalf("CountUserSessions(第二次): %v", err)
	}
	if again != rows {
		t.Fatalf("重启后重复导入：%d → %d", rows, again)
	}

	// BySID 走 SQL，并按 CreatedAt 推导出绝对上限（没有这一列，见 Session 注释）。
	live := s.BySID("s-live-1")
	if live == nil {
		t.Fatal("BySID(s-live-1) 应当能取到（SQL 后端）")
	}
	if live.HardExpiresAt != now-100+hardCap {
		t.Fatalf("绝对上限推导错了：%d，want %d", live.HardExpiresAt, now-100+hardCap)
	}
	if live.UA != "ua-1" || live.IP != "10.0.0.1" {
		t.Fatalf("会话字段没搬全: %+v", live)
	}

	// 过期行确实被导入了（导入不筛过期）：3 行。
	if got := s.Count(); got != 3 {
		t.Fatalf("导入后 Count 应为 3（含那条过期行），实际 %d", got)
	}
	// 清理是**另一个动作**：Cleanup 删掉那条滑动窗口已过期的，返回 1。
	if n := s.Cleanup(); n != 1 {
		t.Fatalf("Cleanup 应当删掉 1 条过期会话，实际 %d", n)
	}
	if got := s.Count(); got != 2 {
		t.Fatalf("Cleanup 后 Count 应为 2，实际 %d", got)
	}
}

// TestSQLSessionBackendIssueLookupAdvance 在 SQL 后端上把"签发 → 校验 → 改口令 +
// 就地升级 → 仍有效"这条链走一遍：它同时验证了 username/role 由 users 表回填、
// AuthVersion 判据仍在生效、以及注册/签发路径真的落到 user_sessions 表。
func TestSQLSessionBackendIssueLookupAdvance(t *testing.T) {
	openAccountSQLDB(t)
	dir := t.TempDir()
	accPath, _ := writeAccountsFile(t, dir, accountRow(t, "boss", "boss-password-001", RoleAdmin))
	accounts, err := New(accPath)
	if err != nil {
		t.Fatalf("New(accounts): %v", err)
	}
	u, err := accounts.Authenticate("boss", "boss-password-001")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	sesPath := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(sesPath, []byte("[]"), 0o600); err != nil {
		t.Fatalf("写空 sessions.json: %v", err)
	}
	s, err := NewSessionStore(sesPath)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}

	sess, err := s.Issue(u, "1.2.3.4", "ua-x")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, gotU, err := s.Lookup(sess.SID, accounts)
	if err != nil {
		t.Fatalf("Lookup(刚签发): %v", err)
	}
	if gotU.ID != u.ID {
		t.Fatalf("Lookup 返回的账号不对: %+v", gotU)
	}
	if got.Username != "boss" || got.Role != RoleAdmin {
		t.Fatalf("username/role 应当从 users 表回填，实际 %+v", got)
	}
	if got.HardExpiresAt != got.CreatedAt+int64(SessionMaxLifetime/time.Second) {
		t.Fatalf("签发出来的绝对上限不是 created_at+30d: %+v", got)
	}

	// 改口令 → 账号 AuthVersion +1；把**当前这条**会话就地升级 ⇒ 自己不掉线。
	next, err := accounts.SetPassword(u.ID, "rotated-password-002")
	if err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, err := s.AdvanceAuthVersion(sess.SID, next); err != nil {
		t.Fatalf("AdvanceAuthVersion: %v", err)
	}
	if _, _, err := s.Lookup(sess.SID, accounts); err != nil {
		t.Fatalf("就地升级后当前会话不该掉线: %v", err)
	}

	// 另签发的一条没升级 ⇒ 改口令后失效（AuthVersion 判据仍在 SQL 后端生效）。
	sess2, err := s.Issue(u, "5.6.7.8", "ua-y")
	if err != nil {
		t.Fatalf("Issue(second): %v", err)
	}
	if _, _, err := s.Lookup(sess2.SID, accounts); err != ErrSessionRevoked {
		t.Fatalf("未升级的会话应当 ErrSessionRevoked，实际 %v", err)
	}

	// 登出：撤销后查不到。
	if !s.Revoke(sess.SID) {
		t.Fatal("Revoke 应当删掉一条")
	}
	if _, _, err := s.Lookup(sess.SID, accounts); err != ErrSessionNotFound {
		t.Fatalf("撤销后应当 ErrSessionNotFound，实际 %v", err)
	}
}

// TestSQLSessionImportSkipsUnrepresentableHardCap 钉住那条**安全取舍**：
//
// user_sessions 没有 hard_expires_at 列，绝对上限只能按 created_at + 30d 推导。
// 若盘上某行的 hard_expires_at **早于**推导值，导入它等于把这行的绝对上限放宽到
// 30 天（被偷的 cookie 能多活）—— 所以这种行**不导入**，宁可让该用户重新登录。
func TestSQLSessionImportSkipsUnrepresentableHardCap(t *testing.T) {
	openAccountSQLDB(t)
	now := time.Now().Unix()
	hardCap := int64(SessionMaxLifetime / time.Second)
	dir := t.TempDir()
	path, _ := writeSessionsFile(t, dir,
		&Session{SID: "s-ok", UserID: "u-1", AuthVersion: 1, Version: 1,
			CreatedAt: now - 100, LastActiveAt: now - 50, ExpiresAt: now + 3600,
			HardExpiresAt: now - 100 + hardCap},
		&Session{SID: "s-tight", UserID: "u-1", AuthVersion: 1, Version: 1,
			CreatedAt: now - 100, LastActiveAt: now - 50, ExpiresAt: now + 3600,
			HardExpiresAt: now - 100 + 3600},
	)
	if _, err := NewSessionStore(path); err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	rows, err := store.CountUserSessions()
	if err != nil {
		t.Fatalf("CountUserSessions: %v", err)
	}
	if rows != 1 {
		t.Fatalf("应当只导入绝对上限可表达的那 1 行，实际 %d", rows)
	}
	if got, err := store.GetUserSessionByID("s-tight"); err != nil || got != nil {
		t.Fatalf("绝对上限无法表达的行不该进库，实际 (%+v, %v)", got, err)
	}
}

// TestSQLSessionCriterionOnRealSessionsJSON 用**真实** data/sessions.json 复现判据。
//
// 默认跳过（CI 不该依赖工作区数据）；本地显式给路径才跑：
//
//	WB2API_SESSIONS_JSON=..\data\sessions.json go test ./internal/account -run RealSessionsJSON -v
//
// 全程**只读**那份文件（SID 是登录凭据，用例不打印它，只打两个数）。
func TestSQLSessionCriterionOnRealSessionsJSON(t *testing.T) {
	path := os.Getenv("WB2API_SESSIONS_JSON")
	if path == "" {
		t.Skip("未设置 WB2API_SESSIONS_JSON：跳过真实数据复现（默认行为）")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	openAccountSQLDB(t)
	want := countJSONRows(t, path)
	if _, err := NewSessionStore(path); err != nil {
		t.Fatalf("NewSessionStore(%s): %v", path, err)
	}
	got, err := store.CountUserSessions()
	if err != nil {
		t.Fatalf("CountUserSessions: %v", err)
	}
	t.Logf("真实文件 %s：sessions.json 元素数=%d，user_sessions 行数=%d", path, want, got)
	if int(got) != want {
		t.Fatalf("user_sessions 行数 %d != sessions.json 元素数 %d", got, want)
	}
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 %s: %v", path, err)
	}
	if !bytes.Equal(raw, now) {
		t.Fatal("导入改写了真实的 sessions.json（只导入不删除被破坏）")
	}
}
