//go:build windows

package qoder

import (
	"os/exec"
	"syscall"
)

// hideWindow 让 spawn 出来的子进程不弹控制台窗口（Windows）。
//
// 不加这个的话，每次生成设备身份都会闪一个黑框 —— 而这条路径在
// 首次积分请求时触发，用户会看到"控制台窗口一闪而过"这种无法解释的现象。
// 与参照实现的 `windowsHide: true` 同义。
func hideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// CREATE_NO_WINDOW
		HideWindow:    true,
		CreationFlags: 0x08000000,
	}
}
