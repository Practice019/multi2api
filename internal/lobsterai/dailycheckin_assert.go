// dailycheckin_assert.go 编译期断言：本上游实现 DailyCheckinExt（T-426）。
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
// ⚠ 与 internal/lobsterai 等包已有的写法一致（codar / codearts / minimax / trae /
// workbuddy 五家本来就有，本文件补齐剩下的三家）。
// ⚠ 不要把它写进某个 `*_test.go`：测试文件不参与 `go build ./...`，
// 那就又变回"只在跑测试时才发现"。
package lobsterai

import "workbuddy2api/internal/gateway"

var _ gateway.DailyCheckinExt = (*Provider)(nil)
