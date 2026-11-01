//go:build windows

package monkeycode

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// killtree_windows.go —— **杀整棵进程树**（Windows）。
//
// # 它治的是什么（用户实测 2026-11-02）
//
//	"这个测试它一直会打开 Monkeycode 的浏览器…打开之后它又不关闭，
//	 你看怎么把这个优化一下，测试成功之后要关闭那个浏览器。"
//
// 现场取证：用户机器上当时有 **18 个 msedge 进程**（全部起于同一时刻）——
// 两个主进程（父进程已退出）+ crashpad-handler + gpu-process + utility ×2 + renderer ×4，
// 活了二十分钟直到人工清理。
//
// 根因：`browserlogin.go` 的 closure 只做了 `proc.Process.Kill()`。
//
// # 为什么 `Kill()` 不够（这是 OS 语义，不是本仓写错）
//
// Windows 的 `TerminateProcess`（Go 的 `Kill` 用的就是它）**只终止那一个进程**，
// 不级联子进程。而 Chromium 系浏览器天生多进程（主 + gpu + crashpad + utility +
// 每个标签页一个 renderer）：主进程一死，子进程**变孤儿继续跑** —— 它们还占着
// profile 目录与端口，于是紧接着的 `os.RemoveAll(profileDir)` 会静默失败
// （文件被占用），"删 profile"成了一句空话。
//
// # 为什么用 Job Object 而不是只靠 `taskkill /T /F`
//
// taskkill 是"按 PID 递归"的外部工具：① 依赖它存在；② 它先枚举子进程，
// 而**主进程一旦已死、父子关系就断了**（孤儿不再出现在那棵树下）—— 我们这种
// "先 Kill 主进程、再想清理"的时序恰好最吃亏。
// Job Object 是内核对象：**子进程一出生就归属它**，`TerminateJobObject` 一次
// 终止整棵树，不依赖外部工具、也不依赖父子关系还在不在。
//
// ⚠ 只用 stdlib：内核 API 走 `syscall.NewLazyDLL`。本仓 sysmon.go:10 的纪律是
// "只用 stdlib，不为一个调用把 golang.org/x/sys 变成直接依赖"（x/sys 在这里是
// indirect）。手法与 gateway/egress_errno_windows.go 一致。
//
// # 两处各自覆盖什么（不是重复实现）
//
//	Job Object：覆盖"挂上之后出生"的子进程（renderer/gpu 都是主进程启动后才 fork 的）
//	taskkill ：覆盖"挂 Job 之前那一瞬"已出生的子进程，以及 Job 不可用的降级环境
//
// ⇒ 两条腿都要走；但 taskkill **不可靠**（见上），所以它只是补充。
var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
)

// jobRegistry 保存"PID → Job Object 句柄"。
//
// 为什么需要它：Job 的句柄要在**会话结束时**才用（登录可能持续几分钟，期间不能杀），
// 而调用方（browserlogin.go 的 closure）手上只有 `*os.Process`。
// 用 PID 做键足够：登记与取用都在同一个进程生命周期内，且取走后立刻删除。
type jobRegistry struct {
	mu sync.Mutex
	m  map[int]uintptr
}

func newJobRegistry() *jobRegistry { return &jobRegistry{m: map[int]uintptr{}} }

func (r *jobRegistry) put(pid int, h uintptr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[pid] = h
}

// take 取走并删除（返回 false 表示这个 PID 没登记过 Job）。
func (r *jobRegistry) take(pid int) (uintptr, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.m[pid]
	if ok {
		delete(r.m, pid)
	}
	return h, ok
}

func (r *jobRegistry) drop(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, pid)
}

var jobs = newJobRegistry()

// prepareProcessForTreeKill 在 **Start 之前**装好"这棵树将来能被整体杀掉"的前提。
//
// ⚠ Windows 这边是空操作（Job 要等有 PID 才能挂），但**接口必须与 Unix 对称**：
// Unix 那边要在 `SysProcAttr` 里设 `Setpgid`，而那是 Start 之后补不上的
// （见 killtree_unix.go）。两平台同一个调用点，写法才不会分叉。
func prepareProcessForTreeKill(cmd *exec.Cmd) { _ = cmd }

// attachProcessTree 在 Start **之后**把进程挂进一个新的 Job Object。
//
// 返回 (清理函数, 错误)。清理函数 = **杀整棵树 + 关句柄**，幂等（重复调用安全）。
//
// # 挂不上时怎么办
//
// 返回错误时调用方**仍然要**继续登录流程（清理降级成 taskkill）——
// 冲着一个清理手段的降级去打断用户的登录，是本末倒置。所以调用方
// 只记一行日志。
func attachProcessTree(proc *os.Process) (func() error, error) {
	if proc == nil || proc.Pid <= 0 {
		return func() error { return nil }, nil
	}
	h, _, callErr := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return func() error { return killProcessTree(proc) },
			fmt.Errorf("CreateJobObject 失败: %v", callErr)
	}
	// 把当前进程也放进同一个 Job？—— **不**。若本进程已在某个 Job 里（CI/服务
	// 管理器常这么做），Windows 不允许跨 Job 嵌套（除非开了 nested jobs），
	// 所以我们只挂浏览器进程，不碰自己。
	const (
		processSetQuota  = 0x0100
		processTerminate = 0x0001
	)
	ph, _, openErr := procOpenProcess.Call(uintptr(processSetQuota|processTerminate), 0, uintptr(proc.Pid))
	if ph == 0 {
		_, _, _ = procCloseHandle.Call(h)
		return func() error { return killProcessTree(proc) },
			fmt.Errorf("OpenProcess 失败: %v", openErr)
	}
	r, _, assignErr := procAssignProcessToJobObject.Call(h, ph)
	_, _, _ = procCloseHandle.Call(ph) // 挂完就能关进程句柄，Job 自己持着引用
	if r == 0 {
		_, _, _ = procCloseHandle.Call(h)
		return func() error { return killProcessTree(proc) },
			fmt.Errorf("AssignProcessToJobObject 失败: %v", assignErr)
	}
	jobs.put(proc.Pid, h)

	var once sync.Once
	return func() error {
		var err error
		once.Do(func() { err = killProcessTree(proc) })
		return err
	}, nil
}

// killProcessTree 杀掉 `proc` **及其全部后代**（Windows）。
//
// 顺序是有讲究的：**先 TerminateJobObject，再 Kill 主进程，最后 taskkill**。
//
//	先 Job   ：此刻整棵树（含已出生的 renderer）一次性终止 —— 孤儿根本没机会产生
//	再 Kill  ：Job 失败/未登记时的兜底，至少把主进程带走
//	后 taskkill：覆盖"挂 Job 之前一瞬"出生的那批
//
// ⚠ 反过来的顺序（先 Kill 主进程）会让 taskkill 的枚举失效 —— 那正是改前
//
//	"18 个 msedge 活了 20 分钟"的成因。
//
// ⚠ 返回值只表示"尽力而为的结果"，**不能**据此断言"树已清空"：真正的验证
//
//	是外部看进程还在不在（killtree_windows_test.go 真机用例 + 手工核对）。
func killProcessTree(proc *os.Process) error {
	if proc == nil || proc.Pid <= 0 {
		return nil
	}
	pid := proc.Pid
	var firstErr error

	if h, ok := jobs.take(pid); ok {
		if r, _, e := procTerminateJobObject.Call(h); r == 0 && firstErr == nil {
			firstErr = fmt.Errorf("TerminateJobObject 失败: %v", e)
		}
		_, _, _ = procCloseHandle.Call(h)
	}

	// ⚠ 这里的错误**必须忽略**（真机实测踩到）：登录成功路径上 `cmd /c start` 那层
	//   已经自己退出，此时 Kill() 返回 "TerminateProcess: Access is denied." ——
	//   那是**预期**结果，不是失败。若当成错误往上抛，closure 会打一行
	//   「清理出错（可能有残留）」，把正常成功路径报成故障 —— 假警报比不报更坏，
	//   它会训练人忽略这条日志。真正的机制是上面的 TerminateJobObject 与下面的 taskkill。
	_ = proc.Kill()

	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err != nil {
		logf("monkeycode: 清理浏览器进程树：taskkill 未能执行（多半是进程已退出）：%v", err)
	}
	jobs.drop(pid)
	return firstErr
}
