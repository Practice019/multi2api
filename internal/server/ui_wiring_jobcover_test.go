package server

// ui_wiring_jobcover_test.go 任务覆盖检查在 webui.html 里的接线守卫。
//
// # 用户要求（2026-10-25 原话）
//
//	「检测一下任务调度是否有上游平台的每日签到或凭据续期 没有进行注册?」
//
// # 背景
//
// monkeycode 的签到驱动早就实现好了（按钮能点、手动能签、历史会写），
// 但没实现 gateway.JobExt 的 Jobs()，调度器**静默跳过**它 —— 于是永远不自动跑，
// 而编译通过、界面一切正常。这是契约的一环没人守，所以补了运行期可视化。
//
// # 守的是什么
//
// 后端判据在 internal/admin/jobcover.go（有 Go 单测）。这里守的是**接线**：
//
//	① manifest 里有 job_coverage 字段（后端算了但前端读不到 = 白算）
//	② 前端有 renderJobCoverage 函数
//	③ 该函数被**调用**（写了不调 = 界面永远是「加载中…」）
//	④ 三态渲染都在（只有两态会让没能力的上游全标红）
//
// 这四条都是**静默失效**形态：不报错、编译通过、页面能打开。

import (
	"strings"
	"testing"
)

// TestJobCoverageManifestFieldIsRead 前端必须读 manifest 的 job_coverage。
func TestJobCoverageManifestFieldIsRead(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, "MANIFEST.job_coverage") {
		t.Error("前端没有读 MANIFEST.job_coverage —— 后端算了覆盖检查但界面拿不到，等于白算")
	}
}

// TestJobCoverageRenderIsCalled 渲染函数必须被调用（写了不调 = 永远「加载中…」）。
func TestJobCoverageRenderIsCalled(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, "function renderJobCoverage(") {
		t.Fatal("缺少 renderJobCoverage 函数")
	}
	// 至少要有一处**调用**（不是定义那行）。
	n := strings.Count(src, "renderJobCoverage();")
	if n < 2 {
		t.Errorf("renderJobCoverage() 只被调了 %d 次，应为 2 次"+
			"（首屏 refresh + 「刷新」按钮）—— 少一处会让那条路径下覆盖检查停在旧值", n)
	}
}

// TestJobCoverageHasThreeStates 三态渲染必须齐全。
//
// ⚠ 这条是本功能的**核心判据**：只做「注册了 / 没注册」两态，
//
//	绝大多数本来没有签到能力的上游全会被标红，真正的缺口反而被噪声埋掉。
func TestJobCoverageHasThreeStates(t *testing.T) {
	src := readWebUI(t)
	body := sliceFuncBody(t, src, "function renderJobCoverage(")
	if body == "" {
		t.Fatal("切不到 renderJobCoverage 函数体")
	}
	for _, want := range []struct{ name, needle string }{
		{"无能力（灰）", "— 不适用"},
		{"已注册（绿）", "✓ 已注册"},
		{"未注册（红）", "✗ 未注册"},
	} {
		if !strings.Contains(body, want.needle) {
			t.Errorf("缺少 %s 态（找 %q）—— 三态缺一都会让用户误读", want.name, want.needle)
		}
	}
}

// TestJobCoverageSortsGapsFirst 缺口必须排最前。
//
// 用户打开这个页就是为了找缺口。按上游名排序会让缺口散落在 19 行里。
func TestJobCoverageSortsGapsFirst(t *testing.T) {
	src := readWebUI(t)
	body := sliceFuncBody(t, src, "function renderJobCoverage(")
	if !strings.Contains(body, "missing_checkin") || !strings.Contains(body, "missing_refresh") {
		t.Error("排序没用 missing_* 判据 —— 缺口不会排到最前")
	}
	if !strings.Contains(body, "sort(") {
		t.Error("没有排序 —— 行顺序会随后端 map 遍历跳动")
	}
}

// TestJobCoverageShowsGapCount 标题必须显示缺口总数。
func TestJobCoverageShowsGapCount(t *testing.T) {
	src := readWebUI(t)
	body := sliceFuncBody(t, src, "function renderJobCoverage(")
	if !strings.Contains(body, "处缺口") {
		t.Error("标题没显示缺口总数 —— 一进页面看不到「有几个要处理」")
	}
}
