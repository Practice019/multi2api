package workbuddy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 守卫：能力表只能有**一份**事实来源（后端），前端不得把任务码抄一份
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug
//
// 前端的按钮由「任务码在不在能力表里」决定，而能力表是**动态取**的
// （GET /admin/growth/auto/actions → webui.html 的 autoCodes，
// 接线判据见 internal/server/webui_task_auto_test.go）。
//
// 如果有人在 webui.html 里写死一份 ['chat_5', 'expert_5', ...]，
// 就出现了第二份事实来源，而它的漂移是**静默**的：
//
//	后端加第 18 条 → 前端永远不给它渲染按钮，且不报错、不告警；
//	后端删掉一条   → 前端仍然渲染按钮，点了得到 501「无法自动完成」，
//	                 用户会以为是自己点坏了或网关坏了。
//
// 这正是本仓库反复出现的那类"第 N 个调用点"问题（同款守卫见
// internal/server/upstream_isolation_test.go 与 webui_endpoint_guard_test.go）。
//
// # 判据为什么是"带引号出现"而不是裸字符串包含
//
// 裸包含会把**注释里的说明文字**算成违例 —— webui.html 里确实有两处注释
// 提到具体任务码（解释 black_cat 为何没有 target、解释它只在 23:00–08:00 计分）。
// 那种红是"有理由的红"，下一个人会去删注释而不是改代码，守卫就此失效
// （webui_endpoint_guard_test.go 的剥注释判据是同一个理由）。
//
// 而 JS 里要写死一个任务码**必须**带引号（'x' / "x" / `x`），
// 所以"带引号出现"恰好等价于"被硬编码成前端字符串字面量"。
func TestAutoActionCodesAreNotHardcodedInWebUI(t *testing.T) {
	uiPath := filepath.Join("..", "server", "webui.html")
	raw, err := os.ReadFile(uiPath)
	if err != nil {
		t.Fatalf("读 %s 失败: %v\n"+
			"这条守卫跨包读前端的源文件：webui.html 一旦改名/移动，必须同步改这里，"+
			"否则守卫会静默失效。", uiPath, err)
	}
	src := string(raw)
	// fail-open 检查：读不到内容时 Guard 会"永远绿灯"。
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}

	codes := AutoTaskCodes()
	if len(codes) == 0 {
		t.Fatal("AutoTaskCodes() 返回空 —— 判据没有输入，守卫失效（fail-open）")
	}

	for _, c := range codes {
		for _, q := range []string{"'", `"`, "`"} {
			lit := q + c + q
			idx := strings.Index(src, lit)
			if idx < 0 {
				continue
			}
			t.Errorf("webui.html 硬编码了任务码 %s（第 %d 行）：\n    %s\n"+
				"能力表只能有一份事实来源：后端 AutoTaskCodes()/autoActions。\n"+
				"前端应改为 GET /admin/growth/auto/actions 动态取（webui.html 的\n"+
				"autoCodes 已这么做）；写死会让后端增删任务时前端**静默漂移**。",
				c, lineNumberAt(src, idx), strings.TrimSpace(lit))
		}
	}
}

// TestAutoTaskCodesCoverTheWiredUIFlows 记录一个容易踩的前提：
//
// 前端对"未在能力表里的任务"退回「去客户端做」。所以**凡是网关能做**的任务
// 都必须在 autoActions 表里 —— 只在 run* 函数里实现了、忘了登记的那一条，
// 界面上会显示成"只能去客户端做"，而它其实能一键完成。
//
// 这里钉住表里几个被界面与文档反复引用的关键码存在，防止改名/误删。
func TestAutoTaskCodesCoverTheWiredUIFlows(t *testing.T) {
	have := map[string]bool{}
	for _, c := range AutoTaskCodes() {
		have[c] = true
	}
	for _, want := range []string{
		"chat_5",            // 纯伪造：对话活跃事件
		"Library_read",      // 纯伪造：资料库阅读（真机验证过 0/1 → 1/1）
		"create_canvas",     // 纯伪造：设计画布（+300 分）
		"expert_5",          // 需真实 chat 拿 requestId
		"Expert_team_use_3", // 需真实 chat（team 类型）
		"Expert_lighthouse", // 需真实 chat（轻量云，带 has_expert）
		"skill_1",           // 需真实对话 + skill_info
		"Model_chat_GLM5.2", // 需真实 glm-5.2 对话
		"black_cat",         // 唯一 Attempt=true：只在 23:00–08:00 计分
	} {
		if !have[want] {
			t.Errorf("能力表里缺少 %q —— 网关能做但未登记的任务，"+
				"界面上会显示成「去客户端做」（静默的能力缺失）", want)
		}
	}
}

// lineNumberAt 返回 idx 在 s 中的 1-based 行号。
//
// 守卫的失败信息必须直接指向要改的那一行：一条只说"文件里存在 X"的报错，
// 在一个 6400 行的 HTML 里等于让人自己 grep。
func lineNumberAt(s string, idx int) int {
	if idx < 0 || idx > len(s) {
		return -1
	}
	return strings.Count(s[:idx], "\n") + 1
}

// ---------------------------------------------------------------------------
// 守卫：一键完成之后，前端读缓存必须能看到新状态
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug（实测发现）
//
// 前端在「一键完成」之后读的是**缓存**快照（`/admin/growth` 不带 refresh=1，
// 见 webui.html 的 loadGrowth(false)）—— 这是刻意的：强制回源要把整池账号
// 重探一遍（3 账号实测 7.2s），而那正是"点了要等 7 秒"的病根。
//
// 但缓存必须由**写入方**负责失效：growth.go 里每条写路径写完都调
// refreshOwnSnapshot（就地重探该账号，~1ms 读、~2s 探）。
// autotask.go 的 AutoTaskOne / AutoTaskAll **都没有调**，于是：
//
//	2026-09-14 实测：对 ca19abfd 跑完 auto-all（186s、14 项领奖成功）后，
//	/admin/growth（缓存）仍显示 15 条待办 —— 前端任务明细原样不动，
//	用户会以为整轮什么都没做，然后再点一次。强制 refresh=1 才看到真实值 1 条。
//
// # 为什么是源码级判据
//
// 判据本该是行为级的（"写完 probeGrowth 被调用一次"），但那需要一套能观测
// 上游探测次数的假客户端；而这条缺陷的本质是**漏了一行调用**，
// 源码级判据足够精确，且失败信息能直接指向要补的那一行
// （与 webui_endpoint_guard_test.go 同款取舍）。
func TestAutotaskWritePathsRefreshGrowthSnapshot(t *testing.T) {
	const srcFile = "autotask.go"
	raw, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", srcFile, err)
	}
	src := string(raw)
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("源码读到了空内容 —— 守卫失效（fail-open）")
	}

	// 两个写入方法各自都必须刷新，所以判据是"至少 2 处调用"而不是"存在"。
	// 只查存在的话，删掉其中一处（比如只修了 AutoTaskOne）依然是绿的。
	const call = "p.refreshOwnSnapshot(uid,"
	if n := strings.Count(src, call); n < 2 {
		t.Errorf("%s 里只有 %d 处 %s，期望至少 2 处（AutoTaskOne 与 AutoTaskAll 各一）：\n"+
			"漏掉的那一条会让「一键完成」之后前端读缓存看到旧状态 ——\n"+
			"任务明细里那些待办原样还在（实测：跑完 186s、14 项领奖成功，\n"+
			"缓存仍显示 15 条待办），用户会以为没生效并重复点击。",
			srcFile, n, call)
	}

	// 两个方法都必须在（防止有人把整段删掉后守卫因为"0 < 2"以外的方式变绿）。
	for _, fn := range []string{"func (p *Provider) AutoTaskOne(", "func (p *Provider) AutoTaskAll("} {
		if !strings.Contains(src, fn) {
			t.Errorf("%s 里找不到 %s —— 判据的前提没了，守卫需要同步更新", srcFile, fn)
		}
	}
}
