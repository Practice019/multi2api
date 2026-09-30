//go:build !windows

package qoder

import "os/exec"

// hideWindow 非 Windows 平台无需隐藏窗口（空实现）。
//
// 保留同名函数是为了让 runtimeinfo.go 里那条调用**不带 build tag** ——
// 否则 spawn 逻辑要被复制两份，而两份实现必然漂移。
func hideWindow(cmd *exec.Cmd) {}
