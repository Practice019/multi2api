//go:build !windows

// sysmon_unix.go —— Unix 侧：读 /proc + syscall.Statfs，全部 stdlib。
//
// # 为什么用 /proc 而不是 x/sys/unix 或 gopsutil
//
// hyxapi 用的是 `golang.org/x/sys/unix` + `gopsutil`（common/system_monitor_unix.go:
// Statfs），但本仓 go.mod 里**没有** x/sys。为三个读数拉两个依赖，与
// "单二进制零依赖"这条部署优势不成比例（PLAN-sql-unify D1 的同一条理由）。
//
// 而且 /proc 比 syscall 更**可移植**：对 CPU 与内存，Linux/macOS/BSD 里
// 只有 Linux 有 `/proc/stat` 与 `/proc/meminfo`，但——
//
//	Linux        /proc 齐全 ⇒ cpu/mem 都 ok
//	macOS/BSD    没有 /proc ⇒ cpu/mem 返回 ok=false（页面显示"—"）
//
// ⚠ 这是刻意的取舍：本仓的生产目标是 Linux（容器/服务器），
// macOS 只用于开发。开发机上 CPU/内存显示"—"是可以接受的；
// 而若为此引入 x/sys，就让**生产**多背一个依赖 —— 方向反了。
// 磁盘用 `syscall.Statfs`（stdlib 里各平台都有），所以三大平台都 ok。
//
// # 为什么不顺便读 cgroup
//
// 容器口径是另一件事（hyxapi 的 containerUsage 花了 200 行处理 v1/v2/quota）。
// 混进这里的 CPU 会让"整机 90% 但容器只用 5%"这种真实情形变得无法分辨。
// 本包只报整机，容器口径留给将来单独的一层。
package sysmon

import (
	"os"
	"syscall"
)

// readCPUTimes 读整机累计 CPU 时间（/proc/stat 的 `cpu` 首行）。
//
// # 字段定义（man proc，第 1 行 cpu = 全部核之和）
//
//	cpu  user nice system idle iowait irq softirq steal guest guest_nice
//
// 本包只关心两个量：
//
//	total = 前 8 个字段之和           （guest/guest_nice 不计，它们已被 user/nice 包含）
//	idle  = idle + iowait
//
// # 为什么 iowait 算 idle
//
// iowait 是"CPU 空闲、但在等磁盘"。从"CPU 有没有在算"这个角度，它属于空闲。
// ⚠ 这与 `top` 的默认口径一致（top 的 idle 也含 iowait —— 它那行叫
// `id`，值是 idle+iowait）。若不算进来，一台被磁盘拖住的机器会显示
// "CPU 80% 忙"，而实际上 CPU 在干等 —— 那会把人的注意力引到错误的资源上。
//
// ⚠ guest/guest_nice 必须**排除**：它们已经包含在 user/nice 里（内核文档明确写了），
// 再加一次会让 total 虚高、占用率被低估。
func readCPUTimes() (cpuTimes, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	return parseProcStat(string(data))
}

// readMemory 读物理内存总量与可用量（/proc/meminfo）。
//
// # 口径：MemAvailable 优先，缺则退化为 Free+Buffers+Cached
//
//	MemTotal      总量
//	MemAvailable  **内核估算的"新程序能用多少"**（含可回收页缓存）← 首选
//	MemFree       完全没被用的（不含缓存，会严重低估可用量）
//
// MemAvailable 是 3.14 才加的。老内核上没有这个键，此时退化为
// Free+Buffers+Cached —— 那是个**近似**（不含可回收的 slab），
// 但比只用 MemFree 准得多（只用 MemFree 会让一台正常运行、缓存饱满的
// Linux 显示内存 95% 用满，那是本仓最容易被误报的假警报）。
//
// ⚠ 与 hyxapi 的差异登记：它用 gopsutil 的 `mem.VirtualMemory`，
// 后者内部也是 MemAvailable 优先、缺则 Used=Total-Free-Buffers-Cached。
// 口径一致，只是我们少了那个依赖。
func readMemory() (total, used uint64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	total, avail, ok := parseMeminfo(string(data))
	if !ok || total == 0 || avail > total {
		return 0, 0, false
	}
	return total, total - avail, true
}

// readDisk 读 path 所在文件系统的总容量与**非特权用户可用**空间。
//
// # 字段选择（syscall.Statfs_t）
//
//	Blocks  文件系统总块数
//	Bfree   空闲块数（含 root 保留区）
//	Bavail  **非特权进程**可用的块数  ← 用这个，对齐 freeBytesAvailable 口径
//	Bsize   块大小（字节）
//
// ⚠ 用 Bavail 而不是 Bfree：ext4 默认给 root 留 5%，而我们的进程不是 root。
// 用 Bfree 会让"还剩 5% 却已经写不进去"变成无法解释的现象。
// 这正是 Windows 侧选 freeBytesAvailableToCaller 的同一条理由（见 sysmon_windows.go）。
//
// ⚠ 显式 uint64 转换以兼容 FreeBSD（其字段是 int64）。
func readDisk(path string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	if bsize == 0 {
		return 0, 0, false
	}
	total = uint64(st.Blocks) * bsize
	free = uint64(st.Bavail) * bsize
	if total == 0 || free > total {
		return 0, 0, false
	}
	return total, free, true
}
