//go:build !windows

package monkeycode

// listProcsMatching 非 Windows 平台不做事。
//
// 临时 profile 的孤儿进程问题是 **Windows 特有**的：
// Chromium 系在 Windows 上父进程死后子进程会变孤儿（Job Object 随父进程消失），
// 且 `os.RemoveAll` 会因文件被占用而失败（Unix 上删除已打开的文件是允许的）。
// ⇒ 返回 nil/空，让调用方逻辑保持一致，不在上层引入平台分支。
func listProcsMatching(exeName string, needles ...string) ([]procInfo, error) {
	return nil, nil
}

// killPID 非 Windows 平台不做事（同上）。
func killPID(pid int) error { return nil }
