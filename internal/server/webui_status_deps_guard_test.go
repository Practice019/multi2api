package server

import (
	"os"
	"strings"
	"testing"
)

// readStatusPage 读状态页源码；读不到就 Fatal（**不许**静默通过 —— 空匹配不算通过）。
func readStatusPage(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("webui-status.html")
	if err != nil {
		t.Fatalf("读 webui-status.html 失败: %v", err)
	}
	if strings.TrimSpace(string(b)) == "" {
		t.Fatal("webui-status.html 读到空内容 —— 守卫失效（fail-open）")
	}
	return string(b)
}

// TestStatusPageHasNoDepsSection 状态页**不许再有**「依赖健康」段（T-454）。
//
// # 用户原话（附图把这一整段框住了）
//
//	「这些都不要」
//
// # 为什么它必须不回来
//
// 它的四张卡（SQLite / data JSON / 号池 / 上游）在用户看来是"运维细节" ——
// 界面上真正要看的是"服务活着吗、请求健不健康"。留着它只会稀释信号。
//
// ⚠ 判据同时查**函数名**与**段标题**：只查其中一个的话，
//
//	把函数改名（或把 section 标题挪到别处）就能绕过。
func TestStatusPageHasNoDepsSection(t *testing.T) {
	page := readStatusPage(t)
	for _, gone := range []string{"renderDeps", "依赖健康"} {
		if strings.Contains(page, gone) {
			t.Errorf("状态页里仍有「%s」—— 用户明确要求删掉整个「依赖健康」段。"+
				"若确实要加回来，先回答「它比『服务状态 + 请求健康』多告诉用户什么」。", gone)
		}
	}
}

// TestStatusPageHasNoNotesCard 状态页**不许再有**「说明」卡（notes 渲染）（T-454）。
//
// # 背景（值得记一句）
//
// 这张卡是 T-452 我按"后端拆了 warnings/notes，前端必须也分开渲染"加上的 ——
// 逻辑上没错，但**用户看到之后说不要**。
// ⇒ 产品的最终判断权在用户；**后端 notes 字段与 T-440 的两条测试保持不变**
//
//	（API 契约与"Redis 不参与 verdict"这个正确性质都不受影响）。
//
// ⚠ 这里查的是前端渲染的标识（srnotes / svcNotes），**不是**后端的 notes 字段 ——
//
//	后者故意保留（见 internal/sysmon/status.go）。
func TestStatusPageHasNoNotesCard(t *testing.T) {
	page := readStatusPage(t)
	for _, gone := range []string{"srnotes", "svcNotes"} {
		if strings.Contains(page, gone) {
			t.Errorf("状态页里仍有「%s」（notes/说明卡的渲染）—— 用户否掉了这张卡。"+
				"⚠ 后端 notes 字段是有意保留的，别顺手一起删。", gone)
		}
	}
}
