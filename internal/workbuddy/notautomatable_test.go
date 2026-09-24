package workbuddy

import (
	"strings"
	"testing"
)

// TestNotAutomatableTasksAreSkippedWithReason 钉住"遇到就跳过"这个约定。
//
// # 守的是什么（用户明确要求）
//
// 用户实测：这几个任务挂在那里一直不动，希望「一键完成」遇到它们**直接跳过**，
// 因为上游的完成判据不在遥测里，造事件也不会点亮：
//
//	Expert_Philanthropy          真实捐款
//	wb_wechat_oa_subscribe_task  关注公众号满 24h
//
// # 为什么需要显式登记，而不是"不放进 autoActions 就行"
//
// 不在 autoActions 里本来就会被跳过，但那是**隐式**的 —— 代码里没有任何一处
// 说明"这几个是明确知道做不到才跳的"。维护者看到待办列表里挂着它们，
// 会以为漏实现而去造遥测事件，白耗上游配额并增加风控面。
//
// ⚠ 下面那条互斥断言抓过一次真实错误：起草清单时把
// skill_1 / Expert_lighthouse / black_cat 也列了进来，而它们**早已实现**
// （autoActions 里各有一条，注释写明"已验证点亮"）——
// 当时是读了文件头那段**过时注释**才误判的。互斥断言当场把它拦下。
func TestNotAutomatableTasksAreSkippedWithReason(t *testing.T) {
	want := []string{
		"Expert_Philanthropy",
		"wb_wechat_oa_subscribe_task",
	}
	for _, code := range want {
		reason, ok := notAutomatableReason(code)
		if !ok {
			t.Errorf("任务 %s 应登记在 notAutomatable 里（否则会被误当成漏实现）", code)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("任务 %s 的原因不能为空 —— 用户要靠它知道为什么跳过", code)
		}
		// 必须说明"做不到"的性质，而不是含糊一句"跳过"
		if !strings.Contains(reason, "无法") && !strings.Contains(reason, "尚未") &&
			!strings.Contains(reason, "非遥测") {
			t.Errorf("任务 %s 的原因应说明为何做不到，实际：%q", code, reason)
		}
	}

	// ★ 互斥：登记为"无法自动化"的任务**绝不能**同时出现在可执行表里。
	// 否则"跳过"会变成"去跑一遍"，与本意相反。
	//
	// 这条同时防住"读注释而非读代码"造成的误判 —— 注释会滞后，表不会。
	for _, act := range autoActions {
		if reason, known := notAutomatableReason(act.TaskCode); known {
			t.Errorf("任务 %s 既在 notAutomatable（%q）又在 autoActions 里 —— "+
				"两者互斥：登记为做不到的就不该有执行动作。\n"+
				"（若它其实已实现，应从 notAutomatable 里移除）", act.TaskCode, reason)
		}
	}
}

// TestNotAutomatableUnknownCodeIsNotSkipped 未登记的 code 不该被当成"无法自动化"。
//
// 这条防的是"把跳过判据写成 catch-all"：那样任何**真正可做但漏加进
// autoActions** 的任务都会被静默跳过，且原因显示成"已知无法自动化" ——
// 一个错误的原因比没有原因更难查。
func TestNotAutomatableUnknownCodeIsNotSkipped(t *testing.T) {
	if _, known := notAutomatableReason("chat_5"); known {
		t.Error("chat_5 是可自动化的任务，不该出现在 notAutomatable 里")
	}
	if _, known := notAutomatableReason("some_future_task_code"); known {
		t.Error("未登记的 code 不该被判定为『已知无法自动化』")
	}
}
