//go:build windows

package gateway

import (
	"errors"
	"syscall"
)

// connRefused 判断错误链里有没有「连接被拒」。
//
// ⚠⚠ 平台差异是实测踩出来的（TestProbeEgressClassifiesRefused 抓的）：
// Windows 上 dial 的失败是 `connectex: No connection could be made because the target
// machine actively refused it`，它包的是 **WSAECONNREFUSED(10061)** ——
// 而 `syscall.ECONNREFUSED` 在 Windows 上是另一个值，errors.Is **匹配不到**。
//
// 后果很具体：本机代理挂掉（正是最常见的现场）在 Windows 上会被归成「其它网络错误」，
// 用户拿不到「代理没起来」这句话 —— 分类器等于在最需要它的那条路上失灵。
// 所以这里按平台分别认，两个都判（同一台机器上不同栈可能给不同的 errno）。
// wsaEconnRefused WSAECONNREFUSED 的字面值。
//
// ⚠ 为什么不写 `syscall.WSAECONNREFUSED`：**标准库的 syscall 包在 Windows 上没有这个
// 常量**（实测编译报 undefined；它只在 golang.org/x/sys/windows 里）。为一个常量引一个新
// 依赖不值得，所以按文档写数值 10061 并把来历留在注释里。
const wsaEconnRefused = syscall.Errno(10061)

func connRefused(err error) bool {
	return errors.Is(err, wsaEconnRefused) || errors.Is(err, syscall.ECONNREFUSED)
}
