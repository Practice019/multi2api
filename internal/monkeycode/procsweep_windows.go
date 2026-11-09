//go:build windows

// procsweep_windows.go —— 按命令行匹配杀掉孤儿的临时 profile 浏览器（Windows）。
//
// # 它治的是什么（2026-11-09 实测）
//
// `SweepStaleLoginProfiles` 只删目录，而 profile 被 msedge 锁住时 RemoveAll 会失败。
// 实测现场：**173 个** wb2api-login-* 目录 = **51.53 GB**（磁盘 97.94%），
// 同时有 **30 个孤儿 msedge** 引用着它们。
// 人工取证：先杀掉那 30 个孤儿，再删目录 ⇒ **165 个全成功**（此前 treatments 全失败）。
// ⇒ 因果明确：**不杀进程就删不掉**。
//
// # 为什么它比"改登录收尾逻辑"更值得做
//
// browserlogin.go 的正常路径已经有树杀（killtree_windows.go）+ 等退出再删
// （removeProfileWhenGone）⇒ 孤儿来自**异常路径**：用户中途放弃、轮询超时、
// 服务重启（Job Object 随父进程消失）。那些路径改起来风险高（可能影响正常登录），
// 而"巡检时兜底杀孤儿"风险低、且覆盖全部异常路径。
//
// # 安全判据（绝不能误杀用户自己的浏览器）
//
// 只杀**命令行同时包含**以下两项的进程：
//
//	· `loginProfilePrefix`（"wb2api-login-"）
//	· 该临时目录的**完整名字**（如 "wb2api-login-1794204331491833700"）
//
// 用户自己的 Edge 命令行里不会有这两样 ⇒ 天然隔离。
// ⚠ 这条判据是**误杀防线**，由 procsweep_test.go 的四条断言守着。
package monkeycode

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// listProcsMatching 返回命令行**同时**含全部 needle 的进程 PID（Windows）。
//
// # 为什么用 PowerShell 的 Get-CimInstance 而不是 tasklist
//
// tasklist 默认拿不到命令行（/V 才有、且格式难解析）；
// Get-CimInstance Win32_Process 有 CommandLine 字段，且 WHERE 交给 PowerShell 过滤更稳。
// 开销约 200ms —— 但本函数**每小时才跑一次**（见 monkeycode-profile-sweep Job），可接受。
//
// # 为什么不用 syscall / x/sys
//
// 依赖铁律（killtree_windows.go 顶部也写着）：**只用 stdlib**，不为一个调用引入 x/sys。
// 代价是启动一个 powershell.exe；收益是零新依赖。
//
// ⚠ needles 是 **AND** 语义（全部命中才算）⇒ 调用方负责传足够具体的串
// （只传 prefix 会命中**所有**临时 profile 的进程，包括正在登录中的那个 ✗）。
// procInfo 的定义在 **procsweep.go**（无平台 tag）。
// ⚠ 不能让 `listProcsMatching` 的返回类型只存在于 Windows 文件里 ——
//
//	那样 !windows 的 stub 与平台无关的测试都编译不过（T-448 实测：linux 交叉编译红过一次）。
//
// 它为什么带 Age（T-448）：见 procsweep.go 里 procInfo 的注释。
func listProcsMatching(exeName string, needles ...string) ([]procInfo, error) {
	if exeName == "" || len(needles) == 0 {
		return nil, nil
	}
	var cond []string
	for _, n := range needles {
		// 单引号内 PowerShell 不转义；我们的 needle 是固定前缀 + 目录名（不会有单引号）。
		cond = append(cond, "$_.CommandLine -like '*"+n+"*'")
	}
	// 每行输出「PID 空格 ISO8601 时间」—— 输出格式在这里**定死**，解析端照它写。
	ps := "@(Get-CimInstance Win32_Process -Filter \"Name='" + exeName + "'\" | " +
		"Where-Object { " + strings.Join(cond, " -and ") + " } | " +
		"ForEach-Object { \"$($_.ProcessId) $($_.CreationDate.ToString('o'))\" })"
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var infos []procInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue // 空行 / 格式不符
		}
		pid, e := strconv.Atoi(f[0])
		if e != nil || pid <= 0 {
			continue
		}
		ts, e := time.Parse(time.RFC3339Nano, f[1])
		if e != nil {
			// ⚠ 单行解析失败只跳过它，**不让整批失败**
			//（进程可能刚退出 ⇒ CreationDate 为 null）
			continue
		}
		infos = append(infos, procInfo{PID: pid, Age: now.Sub(ts)})
	}
	return infos, nil
}

// killPID 终止单个进程**及其子树**（Windows）。
//
// # 为什么用 taskkill 而不是 os.FindProcess + Kill
//
//   - 孤儿可能在我们枚举与杀之间**自己退出**了 ⇒ FindProcess 那边会返回
//     "not found" 之类的错，判起来麻烦；taskkill 的退出码更直白。
//   - 我们要的正是"整棵子树"：`/T` 会级联子进程。
//     （killtree_windows.go 的 Job Object 更强，但它只对**我们启动的**进程有效；
//     孤儿已经是别人的进程了，只能用外部工具。）
func killPID(pid int) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
