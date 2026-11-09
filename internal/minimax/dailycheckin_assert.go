// dailycheckin_assert.go 编译期断言：本上游实现 DailyCheckinExt（T-427）。
//
// # 为什么这行断言值得单独一个文件
//
// 共享签到 Job 靠 `gateway.ExtOf[gateway.DailyCheckinExt](p)` **运行时发现**上游
// ⇒ 如果 `CheckinAll` 的签名写歪了（少一个参数、返回值类型不对、方法挂在别名类型上），
// 类型断言就返回 false，而**日志上一个字都不会有** —— 「安静地漏掉这个上游」。
// 那正是用户这次报障的同一类故障（codar 的自动签到静默失效，手动签了两周才发现）。
//
// `var _ gateway.DailyCheckinExt = (*Provider)(nil)` 把这件事**提前到编译期**：
// 签名一改歪，`go build` 当场失败，不可能再静默。
//
// ⚠⚠ 本文件是**为 minimax 补的**：它原来只在
//
//	`internal/minimax/interfaces_compile_test.go` 里有这条断言 ——
//	而**测试文件不参与 `go build ./...`** ⇒ 编译期保险其实不存在 ✗。
//	这与 codar / codearts / lobsterai / monkeycode / qoder / trae / workbuddy 写法对齐。
package minimax

import "workbuddy2api/internal/gateway"

var _ gateway.DailyCheckinExt = (*Provider)(nil)
