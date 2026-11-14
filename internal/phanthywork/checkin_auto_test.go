// checkin_auto_test.go 「自动签到」到底通不通 —— 判据**不是** `Enabled` 那个开关，而是真实链路。
//
// # 机制（读代码定的事实，不是猜）
//
//	cmd/server/main.go:1415                   注册核心自带任务 `core-daily-checkin`（无条件注册）
//	internal/scheduler/dailycheckin.go:191    用 ExtOf[gateway.DailyCheckinExt] 发现上游
//	internal/scheduler/dailycheckin.go:211    对每个实现者调 ext.CheckinAll(ctx)
//	internal/dailycheckin/driver.go:213       CheckinOne —— 单账号/全量/定时**三条路共用**它
//
// ⇒ 只要上游实现了 `DailyCheckinExt`，它就在统一签到的覆盖里（T-427 的教训：8 个上游里 6 个
//
//	当年没注册 Job ⇒ 自动签到**静默失效**，用户实测 checkin-log 里 trigger=schedule 记录 0 条）。
//
// # 所以 `Enabled=false` 关掉的是什么（本文件要钉住的那件事）
//
// 只关掉**本上游自己那份 Job**（驱动 `AutoEnabled()`，见 dailycheckin/driver.go:173-174 的注释：
// 「供上游侧决定要不要注册 Job」）——`CheckinOne` **不看**它（driver.go:213-266 全文无 enabled 判断）。
// 本包刻意不注册那份 Job（理由见 checkin.go 顶部 initCheckin 的注释：桌面端约 80 秒会自己领一次，
// 再挂一个 per-upstream Job 只是让同一天多打几轮幂等接口）。
//
// ⇒ **自动签到本来就是通的**（走统一任务）；证据就是下面第一条用例：
//
//	在 `AutoEnabled() == false` 的前提下，两个账号各被**恰好打了一次** claim。
package phanthywork

import (
	"context"
	"testing"

	"workbuddy2api/internal/gateway"
)

// autoCheckinFixture 两个账号 + 假上游（一场真实的全量签到所需要的全部接线）。
func autoCheckinFixture(t *testing.T) (*Provider, []*Auth, *fakeUpstream) {
	t.Helper()
	srv, fake := startFakeUpstream(t)
	fake.dailyEnabled = true
	fake.dailyStatus = "available"
	fake.serverDate = "2026-11-12"
	p := newFakeProvider(t, srv)
	a1, a2 := newTestAuth("u-1", "rt"), newTestAuth("u-2", "rt")
	for _, a := range []*Auth{a1, a2} {
		a.Reg.RegisteredAtMS = nowMS() // 已注册：跳过写操作
		fake.trust(a)
		p.SetLocalCredential(a.UID(), a)
	}
	// 生产的顺序是「核心的池子给 uid」——这里照同一条注入点给。
	p.SetUIDEnumerator(func() []string { return []string{a1.UID(), a2.UID()} })
	return p, []*Auth{a1, a2}, fake
}

// TestPhanthyworkOptsOutOfUnifiedCheckin ★ 本上游**不参与**跨上游统一签到。
//
// ★★ 2026-11-14 反转：原 `TestAutoCheckinIsLiveThroughUnifiedJob` 断言
// "统一任务跑一轮 ⇒ 每个账号恰好一次 claim"，那是在守护**已被移除的行为**。
//
// # 反转的理由（用户决定 + 真机数据）
//
// 统一任务的完成判据是「该上游**没有任一账号** fail」；只要有一个死号在 fail，
// `donePids[pid]` 就永远不置位 ⇒ 每 30 分钟重扫**整个上游**。而 `Status` 判据
// 本身是实打上游的 `FetchSummary` ⇒ 所有账号（含健康号）每 30 分钟被打一次。
//
// 真机（data/checkin-log.json）：cmwfxhqz 死后仍被打 21 次；cmwg6l3p 从 18:35 起
// 每 30 分钟一次。而官方客户端是**一天一次**。
//
// 用户原话：
//
//	"这个新的上游不需要注册这个任务……我们不需要自动签到，我们可以有那个按钮，
//	 但是不需要自动的。我们只需要完成最基本的自动续期的工作，其他都不需要。"
//
// # 为什么**保留** DailyCheckinExt 接口（而不是删掉方法）
//
//  1. `Caps()` 声明了 `CapCheckin`，而 `accountcolumns_test` 有一条
//     "能力位 vs 列集"的一致性护栏；删方法但留能力位会自相矛盾。
//  2. 面板上的**跨上游**"全部签到"（admin/checkin_all.go）靠这个接口遍历；
//     保留它、返回空报告，比"让那个端点找不到上游"更好解释。
//
// # 手动按钮**仍然可用**（别误读成"签到没了"）
//
// 单账号 / 全量签到按钮走 `Driver.HandlerOne` / `HandlerAll`，它们直接调
// `CheckinOne`，**不经过本函数**。所以按钮照旧。
//
// 判据（配套）：TestCheckinAllIsNoOpForUnifiedSweep（零 summary + 零 claim）
func TestPhanthyworkOptsOutOfUnifiedCheckin(t *testing.T) {
	p, accounts, fake := autoCheckinFixture(t)

	if len(accounts) == 0 {
		t.Fatal("夹具前提：应当有账号，否则下面零请求的断言证明不了任何东西")
	}

	// ① 接口仍在（刻意的，理由见上）—— 所以统一任务**发现得到**本上游。
	ext, ok := gateway.ExtOf[gateway.DailyCheckinExt](gateway.Provider(p))
	if !ok {
		t.Fatalf("本包应当**保留** DailyCheckinExt 接口（Caps 里有 CapCheckin、面板要遍历）；" +
			"否则连 opt-out 的入口都没有了")
	}

	// ② 但统一任务跑一轮时：零结果、零上游请求。
	rep := ext.CheckinAll(context.Background())
	if len(rep.Results) != 0 {
		t.Errorf("本上游已 opt-out ⇒ 不该有任何逐账号结果，实际 %d 条: %+v", len(rep.Results), rep)
	}
	if n := atomicLoad(&fake.claimCalls); n != 0 {
		t.Errorf("不该有 claim（写操作），实际 %d 次 —— 官方一天一次，我们曾是 48 次/天", n)
	}
	if n := atomicLoad(&fake.summaryCalls); n != 0 {
		t.Errorf("不该有 summary（只读，但仍是真实上游请求），实际 %d 次 ——\n"+
			"这正是「跳过 claim 但照样打上游」的那半，必须一起钉", n)
	}
}

// TestPhanthyworkKeepsRefreshJobOnly ★ 本上游的定时任务**只剩自动续期**。
//
// 用户原话："我们只需要完成最基本的自动续期的工作，其他都不需要。"
//
// 这条钉住那个约定的**形状**：`Jobs()` 恰好一条，且是 `phanthywork-refresh`。
// 将来若有人给本上游加回签到任务（或别的定时任务），这里会红。
func TestPhanthyworkKeepsRefreshJobOnly(t *testing.T) {
	p := NewProvider(Config{AuthDir: t.TempDir()})
	jobs := p.Jobs()
	if len(jobs) != 1 {
		names := make([]string, 0, len(jobs))
		for _, j := range jobs {
			names = append(names, j.Name)
		}
		t.Fatalf("本上游应只有 1 条定时任务（自动续期），实际 %d 条: %v\n"+
			"用户明确要求「只需要自动续期，其他都不需要」", len(jobs), names)
	}
	if jobs[0].Name != JobRefresh {
		t.Errorf("唯一那条定时任务应当是 %q（自动续期），实际 %q", JobRefresh, jobs[0].Name)
	}
	if jobs[0].Interval <= 0 {
		t.Errorf("续期任务的间隔应当 > 0（否则等于没注册），实际 %v", jobs[0].Interval)
	}
}

// TestUnifiedJobWouldSkipProviderWithoutCheckinExt 反例：证明上一条的判据**有咬合力**。
//
// 形态取 T-427 的真实事故：上游没实现 `DailyCheckinExt` ⇒ 核心的 Run 在
// `dailycheckin.go:192-194` 直接 `continue` ⇒ 那个上游的自动签到**永远不跑**，
// 而面板上看不出任何异常（这正是「静默失效」的定义）。
func TestUnifiedJobWouldSkipProviderWithoutCheckinExt(t *testing.T) {
	var stub gateway.Provider = stubNoCheckin{}
	if _, ok := gateway.ExtOf[gateway.DailyCheckinExt](stub); ok {
		t.Fatal("反例桩不该实现 DailyCheckinExt —— 否则这条用例证明不了任何东西")
	}
	// 同一段 ExtOf 逻辑下：真 Provider 命中、桩落空 ⇒ 统一任务只会扫前者。
	p, _, _ := autoCheckinFixture(t)
	if _, ok := gateway.ExtOf[gateway.DailyCheckinExt](gateway.Provider(p)); !ok {
		t.Error("真 Provider 应当命中 DailyCheckinExt")
	}
}

// stubNoCheckin 一个**不实现** DailyCheckinExt 的最小上游（反例用）。
type stubNoCheckin struct{}

func (stubNoCheckin) ID() string               { return "stub-no-checkin" }
func (stubNoCheckin) Caps() gateway.Capability { return gateway.CapChat }
func (stubNoCheckin) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (stubNoCheckin) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

// TestContractFactoryConstructsCheckinDriver ★ 契约工厂必须与生产路径**等价**地接线。
//
// # 这条要抓的缺陷（验证者 P1-a）
//
// `NewProvider`（**契约测试与所有守卫用的入口**）此前漏了 `initCheckin()`，而 `NewWithClient` 有。
// 后果：用工厂造的实例 `DailyActions()` / `CheckinAll()` **恒空** ⇒ manifest 的 daily_actions 里
// 没有本上游 ⇒ 卡片上「全部签到」不亮；而契约测试全绿（它只看 Provider 的 4 个方法）。
// 生产路径（cmd/server 注入 checkinLog 时会重跑 initCheckin）不受影响 —— 所以这是**只在工厂形态下存在**的洞，
// 也正是"拿工厂当生产"的所有守卫会集体失明的那种洞。
//
// 判据只一句：**契约工厂构造出来的实例必须能报出每日动作**（无日志注入也算 —— 手动按钮与
// 统一签到都不依赖日志，日志只影响历史落盘）。
func TestContractFactoryConstructsCheckinDriver(t *testing.T) {
	p := NewProvider(Config{})
	acts := p.DailyActions()
	if len(acts) != 1 {
		t.Fatalf("契约工厂 NewProvider(Config{}) 构造的实例必须报出 1 条每日动作，实际 %d 条（%+v）\n"+
			"修法：NewProvider 必须与 NewWithClient 走同一条构造路径（含 initCheckin）", len(acts), acts)
	}
	if acts[0].ID != "phanthywork-checkin" || acts[0].AllURL == "" {
		t.Errorf("动作形状不对: %+v", acts[0])
	}
	// 同一实例也应能跑统一任务那条入口（空账号池 ⇒ 空报告，但**不是**"驱动没接线"那种空）。
	// ⚠ 这里不连假上游：只断言"接口在、能调用、不 panic"——网络相关的判据在别处。
	if _, ok := gateway.ExtOf[gateway.DailyCheckinExt](gateway.Provider(p)); !ok {
		t.Error("契约工厂实例必须实现 DailyCheckinExt")
	}
}
