package codar

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// T-364：微信绑定页（seam ③ —— RenderWechatPage 的 HTML 输出）
//
// 页面是**自包含**的：二维码 <img> + 状态行 + 轮询脚本；所有 fetch 走**同源相对路径**
//（部署到服务器也要能用 ⇒ 断言里显式禁止写死 host）。
// ─────────────────────────────────────────────────────────────────────────────

// TestRenderWechatPageShowsQR 未绑定 ⇒ 有二维码容器、状态行、轮询、刷新按钮。
func TestRenderWechatPageShowsQR(t *testing.T) {
	html := RenderWechatPage(WechatPageSpec{UID: "user_x", Bound: false})

	for _, want := range []string{
		`id="qrbox"`,                 // 二维码容器
		`id="qrimg"`,                 // 真正的 <img>（pending 时填 src）
		`id="status"`,                // 状态行
		`role="status"`,              // 无障碍：状态变化可被读屏播报
		`id="refresh"`,               // 「刷新二维码」按钮
		"正在加载二维码",                    // 加载中文案
		"请使用微信扫码绑定",                  // 初始状态文案
		"/admin/codar/wechat/qr",     // 取码端点（相对路径）
		"/admin/codar/wechat/status", // 轮询端点（相对路径）
		"setInterval",                // 轮询确实存在
		"1000",                       // 轮询间隔毫秒（与桌面端 POLL_DELAY_MS 一致）
		"刷新二维码",                      // 手动刷新按钮文案
	} {
		if !strings.Contains(html, want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
	// 官方原文（提醒用户"为什么必须绑微信"）
	if !strings.Contains(html, "2,000") {
		t.Errorf("页面应说明官方奖励口径（2,000 点数）")
	}
	// ⚠ 部署到服务器必须能用 ⇒ 不得写死 host
	if strings.Contains(html, "http://127.0.0.1") || strings.Contains(html, "localhost:") {
		t.Errorf("页面写死了 host（部署到服务器会失效）")
	}
}

// TestRenderWechatPageBoundHidesQR 已绑定 ⇒ 不渲染二维码、不轮询、显示成功态。
func TestRenderWechatPageBoundHidesQR(t *testing.T) {
	html := RenderWechatPage(WechatPageSpec{UID: "user_x", Bound: true})

	if strings.Contains(html, "<img") {
		t.Errorf("已绑定时不该渲染二维码 <img>")
	}
	if strings.Contains(html, "setInterval") {
		t.Errorf("已绑定时不该轮询")
	}
	if !strings.Contains(html, "已绑定微信") {
		t.Errorf("已绑定应显示「已绑定微信」")
	}
	if !strings.Contains(html, "user_x") {
		t.Errorf("标题应带 uid")
	}
}

// TestRenderWechatPageEscapesUID uid 里塞引号/尖括号不能跳出标签（页面是同源的）。
func TestRenderWechatPageEscapesUID(t *testing.T) {
	html := RenderWechatPage(WechatPageSpec{UID: `a"b<c>`, Bound: false})
	if strings.Contains(html, `a"b<c>`) {
		t.Errorf("uid 未转义，会跳出 HTML 上下文")
	}
}

// TestHandleWechatPageHeaders 页面头：text/html + no-store（不能缓存扫码页）。
func TestHandleWechatPageHeaders(t *testing.T) {
	p := stubInviteUpstreamFn(t, wechatStubFn(false, "SCENE", "pending", false))
	withTwoAccounts(t, p, t.TempDir(), "u1")

	w := httptest.NewRecorder()
	p.handleWechatPage(w, httptest.NewRequest(http.MethodGet, "/admin/codar/wechat-page?uid=u1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP = %d，want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q，want text/html", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q，want no-store", cc)
	}
	body := w.Body.String()
	if !strings.Contains(body, "user_x") && !strings.Contains(body, "u1") {
		t.Errorf("页面应显示 uid：%s", truncateStr(body, 200))
	}
	// uid 不存在也要能打开页面（用户点进去自己看得到错误），不返 404/空页
	if !strings.Contains(body, "qrbox") && !strings.Contains(body, "已绑定") {
		t.Errorf("页面骨架缺失")
	}
}

// TestHandleWechatPageUnknownUIDStillRenders uid 不存在 ⇒ 仍渲染页面（而不是 500/404）。
func TestHandleWechatPageUnknownUIDStillRenders(t *testing.T) {
	p := stubInviteUpstreamFn(t, wechatStubFn(false, "S", "pending", false))
	withTwoAccounts(t, p, t.TempDir(), "u1")

	w := httptest.NewRecorder()
	p.handleWechatPage(w, httptest.NewRequest(http.MethodGet, "/admin/codar/wechat-page?uid=ghost", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP = %d，want 200（页面要能打开）", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ghost") {
		t.Errorf("页面标题应带 uid ghost")
	}
}
