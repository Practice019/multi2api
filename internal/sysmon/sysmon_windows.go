//go:build windows

// sysmon_windows.go —— Windows 侧的三次 kernel32 调用。
//
// # 思路来源
//
// 照抄 hyxapi 的 common/system_monitor_windows.go:15-22：同一个 `syscall.NewLazyDLL`
// + `NewProc` + `Call` 的路子。区别只有三点，都是本包的需求决定的：
//
//	① 它只取磁盘，本包还要 CPU（GetSystemTimes）与内存（GlobalMemoryStatusEx）
//	② 它把三个 out 参数一股脑塞进结构体，本包让每个 read* 只返回自己那两个值
//	   —— 单项失败只影响自己（见 sysmon.go 的约束 3）
//	③ 它用 `syscall.UTF16PtrFromString` 取路径，本包相同（stdlib 在 Windows 上就有）
//
// # 为什么不用 golang.org/x/sys/windows
//
// 本仓 go.mod 里没有 x/sys（只有 redis / wazero / crypto），加它等于为一个
// 只调三个 kernel32 函数的功能拉一个新依赖 —— 与"单二进制零依赖"这条部署优势
// 不成比例。stdlib 的 syscall 在 Windows 上提供了完全够用的 LazyDLL。
//
// ⚠ 代价：stdlib 没有 `windows.MEMORYSTATUSEX` 这类结构体定义，
// 得自己按 Win32 ABI 对齐写（见 memoryStatusEx）。那是本文件最容易出错的地方，
// 所以下面把对齐算得明明白白，并且有一条测试在真机上验证结果落在合理区间。
package sysmon

import (
	"syscall"
	"unsafe"
)

var (
	// 三个 DLL 都是 kernel32 里**必然存在**的导出（不是可选 API）：
	//
	//	GetSystemTimes        XP SP1+ / Server 2003+
	//	GlobalMemoryStatusEx  XP+  （GlobalMemoryStatus 的替代，后者在 >4GB 上会截断）
	//	GetDiskFreeSpaceExW   2000+
	//
	// 所以这里不做 `Find` 检查：找不到等于系统坏了，那时 LazyProc.Call 会 panic，
	// 而 panic 会被 `sync.OnceValue` 之外的调用方 recover（handler 层已有 panicRecovery）。
	// ⚠ 但**不做** Find 的代价是：如果某个极简 Windows 容器阉割了 kernel32，
	// 本包会 panic 而不是返回 ok=false。取舍：那种环境的概率远低于"多写一层防御"
	// 带来的复杂度，而且 panic 会留下 stack 而不是一个沉默的 0。
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetSystemTimes       = modkernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = modkernel32.NewProc("GetDiskFreeSpaceExW")
)

// fileTime 是 Win32 FILETIME 的 Go 对应物。
//
// # 为什么拆成两个 uint32 而不是直接用 uint64
//
// FILETIME 的 ABI 就是两个 DWORD（低/高）。GetSystemTimes 往里写 8 字节，
// 用 uint64 接在 amd64 上也能跑（小端），但那是**依赖字节序的巧合**，
// 且没人能从 `*uint64` 这个签名看出内核写的是"两个 DWORD"。
// 显式拆开，uint64() 合并在 fileTime.units() 里，读代码的人一眼明白。
type fileTime struct {
	low  uint32
	high uint32
}

// units 把 FILETIME 合成一个 100 纳秒为单位的计数。
//
// ⚠ 单位是 100ns，不是纳秒 —— GetSystemTimes 的契约就是 100ns。
// 这里**不做**换算：cpuPercent 只需要"两个读数可比"，单位是什么无所谓，
// 提前换算反而多一次可能溢出的乘法。
func (f fileTime) units() uint64 {
	return uint64(f.high)<<32 | uint64(f.low)
}

// memoryStatusEx 对应 Win32 的 MEMORYSTATUSEX（Windows 2000+）。
//
// # ABI 对齐（本文件最容易写错的地方）
//
//	DWORD    dwLength          4 字节，偏移 0
//	DWORD    dwMemoryLoad      4 字节，偏移 4
//	DWORD64  ullTotalPhys      8 字节，偏移 8   ← 前面 8 字节恰好对齐，无填充
//	DWORD64  ullAvailPhys      偏移 16
//	DWORD64  ullTotalPageFile  偏移 24
//	DWORD64  ullAvailPageFile  偏移 32
//	DWORD64  ullTotalVirtual   偏移 40
//	DWORD64  ullAvailVirtual   偏移 48
//	DWORD64  ullAvailExtendedVirtual 偏移 56
//	sizeof = 64
//
// ⚠ 与 Go 的默认布局完全一致（两个 uint32 后接 uint64 时，Go 也按 8 字节对齐），
// **不需要**手工 padding。这一点很关键：若两个 DWORD 后面接的是 6 个 DWORD64
// 但总长度不是 8 的倍数，Go 会在末尾补 padding，而 Win32 只看 dwLength ——
// 两边就错位了。这里 64 恰好是 8 的倍数，安全。
//
// ⚠ dwLength **必须**由调用方填成 sizeof，否则 GlobalMemoryStatusEx 直接返回
// FALSE（这是它唯一的结构体版本协商机制，填错不会有任何提示）。
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// readCPUTimes 读整机累计 CPU 时间（kernel32!GetSystemTimes）。
//
// # 三个输出各自是什么（决定了 idle 与 total 的关系）
//
//	idle    系统空闲时间
//	kernel  **内核时间，已经包含 idle**
//	user    用户态时间
//
// 所以 total = kernel + user（不是 idle+kernel+user，那会重复计 idle），
// idle 自然是 total 的子集 —— 正好满足 cpuPercent 的前提。
//
// ⚠ 这个"kernel 含 idle"是 GetSystemTimes 与 Linux /proc/stat 最容易搞混的差异。
// 若按 Linux 直觉写成 total = idle + kernel + user，CPU 使用率会被**系统性低估**
// （分母多算了一份 idle），而且低负载时看不出来、高负载时误差最大。
func readCPUTimes() (cpuTimes, bool) {
	var idle, kernel, user fileTime
	ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return cpuTimes{}, false
	}
	return cpuTimes{
		idle:  idle.units(),
		total: kernel.units() + user.units(),
	}, true
}

// readMemory 读物理内存总量与可用量（kernel32!GlobalMemoryStatusEx）。
//
// ⚠ 用 ullAvailPhys 而不是"总量减去已用"：Windows 的任务管理器显示的
// "可用"就是它，含待回收的 standby 列表。用别的口径会和用户在自己机器上
// 看到的数字不一致 —— 那会让状态页失去可信度。
func readMemory() (total, used uint64, ok bool) {
	var m memoryStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if ret == 0 || m.totalPhys == 0 || m.availPhys > m.totalPhys {
		return 0, 0, false
	}
	return m.totalPhys, m.totalPhys - m.availPhys, true
}

// readDisk 读 path 所在卷的总量与**调用方可用**空间（kernel32!GetDiskFreeSpaceExW）。
//
// # 三个输出参数各是什么（本包只取前两个，第三个刻意忽略）
//
//	freeBytesAvailableToCaller  本进程**实际能写**的字节数（受磁盘配额限制）← 用这个
//	totalNumberOfBytes          卷总容量                                     ← 用这个
//	totalNumberOfFreeBytes      卷空闲（**不受配额限制**，可能远大于可用）
//
// ⚠ 第三个参数不是"多余的等价物"：在启用目录配额的卷上，它能比可用量大几个数量级。
// 若拿它算 DiskPct，一个配额将满的部署会显示"磁盘充足"，直到写盘失败。
// PLAN 明确要求口径是 freeBytesAvailable，这里就是那一条的落点。
//
// ⚠ 路径必须是**目录**（卷根或子目录都行）。传文件路径在旧系统上可能失败，
// 而 probeDiskPath 保证一定是目录。
func readDisk(path string) (total, free uint64, ok bool) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var freeToCaller, totalBytes, totalFree uint64
	ret, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	_ = totalFree // 见上：口径不用它
	if ret == 0 || totalBytes == 0 {
		return 0, 0, false
	}
	return totalBytes, freeToCaller, true
}
