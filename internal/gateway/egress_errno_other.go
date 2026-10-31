//go:build !windows

package gateway

import (
	"errors"
	"syscall"
)

// connRefused 判断错误链里有没有「连接被拒」（非 Windows：就是 ECONNREFUSED）。
//
// 平台差异的由来见 egress_errno_windows.go 的注释：Windows 上是 WSAECONNREFUSED，
// 用这个常量判会**匹配不到**。
func connRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
