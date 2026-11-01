//go:build windows

package monkeycode

import "syscall"

// process_alive_windows.go —— `processAlive` 的 Windows 实现（**只用 stdlib**）。
//
// # 为什么需要"这个 pid 还活着吗"
//
// `removeProfileWhenGone` 用它替代"睡固定时长"：删 profile 的正确判据是
// **进程真退出了**，而不是"等了 1.5 秒" —— 用户机器上 111 个残留 profile 正是
// "固定 1.5 秒 + 静默失败"攒出来的。
//
// ⚠ 只用 stdlib（`syscall`）而不是 `golang.org/x/sys/windows`：本仓
// `internal/sysmon/sysmon.go:10` 明确写着"只用 stdlib，不为一个调用把 x/sys
// 变成直接依赖"（它现在是 indirect）。手法与 killtree_windows.go 的 LazyDLL 同源。
//
// ⚠ 判据是 `GetExitCodeProcess == STILL_ACTIVE(259)` 而**不是** `OpenProcess` 成功与否：
//
//	OpenProcess 对手臂上残留的句柄/权限问题更敏感，会假报"活着"，那样我们就会
//	一直等到 10 秒超时才去删（慢，而且仍可能撞到占用）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false // 打不开 = 已经没了 / 没权限看到 ⇒ 按"不活着"处理（宁可去删）
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
