package lobsterai

import (
	"context"
	"errors"
	"log"

	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// checkin.go —— 把 LobsterAI 的每日签到接到**共享驱动**（internal/dailycheckin）。
//
// # 此前缺三处接线（用户报的：有签到能力但界面用不了）
//
//	lobsterai 有签到端点（client.ClaimDailyCheckin，三步流程）
//	Caps 声明了 CapCheckin，面板上也挂了 POST /admin/lobsterai/checkin
//	但：
//	  ① 账号行**没有**签到按钮（没实现 DailyActionExt）
//	  ② **没有**自动签到（没实现 JobExt）
//	  ③ 签到**不写** checkinlog → 界面「今日签到」列永远是 `—`
//
// ③ 的形态在本仓有先例 —— trae 的代码注释写着同样的话：
//
//	「此前 trae 签到后不写历史，于是今天已经签到也不显示 ——
//	  这就是『签到按钮点了、列还是空』」
//
// 现在三处一起接上：**用共享驱动**，本文件只需实现三个方法。
//
// # 为什么用共享驱动而不是照 trae 抄一遍
//
// 用户要求「所有上游共用一个签到接口」。抄一遍的代价已经显现 ——
// 上面那三处漏接线就是"抄漏"（漏了不报错，编译通过、契约测试也查不到）。
// 共享驱动把"按钮 / 自动化 / 写历史 / 批量端点"收在一处，
// 上游只描述自己的事实（列账号、查状态、领取）。

// 编译期断言：本上游满足共享驱动要求的接口。
var _ dailycheckin.Upstream = (*checkinUpstream)(nil)

// checkinUpstream 把 *Provider 适配成 dailycheckin.Upstream。
//
// 单独一个类型而不是给 *Provider 加方法：驱动的接口是 `Account`（驱动
// 自己的形状），而 *Provider 的方法会暴露给 gateway 的那套形状 ——
// 两套名字相同的 `Status`/`Accounts` 混在同一个类型上会让类型断言意外命中。
type checkinUpstream struct{ p *Provider }

// Accounts 列本上游当前要处理的账号（扫自己的凭证目录）。
func (u *checkinUpstream) Accounts(ctx context.Context) ([]dailycheckin.Account, error) {
	if u == nil || u.p == nil || u.p.authDir == "" {
		return nil, nil
	}
	list, err := LoadDir(u.p.authDir)
	if err != nil {
		return nil, err
	}
	out := make([]dailycheckin.Account, 0, len(list))
	for _, a := range list {
		if a == nil {
			continue
		}
		uid := a.UIDValue()
		if uid == "" {
			continue
		}
		out = append(out, dailycheckin.Account{UID: uid, Secret: a})
	}
	return out, nil
}

// Status 查今天的签到状态。
//
// 走 client.CheckinStatus（只读：复用 ClaimDailyCheckin 的前 5 步判据，
// **不提交领取**）。见那个方法的注释。
func (u *checkinUpstream) Status(ctx context.Context, acc dailycheckin.Account) (bool, bool, error) {
	a, ok := acc.Secret.(*Auth)
	if !ok || a == nil {
		return false, false, errors.New("lobsterai: 凭证类型不对")
	}
	out := u.p.client.CheckinStatus(ctx, a)
	switch out.Kind {
	case "already-claimed":
		return true, true, nil
	case "claimable":
		return false, true, nil
	case "inactive":
		// 上游没开放签到（未开始/已结束/无资格）：跳过，**不记历史**。
		return false, false, nil
	default: // failed
		return false, true, errors.New(out.Message)
	}
}

// Claim 领取今天的签到。
//
// 第二个返回值恒 false —— lobsterai 没有"需要用户去官方侧做一步"
// 这类 inactive（它的 inactive 是"活动未开始/已结束/无资格"，用户无事可做）。
func (u *checkinUpstream) Claim(ctx context.Context, acc dailycheckin.Account) (int64, bool, error) {
	a, ok := acc.Secret.(*Auth)
	if !ok || a == nil {
		return 0, false, errors.New("lobsterai: 凭证类型不对")
	}
	out := u.p.client.ClaimDailyCheckin(ctx, a)
	switch out.Kind {
	case "claimed":
		return int64(out.Credit), false, nil
	case "already-claimed":
		// 竞态：查的时候还没签，领的时候已签。
		// **不当作错误** —— 结果与"签到成功"对用户等价，返回 0 积分即可
		//（重复提交的风险由上游幂等键兜住）。
		return 0, false, nil
	case "inactive":
		// 活动关了：与 Status 的 inactive 同义。
		//
		// ⚠ 返回错误（而不是静默成功）：用户点了按钮应当知道
		// "上游今天不开放"，而不是看到一个假的"签到成功"。
		return 0, false, errors.New("上游未开放签到活动（" + out.Message + "）")
	default:
		if out.Message == "" {
			return 0, false, errors.New("签到失败")
		}
		return 0, false, errors.New(out.Message)
	}
}

// checkinDescriptor 本上游签到动作的展示与路由。
//
// 路径沿用**已有**的 `/admin/lobsterai/checkin`（不新造）——
// 它此前就挂在 AdminRoutes 上，改成驱动提供后路径不变，
// 既有前端行为与文档都不用改。
//
// AllURL 是本轮**新增**的（此前只有单账号端点）：
// 分组按钮与自动签到都走它。
var checkinDescriptor = dailycheckin.Descriptor{
	ID:     "lobsterai-checkin",
	Label:  "签到",
	Title:  "LobsterAI 每日签到领取积分",
	OneURL: "/admin/lobsterai/checkin",
	AllURL: "/admin/lobsterai/checkin/all",
}

// initCheckin 构造并缓存共享驱动（NewWithConfig 里调用；幂等）。
func (p *Provider) initCheckin() {
	if p == nil {
		return
	}
	p.checkin = dailycheckin.New(
		&checkinUpstream{p: p},
		checkinDescriptor,
		dailycheckin.Options{
			Log: p.log,
			// 自动签到：与其余上游同一个节奏（用户要求所有上游统一）。
			Interval: p.checkinInterval,
			Enabled:  p.checkinInterval > 0,
		},
	)
}

// ── gateway 扩展点：把驱动的结果翻成契约类型 ────────────────────────────
//
// ⚠ 这三处**必须在各上游包内**写，不能下沉到 dailycheckin ——
// 因为 dailycheckin **不 import gateway**（架构硬约束，见它的 doc.go：
// 依赖 gateway 的包会被 arch_test 当成"上游实现"，于是"上游依赖上游"
// 变成违规）。代价是每个上游三行样板，收益是架构约束成立。

// DailyActions 账号行的每日动作按钮（gateway.DailyActionExt）。
//
// 实现它之后，账号池里 lobsterai 的**每一行**都会出现「签到」按钮
// （此前没有 —— 用户报的现象）。
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
		// Batch：有全量端点 → 前端把它当"可批量"的动作
		//（分组标题上的批量按钮据此出现）。
		Batch: d.AllURL != "",
	}}
}

// CheckinAll gateway.DailyCheckinExt：核心触发"所有上游签到"时调它。
//
// # 为什么它只转发给驱动，不自己遍历
//
// 遍历、写历史、状态归一化**都在共享驱动里**（dailycheckin.RunAll）。
// 本包若再写一遍遍历，就会回到"抄漏"那条老路 —— 用户报的三处缺接线
// （没按钮 / 不自动 / 不写历史）全是抄漏的产物，且不报错。
//
// # 回执形状
//
// 驱动只回 error（它的 RunAll 是给定时任务用的 `func(ctx) error`），
// 而跨上游入口要**逐账号**结果，否则某个上游整片失败时用户只看到
// "成功了 N 个"，无法判断是哪个上游没动。所以这里调 HandlerAll
// 的同一段业务，而不是 RunAll —— 两个入口仍是同一段签到逻辑
// （HandlerAll 内部就是对每个号调 CheckinOne）。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	if p == nil || p.checkin == nil || !p.checkin.Ready() {
		return gateway.DailyCheckinReport{Results: nil}
	}
	// 复用驱动的执行体：先取本上游账号，再逐个 CheckinOne。
	//
	// ⚠ 不直接用 p.checkin.RunAll —— 它丢弃逐账号结果（定时任务不需要）。
	// 这里需要结果，所以走与 HTTP 全量端点同一条路（见 runCheckinAllOnce）。
	return gateway.DailyCheckinReport{Results: p.runCheckinAllOnce(ctx)}
}

// runCheckinAllOnce 逐个账号签到并收集结果（全量端点与本扩展点共用）。
func (p *Provider) runCheckinAllOnce(ctx context.Context) []gateway.DailyCheckinResult {
	up := &checkinUpstream{p: p}
	accs, err := up.Accounts(ctx)
	if err != nil {
		log.Printf("%s: 全量签到列账号失败: %v", checkinDescriptor.ID, err)
		return nil
	}
	out := make([]gateway.DailyCheckinResult, 0, len(accs))
	for _, acc := range accs {
		if ctx.Err() != nil {
			break
		}
		res := p.checkin.CheckinOne(ctx, acc, "manual")
		out = append(out, gateway.DailyCheckinResult{
			UID:    res.UID,
			Status: res.Status,
			Detail: res.Detail,
		})
	}
	return out
}

// Jobs 自动签到任务（gateway.JobExt）。
//
// 实现它之后，即使没人点按钮，每天也会自动签到并把结果写进历史 ——
// 此前完全没有这条链路（用户问"有设置自动签到吗"，答案是没有）。
func (p *Provider) Jobs() []gateway.Job {
	if p == nil || p.checkin == nil || !p.checkin.AutoEnabled() {
		return nil
	}
	return []gateway.Job{{
		Name:     p.checkin.Descriptor().ID,
		Interval: p.checkin.Interval(),
		Run:      p.checkin.RunAll,
	}}
}
