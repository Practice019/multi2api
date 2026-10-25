// quota.go 实现 gateway.QuotaExt —— 让「额度」列在界面上有值。
//
// # 为什么需要它（用户要求「对齐其他上游」）
//
// 不实现时那一列恒显示 `—`，启动日志也会明确警告：
//
//	admin: 额度刷新跳过上游 "verdent"（未实现 gateway.QuotaExt）
//	       —— 它的额度在界面上将恒为未知
//
// # ⚠ 本上游的额度是**比例**，不是绝对值（与其余上游的关键差别）
//
// MonkeyCode 给的是 token 计数（daily_token 9944867 / 10000000），
// 而 Verdent 的 /verdent/usage_windows 给的是**已用百分比**：
//
//	{"free_mode":{"ratio_5h":0,"ratio_7d":2.05,"is_available":true,
//	               "reset_at_5h":0,"reset_at_7d":1793456518},
//	 "eco_mode":{...}}
//
// 它没有"还剩多少 token"这种数。所以这里把口径**明确成百分比**：
//
//	Remaining = 100 - max(ratio_5h, ratio_7d)   剩余可用比例（0~100）
//	FreeLimit = 100                              上限就是 100%
//
// ⚠ 取 max 而不是分别报两个窗口：界面上只有一列，而**卡住你的是更紧的那个**
// （5 小时窗用了 90% 时，7 天窗还剩 98% 也照样跑不动）。报松的那个会让人
// 在"明明还有额度"的错觉里撞上限流 —— 那是最难查的一类。
//
// ⚠ 这个单位不是 token —— 所以**不要**拿它和别的上游的额度做数值比较。
// 注释写在这里，因为界面上两列长得一模一样。
package verdent

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"workbuddy2api/internal/gateway"
)

// quotaTimeout 额度探测的超时。
//
// 额度刷新是**后台批量**跑的（一轮几十个号），单号不能占太久 ——
// 卡住一个就会拖慢整轮。
const quotaTimeout = 20 * time.Second

// usageResp /verdent/usage_windows 的响应形状（实测字段名）。
type usageResp struct {
	ErrCode int `json:"errCode"`
	Data    struct {
		FreeMode *usageMode `json:"free_mode"`
		EcoMode  *usageMode `json:"eco_mode"`
	} `json:"data"`
}

// usageMode 一个档位的用量窗口。
type usageMode struct {
	Ratio5h      float64 `json:"ratio_5h"`
	Ratio7d      float64 `json:"ratio_7d"`
	IsAvailable  bool    `json:"is_available"`
	IsSubscribed bool    `json:"is_subscribed"`
}

// RefreshQuota 取某个账号的当前额度（口径：剩余百分比，见文件头）。
//
// 三态语义（照 QuotaExt 的注释）：
//
//	uid 不存在 / 没有凭证 → (_, false)      调用方回 404 / 跳过
//	取到了                → (view, true) + HasData=true
//	取不到（网络/限流）    → (view, true) + HasData=false  界面显示"未知"
//
// 第三条**不能**返回 error：上游临时抽风不该刷错误日志，
// 但也**不能**填 0 —— 那会把"没查过"误报成"额度为 0"（用户会据此误删账号）。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	a := p.authOfUID(uid)
	if a == nil {
		return gateway.QuotaView{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout)
	defer cancel()

	status, raw, err := p.client().doJSON(ctx, a.Token, usageURL, "GET", nil)
	if err != nil || status/100 != 2 {
		// 取不到 —— 如实回"无数据"，不填 0。
		return gateway.QuotaView{Kind: gateway.QuotaKindCredits, HasData: false}, true
	}
	var resp usageResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return gateway.QuotaView{Kind: gateway.QuotaKindCredits, HasData: false}, true
	}

	// 选哪个档位：订了 eco 就看 eco，否则看 free。
	// ⚠ 不"两个都看取更紧的" —— 未订阅的 eco 恒是 100%（不可用），
	// 那样所有免费号都会显示"额度用尽"。
	m := resp.Data.FreeMode
	if resp.Data.EcoMode != nil && resp.Data.EcoMode.IsSubscribed {
		m = resp.Data.EcoMode
	}
	if m == nil {
		return gateway.QuotaView{Kind: gateway.QuotaKindCredits, HasData: false}, true
	}

	used := math.Max(m.Ratio5h, m.Ratio7d)
	remain := int64(math.Round(100 - used))
	if remain < 0 {
		remain = 0
	}
	if remain > 100 {
		remain = 100
	}
	return gateway.QuotaView{
		Kind:      gateway.QuotaKindCredits,
		Remaining: remain,
		// Free 与 FreeLimit 一起报，"剩余 / 上限"两列才有意义。
		// ⚠ FreeKnown 必须显式置 true —— 漏了它界面上的那列会恒显示破折号，
		// 尽管 Free 已经有值（core 只认 FreeKnown）。
		Free:      remain,
		FreeLimit: 100,
		FreeKnown: true,
		HasData:   true,
	}, true
}
