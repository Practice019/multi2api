// driver.go 把签到接进共享驱动 internal/dailycheckin，并补齐账号池的列集。
//
// # 为什么用共享驱动
//
// 共享驱动把「行内按钮 / 全量按钮 / 定时任务 / 写签到历史」收在一处，
// 上游只描述自己的事实（列账号、查状态、领取）。自己写一套的代价本仓有先例：
// lobsterai 漏了写历史，表现是「签到按钮点了、今日签到列还是空」——
// 不报错、编译通过、契约测试也查不到。
package monkeycode

import (
	"context"
	"errors"

	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// 编译期断言：本上游满足共享驱动要求的接口。
var _ dailycheckin.Upstream = (*checkinUpstream)(nil)

// checkinDescriptor 签到动作的展示与路由。
//
// ID 必须是 `<provider>-checkin` 形态（跨上游唯一），前端按它写 data-act。
// 本驱动自己挂端点，所以路径与 AccountColumns 无关。
var checkinDescriptor = dailycheckin.Descriptor{
	ID:     "monkeycode-checkin",
	Label:  "签到",
	Title:  "单账号签到（过 cap.js 工作量证明）",
	OneURL: "/admin/monkeycode/checkin",
	AllURL: "/admin/monkeycode/checkin-all",
}

// checkinUpstream 把 *Provider 适配成 dailycheckin.Upstream。
//
// 单独一个类型而不是给 *Provider 加方法：驱动用自己的 Account 形状，
// 混在 *Provider 上会让两套同名方法（Status/Accounts）意外命中类型断言。
type checkinUpstream struct{ p *Provider }

// Accounts 列出本上游全部账号。
func (u *checkinUpstream) Accounts(ctx context.Context) ([]dailycheckin.Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := u.p
	p.mu.RLock()
	enum := p.uidEnumerator
	local := make(map[string]*Auth, len(p.creds))
	for k, v := range p.creds {
		local[k] = v
	}
	p.mu.RUnlock()

	var uids []string
	if enum != nil {
		uids = enum()
	} else {
		// 测试路径：没有池子就用本实例的 creds。
		for k := range local {
			uids = append(uids, k)
		}
	}
	out := make([]dailycheckin.Account, 0, len(uids))
	for _, uid := range uids {
		a := p.cred(uid)
		if a == nil || !a.Usable() {
			continue
		}
		out = append(out, dailycheckin.Account{UID: uid, Secret: a})
	}
	return out, nil
}

// Status 查一个账号今天的签到状态。
//
// 三个返回值的分工（照驱动契约）：
//
//	false,false,nil  上游今天不开放 → 跳过，**不写历史**
//	false,true,err   我们没查到 → 记 StatusFail，让用户看见
func (u *checkinUpstream) Status(ctx context.Context, acc dailycheckin.Account) (signedIn bool, enabled bool, err error) {
	a := authFromSecret(acc.Secret)
	if a == nil {
		return false, false, errNoAuth
	}
	return u.p.CheckinStatus(ctx, a)
}

// Claim 替一个账号签到。
//
// 第二个返回值 actionRequired 恒 false：本上游没有"要用户去官方客户端做一步"的形态。
func (u *checkinUpstream) Claim(ctx context.Context, acc dailycheckin.Account) (int64, bool, error) {
	a := authFromSecret(acc.Secret)
	if a == nil {
		return 0, false, errNoAuth
	}
	return u.p.ClaimCheckin(ctx, a)
}

// authFromSecret 从驱动的 Secret 里取本包 Auth。
func authFromSecret(secret any) *Auth {
	if a, ok := secret.(*Auth); ok {
		return a
	}
	return nil
}

// initCheckin 构造共享驱动（New 里调用；幂等）。
//
// 自动签到的间隔由 Config.CheckinInterval 决定（<=0 = 只给按钮，不自动跑）。
func (p *Provider) initCheckin() {
	if p == nil {
		return
	}
	p.checkin = dailycheckin.New(
		&checkinUpstream{p: p},
		checkinDescriptor,
		dailycheckin.Options{
			Log:      p.checkinLog,
			Interval: p.checkinInterval,
			Enabled:  p.checkinInterval > 0,
		},
	)
}

// ---- gateway 扩展点：把驱动接出来 ----

// DailyActions 自报每日动作（前端按它画按钮）。
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

// CheckinAll 对**本上游全部账号**签到一次，返回逐账号结果。
//
// ⚠ 用 CheckinOne 逐个跑，而不是 RunAll —— 后者丢弃逐账号结果，
// 而顶部「全部签到」需要知道哪个号失败了（否则整片失败也看不出来）。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	if p == nil || p.checkin == nil || !p.checkin.Ready() {
		return gateway.DailyCheckinReport{}
	}
	up := &checkinUpstream{p: p}
	accs, err := up.Accounts(ctx)
	if err != nil {
		return gateway.DailyCheckinReport{}
	}
	results := make([]gateway.DailyCheckinResult, 0, len(accs))
	for _, acc := range accs {
		if err := ctx.Err(); err != nil {
			break
		}
		r := p.checkin.CheckinOne(ctx, acc, "manual")
		results = append(results, gateway.DailyCheckinResult{
			UID:    r.UID,
			Status: r.Status,
			Detail: r.Detail,
		})
	}
	return gateway.DailyCheckinReport{Results: results}
}

// HasToken 报告凭证里有没有可用的 access token。
//
// 本上游用 session cookie（不是 bearer token），但**有**三把凭据可展示 ——
// 界面的「Token」列是"这份凭证还能不能用"，所以判 ChatUsable：
// 三把齐 = 能对话 = 这一列显示有效。
//
// ⚠ 只读、不发网络请求（它是列表渲染路径，每账号调一次）。
func (p *Provider) HasToken(cred gateway.Credential) bool {
	// 两条路径都要覆盖：
	//
	//	① 池里带 secret（LoadCredentialsWithSecrets / 导入时写入）→ 直接用
	//	② 只有投影（启动时 LoadCredentials 给的，Secret 为 nil）→ 按 uid 回查
	//
	// # 为什么必须有 ②（实测）
	//
	// 启动装载走的是不带 secret 的投影，于是 cred.Secret == nil，
	// authOf 直接报「凭证为空」→ HasToken 恒 false。
	// 表现是：注册机导入的账号在界面上永远显示「无 Token」，
	// 而它其实能对话（首次对话会自动补签）。
	//
	// ⚠ 判据必须与「能不能对话」一致：ChatUsable 要求三把齐。
	//   若改成只判 session，界面会显示成有 Token 但一对话就补签失败 ——
	//   那种假的有比显示没有更糟（用户不会去点对话，也就不触发补签）。
	a := p.cred(cred.UID)
	if a == nil {
		// 兜底：secret 里带了一份完整的 *Auth（导入路径）。
		if got, err := p.authOf(cred); err == nil {
			a = got
		}
	}
	if a == nil {
		return false
	}
	return a.ChatUsable()
}
// errNoAuth 驱动里取不到凭证时的错误。
var errNoAuth = errors.New("monkeycode: 取不到凭证")
