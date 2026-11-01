//go:build !windows

package monkeycode

import "syscall"

// process_alive_unix.go —— `processAlive` 的 Unix 实现。
//
// `kill(pid, 0)` 的经典语义：不发信号，只做"存在性 + 权限"检查。
//
//	err == nil        ⇒ 进程存在且我们有权给它发信号
//	EPERM            ⇒ 进程存在但没权限（**仍然算活着**）
//	ESRCH            ⇒ 不存在
//
// ⚠ 为什么不用 `os.FindProcess`：它在 Unix 上**永远成功**（只是构造一个结构体），
//
//	不表示进程存在 —— 用它判活会永远返回"活着"，于是 profile 清理每次都要白等
//	10 秒超时才动手（Windows 那边的注释里有同一条教训）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
