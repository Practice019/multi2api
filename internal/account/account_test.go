package account

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestStore 在临时目录里起一个账号库（绝不碰仓库里的 data/）。
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, path
}

// TestCreateAndAuthenticate 正例 3 条 + 反例 5 条。
func TestCreateAndAuthenticate(t *testing.T) {
	s, _ := newTestStore(t)

	u, err := s.Create("Alice", "correct-horse-battery", RoleMember)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.Role != RoleMember {
		t.Errorf("新账号角色应为成员，得到 %d", u.Role)
	}
	if !u.Enabled {
		t.Error("新账号应当默认启用")
	}
	if u.AuthVersion != 1 {
		t.Errorf("AuthVersion 初值应为 1，得到 %d", u.AuthVersion)
	}
	if !strings.HasPrefix(u.ID, "u-") {
		t.Errorf("ID 形态应为 u-<hex>，得到 %q", u.ID)
	}

	admin, err := s.Create("root", "root-password-123", RoleAdmin)
	if err != nil {
		t.Fatalf("Create admin: %v", err)
	}
	if admin.Role != RoleAdmin {
		t.Errorf("管理员角色应为 %d，得到 %d", RoleAdmin, admin.Role)
	}

	// 正例
	if got, err := s.Authenticate("Alice", "correct-horse-battery"); err != nil || got.ID != u.ID {
		t.Errorf("正例 1（原样用户名）失败: id=%v err=%v", got, err)
	}
	if got, err := s.Authenticate("alice", "correct-horse-battery"); err != nil || got.ID != u.ID {
		t.Errorf("正例 2（大小写不同）应命中同一账号: %v", err)
	}
	if got, err := s.Authenticate("  Alice  ", "correct-horse-battery"); err != nil || got.ID != u.ID {
		t.Errorf("正例 3（首尾空白）应命中同一账号: %v", err)
	}

	// 反例
	for _, tc := range []struct {
		name, user, pass string
		want             error
	}{
		{"口令错误", "Alice", "wrong-password-x", ErrCredentials},
		{"用户不存在", "nobody", "correct-horse-battery", ErrCredentials},
		{"用户不存在且口令为空", "nobody2", "", ErrCredentials},
		{"大小写不同但口令错", "ALICE", "wrong-password-x", ErrCredentials},
		{"空用户名", "", "correct-horse-battery", ErrCredentials},
	} {
		if _, err := s.Authenticate(tc.user, tc.pass); !errors.Is(err, tc.want) {
			t.Errorf("%s：应得 %v，得到 %v", tc.name, tc.want, err)
		}
	}
}

// TestPasswordNotInPlaintextOnDisk 落盘内容不含明文口令，且原子写不留残余。
func TestPasswordNotInPlaintextOnDisk(t *testing.T) {
	s, path := newTestStore(t)
	const plain = "s3cret-plaintext-check"
	if _, err := s.Create("bob", plain, RoleMember); err != nil {
		t.Fatalf("Create: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读账号库: %v", err)
	}
	if strings.Contains(string(raw), plain) {
		t.Error("★ 明文口令出现在 accounts.json 里")
	}
	if !strings.Contains(string(raw), "$2a$") && !strings.Contains(string(raw), "$2b$") {
		t.Error("文件里找不到 bcrypt 哈希（$2a$/$2b$ 前缀）")
	}
	// 原子写：临时文件必须已被 rename 掉
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("★ 残留了临时文件 %s.tmp", path)
	}
	// 文件必须仍是合法 JSON
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Errorf("落盘内容不是合法 JSON: %v", err)
	}
	// 权限：不能被组/其他用户读。
	//
	// ⚠ 只在 Unix 上判：Windows 上 Go 的文件系统抽象**不**按 unix 权限位
	// 工作（它用 ACL），Stat 一律报 0666，与传给 WriteFile 的 0600 无关。
	// 不加这个分支的话，这条断言在开发机（Windows）上必红、
	// 在 CI（Linux）上才是真有意义的 —— 而"本地红的测试"很快就会被无视。
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("★ 账号库权限过宽: %v", info.Mode().Perm())
		}
	}
}

// TestSetPasswordBumpsAuthVersion 改口令后旧口令失效、版本号前进。
func TestSetPasswordBumpsAuthVersion(t *testing.T) {
	s, _ := newTestStore(t)
	u, err := s.Create("carol", "old-password-111", RoleMember)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	next, err := s.SetPassword(u.ID, "new-password-222")
	if err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if next != u.AuthVersion+1 {
		t.Errorf("AuthVersion 应前进到 %d，得到 %d", u.AuthVersion+1, next)
	}
	if _, err := s.Authenticate("carol", "old-password-111"); !errors.Is(err, ErrCredentials) {
		t.Errorf("旧口令应当失效，得到 %v", err)
	}
	if _, err := s.Authenticate("carol", "new-password-222"); err != nil {
		t.Errorf("新口令应当可用，得到 %v", err)
	}

	bumped, err := s.BumpAuthVersion(u.ID)
	if err != nil {
		t.Fatalf("BumpAuthVersion: %v", err)
	}
	if bumped != next+1 {
		t.Errorf("再次提升后应为 %d，得到 %d", next+1, bumped)
	}
}

// TestDisabledCannotLogin 禁用后不能登录（但账号仍在库里）。
func TestDisabledCannotLogin(t *testing.T) {
	s, _ := newTestStore(t)
	u, err := s.Create("dave", "dave-password-333", RoleMember)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SetEnabled(u.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if _, err := s.Authenticate("dave", "dave-password-333"); !errors.Is(err, ErrDisabled) {
		t.Errorf("禁用账号应得 ErrDisabled，得到 %v", err)
	}
	if s.Count() != 1 {
		t.Error("禁用不应删号")
	}
}

// TestViewsOmitPasswordHash 对外视图里不能出现哈希或版本号。
func TestViewsOmitPasswordHash(t *testing.T) {
	s, _ := newTestStore(t)
	u, err := s.Create("erin", "erin-password-444", RoleAdmin)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, err := json.Marshal(s.List())
	if err != nil {
		t.Fatalf("Marshal List: %v", err)
	}
	for _, banned := range []string{"password_hash", "$2a$", "$2b$", "auth_version", "PasswordHash"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("★ 对外列表里出现了 %q：%s", banned, string(raw))
		}
	}
	// 但内部取用（登录/签发会话）必须拿得到哈希
	if got := s.ByID(u.ID); got == nil || got.PasswordHash == "" {
		t.Error("ByID 应当返回带哈希的副本（内部用）")
	}
	if v, ok := s.Snapshot(u.ID); !ok || v.RoleName != "管理员" {
		t.Errorf("Snapshot 应给出角色中文名，得到 %+v ok=%v", v, ok)
	}
}

// TestRoleAndInputValidation 角色与输入校验的边界。
func TestRoleAndInputValidation(t *testing.T) {
	s, _ := newTestStore(t)

	if _, err := s.Create("frank", "frank-password-555", 7); !errors.Is(err, ErrBadRole) {
		t.Errorf("未知角色应被拒，得到 %v", err)
	}
	for _, name := range []string{"ab", strings.Repeat("x", 33), "has space", "零宽\u200b字符", "emoji🙂"} {
		if _, err := s.Create(name, "frank-password-555", RoleMember); !errors.Is(err, ErrBadUsername) {
			t.Errorf("用户名 %q 应被拒，得到 %v", name, err)
		}
	}
	for _, pass := range []string{"short1", strings.Repeat("p", 129)} {
		if _, err := s.Create("frank", pass, RoleMember); !errors.Is(err, ErrBadPassword) {
			t.Errorf("口令长度 %d 应被拒，得到 %v", len(pass), err)
		}
	}
	if _, err := s.Create("frank", "frank-password-555", RoleMember); err != nil {
		t.Fatalf("合法账号被拒: %v", err)
	}
	if _, err := s.Create("FRANK", "other-password-666", RoleMember); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("同名不同大小写应判重，得到 %v", err)
	}
	if _, err := s.Create("  frank  ", "other-password-666", RoleMember); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("同名带空白应判重，得到 %v", err)
	}
	if err := s.SetRole("u-nonexistent", RoleAdmin); !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在的账号应得 ErrNotFound，得到 %v", err)
	}
}

// TestReopenKeepsAccounts 重开（模拟重启）后账号仍在、仍能登录。
func TestReopenKeepsAccounts(t *testing.T) {
	s, path := newTestStore(t)
	u, err := s.Create("grace", "grace-password-777", RoleAdmin)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	again, err := New(path)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	if again.Count() != 1 {
		t.Fatalf("重开后账号数应为 1，得到 %d", again.Count())
	}
	got, err := again.Authenticate("grace", "grace-password-777")
	if err != nil {
		t.Fatalf("重开后登录失败: %v", err)
	}
	if got.ID != u.ID || got.Role != RoleAdmin {
		t.Errorf("重开后账号内容不一致: %+v", got)
	}
}

// TestCorruptFileReportsError 文件损坏时报错，不静默重建（否则等于把库清空）。
func TestCorruptFileReportsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil {
		t.Error("★ 损坏的账号库应当报错，而不是静默重建")
	}
}
