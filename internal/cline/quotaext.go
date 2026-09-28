package cline

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/gateway"
)

// quotaext.go —— cline 通过 `gateway.QuotaExt` 自报「我的额度怎么取」。
//
// # 为什么需要它（用户问「这个账号没有额度吗」）
//
// 账号池里 cline 那行「额度」显示 `—`，看着像"没有额度"。
// 而实测该账号**有** 500000：
//
//	GET /api/v1/users/{account_id}/balance
//	→ {"data":{"userId":"usr-01M3…","balance":500000},"success":true}
//
// 显示 `—` 的原因不是没额度，而是没人**把余额写回池**：
//
//	池里那份 quota 只在 `RefreshQuota` 被调用时才更新
//	cline 此前没实现本扩展点 → 从来没写过 → 界面读到的永远是"没有数据"
//
// 注意这与 `/admin/cline/balance` 那个管理端点**不是一回事**：
// 那个端点按需查询并直接返回，不碰池子；而账户列表读的是池里的值。
// 两者用的都是同一个 FetchBalance，只是出口不同。
//
// # ⚠ 显示**原始值**，不做单位换算（用户明确要求）
//
// `balance: 500000` 的单位**没有确证** —— 参照项目里它只出现在 zod schema
//（`z.string()`）与 redaction 关键词表，**没有任何换算点**。
//
// 我们另有一个 `balanceScale` 常量（推断为 micro-USD，÷100000 → $5.00），
// 但那是**推断**。本扩展点**不用它** —— 直接报原始值，理由：
//
//   - 界面上的数字若带了错误的单位，比没有数字更误导
//    （用户会拿 $5.00 去判断"够不够用"）
//   - 原始值与上游账单/控制台逐一对应，用户自己能核对
//   - 单位一旦由实测确证，改一处即可切换（见 balanceScale 的注释）
//
// 所以这里报 `Remaining = int64(rawBalance)`，并且**不**乘除任何系数。
//
// 编译期断言。
var _ gateway.QuotaExt = (*Provider)(nil)

// RefreshQuota 取某个账号的当前额度（原始数值）。
//
// 返回值语义（见 gateway.QuotaExt）：
//
//	(额度, false)  账号不存在 / 未接线 → 调用方回 404 或跳过
//	(有数据, true) 查到了 → 调用方写回池（HasData=true）
//	(无数据, true) 查不到 → **不写池**，界面保持"未知"
//
// ⚠ 查不到时**不编造 0**：0 是"额度用完"的语义，会让用户以为号废了。
// 这正是 gateway.QuotaView.HasData 存在的理由。
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
		log.Printf("cline: 额度刷新失败 uid=%s: %v（显示为未知）", shortUID(uid), err)
		return gateway.UnknownQuota(), true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res := p.client.FetchBalance(ctx, a)
	if res.Error != "" {
		// 查不到是**正常情况**（凭据过期、上游限流、网络）——
		// 记一行日志但不报错，界面显示"未知"。
		log.Printf("cline: 额度刷新失败 uid=%s: %s（显示为未知）", shortUID(uid), res.Error)
		return gateway.UnknownQuota(), true
	}
	// ⚠ 原始值直接转 int64，**不乘不除**（见文件头）。
	return gateway.CreditsQuota(int64(res.Raw)), true
}
