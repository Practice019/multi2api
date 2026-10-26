package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 子进程退出码：它们是这个门禁的"断言载体"（父进程只能拿到退出码与输出）。
const (
	codeInitialized = 0
	codeAlreadyDone = 3
	codeOtherError  = 4
)

// TestSetupCrossProcessMutex 是 T2 验收里最难的一条：**真起两个进程**同时初始化，
// 只允许一个成功，另一个必须拿到 ErrSetupAlreadyDone。
//
// # 为什么不能用两个 goroutine
//
// 同进程内的并发会被 setup.go 的 setupMu 挡住——那把锁一挡，测试就成了
// "锁能不能用"的自证，而**跨进程互斥**（本仓有多进程部署的真实形态：
// 手工起的 server.exe + 计划任务）根本没被验证。真正的互斥靠的是
// "setups 主键写死 1 + immediate 事务"这两条 SQLite 层的事实。
//
// # 做法
//
//	① 父进程先把表建好（把 AutoMigrate 的 DDL 竞争排除在本门禁之外）
//	② 起两个子进程（用的就是测试二进制自己，-test.run 指到 helper）
//	③ 两个子进程各写一个 ready 文件 ⇒ 父进程确认**两个都到位**后才放行
//	④ 放行 = 创建 gate 文件，两个子进程近乎同时进入初始化事务
//	⑤ 断言退出码是一个 codeInitialized + 一个 codeAlreadyDone，
//	   并且库里恰好 1 行 setups、1 个 root
func TestSetupCrossProcessMutex(t *testing.T) {
	if testing.Short() {
		t.Skip("跨进程门禁要起两个进程（约 1–2s），-short 下跳过")
	}
	dbPath := filepath.Join(t.TempDir(), "gw.db")
	gate := dbPath + ".gate"

	if err := Open(dbPath); err != nil {
		t.Fatalf("父进程 Open: %v", err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("父进程 Migrate: %v", err)
	}
	if err := Close(); err != nil {
		t.Fatalf("父进程 Close: %v", err)
	}

	type child struct {
		cmd    *exec.Cmd
		stdout *bytes.Buffer
		stderr *bytes.Buffer
	}
	spawn := func() *child {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessInitialize", "-test.v")
		cmd.Env = append(os.Environ(),
			"STORE_HELPER_DB="+dbPath,
			"STORE_HELPER_GATE="+gate,
		)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Start(); err != nil {
			t.Fatalf("起子进程: %v", err)
		}
		return &child{cmd: cmd, stdout: &out, stderr: &errb}
	}
	a, b := spawn(), spawn()

	// 等两个子进程都到起跑线（各写一个按 pid 命名的 ready 文件）
	deadline := time.Now().Add(30 * time.Second)
	for {
		ready, _ := filepath.Glob(gate + ".ready.*")
		if len(ready) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			_ = a.cmd.Process.Kill()
			_ = b.cmd.Process.Kill()
			t.Fatalf("子进程没能同时就位（ready 文件数 %d）", len(ready))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatalf("放行失败: %v", err)
	}

	codeA := waitCode(t, a.cmd, a.stdout.String(), a.stderr.String())
	codeB := waitCode(t, b.cmd, b.stdout.String(), b.stderr.String())
	switch {
	case codeA == codeInitialized && codeB == codeAlreadyDone:
	case codeB == codeInitialized && codeA == codeAlreadyDone:
	default:
		t.Fatalf("期望恰好一个成功(%d)+一个已初始化(%d)，实际 %d 与 %d", codeInitialized, codeAlreadyDone, codeA, codeB)
	}

	// 结果落库只能有一份
	if err := Open(dbPath); err != nil {
		t.Fatalf("复查 Open: %v", err)
	}
	defer Close()
	if n := countRows(t, &Setup{}); n != 1 {
		t.Errorf("setups 应恰好 1 行，得到 %d", n)
	}
	if n := countRows(t, &User{}); n != 1 {
		t.Errorf("只该有一个被建出来的账号，users=%d", n)
	}
	ok, err := CheckSetup()
	if err != nil || !ok {
		t.Errorf("CheckSetup 复查失败: ok=%v err=%v", ok, err)
	}
}

func waitCode(t *testing.T, cmd *exec.Cmd, stdout, stderr string) int {
	t.Helper()
	err := cmd.Wait()
	if err == nil {
		return codeInitialized
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("等待子进程失败: %v（stdout=%s stderr=%s）", err, stdout, stderr)
	return -1
}

// TestHelperProcessInitialize 子进程入口。
//
// 它不是"测试"而是被 TestSetupCrossProcessMutex 拉起的**第二个进程**；
// 手工 go test 跑到它时直接跳过（没有 STORE_HELPER_DB 环境变量）。
func TestHelperProcessInitialize(t *testing.T) {
	dbPath := os.Getenv("STORE_HELPER_DB")
	if dbPath == "" {
		t.Skip("仅作为跨进程门禁的子进程入口")
	}
	gate := os.Getenv("STORE_HELPER_GATE")
	ready := gate + ".ready." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(ready, []byte("1"), 0o644); err != nil {
		t.Fatalf("写 ready: %v", err)
	}

	// 等到父进程放行才继续：否则两个子进程会一前一后跑完，测不出并发
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等起跑线超时")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 这里刻意**不再 Migrate**：表由父进程建好，本门禁只测"初始化事务"的互斥
	if err := Open(dbPath); err != nil {
		t.Fatalf("子进程 Open: %v", err)
	}
	_, err := InitializeSetup("rootadmin", "password123")
	switch {
	case err == nil:
		fmt.Println("HELPER: initialized")
		os.Exit(codeInitialized)
	case errors.Is(err, ErrSetupAlreadyDone):
		fmt.Println("HELPER: already-done")
		os.Exit(codeAlreadyDone)
	default:
		fmt.Println("HELPER: error:", err)
		os.Exit(codeOtherError)
	}
}
