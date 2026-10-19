package account

import (
	"encoding/json"
	"errors"
	"fmt"
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

// boot 建**唯一**那个管理员（单账号口径下唯一的建号路径）。
//
// ⚠ 2026-10-19 语义变更：这里原先叫 `s.Create(name, pass, role)`。
// Create 整条删了（它带 role 参数 ⇒ 是一条能建出非管理员账号的路径，
// 而要求 1 明说不许有），所以所有"造一个账号"的测试改成 bootstrap。
func boot(t *testing.T, s *Store, name, pass string) *User {
	t.Helper()
	u, err := s.CreateBootstrap(name, pass)
	if err != nil {
		t.Fatalf("CreateBootstrap(%s): %v", name, err)
	}
	return u
}

// accountRow 造一条**原始盘上记录**（口令用真 bcrypt 哈希）。
//
// 为什么需要它：要求 3 要测的形态是"盘上有改造前留下的非管理员记录"，
// 而单账号口径下**没有**任何 API 能造出第二条记录（那正是目标态 1）。
// 所以测试直接写文件 —— 那也是那条路径的真实复刻（旧版本写出来的就是它）。
func accountRow(t *testing.T, name, plain string, role int) map[string]any {
	t.Helper()
	hash, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return map[string]any{
		"id":            newID(),
		"username":      name,
		"display_name":  name,
		"password_hash": hash,
		"role":          role,
		"enabled":       true,
		"auth_version":  1,
		"created_at":    nowStamp(),
	}
}

// seedAccountsFile 直接往账号文件里写记录（**不经过任何建号 API**）。
func seedAccountsFile(t *testing.T, path string, rows ...map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写账号库: %v", err)
	}
}

// TestBootstrapIsTheOnlyCreationPath 唯一建号路径：空库成功、非空库拒。
//
// 这条对应目标态 1（只有管理员）与目标态 2（注册只在首次）在**存储层**
// 的那一半：能建号的地方只有 CreateBootstrap，而它写死 RoleAdmin、
// 且只认空库。
func TestBootstrapIsTheOnlyCreationPath(t *testing.T) {
	s, _ := newTestStore(t)

	u, err := s.CreateBootstrap("Alice", "correct-horse-battery")
	if err != nil {
		t.Fatalf("CreateBootstrap: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Errorf("★ 唯一的建号路径必须是管理员，得到角色 %d", u.Role)
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

	// ★ 库非空 ⇒ 再建一律 ErrBootstrapDone（**这就是"只有一个账号"**）。
	// 注意这里**不是** ErrUsernameTaken：判定的顺序是"先看库空不空"，
	// 库非空时根本走不到"重名"那一步。
	_, err2 := s.CreateBootstrap("Bob", "another-password-x")
	if !errors.Is(err2, ErrBootstrapDone) {
		t.Errorf("库非空时再建号应得 ErrBootstrapDone，得到 %v", err2)
	}
	t.Logf("证据：空库 CreateBootstrap → 角色=%d（RoleAdmin=%d，RoleName=%q）；再次 → err=%v",
		u.Role, RoleAdmin, RoleName(u.Role), err2)
	if s.Count() != 1 {
		t.Errorf("★ 库里必须只有 1 个账号，得到 %d", s.Count())
	}

	// 正例（原 TestCreateAndAuthenticate 的认证部分，语义不变）
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
	boot(t, s, "bob", plain)

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
	u := boot(t, s, "carol", "old-password-111")

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
	u := boot(t, s, "dave", "dave-password-333")
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
	u := boot(t, s, "erin", "erin-password-444")
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

// TestRoleModelIsSingleAdmin 角色模型的判据只有一处：IsAdminRole。
//
// ⚠ 语义变更（2026-10-19 单账号口径）：这条测试原先叫
// TestRoleAndInputValidation，做三件事 —— ① 未知角色被 Create 拒
// ② 用户名/口令边界 ③ SetRole 改不存在的账号。
//
//	①③ 测的那个面**已经不存在**：Create（带 role 参数）与 SetRole 两条
//	   API 都删了（它们是"能建出/改成非管理员"的路径）。删掉一条测试要说明
//	   理由 —— 这就是理由。
//	② 保留了下来，见 TestBootstrapInputValidation。
//
// 换成了更强的东西：把"什么角色能构成身份"钉死，并断言**成员值不再能构成
// 身份**（改造前它是可以登录的）。
func TestRoleModelIsSingleAdmin(t *testing.T) {
	if !IsAdminRole(RoleAdmin) {
		t.Error("RoleAdmin 必须能构成身份")
	}
	if IsAdminRole(RoleMember) {
		t.Error("★ RoleMember 不得构成身份 —— 成员角色已取消（改造前它可以登录）")
	}
	for _, r := range []int{0, 2, 7, -1} {
		if IsAdminRole(r) {
			t.Errorf("★ 角色 %d 不是管理员，不得构成身份", r)
		}
	}
	// ValidRole 仍要认识 RoleMember：旧数据要被**认出来再拒掉**，
	// 而不是落到含混的"未知角色"上（文案与日志都指着它）。
	if !ValidRole(RoleMember) || !ValidRole(RoleAdmin) {
		t.Error("ValidRole 应当认识两个历史值（只读判据）")
	}
	if ValidRole(7) {
		t.Error("ValidRole(7) 应当为假")
	}
	if RoleName(RoleMember) == RoleName(7) {
		t.Error("成员必须与未知角色有不同的显示名（日志要能点名它是成员旧记录）")
	}
}

// TestBootstrapInputValidation 用户名/口令的边界（走唯一那条建号路径）。
//
// ⚠ 语义变更说明：原先这几条是打 `Store.Create` 的。Create 删了之后
// 同一批判据改打 CreateBootstrap —— **没有放宽**（同一份 validateUsername /
// validatePassword）。
//
// 两条断言**消失**了，各说明理由：
//
//	"同名不同大小写判重"（原 ErrUsernameTaken）
//	    库非空时先撞 ErrBootstrapDone（见 TestBootstrapIsTheOnlyCreationPath），
//	    而库为空时不可能重名 ⇒ 这个反例在单账号口径下**不可达**。
//	    ErrUsernameTaken 保留在错误表里没删（users 的唯一性仍是存储的不变量，
//	    且它是 writeAccountError 映射表的一行），但测试不再假装能触发它。
//	"SetRole 改不存在的账号 ⇒ ErrNotFound"
//	    SetRole 已删除（它是一条能把管理员降级成成员的路径）。
func TestBootstrapInputValidation(t *testing.T) {
	for _, name := range []string{"ab", strings.Repeat("x", 33), "has space", "零宽\u200b字符", "emoji🙂"} {
		s, _ := newTestStore(t)
		if _, err := s.CreateBootstrap(name, "frank-password-555"); !errors.Is(err, ErrBadUsername) {
			t.Errorf("用户名 %q 应被拒，得到 %v", name, err)
		}
	}
	for _, pass := range []string{"short1", strings.Repeat("p", 129)} {
		s, _ := newTestStore(t)
		if _, err := s.CreateBootstrap("frank", pass); !errors.Is(err, ErrBadPassword) {
			t.Errorf("口令长度 %d 应被拒，得到 %v", len(pass), err)
		}
	}
	// 合法账号必须建得出来（否则上面那些"被拒"可能只是"全都建不出来"）
	s, _ := newTestStore(t)
	boot(t, s, "frank", "frank-password-555")
	if s.Count() != 1 {
		t.Fatalf("合法账号应当建出 1 个，得到 %d", s.Count())
	}
}

// TestReopenKeepsAccounts 重开（模拟重启）后账号仍在、仍能登录。
func TestReopenKeepsAccounts(t *testing.T) {
	s, path := newTestStore(t)
	u := boot(t, s, "grace", "grace-password-777")
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

// ★★ TestLegacyMemberRecordIsRefusedWithoutCrash 目标态 3 的正面判据。
//
// 盘上放一条**非管理员**的历史记录（改造前建出来的成员，role=1），要求：
//
//	① 加载**不崩**（New 不报错，也不静默把那条记录改成管理员）
//	② 口令正确也**不许当管理员放进来**（ErrMemberRetired）
//	③ 口令错误仍是 ErrCredentials —— 不然错误码本身就成了一枚举口子
//	④ 同库里那个真管理员（role=10）照常能登录（旧数据兼容不是"整库作废"）
//	⑤ 角色值缺失/手写成别的数（7）也按**非管理员**处理（fail-closed）
func TestLegacyMemberRecordIsRefusedWithoutCrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	seedAccountsFile(t, path,
		accountRow(t, "boss", "boss-password-001", RoleAdmin),
		accountRow(t, "staff", "staff-password-002", RoleMember),
		accountRow(t, "weird", "weird-password-003", 7),
	)

	s, err := New(path)
	if err != nil {
		t.Fatalf("★ 盘上有非管理员历史记录时 New 不得报错（不许崩），得到 %v", err)
	}
	if s.Count() != 3 {
		t.Fatalf("三条记录都该被认下来（不丢数据、不静默重写），得到 %d", s.Count())
	}

	// ② 成员：口令对也不行
	_, errMember := s.Authenticate("staff", "staff-password-002")
	if !errors.Is(errMember, ErrMemberRetired) {
		t.Errorf("★ 成员历史记录口令正确时也必须拒（ErrMemberRetired），得到 %v", errMember)
	}
	t.Logf("证据：盘上成员记录（role=%d）口令正确 → err=%v；管理员（role=%d）→ 登录成功",
		RoleMember, errMember, RoleAdmin)
	// ②b 角色值手写成 7 的那条同样不构成身份
	if _, err := s.Authenticate("weird", "weird-password-003"); !errors.Is(err, ErrMemberRetired) {
		t.Errorf("★ 未知角色（读盘归一成成员）也必须拒，得到 %v", err)
	}
	// ③ 口令错 ⇒ 仍是 ErrCredentials：不给"这个用户名存在吗"的探测信号
	if _, err := s.Authenticate("staff", "wrong-password-zz"); !errors.Is(err, ErrCredentials) {
		t.Errorf("★ 成员记录配错口令必须是 ErrCredentials（不能泄露它存在），得到 %v", err)
	}
	// ④ 同库的管理员照常
	admin, err := s.Authenticate("boss", "boss-password-001")
	if err != nil {
		t.Fatalf("★ 同库的真管理员必须仍能登录，得到 %v", err)
	}
	if !IsAdminRole(admin.Role) {
		t.Errorf("管理员登录后必须能构成身份，得到角色 %d", admin.Role)
	}

	// ⑥ 盘上的记录没有被悄悄改写（New 只在内存里归一化）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("账号库仍是合法 JSON: %v", err)
	}
	if n, _ := rows[1]["role"].(float64); int(n) != RoleMember {
		t.Errorf("★ New 不该改写盘上的角色值（要求「不静默重建」），得到 %v", rows[1]["role"])
	}

	// ⑦ 启动日志点名了这批记录（运维要知道盘上有它们）
	got := retiredUsernames(s.users)
	joined := strings.Join(got, ",")
	if strings.Contains(joined, "boss(") {
		t.Errorf("★ 管理员不该出现在「已退役」名单里：%v", got)
	}
	if !strings.Contains(joined, "staff(成员（已取消）)") {
		t.Errorf("退役名单应当点名 staff 与它的角色名，得到 %v", got)
	}
	if !strings.Contains(joined, "weird(") {
		t.Errorf("退役名单应当点名 weird，得到 %v", got)
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

// ---------------------------------------------------------------------------
// 类级静态判据：全仓不许再有"角色分裂"的代码路径
// ---------------------------------------------------------------------------

// roleSplitResidue 纯函数：从一段 Go 源码里挑出角色分裂的残留。
//
// 抽成纯函数是为了可测（同 internal/gateway/arch_test.go 的做法）：
// 一个永远为绿的判据等于没有判据，所以下面用**好桩/坏桩**反向验证它。
func roleSplitResidue(file, src string) []string {
	var out []string
	for i, line := range strings.Split(src, "\n") {
		at := fmt.Sprintf("%s:%d", file, i+1)
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") {
			continue // 注释里提到这些名字是允许的（本文件的说明文字就很多）
		}
		switch {
		case strings.Contains(line, "RoleMember") && !allowedRoleMemberFile(file):
			out = append(out, at+": 出现 RoleMember（成员角色已取消：只读兼容值只允许留在 account.go / store.go）")
		case strings.Contains(line, "SetRole"):
			out = append(out, at+": 出现 SetRole（改角色的口子已删除 —— 它是能把管理员降级的路径）")
		case strings.Contains(line, `json:"member"`) || strings.Contains(line, `"member":`):
			out = append(out, at+": 出现 member 字段（载荷里的成员标记已删除）")
		case strings.Contains(line, "register_disabled"):
			out = append(out, at+": 出现 register_disabled（旧语义已换成 register_closed）")
		}
	}
	return out
}

// allowedRoleMemberFile 允许出现 RoleMember 的**非测试**源码文件。
//
// 只有两个，各有明确职责：
//
//	account.go  只读兼容常量 + RoleName（文案要一个名字）+ ValidRole（读盘判据）
//	store.go    读盘归一化：畸形角色落到最小权限（成员），从而**被认出来再拒掉**
func allowedRoleMemberFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	return rel == "internal/account/account.go" || rel == "internal/account/store.go"
}

// TestNoRoleSplitResidueInSource ★ 全仓扫描：角色分裂的残留必须为零。
//
// 判据（只扫 internal/ 与 cmd/ 下的**非测试** .go 文件）：
//
//	① RoleMember 只允许出现在上面那两个文件里
//	② 不许出现 SetRole（改角色的口子）
//	③ 不许出现 `json:"member"` / `"member":`（载荷里的成员标记）
//	④ 不许出现 register_disabled（旧语义的稳定码）
//
// 这条与"单账号口径"是同一件事的两面：目标态 1 说"没有任何代码路径会创建
// 非管理员账号"，这句话在源码上是**可判**的 —— 上面四个串一旦出现在别处，
// 就说明有人又把那条路径加回来了（或在载荷里又加了一个成员分支）。
func TestNoRoleSplitResidueInSource(t *testing.T) {
	root := repoRoot(t)
	var files []string
	for _, sub := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, sub), func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			files = append(files, p)
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s: %v", sub, err)
		}
	}
	if len(files) < 50 {
		t.Fatalf("★ 只扫到 %d 个源文件 —— 判据在**空转**（路径推导可能错了）", len(files))
	}
	var hits []string
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读 %s: %v", p, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = p
		}
		hits = append(hits, roleSplitResidue(rel, string(raw))...)
	}
	// 判据空转守卫：它**必须**认得 bad stub，否则上面的"0 命中"什么都不说明
	if len(roleSplitResidue("x.go", "u.Role = RoleMember\ns.SetRole(id, 1)\n\"member\": true\n\"register_disabled\"\n")) != 4 {
		t.Fatal("★ 判据空转：合成坏桩应当命中 4 条")
	}
	if len(roleSplitResidue("internal/account/store.go", "u.Role = RoleMember")) != 0 {
		t.Fatal("判据误报：允许的文件被判红了")
	}
	if len(hits) > 0 {
		t.Errorf("★ 全仓出现 %d 处角色分裂残留：\n  %s", len(hits), strings.Join(hits, "\n  "))
	}
}

// repoRoot 找仓库根目录（go.mod 所在处）。本包位于 <root>/internal/account。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("找不到 go.mod（推断的根目录 %s 不对）: %v", root, err)
	}
	return root
}
