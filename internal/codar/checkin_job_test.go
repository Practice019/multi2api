package codar

import (
	"os"
	"strings"
	"testing"
)

// TestCodarJobsIncludesCheckin codar 的 Jobs() 里**不应再有**签到任务（T-429）。
//
// # 这条测试的历史（方向反过来了）
//
// T-424 时它是「**必须有** codar-daily-checkin」——
// 因为当时 codar 实现了 DailyCheckinExt 却没注册成 Job，自动签到静默失效
// （用户实测：两个号只能手动签，trigger=schedule 记录 0 条）。
//
// T-427 把 8 个上游的签到**统一**成一个跨上游的共享 Job
// （`internal/scheduler/dailycheckin.go`，任务名 `core-daily-checkin`）
// ⇒ codar 自己那个就**重复**了。
//
// # 为什么"重复"必须删，而不是"留着也安全"
//
// 两个 Job 都会调 `CheckinAll` ⇒ codar 每小时被签两次 ⇒
// 违反共享 Job 的「每上游每天 1 趟」闸门，而 codar 面板明确写着
// 「上游已有风控记录，请低频使用」—— 打坏了要人工恢复。
// ⇒ 本测试现在守的是**删除**（方向反转），理由写在这里以免下次有人"顺手加回来"。
func TestCodarJobsIncludesCheckin(t *testing.T) {
	p := NewWithConfig(Config{})
	names := []string{}
	for _, j := range p.Jobs() {
		names = append(names, j.Name)
		if j.Name == "codar-daily-checkin" {
			t.Errorf("Jobs() 里又出现了 codar-daily-checkin —— 共享签到 Job "+
				"（core-daily-checkin）已覆盖本上游 ⇒ 两个 Job 会让 codar 每小时被 "+
				"CheckinAll 两次，而上游有风控。实际任务 = %v", names)
		}
	}
	// 反面也要守：token-refresh 不能因为这次删除被顺手删掉
	found := false
	for _, n := range names {
		if n == "codar-token-refresh" {
			found = true
		}
	}
	if !found {
		t.Errorf("Jobs() 里没有 codar-token-refresh —— 本轮只该删签到任务，不该动令牌续期。实际 = %v", names)
	}
}

// TestCodarDoSignTakesTrigger doSign 的 trigger 必须由调用方传（T-429）。
//
// # 为什么这条留着（虽然自动签到已经搬去共享 Job 了）
//
// 改造前 `doSign` 里写死 `recordSign(res, "manual")`，而用户**正是靠 checkinlog 的
// trigger 字段**发现「自动签到没工作」（schedule 记录 0 条）。
// 即使自动签到搬走了，**手动**走的两条路（全部签到按钮 / 手动单签端点）
// 仍必须是 manual —— 否则历史里分不出"谁点的"。
//
// ⚠ 自动签到那一路现在由共享 Job 侧保证（它调各上游的 CheckinAll ⇒
//
//	记什么 trigger 由 CheckinAll 内部决定）；本条只钉 codar 自己这两处。
func TestCodarDoSignTakesTrigger(t *testing.T) {
	b, err := os.ReadFile("p1.go")
	if err != nil {
		t.Fatalf("读 p1.go 失败：%v", err)
	}
	src := string(b)
	// 参数化必须还在：CheckinAll（全部签到按钮）传 manual
	if !strings.Contains(src, `p.doSign(ctx, a, "manual")`) {
		t.Error("CheckinAll 没有把 manual 传给 doSign —— 手动「全部签到」会被记成别的来源")
	}
	// 而且 doSign 内部不许再把 trigger 写死（那会绕过参数化）
	if strings.Contains(src, `recordSign(res, "manual")`) {
		t.Error("doSign 内部又把 trigger 写死成 manual 了 —— " +
			"参数化会被绕过，自动签到一旦回来就分不出来源（T-424 修的就是这个）")
	}
	// 三个 T-424 的死代码函数必须已删（共享 Job 的内存闸门取代了它们）
	for _, dead := range []string{"checkedInToday", "hasUnsignedToday", "runCheckinScan"} {
		if strings.Contains(src, "func (p *Provider) "+dead+"(") {
			t.Errorf("codar 里还留着 %s —— 共享 Job 用内存闸门判幂等 ⇒ 它是死代码"+
				"（留着会让后来者以为 codar 自己还有一套签到逻辑）", dead)
		}
	}
}
