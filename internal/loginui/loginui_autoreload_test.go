package loginui

import (
	"strings"
	"testing"
)

// TestLoginSuccessAutoReloadsPool 钉住 T-343 的根治动作。
//
// # 用户实测
//
// 新账号登录成功、凭证也落了盘，但界面显示「额度完成：已更新 0 · 失败 1」且 TOKEN 列是 —。
// 实测证据：直查新 uid 得到 `codar: 找不到 uid=… 的凭证`，而启动日志是 `并池 1 个账号`
// ⇒ **登录后没有重扫目录并池**（只在启动时扫一次）。
//
// ⇒ 页面在**两条**成功路径（验码登录 / 第二步 IP 验证）成功后都要触发一次并池，
//
//	调的是 core 的既有路由，与用户手点「重载 auths」等价。
func TestLoginSuccessAutoReloadsPool(t *testing.T) {
	html := Render(PageSpec{Title: "T", SMSURL: "/s", VerifyURL: "/v"})

	if got := strings.Count(html, "/admin/accounts/reload"); got < 2 {
		t.Errorf("并池调用只有 %d 处：两条成功路径都要有（否则新账号不进池 ⇒ 额度失败、TOKEN 列空）", got)
	}
	vi := strings.Index(html, "VERIFY_URL")
	if vi < 0 {
		t.Fatal("找不到 VERIFY_URL 的使用点 —— 先修结构")
	}
	if !strings.Contains(html[vi:], "/admin/accounts/reload") {
		t.Error("reload 调用不在 verify（验码登录）的处理链里")
	}
}
