package raccoon

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/gateway"
)

// quotaext.go —— raccoon 通过 `gateway.QuotaExt` 自报「我的额度怎么取」。
//
// # 为什么需要它
//
// 账号池里 raccoon 那行「额度」此前显示 `—`，看着像"没有额度"。
// 而它有余额端点（`GET /api/web/points/v1/balance`，返回 `available_points`）。
//
// 显示 `—` 的原因不是没额度，而是没人**把余额写回池**：
// 池里那份 quota 只在 `RefreshQuota` 被调用时才更新，而本上游此前
// 没实现本扩展点 → 从来没写过 → 界面读到的永远是"没有数据"。
//
// 注意这与 `/admin/raccoon/balance` 那个管理端点**不是一回事**：
// 那个端点按需查询并直接返回，不碰池子；账户列表读的是池里的值。
//
// # ⚠ 显示**原始值**，不做单位换算（用户明确要求）
//
// 直接报 `available_points` 本身。raccoon 的积分单位我们**没有**可靠依据
// 做换算（"积分"是上游自己的记账单位），所以不引入任何系数 ——
// 界面上的数字若带错误的单位，比没有数字更误导。
//
// 编译期断言。
var _ gateway.QuotaExt = (*Provider)(nil)

// RefreshQuota 取某个账号的当前额度（原始积分数）。
//
// 返回值语义（见 gateway.QuotaExt）：查不到时 `(UnknownQuota, true)`
// ——**不写池**，界面保持"未知"。绝不编造 0（0 是"已用光"的语义）。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	if uid == "" {
		return gateway.UnknownQuota(), false
	}
	if p == nil || p.creds == nil {
		// 未接线（装配层没注入凭证源）：不算错，但取不到。
		return gateway.UnknownQuota(), false
	}
	cred, ok := p.creds(uid)
	if !ok {
		return gateway.UnknownQuota(), false
	}
	a, err := authOf(cred)
	if err != nil || a == nil {
		log.Printf("raccoon: 额度刷新失败 uid=%s: %v（显示为未知）", shortUID(uid), err)
		return gateway.UnknownQuota(), true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b, err := p.client.FetchBalance(ctx, a)
	if err != nil {
		// 查不到是**正常情况**（凭据过期、上游限流、网络）——
		// 记一行日志但不报错，界面显示"未知"。
		log.Printf("raccoon: 额度刷新失败 uid=%s: %v（显示为未知）", shortUID(uid), err)
		return gateway.UnknownQuota(), true
	}
	// ⚠ 原始值直接转 int64，**不乘不除**（见文件头）。
	return gateway.CreditsQuota(int64(b.Total)), true
}
