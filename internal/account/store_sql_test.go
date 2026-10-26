package account

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/store"
)

// 本文件是 T04（account.Store 换 SQL 后端）的验收测试。
//
// # 为什么夹具是"临时库 + 临时目录"而不是起一个服务实例
//
// 真实二元判据是"删掉 data/gw.db 后重启，users 行数 == accounts.json 元素数"。
// 但**注入点**（cmd/server 调 store.Open）是 T11 的活（main.go 是它独占的文件），
// T11 完成前跑真实例只会走 JSON 兼容后端，判不出任何东西。所以这里用 t.TempDir
// 复现**同一判据**：同一个 store.Open + Migrate + account.New(path) 调用序列，
// 同一份 accounts.json 结构（用 account_test.go 的 accountRow 造行）。
//
// 另有一条环境变量开关的真实数据复现：WB2API_ACCOUNTS_JSON=<真实文件> 时直接
// 读那份文件（**只读**，不写回），把两个数打出来。
//
// ⚠ store.DB 是包级句柄：每个用例自己开库、t.Cleanup 关库，本包测试不并行。

// openAccountSQLDB 开一个临时库并建表；用例结束自动关（否则后续用例会静默
// 从 SQL 后端变回 JSON 后端）。
func openAccountSQLDB(t *testing.T) {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
}

// writeAccountsFile 写一份与盘上同形的 accounts.json，返回路径与**原始字节**。
//
// 字节留着是为了断言"只导入不删除"：导入前后必须逐字节相同。
func writeAccountsFile(t *testing.T, dir string, rows ...map[string]any) (string, []byte) {
	t.Helper()
	raw, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写 accounts.json: %v", err)
	}
	return path, raw
}

// countJSONRows 数 accounts.json 的元素个数（判据的右边那个数）。
func countJSONRows(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	return len(arr)
}

// TestSQLBackendImportsAccountsJSON 验收判据 ②（隔离复现）：
// store.DB 已打开 ⇒ users 行数 == accounts.json 元素数，且 JSON 不被改写、
// 二次 New 不重复导入、导入进来的账号真的能登录。
func TestSQLBackendImportsAccountsJSON(t *testing.T) {
	openAccountSQLDB(t)
	dir := t.TempDir()
	path, before := writeAccountsFile(t, dir,
		accountRow(t, "boss", "boss-password-001", RoleAdmin),
		accountRow(t, "second", "second-password-001", RoleAdmin),
		accountRow(t, "legacy", "legacy-password-001", RoleMember),
	)
	wantJSON := countJSONRows(t, path)

	s, err := New(path)
	if err != nil {
		t.Fatalf("New(已有库): %v", err)
	}
	gotSQL, err := store.CountUsers()
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	t.Logf("users 行数=%d，accounts.json 元素数=%d", gotSQL, wantJSON)
	if int(gotSQL) != wantJSON {
		t.Fatalf("users 行数 %d != accounts.json 元素数 %d", gotSQL, wantJSON)
	}

	// 只导入不删除：文件还在，且逐字节没变（New 不许写回 JSON）。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("导入后 accounts.json 不见了: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("导入改写了 accounts.json（不允许，PLAN 6.0 只导入不删除）")
	}

	// 幂等：第二次 New（模拟重启）不会重复插入。
	if _, err := New(path); err != nil {
		t.Fatalf("New(第二次): %v", err)
	}
	again, err := store.CountUsers()
	if err != nil {
		t.Fatalf("CountUsers(第二次): %v", err)
	}
	if again != gotSQL {
		t.Fatalf("重启后重复导入：%d → %d", gotSQL, again)
	}

	// SQL 后端的账号可用：口令校验走库里那份哈希。
	u, err := s.Authenticate("boss", "boss-password-001")
	if err != nil {
		t.Fatalf("导入后的账号应当能登录: %v", err)
	}
	if u.Username != "boss" || u.Role != RoleAdmin {
		t.Fatalf("登录回来的账号不对: %+v", u)
	}
	// 非管理员（RoleMember=1）的历史记录仍然"认出来再拒掉"。
	if _, err := s.Authenticate("legacy", "legacy-password-001"); err != ErrMemberRetired {
		t.Fatalf("成员历史记录应当返回 ErrMemberRetired，实际 %v", err)
	}

	// 列表/单查/按名查都得走 SQL：改一条口令后再读，看到的必须是改后的值。
	if _, err := s.SetPassword(u.ID, "rotated-password-002"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, err := s.Authenticate("boss", "rotated-password-002"); err != nil {
		t.Fatalf("改完口令应当能用新口令登录: %v", err)
	}
	views := s.List()
	if len(views) != wantJSON {
		t.Fatalf("List 应当给出 %d 条，实际 %d", wantJSON, len(views))
	}
	if vs, ok := s.Snapshot(u.ID); !ok || vs.Username != "boss" {
		t.Fatalf("Snapshot: (%+v, %v)", vs, ok)
	}
}

// TestSQLBackendDoesNotLetJSONOverwriteSQL 钉住"SQL 权威期 JSON 不得再被写回"。
//
// 判据分两半：
//
//	① 改过口令后重启（再 New 一次）：库里的口令**不许**被 JSON 里的旧值覆盖
//	② 同一段时间里 accounts.json 的字节不许发生变化
//
// 这条与 internal/store/option.go 的"键不存在才写"是同一条纪律：
// 每次启动都拿旧文件覆盖，界面上改过的值会在重启时被悄悄冲回去。
func TestSQLBackendDoesNotLetJSONOverwriteSQL(t *testing.T) {
	openAccountSQLDB(t)
	dir := t.TempDir()
	path, before := writeAccountsFile(t, dir, accountRow(t, "boss", "old-password-001", RoleAdmin))

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u, err := s.Authenticate("boss", "old-password-001")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if _, err := s.SetPassword(u.ID, "new-password-002"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	// 模拟重启：JSON 里仍是旧口令。
	if _, err := New(path); err != nil {
		t.Fatalf("New(重启): %v", err)
	}
	su, err := store.GetUserByID(u.ID)
	if err != nil || su == nil {
		t.Fatalf("GetUserByID: (%+v, %v)", su, err)
	}
	if !VerifyPassword(su.PasswordHash, "new-password-002") {
		t.Fatal("SQL 侧的改口令被 JSON 里的旧值覆盖了（双写/覆盖语义，禁止）")
	}
	if VerifyPassword(su.PasswordHash, "old-password-001") {
		t.Fatal("库里还是 JSON 的旧口令")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 accounts.json: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("SQL 权威期写回了 accounts.json（PLAN 6.0 禁止）")
	}
}

// TestSQLBackendCriterionOnRealAccountsJSON 用**真实** data/accounts.json 复现判据 ②。
//
// 默认跳过（CI 上不该依赖工作区数据）；本地显式给路径才跑：
//
//	WB2API_ACCOUNTS_JSON=..\data\accounts.json go test ./internal/account -run RealAccountsJSON -v
//
// 全程**只读**那份文件（不复制、不改写），库开在 t.TempDir 里。
func TestSQLBackendCriterionOnRealAccountsJSON(t *testing.T) {
	path := os.Getenv("WB2API_ACCOUNTS_JSON")
	if path == "" {
		t.Skip("未设置 WB2API_ACCOUNTS_JSON：跳过真实数据复现（默认行为）")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	openAccountSQLDB(t)
	want := countJSONRows(t, path)
	if _, err := New(path); err != nil {
		t.Fatalf("New(%s): %v", path, err)
	}
	got, err := store.CountUsers()
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	t.Logf("真实文件 %s：accounts.json 元素数=%d，users 行数=%d", path, want, got)
	if int(got) != want {
		t.Fatalf("users 行数 %d != accounts.json 元素数 %d", got, want)
	}
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 %s: %v", path, err)
	}
	if !bytes.Equal(raw, now) {
		t.Fatal("导入改写了真实的 accounts.json（只导入不删除被破坏）")
	}
}
