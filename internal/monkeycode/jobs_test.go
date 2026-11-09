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
	// ★ T-457：判据从「总数 == 1」改成「**按名字找到签到那条**」。
	// 改造前总数恰好是 1（当时 Jobs() 只有签到），T-456 加了 profile 巡检 Job
	// ⇒ 总数恒为 2 ⇒ 旧判据失配（不是功能坏了）。
	// ⚠ **不要**改成"总数 == 2"—— 那是把间接判据从 1 换成 2，下次再加 Job 又会红。
	if n := jobs.Discover(reg); n < 1 {
		t.Fatalf("调度器一个任务都没发现（实得 %d）—— 这就是「不自动签到」的形态", n)
	}
	names := jobs.Names()
	found := false
	for _, nm := range names {
		if nm == checkinDescriptor.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("任务名 = %v，其中没有签到任务 %s —— 核心收不到它，就永远不会自动跑",
			names, checkinDescriptor.ID)
	}
}

// TestJobsIntervalMatchesConfig 任务的间隔取自配置。
func TestJobsIntervalMatchesConfig(t *testing.T) {
	p := New(Config{CheckinInterval: 45 * time.Minute})
	// ★ T-457：**按名字找签到那条**（不要按下标、也不要数总数 —— 见上一条的注释）。
	var got time.Duration
	var ok bool
	for _, j := range p.Jobs() {
		if j.Name == checkinDescriptor.ID {
			got, ok = j.Interval, true
		}
	}
	if !ok {
		t.Fatalf("Jobs() 里没有签到任务 %s", checkinDescriptor.ID)
	}
	if got != 45*time.Minute {
		t.Errorf("Interval = %v，应为配置的 45m", got)
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
		// ★ T-457：断言的是「**签到** Job 不注册」，而不是「Jobs() 为空」。
		// 理由：自动签到关掉 ≠ 所有 Job 都不该跑 —— profile 巡检（清临时目录）
		// 与签到**无关**，签到关了它照跑 ✓ 旧断言把两件不相干的事绑在了一起。
		js := p.Jobs()
		for _, j := range js {
			if j.Name == checkinDescriptor.ID {
				t.Errorf("interval=%v：签到 Job 不该注册（Job 名 %q）—— "+
					"Interval<=0 的 Job 会被调度器按「没到期」处理，永远不跑，比不注册更难查",
					iv, j.Name)
			}
		}
		// 反向：巡检 Job 必须**仍在**（否则将来有人把 Jobs() 整体清空也能过这条）。
		sweep := false
		for _, j := range js {
			if j.Name == "monkeycode-profile-sweep" {
				sweep = true
			}
		}
		if !sweep {
			t.Errorf("interval=%v：profile 巡检 Job 也不见了 —— 它与签到无关，"+
				"不该因为签关键能被关掉（系统临时目录会继续堆 profile）", iv)
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

	// ★ T-457：**按名字找签到那条**（原来用 jobs[0]，在 Jobs() 有两条后就会拿错）。
	var checkin *gateway.Job
	for i := range p.Jobs() {
		if p.Jobs()[i].Name == checkinDescriptor.ID {
			checkin = &p.Jobs()[i]
		}
	}
	if checkin == nil {
		t.Fatalf("Jobs() 里没有签到任务 %s", checkinDescriptor.ID)
	}
	if checkin.Run == nil {
		t.Fatal("Run 为 nil —— 任务会注册但每次执行都空转")
	}

	// 真打网络会慢/不稳定，这里只看「有没有产生历史记录」这个可观测差异。
	// 驱动会先调 Status（网络失败）→ 记 StatusFail。
	_ = checkin.Run(context.Background())

	if n := len(log.PageAll()); n == 0 {
		t.Error("跑完一条历史都没有 —— Run 很可能没接到驱动上（空实现也能过这个断言，" +
			"但接上了就一定会有记录）")
	}
}

// TestJobsAlwaysIncludesProfileSweep 巡检 Job **必须存在**（T-457 新增，防复发）。
//
// # 为什么需要这条（T-446/T-456 的教训，代价 12 GB 磁盘）
//
// profile 巡检 Job 在 T-446 加进来时，代码里有 `if root := p.profileRoot(); root != ""`
// 那道门 —— 而旧的 profileRoot() 在全空配置下返回 "" ⇒ **Job 静默不注册** ⇒
// 巡检从未运行 ⇒ 用户在 D:\temp 攒了 24 个 ×525MB 的 Edge profile（≈12 GB，1 小时吃掉 8 GB）。
//
// ⚠ 这个缺陷的特殊之处：**代码在、测试绿、部署成功**三样都成立，只有"实际没跑" ✗
// ⇒ 所以必须有一条测试钉住"它一定在"，否则将来谁再加回一个类似的守卫就又会静默失效。
//
// ⚠ 判据是"按名字找到"，不是"总数 >= 2" —— 见 TestJobsDiscoverable 的注释。
func TestJobsAlwaysIncludesProfileSweep(t *testing.T) {
	// 故意用**最空**的配置：这正是当年触发静默失效的那种输入。
	p := New(Config{AuthDir: t.TempDir()})
	found := false
	for _, j := range p.Jobs() {
		if j.Name == "monkeycode-profile-sweep" {
			found = true
			if j.Run == nil {
				t.Error("巡检 Job 的 Run 为 nil —— 注册了也白注册")
			}
			if j.Interval <= 0 {
				t.Errorf("巡检 Job 的 Interval = %v（应 > 0）—— "+
					"⚠ 而且它**不能写 Due**：scheduler 的语义是「有 Due 就完全交给 Due，"+
					"Interval 被忽略」（T-434 的教训）", j.Interval)
			}
		}
	}
	if !found {
		t.Fatal("Jobs() 里没有 profile 巡检 Job —— 它静默失效过一次（见上），" +
			"这次不许再漏（否则系统临时目录会继续堆 525MB 的 profile）")
	}
}
