package qoder

import (
	"context"
	"errors"

	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// checkin.go —— 把 Qoder 的每日积分领取接到**共享驱动**（internal/dailycheckin）。
//
// # 此前缺三处接线（与 lobsterai 完全同形）
//
//	qoder 有领取端点（client.ClaimDailyCheckin，含活动解析）
//	Caps 声明了 CapCheckin，面板上也挂了 POST /admin/{id}/checkin
//	但：
//	  ① 账号行**没有**签到按钮（没实现 DailyActionExt）
//	  ② **没有**自动签到（没实现 JobExt）
//	  ③ 领取**不写** checkinlog → 界面「今日签到」列永远是 `—`
//
// ③ 的形态在本仓有先例 —— trae 的代码注释写着同样的话。现在三处一起
// 接上：**用共享驱动**，本文件只描述 qoder 自己的三个事实。
//
// # ⚠ qoder 是**两份实例**（qoder / qodercn），这带来两个约束
//
// ① **路径必须按实例生成**（`/admin/qoder/checkin` vs `/admin/qodercn/checkin`）——
//    否则路由表里两条同路径注册会冲突，而前端按 uid 调用时还可能
//    把 cn 的签到打到国际版的账号上。
// ② **账号必须按 product_id 过滤** —— 两份实例共用同一个凭证目录
//    （见 Config.AuthDir 的注释），不过滤会让 qoder 实例去签 qodercn 的号。
//
// 这两条都由本文件显式处理 —— 共享驱动不该知道"这个上游有两份实例"
//（那是 qoder 自己的事实）。

// 编译期断言：本上游满足共享驱动要求的接口。
var _ dailycheckin.Upstream = (*checkinUpstream)(nil)

// checkinUpstream 把 *Provider 适配成 dailycheckin.Upstream。
type checkinUpstream struct{ p *Provider }

// Accounts 列**本实例**的账号（按 product_id 过滤）。
//
// ⚠ 两个产品共用同一个 auth_dir（靠凭证里的 product_id 区分，见 Config
// 的注释），所以必须过滤 —— 否则 qoder 实例会去签 qodercn 的账号。
func (u *checkinUpstream) Accounts(ctx context.Context) ([]dailycheckin.Account, error) {
	if u == nil || u.p == nil || u.p.authDir == "" {
		return nil, nil
	}
	// LoadDirFor 已按 productID 过滤（参照 qoder 的凭证读取约定）。
	list, err := LoadDirFor(u.p.authDir, u.p.productID)
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

// Status 查今天的领取状态。
//
// 复用 client.FetchCheckinStatus（它已经是只读的，且注释里记着两条
// 实测判据：不能按"列表非空"判 Active，也不能把"没有可领项"当"已领"）。
func (u *checkinUpstream) Status(ctx context.Context, acc dailycheckin.Account) (bool, bool, error) {
	a, ok := acc.Secret.(*Auth)
	if !ok || a == nil {
		return false, false, errors.New("qoder: 凭证类型不对")
	}
	st, ok := u.p.client.FetchCheckinStatus(ctx, a)
	if !ok {
		// 查不到（网络/凭据/形状）：交给驱动记 fail 让用户看见。
		return false, true, errors.New("签到状态查询失败（网络 / 凭据 / 响应形状异常）")
	}
	if !st.Active {
		// 上游没有签到活动：跳过，**不记历史**。
		return false, false, nil
	}
	if st.ActionRequired {
		// 需要用户先去官方客户端登录一次（未开通每日领取）。
		// 这不是"我们查不到"，也不是"今天没签上" —— 但用户需要知道，
		// 所以按 enabled=true 继续，让驱动的 Claim 去给出那条明确提示。
		return st.TodayCheckedIn, true, nil
	}
	return st.TodayCheckedIn, true, nil
}

// Claim 领取今天的积分。
//
// 第二个返回值是 actionRequired —— qoder 的一条 inactive 是
// "账号尚未在官方客户端登录过"，它**既不是"上游没开放"（等就好），
// 也不是纯粹的失败**（用户去官方客户端登录一次就能领）。必须把它
// 与错误一起交给上层，否则前端只能显示"签到失败"。
func (u *checkinUpstream) Claim(ctx context.Context, acc dailycheckin.Account) (int64, bool, error) {
	a, ok := acc.Secret.(*Auth)
	if !ok || a == nil {
		return 0, false, errors.New("qoder: 凭证类型不对")
	}
	out := u.p.client.ClaimDailyCheckin(ctx, a)
	switch out.Kind {
	case "claimed":
		return int64(out.Credit), false, nil
	case "already-claimed":
		// 竞态：查的时候还没领，领的时候已领。与"领取成功"对用户等价。
		return 0, false, nil
	case "inactive":
		// ⚠ actionRequired 原样带出去 —— 它区分了两种 inactive：
		// "上游活动没开"（用户无事可做）vs "需要你去官方客户端登录"。
		return 0, out.ActionRequired, errors.New("上游未开放签到活动（" + out.Message + "）")
	default:
		if out.Message == "" {
			return 0, out.ActionRequired, errors.New("签到失败")
		}
		return 0, out.ActionRequired, errors.New(out.Message)
	}
}

// checkinDescriptor 本实例的展示与路由信息。
//
// ⚠ 路径按实例 ID 生成（qoder / qodercn 两条不同的路由），
// 与既有的 checkinPath() / balancePath() 同一约定 —— 不新造风格。
func (p *Provider) checkinDescriptor() dailycheckin.Descriptor {
	return dailycheckin.Descriptor{
		ID:    p.ID() + "-checkin",
		Label: "签到",
		// Title 带上产品名（qoder / qoder-cn 的按钮说明不该一模一样）。
		Title:  p.Product().DisplayName + " 每日积分领取",
		OneURL: p.checkinPath(),
		AllURL: "/admin/" + p.ID() + "/checkin/all",
	}
}

// initCheckin 构造共享驱动（NewWithConfig 里调用）。
func (p *Provider) initCheckin() {
	if p == nil {
		return
	}
	p.checkin = dailycheckin.New(
		&checkinUpstream{p: p},
		p.checkinDescriptor(),
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
// 因为 dailycheckin **不 import gateway**（架构硬约束，见它的 doc.go）。
// 代价是每个上游三行样板，收益是架构约束成立（第一次构建时 arch_test
// 就把"上游依赖上游"判成了违规）。

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

// CheckinAll gateway.DailyCheckinExt：核心触发"所有上游签到"时调它。
//
// # 跨上游入口必须逐实例分开（qoder / qodercn 是两个 Provider 实例）
//
// 核心遍历注册表时它们各被问一次，各自只扫自己的账号 ——
// 与 checkinPath() 按实例生成同一个理由（见文件头注释）。
//
// # 为什么不用 p.checkin.RunAll
//
// 它丢弃逐账号结果（定时任务只需要 error）。跨上游入口要逐账号结果，
// 否则某个上游整片失败时用户只看到"成功了 N 个"。
// 签到本体仍是驱动的 CheckinOne —— 与按钮、定时任务同一段。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	if p == nil || p.checkin == nil || !p.checkin.Ready() {
		return gateway.DailyCheckinReport{Results: nil}
	}
	up := &checkinUpstream{p: p}
	accs, err := up.Accounts(ctx)
	if err != nil {
		return gateway.DailyCheckinReport{Error: "列账号失败: " + err.Error()}
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
	return gateway.DailyCheckinReport{Results: out}
}

// Jobs 自动任务（gateway.JobExt）—— 签到 + 续期，实现在 **jobs_refresh.go**。
//
// ⚠ 这个函数**搬到那边去**了，不是删掉。理由：`Jobs()` 只有一处，
// 而本上游需要两个任务（签到 / 续期）。分在两个文件里各实现一份
// `Jobs()` 会编译冲突；只在这边返回签到那一条，续期任务就**永远注册不上**
// —— 那正是本轮缺陷的形态（用户报「没有 Token」：因为从不续期，
// 而过期时刻只能从续期响应拿到）。
//
// 留这条注释是为了让"签到相关的代码在 checkin.go"这个直觉
// 不至于让人在这里重新加一个 Jobs()。
