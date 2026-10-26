// main_test.go 迁移工具的门禁测试。
//
// # 测试的落点（对应任务书的 6 条验收）
//
//	TestImportSkipsBadRows          坏行进 Reasons、其余全进，且打印「导入N/跳过M」
//	TestImportIdempotent            跑两次行数不变（幂等）
//	TestVerifyAfterImport           导完立刻 verify ⇒ 行数一致
//	TestExportByteIdentical         export 出来的 accounts.json 与迁移前**逐字节等价**
//	TestExportRefusesDataDir        拒绝把导出写进 data 目录（只读承诺的守门人）
//	TestImportLeavesJSONUntouched   全程不删不改任何 data/*.json
//	TestStateFileSkipped            state.json 一律跳过且跳过是**可见的**
//	TestExportStableOrdering        同秒记录的顺序稳定（字节等价的前提）
//
// ⚠ 这些测试**共用同一个 store.DB 包级句柄**，所以只能跑串行：
// 需要并行的测试必须自己 store.Close() 后再开自己的库。t.Parallel() 在这里
// 会互相踩指针，别加。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/store"
)

// ── 夹具 ────────────────────────────────────────────────────────────────

// ⚠ 夹具的**缩进必须与写盘格式逐字一致**（两个空格一级、无末尾换行）。
//
// 这不是为了好看：TestExportByteIdentical 拿它做**字节**比对，
// 缩进差一个空格测试就红。真实 data/accounts.json 就是 json.MarshalIndent
// 的输出形态（internal/account/store.go 的 saveLocked），照抄它才有意义。
const fixtureAccountA = `{
    "id": "u-aaaaaaaaaaaaaaaa",
    "username": "alice",
    "display_name": "Alice",
    "password_hash": "$2a$12$aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "role": 100,
    "enabled": true,
    "auth_version": 1,
    "created_at": "2026-10-19T15:18:17Z",
    "last_login_at": "2026-10-25T15:28:45Z",
    "last_login_ip": "127.0.0.1"
  }`

// 第二条：**没有** last_login_* 两键 —— 用来验证 omitempty 路径
//（导出时不该凭空多出两个空字符串键，否则字节不等）。
const fixtureAccountB = `{
    "id": "u-bbbbbbbbbbbbbbbb",
    "username": "bob",
    "display_name": "Bob",
    "password_hash": "$2a$12$bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "role": 10,
    "enabled": true,
    "auth_version": 3,
    "created_at": "2026-10-20T01:02:03Z"
  }`

const fixtureGoodAccounts = "[
  " + fixtureAccountA + ",
  " + fixtureAccountB + "
]"

// fixtureBadAccounts 一条好、一条**坏**（username 为空 ⇒ 会撞 UNIQUE）、一条好。
//
// 坏行落在中间是刻意的：放在末尾的话，"只处理了第一条就停"的错误实现也能通过。
var fixtureBadAccounts = "[
  " + fixtureAccountA + ",
  " + `{
    "id": "u-cccccccccccccccc",
    "username": "",
    "created_at": "2026-10-21T00:00:00Z"
  }` + ",
  " + fixtureAccountB + "
]"

const fixtureSessions = `[
  {
    "sid": "s-0001",
    "user_id": "u-aaaaaaaaaaaaaaaa",
    "username": "alice",
    "role": 100,
    "auth_version": 1,
    "version": 1,
    "created_at": 1792821401,
    "last_active_at": 1792907799,
    "expires_at": 1792994199,
    "hard_expires_at": 1795413401,
    "ip": "127.0.0.1",
    "ua": "Mozilla/5.0"
  }
]`

const fixtureState = `{
  "accounts": {
    "some-uid": {"provider": "workbuddy", "credits": 100}
  }
}`

// fixtureConfig 只放**一个**可热改键：够了，测的是委托语义不是 spec 表。
const fixtureConfig = `{
  "service_name": "test-gw"
}`

// env 一个临时用例的上下文。
type env struct {
	dir     string // 临时 data 目录
	dataDir string
	dbPath  string
	outDir  string
}

// newEnv 建一个临时 data 目录 + 关闭上一个用例留下的包级 DB。
//
// ⚠ 必须 store.Close()：store.DB 是**包级变量**，上一个用例的库不清掉，
// 下一个用例会捡到它（表现为"表格里凭空多出上一个用例的行"）。
// t.Cleanup 保证失败路径也清。
func newEnv(t *testing.T) *env {
	t.Helper()
	_ = store.Close()
	root := t.TempDir()
	e := &env{
		dir:     root,
		dataDir: filepath.Join(root, "data"),
		outDir:  filepath.Join(root, "export"),
	}
	if err := os.MkdirAll(e.dataDir, 0o755); err != nil {
		t.Fatalf("建临时 data 目录: %v", err)
	}
	e.dbPath = filepath.Join(e.dataDir, defaultDBName)
	t.Cleanup(func() { _ = store.Close() })
	return e
}

// write 往 data 目录写一个文件。
func (e *env) write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(e.dataDir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("写夹具 %s: %v", name, err)
	}
	return p
}

// digest 算 data 目录下所有文件的指纹（用于"不删不改"的断言）。
func (e *env) digest(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, err := os.ReadDir(e.dataDir)
	if err != nil {
		t.Fatalf("读 data 目录: %v", err)
	}
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(e.dataDir, ent.Name()))
		if err != nil {
			t.Fatalf("读 %s: %v", ent.Name(), err)
		}
		sum := sha256.Sum256(raw)
		out[ent.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

// runCmd 跑一次子命令，返回退出码与两个输出缓冲。
func runCmd(args ...string) (code int, stdout, stderr string) {
	var ob, eb bytes.Buffer
	code = run(args, &ob, &eb)
	return code, ob.String(), eb.String()
}

// mustImport 断言 import 成功。
func mustImport(t *testing.T, e *env, extra ...string) (stdout, stderr string) {
	t.Helper()
	args := append([]string{"import", "--data", e.dataDir, "--db", e.dbPath}, extra...)
	code, out, errOut := runCmd(args...)
	if code != exitOK {
		t.Fatalf("import 退出码 = %d，期望 %d。stdout=%q stderr=%q", code, exitOK, out, errOut)
	}
	return out, errOut
}

// reopenDB 在命令跑完后重新打开库。
//
// ⚠ 这是必需的，不是保险：import/verify/export 三个子命令都在 defer 里调
// store.Close()，而 Close 会把**包级 DB 置空**。测试若在命令返回后直接
// store.DB.Model(...) 就是一次 nil 解引用 —— 那不是"偶发"，是必现。
// 顺带它还覆盖了"同一个路径重复 Open"这条路（幂等测试正是靠它）。
func reopenDB(t *testing.T, e *env) {
	t.Helper()
	if store.DB != nil {
		return
	}
	if err := store.Open(e.dbPath); err != nil {
		t.Fatalf("重新打开库: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("重新建表: %v", err)
	}
}

// ── 1. 坏行跳过 ─────────────────────────────────────────────────────────

func TestImportSkipsBadRows(t *testing.T) {
	e := newEnv(t)
	e.write(t, fileAccounts, fixtureBadAccounts)
	e.write(t, fileSessions, fixtureSessions)

	stdout, stderr := mustImport(t, e)

	// 验收口径：「跳过坏行、其余全进，且打印 导入N/跳过M」。
	// 2 好账号 + 1 会话 = 3 导入；坏账号 1 条跳过。
	if !strings.Contains(stdout, "导入 3/跳过 1") {
		t.Fatalf("stdout 应含「导入 3/跳过 1」，实际:\n%s", stdout)
	}
	// 跳过原因必须**指名道姓**，否则跳过等于藏起来。
	if !strings.Contains(stderr, "username 为空") && !strings.Contains(stdout, "username 为空") {
		t.Fatalf("应报出坏行原因（username 为空），实际 stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// 库里就是 2 行，不是 3 行（证明坏行确实没进）。
	reopenDB(t, e)
	var n int64
	if err := store.DB.Model(&store.User{}).Count(&n).Error; err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 2 {
		t.Fatalf("users 行数 = %d，期望 2", n)
	}
}

// ── 2. 幂等 ────────────────────────────────────────────────────────────

func TestImportIdempotent(t *testing.T) {
	e := newEnv(t)
	e.write(t, fileAccounts, fixtureGoodAccounts)
	e.write(t, fileSessions, fixtureSessions)

	mustImport(t, e)
	reopenDB(t, e)
	var first int64
	if err := store.DB.Model(&store.User{}).Count(&first).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	var firstSes int64
	if err := store.DB.Model(&store.UserSession{}).Count(&firstSes).Error; err != nil {
		t.Fatalf("count sessions: %v", err)
	}

	// 第二次跑同一个库。store.DB 已经开着，import 会再 Open 一次 —— 那是
	// 幂等的另一半：重复 Open 同一个路径不该出问题（openMu 串行化）。
	mustImport(t, e)
	reopenDB(t, e)

	var second int64
	if err := store.DB.Model(&store.User{}).Count(&second).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	var secondSes int64
	if err := store.DB.Model(&store.UserSession{}).Count(&secondSes).Error; err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if first != second {
		t.Fatalf("users 行数从 %d 变成 %d —— 导入不幂等", first, second)
	}
	if firstSes != secondSes {
		t.Fatalf("user_sessions 行数从 %d 变成 %d —— 导入不幂等", firstSes, secondSes)
	}
}

// ── 3. verify ──────────────────────────────────────────────────────────

func TestVerifyAfterImport(t *testing.T) {
	e := newEnv(t)
	e.write(t, fileAccounts, fixtureGoodAccounts)
	e.write(t, fileSessions, fixtureSessions)
	mustImport(t, e)

	code, stdout, stderr := runCmd("verify", "--data", e.dataDir, "--db", e.dbPath)
	if code != exitOK {
		t.Fatalf("verify 退出码 = %d，期望 0。stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "校验通过") {
		t.Fatalf("应打印「校验通过」，实际:\n%s", stdout)
	}
	// 两行都要出现：JSON 与 SQL 列的数必须一致（2 条账号、1 条会话）。
	for _, want := range []string{"users", "user_sessions"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("核对结果缺 %s 行:\n%s", want, stdout)
		}
	}
}

// ── 4. 逐字节等价 ───────────────────────────────────────────────────────
//
// 这是本工具最硬的一条验收：导出必须与迁移前**一个字节都不差**。
// 差在哪通常是四处：键序、缩进、时间格式（UTC vs 本地）、末尾换行。
func TestExportByteIdentical(t *testing.T) {
	e := newEnv(t)
	src := e.write(t, fileAccounts, fixtureGoodAccounts)
	mustImport(t, e)

	code, stdout, stderr := runCmd("export", "--data", e.dataDir, "--db", e.dbPath, "--out", e.outDir)
	if code != exitOK {
		t.Fatalf("export 退出码 = %d。stdout=%q stderr=%q", code, stdout, stderr)
	}

	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读源文件: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(e.outDir, fileAccounts))
	if err != nil {
		t.Fatalf("读导出文件: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("导出与源不逐字节等价。\n--- want (%d 字节) ---\n%s\n--- got (%d 字节) ---\n%s",
			len(want), want, len(got), got)
	}
}

// ── 5. 拒绝覆盖 data 目录 ───────────────────────────────────────────────

func TestExportRefusesDataDir(t *testing.T) {
	e := newEnv(t)
	e.write(t, fileAccounts, fixtureGoodAccounts)
	mustImport(t, e)

	code, _, stderr := runCmd("export", "--data", e.dataDir, "--db", e.dbPath, "--out", e.dataDir)
	if code != exitUsage {
		t.Fatalf("--out data 的退出码 = %d，期望 %d（用法错误）。stderr=%q", code, exitUsage, stderr)
	}
	if !strings.Contains(stderr, "拒绝") {
		t.Fatalf("应明确拒绝，实际 stderr=%q", stderr)
	}
	// 关键：拒绝之后源文件必须原封不动（这条闸的意义就在这里）。
	got, err := os.ReadFile(filepath.Join(e.dataDir, fileAccounts))
	if err != nil {
		t.Fatalf("读源文件: %v", err)
	}
	if string(got) != fixtureGoodAccounts {
		t.Fatalf("源文件被改动了:\n%s", got)
	}
}

// ── 6. 全程不删不改 data/*.json ─────────────────────────────────────────

func TestImportLeavesJSONUntouched(t *testing.T) {
	e := newEnv(t)
	e.write(t, fileAccounts, fixtureBadAccounts)
	e.write(t, fileSessions, fixtureSessions)
	e.write(t, fileAPIKeys, "[]")
	e.write(t, fileState, fixtureState)
	e.write(t, fileConfig, fixtureConfig)

	before := e.digest(t)
	mustImport(t, e)
	// verify 与 export 也一并不许动源文件。
	if code, _, stderr := runCmd("verify", "--data", e.dataDir, "--db", e.dbPath); code != exitOK {
		t.Fatalf("verify 失败: %q", stderr)
	}
	if code, _, stderr := runCmd("export", "--data", e.dataDir, "--db", e.dbPath, "--out", e.outDir); code != exitOK {
		t.Fatalf("export 失败: %q", stderr)
	}
	after := e.digest(t)

	// 新增文件允许（gw.db 会落在 data 目录里），但**已有文件的指纹必须不变**。
	for name, sum := range before {
		got, ok := after[name]
		if !ok {
			t.Fatalf("%s 被删除了", name)
		}
		if got != sum {
			t.Fatalf("%s 内容被改动了（%s → %s）", name, sum[:8], got[:8])
		}
	}
}

// ── 7. state.json 跳过且可见 ────────────────────────────────────────────

func TestStateFileSkipped(t *testing.T) {
	e := newEnv(t)
	e.write(t, fileAccounts, fixtureGoodAccounts)
	e.write(t, fileState, fixtureState)

	stdout, _ := mustImport(t, e)
	if !strings.Contains(stdout, "[state]") {
		t.Fatalf("应打出一行 state 跳过说明，实际:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Q3") {
		t.Fatalf("跳过理由应指向决议出处（Q3），实际:\n%s", stdout)
	}
	// state.json 的内容一行都不该进 accounts 表（Q3 决议：号池运行态留在 JSON）。
	reopenDB(t, e)
	var n int64
	if err := store.DB.Model(&store.Account{}).Count(&n).Error; err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if n != 0 {
		t.Fatalf("accounts 表有 %d 行，期望 0（state.json 必须跳过）", n)
	}
}

// ── 8. 同秒记录的顺序稳定 ───────────────────────────────────────────────

func TestExportStableOrdering(t *testing.T) {
	e := newEnv(t)
	// 两条**同 created_at**、但 id 不同。只按 created_at 排的话，
	// 顺序取决于 SQLite 的返回顺序，两次导出可能字节不同。
	// 两条**同 created_at**、但 id 不同。只按 created_at 排的话，
	// 顺序取决于 SQLite 的返回顺序，两次导出可能字节不同。
	same := `[
  {
    "id": "u-zzzzzzzzzzzzzzzz",
    "username": "zoe",
    "display_name": "Zoe",
    "password_hash": "h1",
    "role": 10,
    "enabled": true,
    "auth_version": 1,
    "created_at": "2026-10-19T15:18:17Z"
  },
  {
    "id": "u-aaaaaaaaaaaaaaaa",
    "username": "amy",
    "display_name": "Amy",
    "password_hash": "h2",
    "role": 10,
    "enabled": true,
    "auth_version": 1,
    "created_at": "2026-10-19T15:18:17Z"
  }
]`
	e.write(t, fileAccounts, same)
	mustImport(t, e)

	code, _, stderr := runCmd("export", "--data", e.dataDir, "--db", e.dbPath, "--out", e.outDir)
	if code != exitOK {
		t.Fatalf("export 失败: %q", stderr)
	}
	raw, err := os.ReadFile(filepath.Join(e.outDir, fileAccounts))
	if err != nil {
		t.Fatalf("读导出: %v", err)
	}
	var arr []accountJSON
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("解析导出: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("导出 %d 条，期望 2", len(arr))
	}
	// id 升序：u-aaaa… 在 u-zzzz… 之前。
	if arr[0].ID != "u-aaaaaaaaaaaaaaaa" || arr[1].ID != "u-zzzzzzzzzzzzzzzz" {
		t.Fatalf("同秒记录的顺序不稳定：%s, %s", arr[0].ID, arr[1].ID)
	}
	// 再导一次必须得到完全相同的字节（"稳定"的定义）。
	out2 := filepath.Join(e.dir, "export2")
	if code, _, stderr := runCmd("export", "--data", e.dataDir, "--db", e.dbPath, "--out", out2); code != exitOK {
		t.Fatalf("第二次 export 失败: %q", stderr)
	}
	raw2, err := os.ReadFile(filepath.Join(out2, fileAccounts))
	if err != nil {
		t.Fatalf("读第二次导出: %v", err)
	}
	if !bytes.Equal(raw, raw2) {
		t.Fatalf("两次导出字节不同:\n%s\n---\n%s", raw, raw2)
	}
}

// ── 额外：用法与 dry-run ────────────────────────────────────────────────

func TestUsageAndDryRun(t *testing.T) {
	// 无参数 ⇒ 用法错误（不是成功：静默什么都不做最危险）。
	if code, _, _ := runCmd(); code != exitUsage {
		t.Fatalf("无参数退出码 = %d，期望 %d", code, exitUsage)
	}
	if code, out, _ := runCmd("help"); code != exitOK || !strings.Contains(out, "import") {
		t.Fatalf("help 应打印用法，code=%d out=%q", code, out)
	}
	if code, _, _ := runCmd("nope"); code != exitUsage {
		t.Fatalf("未知子命令退出码 = %d，期望 %d", code, exitUsage)
	}

	// dry-run 不建库（这是它存在的意义：迁移前先看一眼）。
	e := newEnv(t)
	e.write(t, fileAccounts, fixtureBadAccounts)
	code, out, _ := runCmd("import", "--data", e.dataDir, "--db", e.dbPath, "--dry-run")
	if code != exitOK {
		t.Fatalf("dry-run 退出码 = %d", code)
	}
	if !strings.Contains(out, "导入 2/跳过 1") {
		t.Fatalf("dry-run 应给出同样的计数，实际:\n%s", out)
	}
	if _, err := os.Stat(e.dbPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run 不该建库文件")
	}
}

// ── 额外：export 默认不写 sessions（有损） ───────────────────────────────

func TestExportSessionsNeedsAllowLossy(t *testing.T) {
	e := newEnv(t)
	e.write(t, fileAccounts, fixtureGoodAccounts)
	e.write(t, fileSessions, fixtureSessions)
	mustImport(t, e)

	// 默认：只写 accounts.json，并且**必须**在 stderr 说清为什么没写 sessions。
	code, _, stderr := runCmd("export", "--data", e.dataDir, "--db", e.dbPath, "--out", e.outDir)
	if code != exitOK {
		t.Fatalf("export 退出码 = %d", code)
	}
	if !strings.Contains(stderr, "hard_expires_at") {
		t.Fatalf("应说明丢 hard_expires_at，实际 stderr=%q", stderr)
	}
	if _, err := os.Stat(filepath.Join(e.outDir, fileSessions)); !os.IsNotExist(err) {
		t.Fatalf("未加 --allow-lossy 时不该写出 %s", fileSessions)
	}

	// 显式允许后写出来，但**仍然**把损失再打一遍（不能因为加了 flag 就闭嘴）。
	out2 := filepath.Join(e.dir, "lossy")
	code, _, stderr = runCmd("export", "--data", e.dataDir, "--db", e.dbPath, "--out", out2, "--allow-lossy")
	if code != exitOK {
		t.Fatalf("--allow-lossy 退出码 = %d stderr=%q", code, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(out2, fileSessions))
	if err != nil {
		t.Fatalf("读导出 sessions: %v", err)
	}
	var arr []sessionJSON
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("解析导出 sessions: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("导出 %d 条会话，期望 1", len(arr))
	}
	// 回填的 username/role 来自 users 表；hard_expires_at 归零（无列）。
	if arr[0].Username != "alice" {
		t.Fatalf("username 未回填：%q", arr[0].Username)
	}
	if arr[0].HardExpiresAt != 0 {
		t.Fatalf("hard_expires_at 应归零（无列可存），实际 %d", arr[0].HardExpiresAt)
	}
}
