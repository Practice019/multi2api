// quotaext.go 实现 gateway.QuotaExt —— 让额度在界面上有值。
//
// # 为什么需要它（真机实测踩到的）
//
// 不实现时启动日志会明确警告：
//
//	admin: 额度刷新跳过上游 "monkeycode"（未实现 gateway.QuotaExt）
//	       —— 它的额度在界面上将恒为未知
//
// 而本上游**确实有额度概念**（`GET /users/wallet` 给 balance + daily_token），
// 所以这是个该补的空缺，不是"上游没有这个能力"。
//
// # 三态语义（照 QuotaExt 的注释）
//
//	uid 不存在        → (_, false)      调用方回 404 / 跳过
//	取到了            → (view, true) + HasData=true
//	取不到（网络/限流） → (view, true) + HasData=false  界面显示"未知"
//
// 第三条**不能**返回 error：上游临时抽风不该刷错误日志，
// 但也**不能**填 0 —— 那会把"没查过"误报成"额度为 0"（AGENTS.md 陷阱 4）。
package monkeycode

import (
	"context"
	"time"

	"workbuddy2api/internal/gateway"
)

// quotaTimeout 额度探测的超时。
//
// 额度刷新是**后台批量**跑的（一轮几十个号），单号不能占太久 ——
// 卡住一个就会拖慢整轮。对话不受这个限制（那个由 ctx 控制）。
const quotaTimeout = 20 * time.Second

// RefreshQuota 取某个账号的当前额度。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	a := p.cred(uid)
	if a == nil {
		return gateway.QuotaView{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout)
	defer cancel()

	wallet, err := p.client.FetchWallet(ctx, a)
	if err != nil {
		// 取不到 —— 如实回"无数据"，不填 0。
		return gateway.QuotaView{Kind: gateway.QuotaKindCredits, HasData: false}, true
	}

	// Free = 每日 token 余量（按周期重置的那部分）。
	//
	// ⚠ 它与 balance 是**两个账本**，关系是并列而非包含：
	//
	//	balance            积分（签到/邀请获得，不重置）
	//	daily_token_balance 每日 token（每天重置到 limit）
	//
	// 所以 Free 报 daily_token_balance，Remaining 报 balance。
	return gateway.QuotaView{
		Kind:      gateway.QuotaKindCredits,
		Remaining: wallet.Balance,
		Free:      wallet.DailyTokenBalance,
		// FreeLimit 是每日 token 的**上限** —— 有了它界面才能显示
		// 「剩余 / 上限」（用户在注册机那边看到的「今日 Token」列就是这形态）。
		// 只报剩余的话读者不知道今天总共能跑多少。
		FreeLimit: wallet.DailyTokenLimit,
		// FreeKnown 必须显式置 true —— 漏了它界面上的「今日 Token」列
		// 会恒显示破折号，尽管 Free 已经有值。core 只认 FreeKnown
		// （见 admin.accountViews 的 "只在 FreeKnown 时才取址"）。
		// 写了 Free 却没写 FreeKnown 等于把数据算出来又扔掉，且不报错。
		FreeKnown: true,
		// HasData 是**有值**的判据：balance 的 0 是合法值（免费模型走
		// daily_token），所以这里判"接口有没有给，而不是数字是不是非零"。
		// 走到这一行就说明接口给了 —— 恒 true。
		HasData: true,
	}, true
}
