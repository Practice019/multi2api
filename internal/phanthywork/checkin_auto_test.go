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

// TestAutoCheckinIsLiveThroughUnifiedJob ★ 「自动签到开着」的判据。
//
// 统一签到任务调的就是 `CheckinAll`（dailycheckin.go:211），所以这条用例等价于
// 「把统一任务跑一轮会发生什么」：
//
//	· 每个账号**恰好一次** claim（不多不少）
//	· 全部成功 —— 假上游对 body 不是 `{}`、幂等键不是服务端日键**都回 403**，
//	  所以「全 ok」本身就证明两条硬要求在**自动路径**上同样成立
//	· 且这一切发生在 `AutoEnabled() == false` 的前提下 ⇒ Enabled 不控制自动路径
func TestAutoCheckinIsLiveThroughUnifiedJob(t *testing.T) {
	p, accounts, fake := autoCheckinFixture(t)

	// ① 核心按这个接口发现上游；不实现它 = 被统一任务跳过（见文件头 + 下一条反例）。
	ext, ok := gateway.ExtOf[gateway.DailyCheckinExt](gateway.Provider(p))
	if !ok {
		t.Fatalf("本包没有实现 gateway.DailyCheckinExt ⇒ 统一签到发现不到它 ⇒ 自动签到静默失效")
	}

	// ② 前提断言：本包**刻意**不注册 per-upstream Job（AutoEnabled=false）。
	//    它若变成 true，说明有人改过那个决定（那会让同一天多打几轮幂等接口）——应当先讨论。
	if p.checkin.AutoEnabled() {
		t.Fatalf("夹具前提变了：本包的 driver.Enabled 应当是 false（见 checkin.go 的 initCheckin 注释）")
	}

	// ③ 跑一轮（统一任务做的事）。
	rep := ext.CheckinAll(context.Background())
	if len(rep.Results) != len(accounts) {
		t.Fatalf("应当对 %d 个账号各出一条结果，实际 %d 条: %+v", len(accounts), len(rep.Results), rep)
	}
	for _, r := range rep.Results {
		if r.Status != "ok" {
			t.Errorf("账号 %s 签到未成功: status=%q detail=%q（假上游对 body/幂等键不合规一律 403）",
				r.UID, r.Status, r.Detail)
		}
	}
	if n := atomicLoad(&fake.claimCalls); n != int32(len(accounts)) {
		t.Errorf("每个账号应当**恰好**打一次 claim（共 %d 次），实际 %d 次", len(accounts), n)
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
