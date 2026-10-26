package store

import (
	"errors"
	"testing"
)

// 本文件覆盖 user.go 的两张表仓储。夹具复用 setup_test.go 的 openTempDB / countRows：
// DB 是包级句柄，本包测试约定是**每个用例自己开库、t.Cleanup 关库**，不并行。

func TestUserRepoCRUD(t *testing.T) {
	openTempDB(t)

	first := User{ID: "u-1", Username: "Alice", DisplayName: "Alice", PasswordHash: "h1", Role: 10, Enabled: true, AuthVersion: 1, CreatedAt: 100}
	if err := CreateUser(&first); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if n := countRows(t, &User{}); n != 1 {
		t.Fatalf("users 应有 1 行，实际 %d", n)
	}

	// 大小写不敏感：库里存 Alice，按 alice 必须查得到（account.normalizeUsername 的口径）。
	got, err := GetUserByUsername("alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got == nil || got.ID != "u-1" {
		t.Fatalf("按小写名查不到（库里存的是 Alice）: %+v", got)
	}

	// 不存在 → (nil, nil)，不是错误（调用点把它当正常分支）。
	miss, err := GetUserByUsername("nobody")
	if err != nil || miss != nil {
		t.Fatalf("查不到应当是 (nil, nil)，实际 (%+v, %v)", miss, err)
	}
	if u, err := GetUserByID("u-nope"); err != nil || u != nil {
		t.Fatalf("按 ID 查不到应当是 (nil, nil)，实际 (%+v, %v)", u, err)
	}

	// 主键冲突必须**报错**（建号路径不能把冲突吞掉）。
	dup := first
	if err := CreateUser(&dup); !errors.Is(err, ErrUserExists) {
		t.Fatalf("重复 ID 应当返回 ErrUserExists，实际 %v", err)
	}
	// 用户名冲突（不同 ID 同名）同样要报错。
	dup2 := User{ID: "u-2", Username: "Alice", Role: 10, Enabled: true, CreatedAt: 101}
	if err := CreateUser(&dup2); !errors.Is(err, ErrUserExists) {
		t.Fatalf("重复用户名应当返回 ErrUserExists，实际 %v", err)
	}

	// List 顺序：created_at 升序（与 account.Store.List 的展示顺序一致）。
	second := User{ID: "u-2", Username: "bob", DisplayName: "bob", Role: 1, Enabled: true, CreatedAt: 90}
	if err := CreateUser(&second); err != nil {
		t.Fatalf("CreateUser(second): %v", err)
	}
	rows, err := ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "u-2" || rows[1].ID != "u-1" {
		t.Fatalf("ListUsers 应当按 created_at 升序，实际 %+v", rows)
	}

	// SaveUser：整行写回（调用方是读-改-写）。
	first.PasswordHash = "h2"
	first.AuthVersion = 2
	if err := SaveUser(&first); err != nil {
		t.Fatalf("SaveUser: %v", err)
	}
	after, err := GetUserByID("u-1")
	if err != nil || after == nil {
		t.Fatalf("GetUserByID(after): %v", err)
	}
	if after.PasswordHash != "h2" || after.AuthVersion != 2 {
		t.Fatalf("SaveUser 没写进去: %+v", after)
	}

	// SaveUser 不存在的行 → ErrUserNotFound（调用方据此回 404 而不是 500）。
	ghost := User{ID: "u-ghost", Username: "ghost"}
	if err := SaveUser(&ghost); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("SaveUser 不存在的行应当返回 ErrUserNotFound，实际 %v", err)
	}

	// TouchUserLogin 只动两列。
	if err := TouchUserLogin("u-1", 12345, "10.0.0.1"); err != nil {
		t.Fatalf("TouchUserLogin: %v", err)
	}
	touched, _ := GetUserByID("u-1")
	if touched.LastLoginAt != 12345 || touched.LastLoginIP != "10.0.0.1" {
		t.Fatalf("TouchUserLogin 没写进去: %+v", touched)
	}
	if touched.PasswordHash != "h2" {
		t.Fatalf("TouchUserLogin 不该碰口令哈希，实际 %q", touched.PasswordHash)
	}
}

// TestImportUsersIfAbsentLeavesExistingRowsAlone 钉住"只补缺、不覆盖"这条语义。
//
// 它与 cmd/migrate-sql 的 UpdateAll **刻意不同**：运行时每次启动都会尝试导入，
// 若用 UpdateAll，用户改过的口令会在下次重启时被旧 JSON 冲回去。
func TestImportUsersIfAbsentLeavesExistingRowsAlone(t *testing.T) {
	openTempDB(t)

	seed := User{ID: "u-1", Username: "boss", PasswordHash: "hash-in-sql", Role: 10, Enabled: true, AuthVersion: 3, CreatedAt: 100}
	if err := CreateUser(&seed); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	rows := []User{
		{ID: "u-1", Username: "boss", PasswordHash: "hash-in-json", Role: 10, Enabled: true, AuthVersion: 1, CreatedAt: 100},
		{ID: "u-2", Username: "newbie", PasswordHash: "h", Role: 10, Enabled: true, AuthVersion: 1, CreatedAt: 200},
	}
	n, err := ImportUsersIfAbsent(rows)
	if err != nil {
		t.Fatalf("ImportUsersIfAbsent: %v", err)
	}
	if n != 1 {
		t.Fatalf("应当只补 1 行（u-1 已存在），实际 %d", n)
	}
	kept, _ := GetUserByID("u-1")
	if kept.PasswordHash != "hash-in-sql" || kept.AuthVersion != 3 {
		t.Fatalf("已存在的行被覆盖了: %+v", kept)
	}
	// 幂等：再导一次不新增。
	again, err := ImportUsersIfAbsent(rows)
	if err != nil || again != 0 {
		t.Fatalf("二次导入应当是 0 行，实际 (%d, %v)", again, err)
	}
	if total := countRows(t, &User{}); total != 2 {
		t.Fatalf("users 应有 2 行，实际 %d", total)
	}
}

func TestUserSessionRepoCRUD(t *testing.T) {
	openTempDB(t)

	s1 := UserSession{ID: "s1", UserID: "u-1", AuthVersion: 1, CreatedAt: 10, LastActiveAt: 20, ExpiresAt: 1000, IP: "1.1.1.1", UserAgent: "ua"}
	s2 := UserSession{ID: "s2", UserID: "u-1", AuthVersion: 1, CreatedAt: 11, LastActiveAt: 30, ExpiresAt: 1000}
	s3 := UserSession{ID: "s3", UserID: "u-2", AuthVersion: 1, CreatedAt: 12, LastActiveAt: 40, ExpiresAt: 5}
	for _, s := range []*UserSession{&s1, &s2, &s3} {
		if err := CreateUserSession(s); err != nil {
			t.Fatalf("CreateUserSession(%s): %v", s.ID, err)
		}
	}
	if n, err := CountUserSessions(); err != nil || n != 3 {
		t.Fatalf("CountUserSessions = (%d, %v)，want 3", n, err)
	}

	got, err := GetUserSessionByID("s1")
	if err != nil || got == nil || got.UserID != "u-1" {
		t.Fatalf("GetUserSessionByID: (%+v, %v)", got, err)
	}
	if miss, err := GetUserSessionByID("nope"); err != nil || miss != nil {
		t.Fatalf("不存在的 sid 应当是 (nil, nil)，实际 (%+v, %v)", miss, err)
	}
	if err := CreateUserSession(&s1); !errors.Is(err, ErrUserExists) {
		t.Fatalf("重复 sid 应当返回 ErrUserExists，实际 %v", err)
	}

	// 按账号列出，last_active_at 倒序。
	mine, err := ListUserSessionsByUser("u-1")
	if err != nil || len(mine) != 2 || mine[0].ID != "s2" {
		t.Fatalf("ListUserSessionsByUser: (%+v, %v)", mine, err)
	}

	// 滑动续期只动两列。
	if err := TouchUserSession("s1", 55, 9999); err != nil {
		t.Fatalf("TouchUserSession: %v", err)
	}
	if got, _ := GetUserSessionByID("s1"); got.LastActiveAt != 55 || got.ExpiresAt != 9999 || got.AuthVersion != 1 {
		t.Fatalf("TouchUserSession 写错了: %+v", got)
	}

	// 整行写回。
	s1.AuthVersion = 7
	if err := SaveUserSession(&s1); err != nil {
		t.Fatalf("SaveUserSession: %v", err)
	}
	if got, _ := GetUserSessionByID("s1"); got == nil || got.AuthVersion != 7 {
		t.Fatalf("SaveUserSession 没写进去: %+v", got)
	}

	// 退出其它设备：保留 keepID。
	n, err := DeleteUserSessionsExcept("u-1", "s1")
	if err != nil || n != 1 {
		t.Fatalf("DeleteUserSessionsExcept = (%d, %v)，want (1, nil)", n, err)
	}

	// 清理过期（s3 的 expires_at=5）。
	if n, err := DeleteExpiredUserSessions(100); err != nil || n != 1 {
		t.Fatalf("DeleteExpiredUserSessions = (%d, %v)，want (1, nil)", n, err)
	}

	// 单条删除幂等。
	if n, err := DeleteUserSession("s1"); err != nil || n != 1 {
		t.Fatalf("DeleteUserSession = (%d, %v)，want (1, nil)", n, err)
	}
	if n, err := DeleteUserSession("s1"); err != nil || n != 0 {
		t.Fatalf("二次删除应当幂等返回 0，实际 (%d, %v)", n, err)
	}
}

// TestUserRepoClosedDatabase 库没打开时每个入口都必须**响亮地**返回 ErrNotOpen，
// 而不是 panic（store.DB 是包级变量，漏接线时这正是第一现场）。
func TestUserRepoClosedDatabase(t *testing.T) {
	if err := Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := CountUsers(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("CountUsers = %v，want ErrNotOpen", err)
	}
	if _, err := GetUserByID("u-1"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("GetUserByID = %v，want ErrNotOpen", err)
	}
	if err := CreateUser(&User{ID: "u-1", Username: "x"}); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("CreateUser = %v，want ErrNotOpen", err)
	}
	if _, err := ImportUsersIfAbsent([]User{{ID: "u-1"}}); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("ImportUsersIfAbsent = %v，want ErrNotOpen", err)
	}
	if err := CreateUserSession(&UserSession{ID: "s1"}); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("CreateUserSession = %v，want ErrNotOpen", err)
	}
}
