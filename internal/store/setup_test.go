// setup_test.go 初始化单例 / 自愈 / 跨进程互斥的门禁。
//
// # 为什么"跨进程"必须是**真进程**而不是 goroutine
//
// 单例与互斥这两件事的判据全在数据库里：主键冲突、事务隔离、busy_timeout。
// 同进程里的两个 goroutine 共享同一个 *gorm.DB 连接池，很多竞争会被连接池
// 自己串行化掉 —— 于是哪怕实现里根本没有互斥，goroutine 版的测试也会是绿的。
// 那种测试证明不了任何事（这是最容易骗过自己的一类测试）。
//
// 所以本文件用 os.Executable 重新拉起**测试二进制本身**当子进程，N 个真进程
// 抢同一个库文件。这才是生产里"多副本同时首启"的形态。
package store

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// openTempDB 为单个测试开一个独立的库文件并建表。
//
// ⚠ 必须 Close：DB 是包级变量，不关掉的话下一个测试会在**同一个库**上跑，
// 于是"空库"这个前提静默失效（表现为一组看着莫名其妙的连锁失败）。
func openTempDB(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gw.db")
	if err := Open(path); err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

// countRows 数一张表的行数（用 Model 而不是原生 SQL，免得写死表名）。
func countRows(t *testing.T, model any) int64 {
	t.Helper()
	var n int64
	if err := DB.Model(model).Count(&n).Error; err != nil {
		t.Fatalf("count %T: %v", model, err)
	}
	return n
}

// TestIsSetupEmptyDatabase 验收 ①：空库 ⇒ false。
func TestIsSetupEmptyDatabase(t *testing.T) {
	openTempDB(t)

	ok, err := CheckSetup()
	if err != nil {
		t.Fatalf("CheckSetup: %v", err)
	}
	if ok {
		t.Fatal("空库不该报告「已初始化」")
	}
	if IsSetup() {
		t.Fatal("空库时 IsSetup 必须是 false")
	}
	if n := countRows(t, &Setup{}); n != 0 {
		t.Fatalf("空库不该有 setups 行，实际 %d 行", n)
	}
	if n := countRows(t, &User{}); n != 0 {
		t.Fatalf("空库不该有 users 行，实际 %d 行", n)
	}
}

// TestCheckSetupSelfHeals 验收 ②（自愈）。
//
// 场景就是真实会发生的那个：users 里已经有 role=100 的管理员，但 setups 表
// （这一版才有的表）是空的 —— 旧库迁移过来就是这样。CheckSetup 必须补上标记，
// 而且**只补一行**。
func TestCheckSetupSelfHeals(t *testing.T) {
	openTempDB(t)

	// 手工造那个"半初始化"的库：只有用户，没有标记。
	root := &User{
		ID: "u-0123456789abcdef", Username: "root", DisplayName: "root",
		PasswordHash: "x", Role: RoleRoot, Enabled: true, AuthVersion: 1, CreatedAt: 1,
	}
	if err := DB.Create(root).Error; err != nil {
		t.Fatalf("造 root 用户: %v", err)
	}
	if n := countRows(t, &Setup{}); n != 0 {
		t.Fatalf("前置条件不成立：已有 setups 行 %d 条", n)
	}

	ok, err := CheckSetup()
	if err != nil {
		t.Fatalf("CheckSetup: %v", err)
	}
	if !ok {
		t.Fatal("有 root 用户时 CheckSetup 必须返回 true（自愈）")
	}
	if n := countRows(t, &Setup{}); n != 1 {
		t.Fatalf("自愈后应当**有且仅有** 1 行 setups，实际 %d 行", n)
	}

	var got Setup
	if err := DB.First(&got).Error; err != nil {
		t.Fatalf("读回 setups: %v", err)
	}
	if got.ID != setupID {
		t.Fatalf("自愈行的主键应当是 %d（单例），实际 %d —— 自增主键会破坏互斥语义", setupID, got.ID)
	}

	// 再跑一次必须幂等：自愈不该每次都插一行。
	if _, err := CheckSetup(); err != nil {
		t.Fatalf("第二次 CheckSetup: %v", err)
	}
	if n := countRows(t, &Setup{}); n != 1 {
		t.Fatalf("自愈必须幂等，第二次跑完变成 %d 行", n)
	}
}

// TestInitializeSetupWritesEverything 正向：一次成功的初始化把该写的都写了。
func TestInitializeSetupWritesEverything(t *testing.T) {
	openTempDB(t)

	u, err := InitializeSetup("admin", "password123")
	if err != nil {
		t.Fatalf("InitializeSetup: %v", err)
	}
	if u == nil {
		t.Fatal("成功初始化必须返回建出来的用户")
	}
	if u.Role != RoleRoot {
		t.Fatalf("首个用户角色应当是 RoleRoot(%d)，实际 %d", RoleRoot, u.Role)
	}
	if u.PasswordHash == "" || u.PasswordHash == "password123" {
		t.Fatalf("口令必须被哈希后存储，实际 %q", u.PasswordHash)
	}
	if !u.Enabled {
		t.Fatal("首个管理员必须是启用状态")
	}
	if u.AuthVersion != 1 {
		t.Fatalf("AuthVersion 应当初始化成 1，实际 %d", u.AuthVersion)
	}
	if n := countRows(t, &Setup{}); n != 1 {
		t.Fatalf("初始化后应当有且仅有 1 行 setups，实际 %d 行", n)
	}
	if !IsSetup() {
		t.Fatal("初始化后 IsSetup 必须是 true")
	}
}

// TestInitializeSetupTwiceKeepsOptions 验收 ④：重复初始化报 ErrSetupAlreadyDone，
// 且 options 行数**不变**。
//
// # 为什么专门盯 options 行数
//
// 重复初始化最可能的 bug 不是"多插一个用户"（唯一索引会拦住），而是"事务里
// 那一串 upsert 又被跑了一遍"。若哪天 upsert 从 DO UPDATE 改成普通 INSERT，
// 或 setupOptionDefaults 里出现带随机值的键，行数/内容就会变 —— 而用户
// 重新点一次"初始化"是个很常见的误操作。
func TestInitializeSetupTwiceKeepsOptions(t *testing.T) {
	openTempDB(t)

	// 先手工放两条 option 进去，让"行数不变"这件事有东西可比。
	if err := PutOption("site_name", "gateway"); err != nil {
		t.Fatalf("PutOption: %v", err)
	}
	if err := PutOption("pool.max_in_flight", "3"); err != nil {
		t.Fatalf("PutOption: %v", err)
	}
	before := countRows(t, &Option{})
	usersBefore := countRows(t, &User{})

	if _, err := InitializeSetup("admin", "password123"); err != nil {
		t.Fatalf("首次 InitializeSetup: %v", err)
	}
	afterFirst := countRows(t, &Option{})

	_, err := InitializeSetup("other", "password456")
	if !errors.Is(err, ErrSetupAlreadyDone) {
		t.Fatalf("重复初始化应当返回 ErrSetupAlreadyDone，实际 %v", err)
	}
	if err == nil {
		t.Fatal("重复初始化不该成功")
	}

	if got := countRows(t, &Option{}); got != afterFirst {
		t.Fatalf("重复初始化后 options 行数从 %d 变成 %d —— 事务里那串 upsert 又被跑了一遍", afterFirst, got)
	}
	if afterFirst < before {
		t.Fatalf("首次初始化不该删 option：之前 %d 行，之后 %d 行", before, afterFirst)
	}
	if got := countRows(t, &User{}); got != usersBefore+1 {
		t.Fatalf("重复初始化不该再建账号：初始 %d 行，现在 %d 行（期望 +1）", usersBefore, got)
	}
	if n := countRows(t, &Setup{}); n != 1 {
		t.Fatalf("setups 必须是单例，实际 %d 行", n)
	}
}

// TestInitializeSetupValidation 参数校验在事务**之前**返回，且不改库。
func TestInitializeSetupValidation(t *testing.T) {
	openTempDB(t)

	cases := []struct {
		name     string
		username string
		password string
	}{
		{"用户名太短", "ab", "password123"},
		{"用户名太长", strings.Repeat("a", 33), "password123"},
		{"用户名含中文", "管理员", "password123"},
		{"用户名含空格", "ad min", "password123"},
		{"口令太短", "admin", "short"},
		{"口令含换行", "admin", "password\n123"},
		{"口令太长", "admin", strings.Repeat("x", 129)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := InitializeSetup(tc.username, tc.password); err == nil {
				t.Fatalf("非法参数 %q/%q 应当被拒", tc.username, tc.password)
			}
			if n := countRows(t, &Setup{}); n != 0 {
				t.Fatalf("校验失败不该写 setups 行，实际 %d 行", n)
			}
			if n := countRows(t, &User{}); n != 0 {
				t.Fatalf("校验失败不该建用户，实际 %d 行", n)
			}
		})
	}
}

// TestInitializeSetupSelfHealsRoot 已有 root、无标记时初始化：只补标记不重建号。
func TestInitializeSetupSelfHealsRoot(t *testing.T) {
	openTempDB(t)

	existing := &User{
		ID: "u-aaaaaaaaaaaaaaaa", Username: "legacy", DisplayName: "legacy",
		PasswordHash: "x", Role: RoleRoot, Enabled: true, AuthVersion: 1, CreatedAt: 1,
	}
	if err := DB.Create(existing).Error; err != nil {
		t.Fatalf("造 root: %v", err)
	}

	u, err := InitializeSetup("admin", "password123")
	if err != nil {
		t.Fatalf("InitializeSetup: %v", err)
	}
	if u == nil || u.Username != "legacy" {
		t.Fatalf("应当返回**已存在**的 root（legacy），实际 %+v", u)
	}
	if n := countRows(t, &User{}); n != 1 {
		t.Fatalf("不该再建账号，实际 %d 行", n)
	}
	if n := countRows(t, &Setup{}); n != 1 {
		t.Fatalf("应当补上 1 行 setups，实际 %d 行", n)
	}
}

// TestSetupClosedDatabase DB 未打开时的行为：错误而不是 panic。
func TestSetupClosedDatabase(t *testing.T) {
	if err := Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if IsSetup() {
		t.Fatal("库没打开时 IsSetup 必须返回 false（安全方向）")
	}
	if _, err := CheckSetup(); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("CheckSetup 应当返回 ErrNotOpen，实际 %v", err)
	}
	if _, err := InitializeSetup("admin", "password123"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("InitializeSetup 应当返回 ErrNotOpen，实际 %v", err)
	}
}

// ── 跨进程互斥（验收 ③） ────────────────────────────────────────────────

// TestHelperSetupProcess 是**子进程入口**，不是给 go test 直接跑的用例。
//
// 父进程用 -test.run=^TestHelperSetupProcess$ 重新拉起测试二进制，并通过环境
// 变量告诉它：库在哪、用哪个用户名。退出码就是结论（见下）。
//
// ⚠ 没有 STORE_SETUP_CHILD_DB 时必须立刻返回 —— 否则 go test ./internal/store
// 会把父进程里的所有测试都"再跑一遍"，而那是另一件完全不同的事。
func TestHelperSetupProcess(t *testing.T) {
	dbPath := os.Getenv("STORE_SETUP_CHILD_DB")
	if dbPath == "" {
		t.Skip("不是子进程（无 STORE_SETUP_CHILD_DB），跳过")
	}
	user := os.Getenv("STORE_SETUP_CHILD_USER")

	if err := Open(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "child Open: %v\n", err)
		os.Exit(11)
	}
	if err := Migrate(); err != nil {
		fmt.Fprintf(os.Stderr, "child Migrate: %v\n", err)
		os.Exit(12)
	}
	_, err := InitializeSetup(user, "password123")
	switch {
	case err == nil:
		os.Exit(0) // 初始化成功
	case errors.Is(err, ErrSetupAlreadyDone):
		os.Exit(3) // 已经被别人初始化了（正确结果）
	default:
		fmt.Fprintf(os.Stderr, "child InitializeSetup: %v\n", err)
		os.Exit(13)
	}
}

// TestInitializeSetupConcurrentProcesses 验收 ③：N 个**真进程**同时初始化，
// 有且只有一个成功。
//
// # 判据为什么是"退出码之和"而不是"看日志"
//
// 每个子进程用退出码回答：0=我初始化成功了，3=别人已经初始化了。
// 于是"恰好一个 0、其余全是 3"是一个**二元可判**的事实 —— 不需要解析任何文本。
//
// # 为什么要先让子进程都起来再等结果
//
// 串行执行会退化成"第一个建、其余的看到标记" —— 那测的是幂等，不是互斥。
// 这里用 Start 全部拉起、再逐一 Wait，让它们的 BEGIN 尽量重叠。
func TestInitializeSetupConcurrentProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("跨进程测试需要拉起子进程，-short 下跳过")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试二进制路径: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "race.db")

	const procs = 4
	var cmds []*exec.Cmd
	for i := 0; i < procs; i++ {
		cmd := exec.Command(exe, "-test.run=^TestHelperSetupProcess$", "-test.v")
		cmd.Env = append(os.Environ(),
			"STORE_SETUP_CHILD_DB="+dbPath,
			"STORE_SETUP_CHILD_USER=root"+strconv.Itoa(i),
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动子进程 %d: %v", i, err)
		}
		cmds = append(cmds, cmd)
	}

	codes := make([]int, procs)
	var wg sync.WaitGroup
	for i, cmd := range cmds {
		wg.Add(1)
		go func(i int, cmd *exec.Cmd) {
			defer wg.Done()
			err := cmd.Wait()
			if err == nil {
				codes[i] = 0
				return
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				codes[i] = ee.ExitCode()
				return
			}
			codes[i] = -1
		}(i, cmd)
	}
	wg.Wait()

	success, done := 0, 0
	for i, c := range codes {
		switch c {
		case 0:
			success++
		case 3:
			done++
		default:
			t.Errorf("子进程 %d 异常退出 %d（期望 0 或 3）", i, c)
		}
	}
	if success != 1 {
		t.Fatalf("%d 个进程同时初始化，成功数 = %d（期望恰好 1）；退出码 %v", procs, success, codes)
	}
	if done != procs-1 {
		t.Fatalf("其余 %d 个进程应当拿到 ErrSetupAlreadyDone，实际 %d 个；退出码 %v",
			procs-1, done, codes)
	}

	// 库里的最终状态：仍然是**单例**，且只有一个用户。
	if err := Open(dbPath); err != nil {
		t.Fatalf("打开竞争后的库: %v", err)
	}
	defer func() { _ = Close() }()
	if n := countRows(t, &Setup{}); n != 1 {
		t.Fatalf("竞争结束后 setups 必须是 1 行，实际 %d 行", n)
	}
	if n := countRows(t, &User{}); n != 1 {
		t.Fatalf("竞争结束后 users 必须是 1 行（只该有一个赢家建号），实际 %d 行", n)
	}
}
