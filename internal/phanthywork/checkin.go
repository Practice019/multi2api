// checkin.go 每日签到 —— 接本仓的**共享驱动**（internal/dailycheckin）。
//
// # 为什么用共享驱动而不是自己抄一份
//
// 本仓已经为"签到接线漏三处"付过学费（lobsterai 的注释写了完整形态）：
//
//	① 账号行没有按钮（没实现 DailyActionExt）
//	② 没有自动签到（没实现 JobExt 的那条）
//	③ 签到不写 checkinlog ⇒ 界面「今日签到」列永远是 `—`
//
// 三处都**不报错**：编译通过、契约测试也查不到。所以正确做法是实现驱动要求的
// 三个方法（列账号 / 查状态 / 领取），其余（按钮、批量端点、写历史、状态归一化）
// 全由驱动提供 —— 判断依据只有一处，不可能抄漏。
//
// # ⚠ 本包**不 import checkinlog**
//
// checkinlog 在 arch_test 的 corePackages 里（上游不得依赖核心业务包）。
// 写历史这件事由共享驱动完成 —— 它 import checkinlog 是合法的（共享基础设施），
// 而本包只 import dailycheckin。这就是"用一个驱动而不是各写一遍"的架构收益。
package phanthywork

import (
	"context"
	"errors"
	"log"
	"strings"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// 编译期断言：驱动要求的接口 + 两个 gateway 侧的自报面。
var (
	_ dailycheckin.Upstream   = (*checkinUpstream)(nil)
	_ gateway.DailyActionExt  = (*Provider)(nil)
	_ gateway.DailyCheckinExt = (*Provider)(nil)
)

// checkinDescriptor 本上游签到动作的展示与路由（驱动要求的事实）。
//
// ID 约定 `<provider>-checkin`（跨上游唯一：前端按它写 data-act）。
var checkinDescriptor = dailycheckin.Descriptor{
	ID:     "phanthywork-checkin",
	Label:  "签到",
	Title:  "领取当天的签到积分（300 分/天，连续上限 5 天；已领过会提示今日已签到）",
	OneURL: "/admin/phanthywork/checkin",
	AllURL: "/admin/phanthywork/checkin/all",
}

// initCheckin 构造并缓存共享驱动（构造器里调用；幂等）。
//
// # ⚠ `Enabled=false` / `Interval=0` 关掉的**不是自动签到本身**
//
// 这两项只关掉**本上游自己那份 Job** —— 驱动里 `Enabled` 的唯一用途就是 `AutoEnabled()`
// （internal/dailycheckin/driver.go:173-174 的注释写着"供上游侧决定要不要注册 Job"），
// 而 `CheckinOne` **从不看它**（driver.go:213-266 全文无 enabled 判断）。
//
// 真正的自动签到走的是**核心自带的统一任务**：
//
//	cmd/server/main.go:1415             注册 core-daily-checkin（无条件注册）
//	scheduler/dailycheckin.go:191/211   用 ExtOf[DailyCheckinExt] 发现上游 → 调 CheckinAll
//	→ 本包 CheckinAll（下方）→ 每账号一次 CheckinOne（单账号/全量/定时三条路同一个实现）
//
// ⇒ 只要实现了 `DailyCheckinExt`，自动签到就是**通的**（T-427 的教训：没实现 = 静默失效）。
//
//	⚠ 别把这句读成"关着"：`checkin_auto_test.go` 的 TestAutoCheckinIsLiveThroughUnifiedJob
//	就是在 `AutoEnabled()==false` 的前提下断言两个账号各被**恰好打一次** claim。
//
// # 那为什么不注册自己那份 Job
//
// 本上游的桌面端启动后约 80 秒**会自己领**一次（报告 §4.7 实测）⇒ 再挂一个 per-upstream
// Job 只会让同一天多打几轮幂等接口（`daily:<installationId>:<server_date>` 幂等，多打无害但无益）。
// 手动按钮 + 分组「全部签到」+ 统一任务已完整覆盖诉求。
func (p *Provider) initCheckin() {
	if p == nil {
		return
	}
	p.checkin = dailycheckin.New(
		&checkinUpstream{p: p},
		checkinDescriptor,
		dailycheckin.Options{
			// Log 由装配层注入（cmd/server 有 checkinlog 的实例）——
			// nil 也能跑，只是不写历史（那会让「今日签到」列恒为空，见驱动注释）。
			Log:      p.checkinLog,
			Interval: 0,
			Enabled:  false,
		},
	)
}

// SetCheckinLog 装配签到历史日志（可选：不接线时"今日签到"列恒为空）。
//
// ⚠ 参数类型是 any 而不是 *checkinlog.Log —— 本包**不得** import checkinlog
// （它在 arch_test 的 corePackages 里）。装配层传进来的就是那个类型，
// 驱动会自己断言（它认识自己的依赖）。
func (p *Provider) SetCheckinLog(l *checkinlog.Log) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.checkinLog = l
	p.mu.Unlock()
	p.initCheckin() // 让新日志立刻生效（驱动在构造时持有它）
}

// SetDisabledChecker 装配"这个号是否已被禁用"的判据（可选）。
//
// 驱动会在发任何上游请求**之前**跳过一个已禁用的号（L1 梯级"别做这件事"：
// 它已经不可用了，打上游只会白喂风控）。本包自己拿不到池子，所以由装配层注入；
// 不接线时恒为 false（每个号都试一遍），行为仍然正确，只是多打几次请求。
func (p *Provider) SetDisabledChecker(fn func(uid string) bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.checkinDisabled = fn
	p.mu.Unlock()
}

// checkinUpstream 把 *Provider 适配成 dailycheckin.Upstream。
//
// 单独一个类型而不是给 *Provider 直接加这三个方法：驱动的 Account 是它自己的
// 形状，而 *Provider 上已经有/将会有名为 Status、Claim 的 gateway 侧调用面，
// 两套同名方法混在一个类型上会让类型断言意外命中。
type checkinUpstream struct{ p *Provider }

// Accounts 列本上游当前要处理的账号。
//
// 顺序与判据与后台续期一致：优先注入的 uid 枚举（生产：核心知道池里有哪些号），
// 拿不到才回落本实例的 creds（测试路径）。**反过来会让生产永远扫不到账号**。
func (u *checkinUpstream) Accounts(ctx context.Context) ([]dailycheckin.Account, error) {
	if u == nil || u.p == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := u.p
	p.mu.RLock()
	enum := p.uidEnumerator
	disabled := p.checkinDisabled
	local := make([]*Auth, 0, len(p.creds))
	for _, a := range p.creds {
		local = append(local, a)
	}
	p.mu.RUnlock()

	var accounts []*Auth
	if enum != nil {
		for _, uid := range enum() {
			if a := p.cred(uid); a != nil {
				accounts = append(accounts, a)
			}
		}
	}
	if len(accounts) == 0 {
		accounts = local
	}
	out := make([]dailycheckin.Account, 0, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		uid := a.UID()
		if uid == "" {
			continue
		}
		out = append(out, dailycheckin.Account{
			UID:      uid,
			Secret:   a,
			Disabled: disabled != nil && disabled(uid),
		})
	}
	return out, nil
}

// Status 查今天的签到状态（只读：一次 summary 调用）。
//
// 三个返回值的语义（驱动契约）：
//
//	signedIn=true  今天已领（驱动**不会**再调 Claim）
//	enabled=false  上游今天不开放（驱动跳过、**不记历史** —— 那是正常状态）
//	err != nil     查询失败（驱动记 fail 让用户看见原因）
//
// ⚠ 状态字面量只认服务端给的：summary 用 `granted_today`，claim 用 `already_granted`。
// 报告 §2.2 明确「两个端点用两套词说同一件事」，所以两处都要判，且**不**用本地日期推。
func (u *checkinUpstream) Status(ctx context.Context, acc dailycheckin.Account) (bool, bool, error) {
	a, ok := acc.Secret.(*Auth)
	if !ok || a == nil {
		return false, false, errors.New("phanthywork: 凭证类型不对")
	}
	// ★ 统一入口（2026-11-13）：设备未注册时签名请求必被拒 ⇒ 先确保就绪。
	if err := u.p.ensureReady(ctx, a); err != nil {
		return false, true, err
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	sum, localMS, err := u.p.client.FetchSummary(ctx, a)
	if err != nil {
		return false, true, err
	}
	// 顺带记下服务端日键与状态：Claim 拼幂等键**必须**用它（不许本地算日期）。
	a.noteDaily(sum.Daily.ServerDate, sum.Daily.Status)
	if sum.ServerTime != "" {
		a.applyServerTime(sum.ServerTime, localMS)
	}
	// 活动开关：字段缺失时按"开放"处理（照旧行为），只有明确 false 才跳过。
	enabled := true
	if !sum.FeatureFlags.DailyEnabled && sum.Daily.Status == "" && sum.Daily.ServerDate == "" {
		enabled = false
	}
	return isCheckedInToday(sum.Daily.Status), enabled, nil
}

// isCheckedInToday 判服务端状态字面量是否表示"今天已领"。
//
// 两套词都在这里（一处判据）：
//
//	granted_today   summary 的写法（报告 §2.2 实测：= 今天已领）
//	already_granted claim 的写法（报告 §4.7 实测：= 今天已领，**也是 200**）
//
// ⚠ **不含** "granted" / "claimed" 这类词：报告明确说"首次领取的确切 status
// 值没测到"，而把它猜成"已领"会有**实际后果** —— 第一次领到 300 分会被当成
// "今天已经领过"，于是 Credits 报 0（我第一版就是这么写的，被用例当场抓住：
// "领到积分 = 0, want 300"）。只认**明确的已领词**，其余一律按"刚领到"处理：
// 那方向最坏也只是多记一次成功，而反过来会把用户的积分记成 0。
func isCheckedInToday(status string) bool {
	switch strings.TrimSpace(status) {
	case "granted_today", "already_granted", "already_claimed", "already":
		return true
	}
	return false
}

// Claim 领取今天的签到（**写操作**，但幂等）。
//
// # 三条硬要求逐条落地
//
//	① body 是**空对象** —— 由 client.ClaimDailyLogin 里的字面量保证
//	② 幂等键 `daily:<installationId>:<server_date>`，日期**取服务端给的**；
//	   本地没缓存到日键时**先读一次 summary**（宁可多一次只读，也不本地算日期）
//	③ "今日已领" → (0, false, nil)：**幂等成功**，不是失败
func (u *checkinUpstream) Claim(ctx context.Context, acc dailycheckin.Account) (int64, bool, error) {
	a, ok := acc.Secret.(*Auth)
	if !ok || a == nil {
		return 0, false, errors.New("phanthywork: 凭证类型不对")
	}
	serverDate := a.dailyServerDate()
	// ★ 同上：claim 是**签名写操作**，同样先就绪（含注册）—— 判据只留 ensureReady 一处。
	if err := u.p.ensureReady(ctx, a); err != nil {
		return 0, false, err
	}
	if serverDate == "" {
		// 先读一次（只读）拿服务端日键 —— 这是"绝不用本地日期"的代价，值。
		sum, _, err := u.p.client.FetchSummary(ctx, a)
		if err != nil {
			return 0, false, err
		}
		a.noteDaily(sum.Daily.ServerDate, sum.Daily.Status)
		serverDate = sum.Daily.ServerDate
	}
	resp, err := u.p.client.ClaimDailyLogin(ctx, a, serverDate)
	if err != nil {
		return 0, false, err
	}
	// 服务端可能回一个**新的**日键（跨零点的竞态）：记下来供下一次用。
	a.noteDaily(resp.ServerDate, resp.Status)
	if isCheckedInToday(resp.Status) {
		// ③ 今日已领：幂等成功。回 0 分（分已经在上一次领到手了）。
		return 0, false, nil
	}
	credits := resp.Points
	if credits <= 0 {
		credits = resp.Reward.Points
	}
	return credits, false, nil
}

// ── gateway 侧的自报面（三行样板，不能下沉到驱动）──────────────────────

// DailyActions 账号行的每日动作按钮（gateway.DailyActionExt）。
func (p *Provider) DailyActions() []gateway.DailyAction {
	if p == nil || p.checkin == nil || !p.checkin.Ready() {
		return nil
	}
	d := p.checkin.Descriptor()
	return []gateway.DailyAction{{
		ID:     d.ID,
		Label:  d.Label,
		Title:  d.Title,
		OneURL: d.OneURL,
		AllURL: d.AllURL,
		Batch:  d.AllURL != "",
	}}
}

// CheckinAll 对本上游全部账号执行一次签到（gateway.DailyCheckinExt）。
//
// ⚠ 不直接用驱动的 RunAll：它丢弃逐账号结果（定时任务不需要），而跨上游入口
// 需要逐账号结果 —— 否则某个上游整片失败时用户只看到"成功了 N 个"。
// 所以这里走与 HTTP 全量端点同一条路（对每个号调 CheckinOne）。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	if p == nil || p.checkin == nil || !p.checkin.Ready() {
		return gateway.DailyCheckinReport{}
	}
	up := &checkinUpstream{p: p}
	accounts, err := up.Accounts(ctx)
	if err != nil {
		return gateway.DailyCheckinReport{Error: "列账号失败: " + err.Error()}
	}
	out := make([]gateway.DailyCheckinResult, 0, len(accounts))
	for _, acc := range accounts {
		if ctx.Err() != nil {
			break
		}
		res := p.checkin.CheckinOne(ctx, acc, "manual")
		out = append(out, gateway.DailyCheckinResult{UID: res.UID, Status: res.Status, Detail: res.Detail})
	}
	return gateway.DailyCheckinReport{Results: out}
}

// checkinRoutes 驱动提供的两条 HTTP 端点（单账号 / 全量）。
//
// ⚠ 路径必须与 checkinDescriptor 里的 OneURL/AllURL 逐字一致 ——
// 前端按那份声明打过来，不一致就是"按钮在但点了 404"。
func (p *Provider) checkinRoutes() []gateway.AdminRoute {
	if p == nil || p.checkin == nil || !p.checkin.Ready() {
		return nil
	}
	d := p.checkin.Descriptor()
	return []gateway.AdminRoute{
		{
			Method:     "POST",
			Path:       d.OneURL,
			Handler:    p.checkin.HandlerOne,
			Capability: gateway.CapCheckin,
			Title:      "签到（单账号）",
		},
		{
			Method:     "POST",
			Path:       d.AllURL,
			Handler:    p.checkin.HandlerAll,
			Capability: gateway.CapCheckin,
			Title:      "全部签到",
		},
	}
}

// checkinLogWarn 装配层没注入签到日志时的提示（只提示一次）。
var checkinLogWarn = func() {}

// logNoCheckinLog 在没接线签到日志时提示一次。
func logNoCheckinLog() {
	log.Printf("phanthywork: 未接线签到历史日志（SetCheckinLog）—— 签到仍可用，但界面「今日签到」列会一直为空")
}
