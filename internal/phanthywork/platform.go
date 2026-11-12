// platform.go 平台名的唯一来源。
//
// 独立一个小文件而不是在 provider.go 里直接 runtime.GOOS：设备注册 body 的
// platform 字段是**客户端自报的事实**（报告 §2.4：windows / macos / linux 三档），
// 把它收在一处，将来若要按构建标签改写（例如 Windows 上回报 windows）只改这里。
package phanthywork

import "runtime"

// runtimeGOOS 返回注册 body 用的平台名。
//
// runtime.GOOS 的取值正好是上游认的三档（windows / darwin / linux），
// 但上游的档位名是 macos 而不是 darwin ⇒ 做一次映射。
func runtimeGOOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		// 其它平台（freebsd 等）：上游不认，回落到 linux —— 它至少能让
		// 注册走完，而"注册失败"的代价是整条推理链路不可用。
		return "linux"
	}
}
