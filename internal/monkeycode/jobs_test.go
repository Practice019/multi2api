package monkeycode

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/scheduler"
)

// TestJobsDiscoverable 自动签到任务必须能被调度器发现。
//
// # 为什么单独钉这一条（这是真实发生过的故障）
//
// 签到驱动 internal/dailycheckin 早就实现好了：按钮能点、历史会写、手动签到成功。
// 但调度器发现任务的方式是构造期遍历注册表、对每个 Provider 做
// gateway.ExtOf[gateway.JobExt] 类型断言，**没实现就静默跳过**
// （scheduler/job.go 里是 continue，不是 error）。
//
// 于是出现过这样的形态：编译通过、按钮可用、手动能签，
// 但永远不会自动跑。
//
// 本用例走**调度器真实路径**（scheduler.NewJobs + Discover），
// 而不是只看 p.Jobs() —— 后者证明不了核心收得到。
func TestJobsDiscoverable(t *testing.T) {
	p := New(Config{CheckinInterval: 30 * time.Minute})

	// 编译期事实：接口真的挂上了（缺它时 ExtOf 返回 false）。
	if _, ok := gateway.ExtOf[gateway.JobExt](p); !ok {
		t.Fatal("ExtOf[JobExt] 没发现 *Provider —— 签到永远不会自动跑（只能手点）")
	}

	// 端到端：走调度器的发现逻辑，确认任务真被收进任务表。
	reg := gateway.NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("注册上游失败: %v", err)
	}
	jobs := scheduler.NewJobs()
	if n := jobs.Discover(reg); n != 1 {
		t.Fatalf("调度器应发现 1 个任务（签到），实得 %d —— 这就是「不自动签到」的形态", n)
	}
	names := jobs.Names()
	if len(names) != 1 || names[0] != checkinDescriptor.ID {
		t.Errorf("任务名 = %v，应为 [%s]", names, checkinDescriptor.ID)
	}
}

// TestJobsIntervalMatchesConfig 任务的间隔取自配置。
func TestJobsIntervalMatchesConfig(t *testing.T) {
	p := New(Config{CheckinInterval: 45 * time.Minute})
	jobs := p.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("应有 1 个任务，实得 %d", len(jobs))
	}
	if jobs[0].Interval != 45*time.Minute {
		t.Errorf("Interval = %v，应为配置的 45m", jobs[0].Interval)
	}
}

// TestJobsEmptyWhenAutoDisabled 间隔 <=0 时不注册任务。
//
// 返回 nil 是**合法**形态：只保留手动按钮，不自动跑。
//
// ⚠ 为什么这条重要：若判据写成「p.checkin != nil 就注册」，
// 配成 0 时会得到一个 Interval<=0 的 Job —— 调度器按「没到期」处理，
// 看起来正常但永远不跑，比不注册更难查。
func TestJobsEmptyWhenAutoDisabled(t *testing.T) {
	for _, iv := range []time.Duration{0, -1} {
		p := New(Config{CheckinInterval: iv})
		if p.checkin == nil {
			t.Fatalf("interval=%v：驱动本身仍应存在（按钮还要能用）", iv)
		}
		if jobs := p.Jobs(); len(jobs) != 0 {
			t.Errorf("interval=%v：不该注册任务，实得 %d 个", iv, len(jobs))
		}
	}
}

// TestJobsRunActuallyDrivesCheckin Run 必须真的驱动签到，不能是空实现。
//
// # 这条防什么（变异验证逼出来的）
//
// 最初我只写了「空实例上 Run 返回 nil」——那个断言对**空实现**同样成立
// （`func(ctx) error { return nil }` 也返回 nil），所以它证明不了任何事。
// 实测把 Run 换成空实现，那版测试照样绿。
//
// 现在的判据：注入一个账号枚举器，让驱动**真的有账号要签**。
// 真驱动会走 CheckinOne → 写签到历史；空实现一条历史都不会写。
// 用历史条数区分，这是可观测的行为差异。
func TestJobsRunActuallyDrivesCheckin(t *testing.T) {
	dir := t.TempDir()
	log := checkinlog.New(filepath.Join(dir, "checkin.json"), 30)
	p := New(Config{CheckinInterval: time.Hour, CheckinLog: log})

	// 注入一个不存在的账号：驱动会尝试签它并失败。
	// 失败也要写历史（StatusFail）—— 这正是「驱动真的跑了」的证据。
	p.SetUIDEnumerator(func() []string { return []string{"uid-probe"} })
	p.mu.Lock()
	p.creds["uid-probe"] = &Auth{Session: "fake-session-for-test"}
	p.mu.Unlock()

	jobs := p.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("应有 1 个任务，实得 %d", len(jobs))
	}
	if jobs[0].Run == nil {
		t.Fatal("Run 为 nil —— 任务会注册但每次执行都空转")
	}

	// 真打网络会慢/不稳定，这里只看「有没有产生历史记录」这个可观测差异。
	// 驱动会先调 Status（网络失败）→ 记 StatusFail。
	_ = jobs[0].Run(context.Background())

	if n := len(log.PageAll()); n == 0 {
		t.Error("跑完一条历史都没有 —— Run 很可能没接到驱动上（空实现也能过这个断言，" +
			"但接上了就一定会有记录）")
	}
}
