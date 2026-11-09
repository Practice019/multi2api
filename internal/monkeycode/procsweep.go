// procsweep.go 巡检用的进程枚举/终止**接缝**（平台无关）。
//
// # 为什么要单独一个文件放这两个变量
//
// 实现是平台相关的（Windows 用 PowerShell 枚举 + taskkill 杀；其它平台 no-op），
// 但**测试必须能替换它们**，而测试文件不该带平台 tag（否则 CI 在 Linux 上跑不到）。
// ⇒ 变量声明放这里（无 build tag），实现由各平台的同名字段提供：
//
//	procsweep_windows.go  func listProcsMatching(...) / func killPID(...)
//	procsweep_other.go    同名 no-op
//
// # 为什么必须可注入（这是本包最危险的一处代码）
//
// `killPID` 最终会执行 `taskkill /T /F` ⇒ **判据写错就是杀掉用户的浏览器**。
// 所以选择逻辑（"哪些 PID 该杀"）必须能在测试里用假进程列表验证 ✓
// 见 procsweep_test.go 的四条断言。
package monkeycode

import "time"

// procInfo 一个匹配到的进程（★ T-448）。
//
// ⚠ 它必须住在这个**无平台 tag** 的文件里：返回类型不能只存在于 Windows 文件里，
//
//	否则 !windows 的 stub 与平台无关的测试都编译不过
//	（T-448 实测：`GOOS=linux go build` 与单测各红过一次）。
//
// # 为什么带 Age（绕开 mtime 死结）
//
// 改造前只用"profile 目录 mtime 是否陈旧"判孤儿 —— 但**孤儿进程会持续刷新 mtime**
// （浏览器在写 profile）⇒ 它与"正在登录"在 mtime 上**完全同形** ⇒ 永远清不掉。
// 实测：造场景（mtime 老 + msedge 指向它）后 Sweep 报告 removed=1，
// 但那个目录与 8 个孤儿都还在（mtime 被刷成了"现在"）。
// ⇒ 换一个维度：**进程自己的启动时长**。它与 profile 的 mtime **无关**，
//
//	因此不会被孤儿自己刷新掉 ⇒ 绕开那个死结。
//
// 登录窗口 10 分钟（LoginFlow 的 timeout）⇒ 启动 > 30 分钟的不可能是当前登录。
type procInfo struct {
	PID int
	Age time.Duration
}

var (
	// listProcsFn 枚举"命令行同时含全部 needle"的进程 PID。
	// 生产指向 listProcsMatching；测试替换成假实现。
	listProcsFn = listProcsMatching
	// killPIDFn 终止一个进程（Windows 上走 taskkill /T /F）。
	// 生产指向 killPID；测试替换成记录器（**绝不真杀**）。
	killPIDFn = killPID
)
