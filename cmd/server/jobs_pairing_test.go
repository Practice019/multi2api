// jobs_pairing_test.go 跨上游不变量：**能续期 ⇒ 必须有后台续期任务**。
//
// # 这条守卫是三次同类报障的产物
//
// 顺序回顾（每一次都是用户先发现）：
//
//	第一次  cline    「Token 已过期，怎么不会自动刷新」
//	第二次  raccoon  「这个 token 续期逻辑有 bug」（已过期 532 分钟而
//	                  refresh_token 还剩 29.5 天）
//	第三次  minimax  「MiniMax 的账号没有自动续期」（本轮）
//
// 三次的形态**逐字相同**：上游实现了 `CredentialRefresher`（方法签名对）、
// 也实现了 `RefreshSkewExt`（知道"多早该刷"），但**没有 Jobs()** ——
// 于是核心的两条续期触发路径（出站请求 / 后台任务）它一条都不占，
// 只有"有人拿它的号发对话请求"时才会顺手续期。没人用时静静过期。
//
// # 为什么已有守卫没拦住第三次
//
// `refresh_pairing_test.go` 守的是另一个方向：
//
//	RefreshSkew 回正数 ⇒ 必须实现 CredentialRefresher     （修"能刷却不认"）
//
// 而 minimax **两个都有** —— 它缺的是第三件东西（后台任务）。
// 那条守卫对它**天然为真**，所以永远不会红。
//
// 也就是说：上一轮修完 minimax 的 refresher 之后，我以为同类问题已经防住了，
// 实际只防住了**一半**。这条补上另一半。
//
// # 判据为什么是「实现 JobExt」而不是「Jobs() 非空」
//
// 我第一版判的是 `Jobs()` 非空，结果在这个测试的夹具上误报：
// `allUpstreams` 用 `minimax.New(minimax.Config{})` 构造，**authDir 为空**，
// 而生产用的那套 "authDir 为空 ⇒ 不起任务（没有账号要刷）" 的判据于是成立 ——
// 夹具与生产**形态不同**。
//
// 改用"是否实现 JobExt"：它是**编译期事实**，与配置无关，因此夹具与生产一致。
// 而"Jobs() 到底返不返非空"由下一条针对**生产形态**的用例单独守。
package main

import (
	"context"
	"fmt"
	"testing"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/minimax"
)

func TestRefresherImpliesBackgroundJob(t *testing.T) {
	checked := 0
	for _, u := range allUpstreams(t) {
		if !hasRefresher(u.p) {
			continue // 不能续期的上游（如 zcode JWT）—— 不适用
		}
		checked++

		if err := checkRefresherHasJobs(u.label, u.p); err != nil {
			t.Error(err)
		}
	}
	if checked == 0 {
		t.Error("一条都没检查到 —— 所有上游都没实现 CredentialRefresher 时，本守卫形同虚设")
	}
}

// checkRefresherHasJobs 判据本体（抽出来是为了能被**证伪**）。
//
// # ⚠ 为什么必须抽出来（这是变异验证逼出来的）
//
// 我第一版把断言直接写在测试函数里。变异验证时我把条件改成
// `false && !hasJobs`（等于关掉断言），结果**测试仍然绿** ——
// 看起来像"守卫没咬合力"。
//
// 实际原因是：断言体只在 `hasJobs == false` 时执行，而**现在没有任何
// 上游违反它**（修完之后全都有 JobExt 了）。没有反例 → 关掉条件也没有差异
// → 变异等价。这是"守卫保护的是一个已修好的 bug"必然遇到的形态。
//
// 修法不是放宽断言，而是**让它可被证伪**：把判据抽成这个纯函数，
// 配一条用**人造违规 Provider**（见下面的 stubNoJobs）调用它的用例。
// 那条用例才是"这条守卫到底有没有咬合力"的证明。
func checkRefresherHasJobs(label string, pv gateway.Provider) error {
	if !hasRefresher(pv) {
		return nil // 不能续期的不适用
	}
	if _, hasJobs := gateway.ExtOf[gateway.JobExt](pv); !hasJobs {
		return fmt.Errorf("%s: 实现了 CredentialRefresher（能续期），却**没有**实现 JobExt。\n"+
			"\n"+
			"核心的续期只有两条触发路径：\n"+
			"  ① 出站请求时 handler 判「该刷了」才刷\n"+
			"  ② 后台定时任务（上游通过 JobExt 自报的 Jobs()）\n"+
			"\n"+
			"缺 ② 意味着：**没人拿它的号发请求时，它的 token 会静静过期**，\n"+
			"而 refresh_token 明明还能用。界面上表现为「Token」列一直显示已过期。\n"+
			"这条链断掉时**没有任何报错** —— 所以只能靠这条守卫发现。\n"+
			"\n"+
			"（cline / raccoon / minimax 都栽在这里，三次都是用户先发现的。）\n"+
			"\n"+
			"修法：照 internal/raccoon/jobs.go 补一份 jobs.go ——\n"+
			"  注册一个 Interval>0 的任务，Run 里复用 RefreshSkew 的同一判据，\n"+
			"  并且**必须刷池里持有的那个 *Auth 对象**（不是磁盘副本）。", label)
	}
	return nil
}

// stubNoJobs 一个人造违规上游：能续期、但**没有**后台任务。
//
// 这正是 cline / raccoon / minimax 三次出事时的形态。它的唯一用途是
// 证明 checkRefresherHasJobs 真的会红 —— 没有它，那条守卫在"所有上游
// 都修好了"之后就无法被证伪，读代码的人也不知道它到底还能不能抓。
type stubNoJobs struct{}

func (stubNoJobs) ID() string               { return "stub-no-jobs" }
func (stubNoJobs) Caps() gateway.Capability { return 0 }
func (stubNoJobs) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (stubNoJobs) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

// RefreshCredential 让它"能续期"（有 CredentialRefresher），
// 但**故意不实现 JobExt** —— 这就是要抓的形态。
func (stubNoJobs) RefreshCredential(gateway.Credential) error { return nil }

// ⚠ 这条是"守卫有咬合力"的**证明**，不是重复。
//
// 它拿人造违规上游调用判据，必须**报错**。如果哪天有人把判据改回去
// （例如误删了 hasJobs 判断），这条会红 —— 而上面那条遍历真上游的
// 用例**不会**（因为真上游都合规了）。
func TestCheckRefresherHasJobsCatchesViolator(t *testing.T) {
	if err := checkRefresherHasJobs("人造违规上游", stubNoJobs{}); err == nil {
		t.Error("对一个人造违规上游（能续期、无 JobExt），判据**必须**报错 ——\n" +
			"它没报错说明这条守卫在真上游都合规之后已经失效，" +
			"下次有新上游漏掉 Jobs() 时不会有人发现。")
	}
}

// 反向：合规的上游不该被误报（判据不能恒报错）。
func TestCheckRefresherHasJobsAcceptsCompliant(t *testing.T) {
	for _, u := range allUpstreams(t) {
		if !hasRefresher(u.p) {
			continue
		}
		if err := checkRefresherHasJobs(u.label, u.p); err != nil {
			t.Errorf("合规上游被误报：%v", err)
		}
	}
}

// ⚠ 生产形态下 `Jobs()` 必须真的返回任务（而不是被注册条件悄悄挡掉）。
//
// 这一条是上一条的补充，缺了它会出现一种很隐蔽的假绿：
// 上游**实现了** JobExt（编译期断言过、上一条通过），但 `Jobs()` 里
// 有一个恒假的注册条件（例如"authDir 非空"而生产从不设它）——
// 于是它在生产上**一个任务都注册不上**，而所有测试都是绿的。
//
// 所以这里用**生产同款配置**（authDir 非空）构造，断言任务真的在。
func TestMinimaxJobsRegisteredInProductionShape(t *testing.T) {
	p := minimax.New(minimax.Config{AuthDir: t.TempDir()})

	ext, ok := gateway.ExtOf[gateway.JobExt](p)
	if !ok {
		t.Fatal("minimax 未实现 JobExt —— 生产形态下它拿不到后台续期")
	}
	jobs := ext.Jobs()
	if len(jobs) == 0 {
		t.Fatal("生产形态（authDir 非空）下 minimax 应当注册后台续期任务，实际一个都没有 ——\n" +
			"这正是本次报障的形态：能续期、有 skew，但没人用它的号时 token 静静过期")
	}

	// 任务形状：有名字、间隔为正、有 Run。
	var found bool
	for _, j := range jobs {
		if j.Name != minimax.JobRefresh {
			continue
		}
		found = true
		if j.Interval <= 0 {
			t.Errorf("%s 的 Interval = %v，必须为正", j.Name, j.Interval)
		}
		if j.Run == nil {
			t.Errorf("%s 的 Run 为 nil —— 任务会被注册但点了什么也不做", j.Name)
		}
	}
	if !found {
		t.Errorf("没有名为 %q 的后台续期任务；实际任务: %v", minimax.JobRefresh, jobNames(jobs))
	}
}

// 反过来：**没有凭证目录时不该起任务**（没有账号要刷，起任务只是空转）。
//
// 守卫两个方向都要有 —— 只守"必须有"会让人把判据写成恒真，
// 那样"未配置的上游也在空转打上游"就没人发现了。
func TestMinimaxJobsEmptyWithoutAuthDir(t *testing.T) {
	p := minimax.New(minimax.Config{}) // 无 authDir
	ext, ok := gateway.ExtOf[gateway.JobExt](p)
	if !ok {
		t.Fatal("未实现 JobExt")
	}
	if jobs := ext.Jobs(); len(jobs) != 0 {
		t.Errorf("没有凭证目录时不该注册后台任务（没有账号要刷），实际 %v", jobNames(jobs))
	}
}

func jobNames(jobs []gateway.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

// ⚠ 出过事的上游**必须**被这条守卫真的检查到（防它悄悄退化成 skip）。
//
// 与 refresh_pairing_test.go 里那条同型：抓不住目标的守卫比没有守卫更糟 ——
// 它给人"已经防住了"的错觉。（上一轮我就是这么以为的。）
func TestJobPairingCoversTheRepeatOffenders(t *testing.T) {
	// 三个栽过跟头的上游。它们的共同点：有 refresher，曾经没有 jobs。
	offenders := map[string]bool{
		"cline":   false,
		"raccoon": false,
		"minimax": false, // ← 本轮报障的那个
	}
	for _, u := range allUpstreams(t) {
		if _, interested := offenders[u.label]; !interested {
			continue
		}
		if hasRefresher(u.p) {
			offenders[u.label] = true
		}
	}
	for label, got := range offenders {
		if !got {
			t.Errorf("%s 没有被这条守卫检查到（它没实现 CredentialRefresher？）——\n"+
				"它正是栽过「有续期能力但不续期」的上游之一，"+
				"守卫必须对它真的做判断，而不是 skip。", label)
		}
	}
}
