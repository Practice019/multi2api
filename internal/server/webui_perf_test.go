package server

import (
	"strings"
	"testing"
)

// TestWebUIPerfRequestConvergence 前端性能第一批：请求收敛与去重（T-408）。
//
// # 实测基线（Playwright，主脑提供）
//
//	请求数 49 · 总传输 1,315,424 字节 · FCP 432ms · load 485ms
//	其中纯重复（同 URL 200ms 内两次）：/healthz ×2 · /status ×2 · /admin/stats ×2 · /admin/ui/manifest ×2
//	并且首屏就拉了**用户没打开的面板**：loomy tasks/invite(page=1 与 page=2)、codar invite、
//	travel、growth、client-login、models/preview —— 用户原话「仪表盘的表加载很慢啊」。
//
// # 四条判据
//
//	① 面板 loader 只在**当前显示**的面板里触发（syncProviderPanels 收紧到 .active）
//	② in-flight 去重存在，且**只作用于 GET**（POST 是两个真实操作，不能合并）
//	③ warmOtherPages 延迟 ≥3 秒（快速切面板时不该白打后端）
//	④ models/preview 只有**一处**直呼，且走共享取数器（单飞 + 30 秒缓存）
func TestWebUIPerfRequestConvergence(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// ① 首屏不再预取非当前面板
	if !strings.Contains(html, "if (!sec.classList.contains('active')) continue;") {
		t.Error("syncProviderPanels 没有收紧到「只加载当前显示的面板」" +
			"⇒ 首屏会给所有可见上游面板各发一遍请求（用户没打开的面板也发）")
	}

	// ② in-flight 去重
	if !strings.Contains(html, "const inflight = new Map()") {
		t.Error("缺 in-flight 去重表（const inflight = new Map()）")
	}
	if !strings.Contains(html, "function dedupe(") {
		t.Error("缺 dedupe() —— 同一 URL 在飞时应复用同一个 Promise")
	}
	if !strings.Contains(html, "inflight.delete(key)") {
		t.Error("dedupe 没有在结束时清理 inflight ⇒ 第一次请求完成后会永远复用一个已完成的 Promise")
	}
	// ②b 只对 GET 生效
	if !strings.Contains(html, "if (_m === 'GET' && !(opt && opt.force)) return dedupe(") {
		t.Error("api() 的 GET-only 去重判据缺失（POST 不能去重，且 force 必须穿透）")
	}
	if !strings.Contains(html, "if (_m === 'GET' && !(opts && opts.force)) return dedupe(") {
		t.Error("admin() 的 GET-only 去重判据缺失")
	}
	// ②c 原实现仍在（只是改名 apiRaw/adminRaw，行为不变）
	if !strings.Contains(html, "async function apiRaw(") || !strings.Contains(html, "async function adminRaw(") {
		t.Error("apiRaw/adminRaw 不存在 ⇒ 去重包装把原实现弄丢了")
	}

	// ③ warmOtherPages 延迟
	if !strings.Contains(html, "WARM_DWELL_MS") {
		t.Error("缺 WARM_DWELL_MS 常量 —— 预热必须延迟而不是一开面板就打")
	}
	if !strings.Contains(html, "setTimeout(() => {") || !strings.Contains(html, "warmTimer = setTimeout(") {
		t.Error("warmOtherPages 没有把预热包进 setTimeout")
	}
	if !strings.Contains(html, "function clearWarmTimer(") {
		t.Error("缺 clearWarmTimer —— 面板切走时应取消预热（停留不足 3 秒就不预热）")
	}

	// ④ models/preview 只有一处直呼且走共享取数器
	n := strings.Count(html, "admin('/admin/models/preview')")
	if n != 1 {
		t.Errorf("admin('/admin/models/preview') 直呼 %d 处，应恰好 1 处（多出来的那处就是首屏重复的 400~500ms）", n)
	}
	if !strings.Contains(html, "sharedGet('ep:/admin/models/preview'") {
		t.Error("models/preview 没有走 sharedGet（单飞 + 缓存）⇒ 会被发两次")
	}
}
