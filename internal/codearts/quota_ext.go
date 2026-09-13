// quota_ext.go CodeArts 实现 gateway.QuotaExt —— "我的额度怎么取"。
//
// # 为什么额度必须走扩展点，而不是核心写死
//
// 改造前 `/admin/accounts` 的 `quota`/`credits` **只有 workbuddy 有值**，
// 因为只有它有一条写回池的路径（POST /admin/credits/refresh →
// workbuddy.RefreshCredits → pool.SetCredits）。codearts 一条都没有，
// 于是它的额度落到 pool.restoreQuota 的兜底分支，
// 被造成 `{kind:credits, has_data:true, remaining:0}` ——
// **一个把"从没查过"伪装成"查过了，确实是 0"的假数据**。
//
// 用户看到的现象就是"codearts 额度是空的/0"。
//
// # 实测数据（真实凭证，2026-09-13 跑的）
//
//	FetchSubscription → 套餐=体验版 信用包=true 状态=normal
//	                    total=7500.00  remain=7474.04  used=25.96
//
// 所以 codearts **有**真实可取的额度，来源是既有的 FetchSubscription
//（GET {engineBase}/snap-manager/v1/statistics/plugin，见 welfare.go）。
//
// # 为什么是**单值**而不是按模型（这里纠正过一条错误的注释）
//
// `pool/quota.go` 开头那条注释写的是"codearts → 按模型配额（一个 int64 装不下）"。
// **那条注释是错的**，已在 pool 侧订正。依据是类型本身：
//
//	type QuotaState struct {      // quota.go:33
//	    Exhausted bool            // ← 只有"耗尽没耗尽"
//	    Reason    string
//	    CheckedAt time.Time
//	}
//
// 按模型探测**没有任何数值字段** —— 它回答的是"这个模型现在能不能用"
// （benefit 免费额度会独立于积分耗尽，见 quota.go 顶部），
// 而不是"还剩多少"。把布尔当额度数值用是不可能的。
//
// 唯一的数字来源就是订阅级积分 `CreditRemain`（账号整体还剩多少），
// 它就是一个标量，单值形态完全装得下。
package codearts

import (
	"log"

	"workbuddy2api/internal/gateway"
)

// RefreshQuota 取该账号的当前额度（gateway.QuotaExt）。
//
// 返回 (额度视图, ok)：
//
//	ok=false          账号解析不出来（uid 不存在 / 没有可用凭证）→ 调用方跳过
//	ok=true + 有数据  写回池
//	ok=true + 取不到  HasData=false → 调用方**不写池**，界面显示 `—`
//
// # 为什么取不到时返回 HasData=false 而不是 0
//
// 这正是本次要修的坑。0 会被读成"这个号没额度了"，
// 而"上游没告诉我"完全是另一件事（网络抖动、STS 需要续期、上游 5xx）。
// 用户在界面上看到的应该是 `—`（未知），不是 `0`（确定为零）。
//
// # 为什么失败**只记日志**、不返回 error
//
// 额度刷新是**展示性**操作，不是业务路径。它失败不该让
// /admin/accounts 报错，也不该刷错误日志把真正的故障淹掉。
// 契约把 error 留给"调用方需要知道并处理"的情况，这里不需要。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	a, err := p.accountFor(uid)
	if err != nil {
		// 账号不存在：让调用方知道"这条 uid 我服务不了"（可据此回 404）。
		return gateway.UnknownQuota(), false
	}

	sub, err := p.client.FetchSubscription(a)
	if err != nil {
		// 取不到 → 未知。**不返回 0**，也**不沿用旧值**。
		log.Printf("codearts: 额度刷新失败 uid=%s: %v（显示为未知，不填 0）", a.UID, err)
		return gateway.UnknownQuota(), true
	}

	// # 什么情况下算"上游没给额度数据"
	//
	// 上游对"没有积分套餐"的账号会返回**零值 Subscription**
	//（package 与 metrics 都缺），而不是报错。若此时把它当成
	// "查到 0"，就又是一个假数据 —— 与改造前的坑同源。
	//
	// 判据用 `Status == ""`：实测正常账号该字段恒为 "normal"，
	// 缺它说明上游压根没给套餐块。**比"CreditTotal==0"更可靠** ——
	// 真实存在总额为 0 的合法套餐（刚用完），那种"确实是 0"必须显示成 0。
	if sub.Status == "" {
		log.Printf("codearts: 额度刷新拉到空套餐 uid=%s（上游未返回 package 块，显示为未知）", a.UID)
		return gateway.UnknownQuota(), true
	}

	// CreditRemain 是 float64，而额度视图是 int64。
	// **截断**而不是四舍五入：额度宁可少报不多报 ——
	// 多报会让选号误以为这个号更富余（见 pool 的三因子权重）。
	// 小数位对"还剩多少额度"的展示没有意义。
	remain := int64(sub.CreditRemain)
	if sub.CreditRemain < 0 {
		// 上游可能返回负数（超额透支）。Clamp 到 0：
		// 负额度在展示上无意义，且会污染选号权重（负数权重 < 无记录）。
		remain = 0
	}

	return gateway.CreditsQuota(remain), true
}

// 编译期断言：Provider 实现额度扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"界面上额度一直空着"。
// 与 provider.go 里那两条断言同一理由。
var _ gateway.QuotaExt = (*Provider)(nil)
