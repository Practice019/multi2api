//go:build windows

package monkeycode

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// killtree_windows_test.go —— 证明"整棵树真的被杀干净了"。
//
// # 为什么必须有真机用例（而不是 mock）
//
// 这个 bug 的全部难点都在**OS 语义**上：`Process.Kill()` 在 Windows 上不级联。
// 任何"断言我们调用了 Kill"的测试都会**通过**，而用户照样看到 18 个孤儿 msedge。
// 所以这里的断言只有一种形态：**杀完之后去问操作系统"还有没有活着的"**。
//
// 判据用**命令行的唯一标记**（`monkeycode-killtree-test-<pid>`），不碰用户
// 正在用的任何浏览器 —— 这个标记只会出现在本用例自己起的进程里。

// markerIP 用**唯一的目标 IP**当标记：那个 IP 不可路由，ping 会一直等（正是我们要的
// "活着的子进程"），而它的命令行里带着这个独一无二的串，可用来数进程。
//
// ⚠ 为什么不用 `start "" /b …`（T-031 现场事故）：Go 的 exec 会把空标题转义成 `\"\"`，
//
//	cmd 解析之后会把下一个 token 当成**名叫 `\` 的文件**去执行 ⇒ Windows 弹出
//	「Windows 找不到 '\' 文件」的对话框，而且每跑一次测试弹一次（用户实测"一直弹"）。
//	现在的写法不带标题，cmd 不会产生那个误解析。
const markerIP = "10.123.45.67"

// countTreeProcesses 数"命令行里含 markerIP 的 ping"，以及带标记的 cmd（父层）。
func countTreeProcesses(t *testing.T) int {
	t.Helper()
	ps := "Get-CimInstance Win32_Process -Filter \"Name='ping.exe'\" | " +
		"Where-Object { $_.CommandLine -like \"*" + markerIP + "*\" } | Measure-Object | " +
		"Select-Object -ExpandProperty Count"
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		t.Fatalf("查询进程失败: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil {
		return 0 // 计数拿不到时按 0 处理（用例会给出更明确的失败信息）
	}
	return n
}

// TestKillProcessTreeKillsOrphanGrandchildren 核心用例：
// 父进程**已经退出**、留下一个"孙子进程"活着 —— 这种孤儿正是改前会残留 20 分钟的东西。
//
// ⚠ 为什么让父进程先退出：那正是 Chronium 主进程被 Kill 之后的状态。
//
//	父子关系断了，靠 `taskkill /T` 递归**找不到**它（taskkill 按关系枚举），
//	只有"进程一出生就归属"的 Job Object 才抓得住。
func TestKillProcessTreeKillsOrphanGrandchildren(t *testing.T) {
	// `start /b` 的语义：立刻返回 ⇒ 子命令变成**独立运行的孙子进程**（父进程随后退出）。
	// ⚠ 这里**不能**写 `start "" /b`（空标题会让 cmd 去执行一个名叫 `\` 的文件，
	//   弹出「Windows 找不到 '\' 文件」的对话框 —— T-031 现场事故）。
	script := "start /b ping -n 120 " + markerIP
	cmd := exec.Command("cmd", "/c", script)
	prepareProcessForTreeKill(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("起测试进程树失败: %v", err)
	}
	// ⚠ 兜底：用例中途 Fatal 也要收掉这棵树（测试自己绝不能变成残留源）。
	t.Cleanup(func() { killPidTreeBestEffort(cmd.Process.Pid) })
	killTree, err := attachProcessTree(cmd.Process)
	if err != nil {
		t.Fatalf("挂 Job Object 失败（本用例的前提）: %v", err)
	}
	// 等孙子进程真的起来（最多 5 秒）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && countTreeProcesses(t) == 0 {
		time.Sleep(200 * time.Millisecond)
	}
	before := countTreeProcesses(t)
	if before == 0 {
		t.Skip("环境里没能起出带标记的进程（可能被策略拦住），跳过")
	}
	t.Logf("杀掉之前：命令行含 %s 的 ping 进程 = %d 个", markerIP, before)

	// ★ 真正的动作
	if err := killTree(); err != nil {
		t.Logf("清理返回的错误（允许非 nil，例如目标已退出）：%v", err)
	}

	// 断言：杀完之后不该再有带标记的进程活着。
	//
	// ⚠ 给 2 秒宽限：TerminateJobObject 是异步生效的（内核要回收）。
	gone := false
	for i := 0; i < 20; i++ {
		if countTreeProcesses(t) == 0 {
			gone = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if after := countTreeProcesses(t); !gone || after != 0 {
		t.Fatalf("杀完整棵树之后仍有 %d 个进程带标记 %s 活着 —— "+
			"这正是用户遇到的 bug（Process.Kill 不级联，孤儿会活到人工清理）", after, markerIP)
	}
	t.Logf("杀掉之后：命令行含 %s 的 ping 进程 = 0 个 ✓", markerIP)
}

// TestAttachProcessTreeIsIdempotent 清理函数重复调用安全（closure 可能被走两次：
// 取消一次、defer 一次 —— 见 browserlogin.go 的两条路径）。
func TestAttachProcessTreeIdempotent(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 >nul")
	prepareProcessForTreeKill(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("起进程失败: %v", err)
	}
	killTree, err := attachProcessTree(cmd.Process)
	if err != nil {
		t.Fatalf("挂 Job 失败: %v", err)
	}
	if err := killTree(); err != nil {
		t.Logf("第一次清理：%v（允许非 nil）", err)
	}
	if err := killTree(); err != nil {
		t.Errorf("第二次清理不该报错（幂等）：%v", err)
	}
}

// TestPrepareProcessForTreeKillSetsNothingOnWindows 记录一条平台事实：
// Windows 侧这个钩子是空操作（Job 要等有 PID 才能挂），接口存在只为与 Unix 对称。
func TestPrepareProcessForTreeKillSetsNothingOnWindows(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo hi")
	prepareProcessForTreeKill(cmd)
	if cmd.SysProcAttr != nil {
		t.Errorf("Windows 侧不设 SysProcAttr（Setpgid 是 Unix 概念），实得 %+v", cmd.SysProcAttr)
	}
}

// TestKillRealEdgeTreeZeroResidual 真机验证（默认跳过）：
// 用**本工具自己的**profile 目录与调试端口起一个真 Edge，杀完整棵树后断言 0 残留。
//
// ⚠ 环境变量开启（`$env:WB_KILLTREE_EDGE=1` + `go test -run TestKillRealEdgeTree`）：
//
//	它会真的起一个浏览器窗口，不适合放进常规套件；但它才是"用户那句话"的直接证明。
//
// ⚠ 判据严格限定在**我们自己起的那个端口/profile**上 —— 绝不去碰用户正在用的 Edge。
func TestKillRealEdgeTreeZeroResidual(t *testing.T) {
	if os.Getenv("WB_KILLTREE_EDGE") == "" {
		t.Skip("默认跳过（会真的起浏览器）：设 WB_KILLTREE_EDGE=1 开启")
	}
	exe := findEdgeExe(t)
	if exe == "" {
		t.Skip("找不到 msedge.exe")
	}
	const port = 9333 // 用一个冷端口，别撞用户/服务端在用的
	profileDir := t.TempDir()
	cmd := exec.Command(exe,
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+profileDir,
		"--no-first-run", "--no-default-browser-check",
		"about:blank",
	)
	prepareProcessForTreeKill(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("起 Edge 失败: %v", err)
	}
	t.Cleanup(func() { killPidTreeBestEffort(cmd.Process.Pid) })
	killTree, err := attachProcessTree(cmd.Process)
	if err != nil {
		t.Fatalf("挂 Job 失败: %v", err)
	}
	// 等它把子进程都拉起来（renderer/gpu/crashpad）——这正是"18 个进程"的来源。
	time.Sleep(4 * time.Second)
	before := countEdgeWithPort(t, port)
	t.Logf("杀掉之前：带 --remote-debugging-port=%d 的 msedge = %d 个（含 gpu/renderer/crashpad 子进程）", port, before)
	if before < 2 {
		t.Logf("⚠ 只起了 %d 个进程，说明这次没拉起完整进程树（结论仍然有效，但说服力弱）", before)
	}

	if err := killTree(); err != nil {
		t.Logf("清理返回：%v（允许非 nil）", err)
	}
	gone := false
	for i := 0; i < 40; i++ {
		if countEdgeWithPort(t, port) == 0 {
			gone = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	after := countEdgeWithPort(t, port)
	t.Logf("杀掉之后：带同一个端口的 msedge = %d 个", after)
	if !gone || after != 0 {
		t.Fatalf("仍有 %d 个 msedge 残留 —— 用户报的就是这个", after)
	}
}

// countEdgeWithPort 数"命令行里带这个调试端口"的 msedge 进程。
func countEdgeWithPort(t *testing.T, port int) int {
	t.Helper()
	ps := fmt.Sprintf(
		"@(Get-CimInstance Win32_Process -Filter \"Name='msedge.exe'\" | "+
			"Where-Object { $_.CommandLine -like '*--remote-debugging-port=%d*' }).Count", port)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		t.Fatalf("查询 msedge 失败: %v", err)
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

func findEdgeExe(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
