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
// # 值的来源：**实时 spawn** `runtime-info.exe`（主路径），磁盘缓存只作退路
//
//	主路径  <home>/.qoder{,-cn}/.bin/umid-<hash>/runtime-info(.exe) 3 --account-stdin
//	退路    %APPDATA%\Qoder\SharedClientCache\cache\machine_token.json
//	        { "token": "P1gA…", "type": "f677427e14abd0f6c1", "updateAt": … }
//
// ⚠ **本轮改的（用户报障"qodercn 签到失败"）**：旧实现**只读缓存文件**，
// 而实测该文件在本机**根本不存在**（纯登录流程不产生它）—— 于是
// "读不到就保守降级"实际变成"两个头永远不发 → 签到永远失败"。
//
// 旧注释里的三条理由也都被实测推翻或不再成立，逐条记下免得有人再改回去：
//
//	"spawn 是部署耦合"      → 但 exe 本来就在用户机上（IDE 自带），
//	                          且找不到就退回缓存，不会让功能报错
//	"参照说旧文件仍然有效"   → 前提是**文件存在**；本机不存在
//	"代价可控（少一次可领）" → 实际代价是**整个签到功能不可用**
//
// 因此现在与参照实现同序：实时 spawn 优先，缓存兜底。详见 runtimeinfo.go。
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
// # 顺序：**先实时 spawn，再退磁盘缓存**（本轮改的，用户报障）
//
// 旧实现只读磁盘缓存，前提是"那个文件存在、只是陈旧"。
// 实测（2026-09-30）：**文件根本不存在**（纯登录流程不产生它），
// 于是两个头从来没发出去 → 服务端不下发可领活动 → 签到**永远失败**，
// 而额度、Token 都正常，看起来像"上游不给签"。
//
// 现在与参照实现同序：实时 spawn 是主路径（见 runtimeinfo.go 的实测证据），
// 缓存只是退路（万一 exe 未安装 / 超时）。两条都拿不到才算 nil。
//
// 结果被缓存（**含"拿不到"这一结果**）—— 不缓存的话每次积分请求都要
// spawn 一个 exe（实测约 3.8 秒），那会让额度查询慢到不可用。
func resolveMachineIdentity() *MachineIdentity {
	machineOnce.Do(func() {
		if id := resolveMachineIdentityLive(); id != nil {
			machineVal = id
			return
		}
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
