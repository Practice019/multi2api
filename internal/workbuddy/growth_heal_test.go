// growth_heal_test.go 守卫轮的「先补前置、再重试接单」自愈。
//
// # 守的是用户实测的那个卡死状态
//
// 用户："成长计划哪里 对于每个项目 先进行领取一只 Buddy 然后接单所有任务
//
//	这个设置成循环任务 只要有一个账号有单可接 就执行"
//
// 他的账号（17005247808）实测：18 个任务里 17 个卡在 `not_accepted`，
// 而 `first_buddy` 是它们的**前置**。上游对接单的回应是
//
//	{"status":"error","message":"prerequisite not met: first_buddy"}
//
// 这条被 `acceptRejectionReason` 正确归成 skip（不是故障）—— 但改造前
// **守卫轮里没有任何一步会去做前置任务**，于是：
//
//	每轮：探快照 → 看到 AcceptableCount=17 → 接单 → 全被前置挡住 → skip
//	      → 下一轮重复，永远如此
//
// 界面看起来只是"在等"，实际永远不会自愈。用户手工点一次「一键完成」
// 就解开了（实测 ok=true，+300 分 +8 能量）—— 也就是说：
// **手工能做、循环里没人做**。这条测试守的就是"循环里也做"。
package workbuddy

import (
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

// TestGrowthWatchHealsBlockingPrerequisite 被 first_buddy 挡住时，
// 守卫轮必须先补前置、再重试一次接单。
func TestGrowthWatchHealsBlockingPrerequisite(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
	  {"task_code":"first_buddy","accept_status":"not_accepted","reward_credit":300},
	  {"task_code":"chat_5","accept_status":"not_accepted","reward_credit":100},
	  {"task_code":"create_canvas","accept_status":"not_accepted","reward_credit":300}
	]}`
	// 前两项被 first_buddy 挡住；first_buddy 自身不需要接单（上游原话）。
	g.acceptPerCode.Store(map[string]string{
		"chat_5":        `{"task_code":"chat_5","status":"error","message":"prerequisite not met: first_buddy"}`,
		"create_canvas": `{"task_code":"create_canvas","status":"error","message":"prerequisite not met: first_buddy"}`,
		"first_buddy":   `{"task_code":"first_buddy","status":"error","message":"task does not require acceptance"}`,
	})

	s := newGrowthHarnessPerCode(t, g)
	// 默认开关里 accept 是开的（见 growthWatchState 的注释），直接跑一轮。
	s.RefreshGrowth(true, true)

	// ① 必须真的去补了前置 —— 这是改造前**完全没有**的一步。
	if n := g.buddyFirstCalls.Load(); n == 0 {
		t.Error("被 first_buddy 挡住时守卫轮没有去补前置（buddy/first 一次都没打）——\n" +
			"这就是用户那个账号卡死的原因：「手工点一键完成能解开，循环里没人点」。\n" +
			"实测账号 17005247808：17 个任务永远接不上，界面看起来只是在等。")
	}
	if n := g.buddyAgreementCalls.Load(); n == 0 {
		t.Error("补前置必须先同意协议（buddy/agreement），实际一次都没打")
	}
	if n := g.reportCalls.Load(); n == 0 {
		t.Error("补前置必须先上报活跃（前置解锁），实际一次都没打")
	}

	// ② 补完必须**重试接单**：只补不重试的话，那一轮白跑，
	//    要再等一个完整间隔（默认 10 分钟）才可能接上 —— 而"重试"是这个
	//    自愈唯一的收益，少了它就退化成"每轮多打三次上游请求、什么都不解决"。
	if n := g.acceptCalls.Load(); n < 2 {
		t.Errorf("补完前置后没有重试接单（accept 只被调用 %d 次，期望 ≥2）——\n"+
			"只补不重试等于每轮白打三次上游请求，任务仍要等下一轮才可能接上", n)
	}
}

// 不可自动化的前置**不该**假装能补（否则每轮白打上游、还掩盖真实原因）。
//
// 判据用 upstream 的两个登记项之一：它们只能由用户去客户端做。
func TestGrowthWatchDoesNotFakeUnhealablePrerequisite(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
	  {"task_code":"chat_5","accept_status":"not_accepted","reward_credit":100}
	]}`
	// 被一个**不可自动化**的任务挡住。
	g.acceptPerCode.Store(map[string]string{
		"chat_5": `{"task_code":"chat_5","status":"error","message":"prerequisite not met: Expert_Philanthropy"}`,
	})

	s := newGrowthHarnessPerCode(t, g)
	s.RefreshGrowth(true, true)

	if n := g.buddyFirstCalls.Load(); n != 0 {
		t.Errorf("被不可自动化的前置挡住时不该去打 buddy/first（实际 %d 次）——\n"+
			"那等于假装能补，白耗上游请求，还会把真实原因（只能去客户端）掩盖掉", n)
	}
}

// BlockedBy 必须是**结构化字段**，不能靠从 Detail 里抠字符串。
//
// # 为什么单独立一条
//
// 守卫轮的自愈判据就是 `res.BlockedBy`。若有人图省事改成
// `strings.Contains(res.Detail, "first_buddy")`，那自愈就变成了
// **用文案当协议**：Detail 是给人看的中文，改一个标点/换一个译名就静默失效，
// 而失效的表现与"没实现自愈"完全一样（账号继续卡死、没有任何报错）。
func TestGrowthAcceptReportsBlockedByStructurally(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
	  {"task_code":"chat_5","accept_status":"not_accepted","reward_credit":100}
	]}`
	g.acceptPerCode.Store(map[string]string{
		"chat_5": `{"task_code":"chat_5","status":"error","message":"prerequisite not met: first_buddy"}`,
	})

	s := newGrowthHarnessPerCode(t, g)
	res := s.GrowthAcceptFor("u1", "", triggerManual)

	if res.BlockedBy != "first_buddy" {
		t.Errorf("被前置挡住时 BlockedBy 应为 first_buddy，实际 %q\n"+
			"（自愈判据就是这个字段；它空着的话守卫轮不会去补前置，"+
			"账号会永远卡住而界面看不出问题）", res.BlockedBy)
	}
	// 顺带钉住 Detail 仍然是给人看的中文（两者分工不同，都要在）。
	if !strings.Contains(res.Detail, "领取一只 Buddy") {
		t.Errorf("Detail 仍应带中文指引，实际 %q", res.Detail)
	}
	if res.Status != "skip" {
		t.Errorf("被前置挡住是**跳过**不是故障，实际 status=%q", res.Status)
	}
}

// 前置**已做完**时不该再打一遍（自愈只治"确实缺"的情况）。
//
// 判据：快照里 first_buddy 是 claimed 且接单没有被前置拒绝 → 不碰 buddy/first。
func TestGrowthWatchSkipsHealWhenNotBlocked(t *testing.T) {
	g := defaultGrowthStub()
	g.tasksJSON = `{"tasks":[
	  {"task_code":"first_buddy","accept_status":"claimed"},
	  {"task_code":"chat_5","accept_status":"not_accepted","reward_credit":100}
	]}`
	// 这次接单**正常成功**（前置已 claimed）。
	g.acceptPerCode.Store(map[string]string{
		"chat_5": `{"task_code":"chat_5","status":"accepted"}`,
	})

	s := newGrowthHarnessPerCode(t, g)
	s.RefreshGrowth(true, true)

	if n := g.buddyFirstCalls.Load(); n != 0 {
		t.Errorf("接单没被前置挡住，却仍然打了 buddy/first（%d 次）——\n"+
			"每轮重复领取是白耗上游请求，也是风控面", n)
	}
	if n := g.acceptCalls.Load(); n == 0 {
		t.Error("前置已满足时应正常接单")
	}
}

// upstream 的期望值来自这里，避免上面几条用例里的字面量与常量漂移。
var _ = upstream.GrowthStatusNotAccepted
