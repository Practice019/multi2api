// runtimeinfo.go 实时生成 Qoder 设备身份（spawn `runtime-info.exe`）。
//
// # 为什么必须走这条路（本轮修的缺陷）
//
// 用户报障：qodercn 账号「今日签到 失败」，而 IDE 里明明可以领。
//
// 根因链（实测确认）：
//
//	① `/sash/api/v1/me/campaigns` 需要 `Cosy-MachineToken` + `Cosy-MachineType`
//	   成对出现，缺一服务端**不下发** `CLAIM_BENEFIT/CLAIMABLE`
//	   （消融实验见 machine.go 的文件头）
//	② 我们那对头的唯一来源是**读磁盘缓存**
//	   `%APPDATA%\Qoder\SharedClientCache\cache\machine_token.json`
//	③ 而本机**根本没有这个文件** —— 于是两个头从来没发出去
//
// machine.go 旧注释里的判断是「读不到就保守降级（少一次可领活动）」。
// 那条判断建立在一个**未经检验的前提**上：文件存在、只是陈旧。
// 实测：文件**不存在**（纯登录流程不产生它），于是"保守降级"实际变成
// **永久不发头 → 签到永远失败**。用户看到的"失败"就是这个。
//
// # 主路径 = 实时 spawn（与参照实现一致）
//
// 参照实现（dsh-codearts-auth 的 qoder-machine.ts）的主路径就是实时
// spawn `runtime-info.exe`，磁盘缓存只作**最后退路**，理由它写得很清楚：
// 缓存会长期陈旧（开发机上停在 179 天前），照它发头会让服务端不下发可领活动。
//
// 本文件把这套移植过来。**实测本机 exe 输出**（283 字节）：
//
//	{"machineToken":"P1gAsLi8…","machineType":"127b4f9014042f1a85",
//	 "machineCode":"18eb8dd976c832ba43","vmInfo":{…},"accountOutcome":"invalid_input"}
//
// 带上这对头后，同一个账号、同一个 token 的 campaigns 响应立刻变成
// `claimable:true` + `CLAIM_BENEFIT/CLAIMABLE/amount:100`。
//
// # ⚠ 参数顺序不能错：`3 --account-stdin`
//
// 参照用实测消融证明过：**漏掉第一个参数（environment）会拿到另一套身份**，
// 服务端只回 1 条 `VIEW_DETAILS`、**没有**可领项 —— 症状是"今天已领"的假象，
// 极难定位（因为身份看起来"有值"，只是错的）。
// 故参数形态由测试逐字锁住（runtimeinfo_test.go）。
package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// runtimeInfoEnvironment `runtime-info.exe` 的第一个位置参数。
//
// ⚠ **不可省略**，且必须是 3：asar 源码里 `environment === 'global' ? 3 : 0`，
// 而本机 IDE 抓包实测用的就是 global（=3）。漏它会拿到另一套身份。
const runtimeInfoEnvironment = "3"

// runtimeInfoTimeoutMS 单次 spawn 的超时。
//
// 参照实测正常耗时约 3.8 秒（bun/Go 单文件程序，启动开销占大头）。
// 取 20 秒是给冷启动 / 杀软扫描留余量；超时即放弃并退回磁盘缓存 ——
// 宁可少发一次头（回到修复前行为），也不要让积分查询长时间挂住。
const runtimeInfoTimeoutMS = 20_000

// qoderDataDirNames 本机 Qoder 系客户端的数据目录名（相对 home，按序尝试）。
//
// ⚠ 必须**两个都列**（参照记过这个真实缺陷）：原先只认国际版的 `.qoder`，
// 于是"只装了中国版"的用户找不到 exe → 退回陈旧缓存 → 拿不到 machine 头。
// 本机只有 `.qoder-cn`，正是这一种情形。
var qoderDataDirNames = []string{".qoder", ".qoder-cn"}

// resolveMachineIdentityLive 实时 spawn `runtime-info.exe` 生成身份。
//
// 拿不到（未安装 / 超时 / 输出形状不符）返回 nil —— 由调用方退回磁盘缓存。
func resolveMachineIdentityLive() *MachineIdentity {
	exe := locateRuntimeInfo()
	if exe == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(runtimeInfoTimeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, runtimeInfoArgs()...)
	// stdin 给一个**形状合法**的 JSON（参照用 `{account:""}`）。
	//
	// ⚠ 即便 accountOutcome 不是 success（实测返回 invalid_input），
	// machineToken / machineType **仍然有效** —— 身份由「设备 + environment」
	// 决定，与传入的 account 无关，故不因它而丢弃。
	cmd.Stdin = strings.NewReader(`{"account":""}`)
	var out bytes.Buffer
	cmd.Stdout = &out
	// stderr 丢弃：那是 exe 自己的诊断输出，不影响身份解析。
	cmd.Stderr = nil
	// Windows 下不弹控制台窗口（与参照的 windowsHide: true 同义）。
	hideWindow(cmd)

	if err := cmd.Run(); err != nil {
		return nil
	}
	return parseRuntimeInfoOutput(out.String())
}

// runtimeInfoArgs 构造 `runtime-info.exe` 的参数。
//
// 顺序**必须**是 `[environment, "--account-stdin"]` —— 与 asar 源码一致。
// 单独抽成函数是为了让测试能逐字锁住这个数组（漏参数是这条路径出过的缺陷）。
func runtimeInfoArgs() []string {
	return []string{runtimeInfoEnvironment, "--account-stdin"}
}

// parseRuntimeInfoOutput 从 exe 输出里取身份。
//
// 输出是**单行 JSON**，但可能带前缀噪音，所以从第一个 `{` 开始截。
//
// ⚠ 字段名是 `machineToken` / `machineType`（**驼峰**），与磁盘缓存的
// `token` / `type` **不同** —— 弄混会让解析静默失败（返回 nil → 退回缓存 →
// 拿不到头 → 签到失败，症状与没修一样）。
func parseRuntimeInfoOutput(out string) *MachineIdentity {
	start := strings.Index(out, "{")
	if start < 0 {
		return nil
	}
	var rec struct {
		MachineToken string `json:"machineToken"`
		MachineType  string `json:"machineType"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &rec); err != nil {
		return nil
	}
	// 两者必须都非空：缺任一即无法配对，而配对是服务端下发活动的必要条件。
	if rec.MachineToken == "" || rec.MachineType == "" {
		return nil
	}
	return &MachineIdentity{Token: rec.MachineToken, Type: rec.MachineType}
}

// locateRuntimeInfo 定位本机 `runtime-info.exe`；找不到返回空串。
//
// 路径形态：`<home>/<dataDir>/.bin/umid-<platform>-<hash>/runtime-info(.exe)`
// —— **目录名带哈希**（随版本变），故必须枚举目录而不能写死。
//
// ⚠ 环境变量 `QODER_RUNTIME_INFO` 可覆盖（供测试隔离：默认会真的 spawn
// 开发机上的可执行文件，测试必须能把它指到不存在的路径以强制走缓存退路，
// 否则用例结果随"开发机是否装了 Qoder"而变，且每次白等 3.8 秒）。
func locateRuntimeInfo() string {
	if override := os.Getenv("QODER_RUNTIME_INFO"); override != "" {
		// 显式指定的路径必须真实存在，否则视为"没有可执行文件"而走退路。
		if fileExists(override) {
			return override
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	exeName := "runtime-info"
	if runtime.GOOS == "windows" {
		exeName = "runtime-info.exe"
	}
	for _, dirName := range qoderDataDirNames {
		binDir := filepath.Join(home, dirName, ".bin")
		entries, err := os.ReadDir(binDir)
		if err != nil {
			continue // 该目录不存在（未装该客户端）：试下一个
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "umid-") {
				continue
			}
			exe := filepath.Join(binDir, e.Name(), exeName)
			if fileExists(exe) {
				return exe
			}
		}
		// ⚠ 本目录存在但没有可用 exe 时**继续试下一个**，不返回 ——
		// 半安装 / 清理残留会留下空的 `.bin/umid-*` 目录。
	}
	return ""
}

// fileExists 报告路径是否是一个存在的**文件**。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
