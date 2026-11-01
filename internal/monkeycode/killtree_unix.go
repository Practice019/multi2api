//go:build !windows

package monkeycode

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// killtree_unix.go —— **杀整棵进程组**（Linux/macOS/BSD）。
//
// 与 killtree_windows.go 同题：Chromium 是多进程架构，杀主进程会留下孤儿
// （Windows 那边实测 18 个 msedge 活了 20 分钟）。
//
// # Unix 上正确的做法是"进程组"（不是遍历子进程）
//
// `SysProcAttr{Setpgid: true}` 让子进程**自成一个进程组**（pgid = 子进程 pid），
// 于是 Chromium 之后 fork 的 gpu/renderer/crashpad **全部落在同一个组里**。
// 杀的时候 `kill(-pgid, SIGKILL)` 一次带走整组 —— 不依赖父子关系还在不在，
// 也不需要遍历 /proc（遍历法会漏掉"父进程已死"的孤儿，与 Windows 的 taskkill 同病）。
//
// ⚠ **Setpgid 必须在 Start 之前设**：进程一旦启动，进程组就定了，事后改不了。
//
//	这就是 `prepareProcessForTreeKill` 这个"Start 前钩子"存在的唯一理由 ——
//	两个平台共用同一个调用点，Unix 有实质动作、Windows 是空操作。
func prepareProcessForTreeKill(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// attachProcessTree Unix 不需要"提前挂对象"：进程组在 Start 时就已经成型，
// 这里只需要把 pid 记下来，返回一个幂等的清理函数。
//
// ⚠ 仍然返回同样的签名（与 Windows 对称）：调用方不该关心平台差异。
func attachProcessTree(proc *os.Process) (func() error, error) {
	if proc == nil || proc.Pid <= 0 {
		return func() error { return nil }, nil
	}
	var done bool
	return func() error {
		if done {
			return nil
		}
		done = true
		return killProcessTree(proc)
	}, nil
}

// killProcessTree 杀掉 `proc` **及其整个进程组**（Unix）。
//
// 顺序：先 SIGTERM 给一次机会（让它自己收尾、刷 profile），一小会儿后再 SIGKILL。
//
// ⚠ 为什么要两步而不是直接 SIGKILL：Chromium 收到 SIGTERM 会清理自己的
//
//	profile/lock 文件，那样紧接着的 `os.RemoveAll(profileDir)` 才删得干净
//
// （Windows 那边靠 Job Object 硬杀，所以那边额外靠 taskkill 兜底 —— 两边都不完美，
//
//	但都比"只杀主进程"强得多）。
func killProcessTree(proc *os.Process) error {
	if proc == nil || proc.Pid <= 0 {
		return nil
	}
	pid := proc.Pid
	var firstErr error

	// -pid = "整个进程组"。⚠ 这里必须用负数：正数只杀那一个进程，
	// 那就退化成改前的行为了（本文件存在的意义就是这一行的负号）。
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		firstErr = fmt.Errorf("SIGTERM 进程组失败: %w", err)
	}
	// 给 300ms 收尾窗口；期间进程可能已退出（那是好事）。
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && firstErr != nil {
		// 组已经空了的报错（ESRCH）在这里是**预期**结果之一，不覆盖第一个错误。
		firstErr = nil
	}
	// 主进程本身也补一刀（万一 Setpgid 没生效，它就是最后一道保险）。
	if err := proc.Kill(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
