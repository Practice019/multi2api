package server

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 源码级守卫：成长面板必须真的接上「一键完成」这条能力
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug
//
// 后端有 7 条伪造遥测端点在 internal/workbuddy/autotask_admin.go（一键完成单个
// 任务 / 一键完成整账号 / 待办扫描 / 开学季 / 夜猫子）。移植完成后，
// **webui.html 一条都没引用** —— 能力只存在于 curl 与 PowerShell 里。
//
// 而且后果不只是"少几个按钮"。任务行对 accepted / in_progress 一律渲染：
//
//	? '<span class="dim">去客户端做</span>'
//
// 上面的注释写着"这两类任务**本网页无法代做**，必须去 WorkBuddy 客户端里
// 真正把动作做掉"。移植让这句话变成了**假话**：autoActions 里那 17 条
// 正是网关能直接做完的。于是界面主动劝用户去做一件已经不需要做的事。
//
// # 为什么必须立守卫而不是"改完就好"
//
// 这类回归**不会报错**：按钮被回退掉，页面照样正常渲染，接口照样返回 200。
// 唯一的信号是人眼去看那一屏。而这个仓库里"下一个人照着旁边旧代码抄"已有先例
// （见 webui_endpoint_guard_test.go 与 upstream_isolation_test.go 的注释）。
//
// 所以判据做成**对能力的接线断言**，与按钮数量、位置无关：
//
//	能力表端点、单任务端点、整账号端点、开学季端点、行内按钮的渲染与委托、
//	能力表的消费点、两个按钮 id 的绑定 —— 少任何一项都会红。
//
// # 为什么"消费点"也算一条判据
//
// 只查端点字面量不够：端点可以被引用了却没有任何按钮真正走它。
// 因此额外钉住 `autoCodes.has(t.task_code)` —— 它是"任务行先查能力表、
// 再决定渲染一键完成还是去客户端做"这一分支的唯一标志。
// 少了它，端点还在，但界面又回到对可自动化任务说"去客户端做"。

// checkWebUITaskAutoWiring 返回 src 里缺失的接线项（空 = 通过）。
//
// 抽成纯函数（而不是直接断言真实文件）是为了能在下面注入"被回退的源码"
// 证明这条守卫**真的会红** —— 与 TestWebUIEndpointGuardCatchesInjectedDefect
// 同款做法：一个从没红过的守卫无法证明它守得住东西。
func checkWebUITaskAutoWiring(src string) []string {
	code := stripJSComments(src)
	var problems []string

	need := []struct {
		name string
		want string
	}{
		{"能力表端点（前端据此决定哪些任务有按钮）", "/admin/growth/auto/actions"},
		// ⚠ 必须带结尾引号：否则会被 /admin/growth/auto/actions 与
		// /admin/growth/auto-all 前缀命中，判据退化成"文件里有 auto 字样"。
		{"单任务端点（行内一键完成）", "/admin/growth/auto'"},
		{"整账号端点（一键完成待办）", "/admin/growth/auto-all"},
		{"开学季端点", "/admin/school/run"},
		{"行内按钮的渲染", `data-gauto="${esc(t.task_code)}"`},
		{"行内按钮的事件委托", "closest('button[data-gauto]')"},
		{"能力表的消费点（任务行先查表再决定渲染什么）", "autoCodes.has(t.task_code)"},
		// 已做完的任务不得长出「一键完成」按钮。
		//
		// 这一条来自**实测发现的缺陷**：claimed 的任务 claimable 为 false，
		// 于是它会一路落到一键完成那一分支 —— 只看能力表的话，「已领取」的行
		// 也会显示按钮，点了只回一句"已完成（1/1）"。
		// 判据钉住"排除 done"，并要求复用 GROWTH_VIEWS.done.test 而不是另写
		// 一遍 status 判断（同一语义在视图与按钮两处各定义一次必然漂移）。
		{"已完成的任务不得渲染一键完成", "!GROWTH_VIEWS.done.test(t)"},
		{"「一键完成待办」按钮的绑定", "$('btnGrowthAutoAll').onclick"},
		{"「开学季一键完成」按钮的绑定", "$('btnGrowthSchoolRun').onclick"},
	}
	for _, n := range need {
		if !strings.Contains(code, n.want) {
			problems = append(problems, n.name+" → 期望源码里包含 "+n.want)
		}
	}

	// 反向断言：那句已被证伪的措辞不得回到**代码**里。
	//
	// stripJSComments 已经把注释剥掉，所以这里命中 = 它出现在会被执行的
	// JS 字符串里（例如某个 notice/toast 的文案）—— 那正是要防的。
	// 注释里提到"原文写的是 XX"是允许的，剥注释恰好实现了这个豁免，
	// 否则说明文字会让守卫自己红，下一个人会去删注释而不是改代码。
	if strings.Contains(code, "本网页无法代做") {
		problems = append(problems,
			"任务行/错误提示里仍宣称「本网页无法代做」→ 该结论已被伪造遥测层证伪，"+
				"应改为「带一键完成按钮的点它，其余才去客户端」（注释里出现不受影响）")
	}

	return problems
}

// TestWebUITaskAutoWiring 真实 webui.html 必须通过全部接线判据。
func TestWebUITaskAutoWiring(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	// 必须真的读到内容，否则守卫会静默变成"永远绿灯"（fail-open）。
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}
	for _, p := range checkWebUITaskAutoWiring(src) {
		t.Errorf("webui.html 缺少一键完成的接线：%s", p)
	}
}

// TestWebUITaskAutoWiringGuardCatchesRevertedCode 证明守卫在被回退的源码上会红。
//
// 注入的 defect 就是**移植完成但前端未接线**的那个真实形态（本次修的就是它）：
// 端点存在，但任务行对可自动化任务仍显示「去客户端做」。
func TestWebUITaskAutoWiringGuardCatchesRevertedCode(t *testing.T) {
	// 回退形态①：完全没有接线。
	reverted := `
    // 领取优先于接单：completed 的任务已经做完了，唯一可做的就是领奖。
    const btn = t.claimable
      ? '<button data-gclaimreward="...">领取 N 分</button>'
      : (t.status === 'not_accepted' && !t.locked)
        ? '<button data-gaccept="...">接单</button>'
        : (t.status === 'accepted' || t.status === 'in_progress')
          ? '<span class="dim">去客户端做</span>'
          : '<span class="dim">—</span>';
  `
	if got := checkWebUITaskAutoWiring(reverted); len(got) == 0 {
		t.Error("守卫没有在「完全未接线」的源码上报错 —— 它是 fail-open 的")
	}

	// 回退形态②：端点都引用了、按钮也渲染了，但**没人消费能力表** ——
	// 于是按钮会出现在所有任务行上（包括不可自动化的），或者干脆不出现。
	// 这一形态专门证明"消费点"那条判据不是装饰。
	halfWired := `
  async function loadGrowth(refresh) {
    const a = await admin('/admin/growth/auto/actions');
    autoCodes = new Set(a.actions.map(x => x.task_code));
  }
  <button data-gauto="${esc(t.task_code)}" data-uid="${esc(g.uid)}">一键完成</button>
  const au = ev.target.closest('button[data-gauto]');
  await admin('/admin/growth/auto', { method: 'POST' });
  await admin('/admin/growth/auto-all', { method: 'POST' });
  await admin('/admin/school/run', { method: 'POST' });
  $('btnGrowthAutoAll').onclick = () => growthAutoAll($('btnGrowthAutoAll'));
  $('btnGrowthSchoolRun').onclick = () => growthSchoolRun($('btnGrowthSchoolRun'));
  `
	got := checkWebUITaskAutoWiring(halfWired)
	if len(got) == 0 {
		t.Error("守卫没有在「引用了端点但没人消费能力表」的源码上报错")
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "autoCodes.has(t.task_code)") {
		t.Errorf("期望失败信息点名缺失的消费点，实际是：\n%s", joined)
	}

	// 反向断言本身也要有效：把假话写进**代码**（不在注释里）必须被抓到。
	falseClaim := `
  notice('接单被前置任务挡住', '这一步只能在 WorkBuddy 客户端里做，本网页无法代做。');
  `
	if got := checkWebUITaskAutoWiring(falseClaim); !strings.Contains(strings.Join(got, "\n"), "本网页无法代做") {
		t.Errorf("写进 JS 字符串的「本网页无法代做」没有被抓到：\n%s", strings.Join(got, "\n"))
	}

	// 而写在**注释**里必须被豁免 —— 否则说明文字会让守卫自己红，
	// 下一个人会去删注释而不是改代码（webui_endpoint_guard_test.go 的同款理由）。
	commentOnly := `
  // 改造前这一格写的是"这两类任务本网页无法代做"，移植后该结论不成立。
  `
	if got := checkWebUITaskAutoWiring(commentOnly); strings.Contains(strings.Join(got, "\n"), "本网页无法代做") {
		t.Errorf("注释里的说明文字不该触发这条判据，实际报了：\n%s", strings.Join(got, "\n"))
	}
}

// TestWebUITaskAutoGuardCatchesMissingDoneExclusion 针对**本次实测发现的缺陷**
// 做一次外科式反向验证：只把"排除已完成"这一处从真实文件里退回去，守卫必须点名它。
//
// # 为什么要单独做这一条
//
// 缺陷形态很隐蔽：接线全在、端点全对、按钮也会出现 —— 唯一的错是
// **「已领取」的任务行也长了按钮**。它不会报错，只会让用户点了得到
// "已完成（1/1）"。上面 forms①② 都是"整体未接线"，抓不到这一种。
//
// 用真实文件做单点回退（而不是手写一小段 fixture）的好处：判据里的其它 8 项
// 必然全部满足，于是**只有**这一条能红 —— 失败信息必然指向这就是根因。
func TestWebUITaskAutoGuardCatchesMissingDoneExclusion(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	const wired = "autoCodes.has(t.task_code) && !GROWTH_VIEWS.done.test(t)"
	const reverted = "autoCodes.has(t.task_code)"
	broken := strings.Replace(src, wired, reverted, 1)
	// 替换必须真的发生：否则（比如将来改写了这个表达式）本测试会变成
	// "在未改动的文件上跑一遍然后通过"的空转测试 —— fail-open。
	if broken == src {
		t.Fatalf("单点回退没有生效：webui.html 里找不到 %q\n"+
			"说明这一格的写法变了，本测试已失去意义，请同步更新判据。", wired)
	}
	got := checkWebUITaskAutoWiring(broken)
	if len(got) != 1 {
		t.Errorf("期望只报「已完成的任务不得渲染一键完成」这 1 条，实际报了 %d 条：\n%s",
			len(got), strings.Join(got, "\n"))
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "已完成的任务不得渲染一键完成") {
		t.Errorf("守卫没有点名缺陷根因（已完成的任务不得渲染一键完成）：\n%s", joined)
	}
}
