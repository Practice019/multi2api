// running.go 探测 WorkBuddy 桌面客户端进程是否在运行。
//
// 为什么需要这个判断（实测教训）：
//
// 客户端是常驻 Electron 应用，登录态主要活在它自己的内存里。我们在磁盘上把
// workbuddy-desktop.info 换成别的账号后，客户端**不会察觉**，它下一次刷新 token
// 时仍按内存里的旧会话写回磁盘 —— 现场表现就是「切了但过一会儿自己变回去了」。
//
// 实测证据：08:30 切到目标账号 A 后，09:40 客户端自己把 token 刷了一遍，
// 写回的 sessionState 与它 09-10 就持有的那份完全相同，说明磁盘写入被原地覆盖。
//
// 所以切换前必须先确认客户端没在跑；在跑就明确拒绝并让用户先退出，
// 而不是写完让用户以为成功、过一会儿又被还原。
package clientlogin

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// procCacheTTL 是进程探测结果的缓存时长。
// 管理台 5s 轮询一次状态，没必要每次都去拉一遍进程列表；
// 但也不能缓存太久，否则用户刚退出客户端仍会被拒。
const procCacheTTL = 3 * time.Second

// clientImageNames 是可能代表「客户端正在运行」的进程名（小写比较）。
// 主进程国内版是 WorkBuddy.exe，海外版（WorkBuddy AI）是 WorkBuddyAI.exe；
// 不同版本/安装方式下可能还有别的名字，一并覆盖。
var clientImageNames = []string{
	"workbuddy.exe",
	"workbuddyai.exe", // 海外版主进程 WorkBuddyAI.exe
	"workbuddy",
	"workbuddy-ai",
	"codebuddy.exe",
}

// processChecker 缓存一次进程探测结果，避免高频轮询反复拉起 tasklist。
type processChecker struct {
	mu    sync.Mutex
	at    time.Time
	alive bool
	// probe 是实际探测函数；nil 表示走平台默认实现。测试用它注入假结果。
	probe func() bool
}

// running 报告客户端是否在运行（带短缓存）。
func (p *processChecker) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && time.Since(p.at) < procCacheTTL {
		return p.alive
	}
	probe := p.probe
	if probe == nil {
		probe = defaultProbe
	}
	p.alive = probe()
	p.at = time.Now()
	return p.alive
}

// invalidate 让下一次 running 重新探测。
// 切换/回滚成功后调用：刚改完盘，用户可能马上就退出了客户端。
func (p *processChecker) invalidate() {
	p.mu.Lock()
	p.at = time.Time{}
	p.mu.Unlock()
}

// defaultProbe 按平台选择探测方式。
func defaultProbe() bool {
	if runtime.GOOS == "windows" {
		return probeWindows()
	}
	return probeUnix()
}

// probeWindows 用 tasklist 查进程。
//
// 参数说明：/FI 做服务端过滤（比拉全量列表再自己找快得多），
// /NH 去掉表头，/FO CSV 让输出好解析。
// 没有任何匹配时 tasklist 输出的是 "INFO: No tasks are running..."，
// 因此不能只看退出码，必须看输出里有没有进程名。
// 国内版（WorkBuddy.exe）与海外版（WorkBuddyAI.exe）都要查。
func probeWindows() bool {
	// tasklist 在 System32 下；万一 PATH 被裁剪，用绝对路径兜底。
	candidates := []string{"tasklist", `C:\Windows\System32\tasklist.exe`}
	for _, bin := range candidates {
		// 任一 image 查询失败 = 这个 bin 不可用（命令缺失/超时），换下一个 bin；
		// 全部查询成功且无匹配则 bin 可用、结论为未运行，直接返回。
		usable := true
		for _, image := range []string{"WorkBuddy.exe", "WorkBuddyAI.exe"} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			out, err := exec.CommandContext(ctx, bin,
				"/FI", "IMAGENAME eq "+image, "/NH", "/FO", "CSV").Output()
			cancel()
			if err != nil {
				// 换个候选路径再试；全都失败就只能返回 false（不阻断功能，
				// 只是失去这层保护，由 UI 上的说明兜住）。
				usable = false
				break
			}
			if containsClientImage(string(out)) {
				return true
			}
		}
		if usable {
			return false
		}
	}
	return false
}

// probeUnix 在非 Windows 上用 pgrep -f 匹配进程名。
// pgrep 不存在时返回 false，同样只是失去保护而非报错。
func probeUnix() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pgrep", "-f", "-i", "workbuddy").Output()
	if err != nil {
		return false
	}
	return containsClientImage(string(out))
}

// containsClientImage 判断进程列表输出里是否出现客户端进程名。
func containsClientImage(out string) bool {
	low := strings.ToLower(out)
	for _, name := range clientImageNames {
		if strings.Contains(low, name) {
			return true
		}
	}
	return false
}
