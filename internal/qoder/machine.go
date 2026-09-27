// machine.go Qoder 设备身份（Cosy-MachineToken + Cosy-MachineType）。
//
// # 为什么需要它（真实缺陷，2026-09-25 用抓包 + 消融实验定位）
//
// 用户报障：「Qoder 没领过积分却显示『今日已领取』，去 IDE 看还是可领取」。
//
// 早期实现认为 `/sash/` 端点**只需 Bearer + Cosy-ClientType**。该判断
// **不完整** —— 服务端还要校验设备身份，**缺少 machine 头族时不下发
// 「可领取」的活动**。消融实验（同一账号、同一 token、只改头）：
//
//	仅 Cosy-ClientType: 10                      → 1 条 VIEW_DETAILS
//	＋ MachineToken ＋ MachineType              → 2 条，含 CLAIM_BENEFIT/CLAIMABLE/100
//	去掉 MachineToken 或 MachineType 任一        → 退回 1 条
//	单独加任一头（不含配对）                     → 全部无效
//
// 结论：**两个头必须成对**，缺一即失效。
//
// # 值的来源：本机 IDE 的 machine_token.json
//
//	%APPDATA%\Qoder\SharedClientCache\cache\machine_token.json
//	{ "token": "P1gA…", "type": "f677427e14abd0f6c1", "updateAt": … }
//
// 即 Cosy-MachineToken = token，Cosy-MachineType = type。
//
// ⚠ **读不到时返回 nil，调用方照常发请求（只是不带这两个头）**：
// 这与修复前的行为一致，属**保守降级** —— 用户若未安装 Qoder 桌面端
//（纯插件登录的账号）就没有该文件，此时不能让整个积分功能报错。
//
// # 为什么直接读文件而不自己生成 token
//
// 官方经 `runtime-info.exe`（UMID 模块）生成，内部含设备指纹与签名逻辑，
// 复刻代价高；而该文件就在本机、格式稳定，直接读更可靠。
//
// ⚠ 参照实现后来改成"实时 spawn runtime-info.exe"（因为该文件会长期陈旧，
// 实测停在 179 天前）。**我们没有走那条路**，理由：
//
//  1. spawn 一个外部 exe 是**部署耦合**（网关要能找到那个 exe，
//     还要处理超时/杀软扫描）—— 与"网关是纯 Go 单二进制"的定位冲突
//  2. 参照实现自己也说"实测该文件即使很旧，token 依然有效"
//  3. 拿不到就降级（少一次可领活动），而不是报错 —— 代价可控
//
// 所以这里只读文件。若将来发现陈旧文件真的会失效，再评估 spawn。
package qoder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// MachineIdentity 设备身份的两个必需值。
type MachineIdentity struct {
	// Token 发送为 `Cosy-MachineToken`。
	Token string
	// Type 发送为 `Cosy-MachineType`。
	Type string
}

// machineOnce 保证只读一次盘（该值参与每次积分请求的头构造，而文件读取是同步 IO）。
var (
	machineOnce sync.Once
	machineVal  *MachineIdentity
)

// resolveMachineIdentity 解析本机 Qoder 设备身份；拿不到返回 nil。
//
// 结果被缓存（含「拿不到」这一结果）—— 不缓存的话每次积分请求都要读盘，
// 而"没装 Qoder 桌面端"是常见情形，那会让每次请求都白做一次失败的系统调用。
func resolveMachineIdentity() *MachineIdentity {
	machineOnce.Do(func() {
		for _, p := range machineTokenPaths() {
			if id, ok := parseMachineTokenFile(p); ok {
				machineVal = id
				return
			}
		}
	})
	return machineVal
}

// resetMachineIdentity 复位缓存（测试用）。
//
// 导出给测试是因为**默认路径落在用户真实 APPDATA 下**，
// 测试必须能指向 fixture，否则用例结果会随开发机是否装了 Qoder 而变。
func resetMachineIdentity() {
	machineOnce = sync.Once{}
	machineVal = nil
}

// machineTokenPaths machine_token.json 的候选路径。
//
// 桌面端在各平台的目录名带空格（`Qoder` / `Qoder CN`），**两站都列**：
// 只装了其中一个的用户仍应有机会命中。读不到的文件会被跳过，多列无副作用。
//
// ⚠ 环境变量 `QODER_MACHINE_TOKEN_PATH` 可覆盖（供测试隔离与排查用）。
func machineTokenPaths() []string {
	if override := os.Getenv("QODER_MACHINE_TOKEN_PATH"); override != "" {
		return []string{override}
	}
	var paths []string
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			for _, dir := range []string{"Qoder", "Qoder CN"} {
				paths = append(paths, filepath.Join(appData, dir,
					"SharedClientCache", "cache", "machine_token.json"))
			}
		}
	case "darwin":
		for _, dir := range []string{"Qoder", "Qoder CN"} {
			paths = append(paths, filepath.Join(home, "Library", "Application Support",
				dir, "SharedClientCache", "cache", "machine_token.json"))
		}
	default: // linux 等
		for _, dir := range []string{"Qoder", "Qoder CN"} {
			paths = append(paths, filepath.Join(home, ".config",
				dir, "SharedClientCache", "cache", "machine_token.json"))
		}
	}
	return paths
}

// parseMachineTokenFile 从单个文件解析；形状不符或读取失败返回 false。
func parseMachineTokenFile(path string) (*MachineIdentity, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		// 文件不存在 / 无权限：正常降级，不报错。
		return nil, false
	}
	var parsed struct {
		Token string `json:"token"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false
	}
	// 两者必须都是**非空字符串**：缺任一即无法配对，
	// 而配对是服务端下发活动的必要条件，故视为拿不到而不是发一半。
	if parsed.Token == "" || parsed.Type == "" {
		return nil, false
	}
	return &MachineIdentity{Token: parsed.Token, Type: parsed.Type}, true
}
