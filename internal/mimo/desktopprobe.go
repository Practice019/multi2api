// desktopprobe.go — 登录完成后自动打通客户端计费（route）通道：
// 统一从浏览器获取小米账号登录会话（passToken 套），彻底解耦桌面客户端软件。
// 来源（scripts/mimo/read-mimo-cookies.py）：
//   ① Edge 全部 profile（v10 DPAPI 解密）
//   ② Chrome 全部 profile（v20 app-bound 跳过）
//   ③ MiMo 桌面端 Cookie 库（明文，最后兜底）
// 探测按登录账号 uid 精确匹配（多 profile 多账号时不绑错），命中 → 登录产物
// route 化（channel=route + passToken/cUserId/deviceId），此后网关 SSO 链自动
// 换 serviceToken 走 mimo-server-cn 主网关（免费服务端计费）。
//
// 依赖：python + scripts/mimo/read-mimo-cookies.py（脚本路径可经环境变量覆盖）。
// 未命中（本机无该账号的浏览器/桌面登录会话）→ 探测失败 → 登录产物保持 paid
// 兜底；可走 /admin/mimo/sync 手动上送该账号 Cookie 串（handleSync）。
package mimo

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// DesktopCookie 本机登录会话 Cookie 快照（route 通道弹药，SSO 链最低要求
// passToken+cUserId，userId/deviceId 让 serviceLogin 走正常 STS 分支）。
type DesktopCookie struct {
	PassToken string `json:"passToken"`
	CUserID   string `json:"cUserId"`
	UserID    string `json:"userId"`
	DeviceID  string `json:"deviceId"`
}

// desktopProbeScript 候选脚本路径（运行目录=项目根；可经环境变量覆盖）。
func desktopProbeScript() string {
	if v := os.Getenv("MIMO_DESKTOP_COOKIE_SCRIPT"); v != "" {
		return v
	}
	return filepath.Join("scripts", "mimo", "read-mimo-cookies.py")
}

// DefaultDesktopProbe 默认探测实现：exec python 按 uid 多源读本机登录会话 Cookie。
var DefaultDesktopProbe = func(uid string) (*DesktopCookie, error) {
	cmd := exec.Command("python", desktopProbeScript(), "--uid="+uid)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("desktop cookie probe failed: %v", err)
	}
	var ck DesktopCookie
	if err := json.Unmarshal(out, &ck); err != nil {
		return nil, fmt.Errorf("desktop cookie probe parse failed: %v", err)
	}
	return &ck, nil
}

// DefaultLaunchBrowserLogin 受控浏览器实时登录：自启 Edge/Chrome **独立实例**
// （headful 窗口自动弹出，不污染用户日常浏览器），导航到小米账号登录页，
// 用户在弹出窗口登录该账号，CDP 监控 Network.getAllCookies 自动收割 Cookie
// （passToken 套）。超时/用户关窗 → 错误。见 scripts/mimo/login-with-browser.mjs。
var DefaultLaunchBrowserLogin = func(uid string) (*DesktopCookie, error) {
	script := filepath.Join("scripts", "mimo", "login-with-browser.mjs")
	cmd := exec.Command("node", script, "--uid="+uid)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("browser login failed: %v", err)
	}
	var ck DesktopCookie
	if err := json.Unmarshal(out, &ck); err != nil {
		return nil, fmt.Errorf("browser login parse failed: %v", err)
	}
	return &ck, nil
}
