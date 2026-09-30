// dailycheckin.go codearts 的**全量签到**（福利领取）入口。
//
// # 为什么本包需要一个"全量"端点（用户本轮要求）
//
//	"只有单账号的那就创建全部签到  统一所有的上游"
//
// 此前 codearts 只报了 OneURL（/admin/welfare/claim，对一个账号领取），
// 于是账号池顶部的全量按钮**没有它的位置** —— 而用户的判据是
// "所有上游统一"，每个上游都得有「全部签到」。
//
// 所以这里补一条 POST /admin/welfare/claim/all：对本上游全部账号
// 各领一次。它与单账号端点**共用** claimWelfareFor（同一段业务、
// 同一条历史记录判据），不是另写一份遍历。
//
// # 为什么路径是 /claim/all 而不是 /claim-all
//
// 与其余上游的全量端点同形（/admin/trae/checkin/all、
// /admin/lobsterai/checkin/all）。命名风格统一是"接口统一"的一部分 ——
// 用户要的统一包括这一层。
package codearts

import (
	"context"
	"net/http"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// checkinAllPath 全量福利领取端点的路径。
//
// ⚠ 它是 DailyAction.AllURL 的值 —— 改它等于改前端按钮打的地址。
const checkinAllPath = "/admin/welfare/claim/all"

// CheckinAll gateway.DailyCheckinExt：核心触发"所有上游签到"时调它。
//
// # 与 HTTP 全量端点共用同一段业务
//
// 顶部那个跨上游的「全部签到」走核心 → 本方法；分组头的按钮走
// HTTP → handleWelfareClaimAll。两者都调 checkinAllOnce，
// 否则"点分组按钮和点顶部按钮结果不同"这种差异没有任何测试会发现。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	list, err := p.localAccounts()
	if err != nil {
		return gateway.DailyCheckinReport{Error: "列账号失败: " + err.Error()}
	}
	return gateway.DailyCheckinReport{Results: p.checkinAllOnce(ctx, list)}
}

// checkinAllOnce 对给定账号逐个领取福利并收集结果。
//
// # 状态映射（与其它上游刻意一致）
//
//	领到 ≥1 项 → ok
//	今日已领   → already
//	一项都没领 → skip
//	请求报错   → fail
//
// 判据取自 claimWelfareFor 已写进历史的**那条记录**（它是本包唯一的
// 判据来源），而不是在这里重新判断一遍 —— 重新判断就会与历史列漂移，
// 表现为"按钮说成功了、列里却写着未领到"。
func (p *Provider) checkinAllOnce(ctx context.Context, list []*Auth) []gateway.DailyCheckinResult {
	out := make([]gateway.DailyCheckinResult, 0, len(list))
	for _, a := range list {
		if a == nil {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		res := gateway.DailyCheckinResult{UID: a.UID}
		claimed, _, err := p.claimWelfareFor(a, "manual")
		switch {
		case err != nil:
			res.Status, res.Detail = checkinlog.StatusFail, truncate(err.Error(), 200)
		case claimed > 0:
			res.Status, res.Detail = checkinlog.StatusOK, "领到 "+itoa(claimed)+" 项"
		default:
			// 领到 0 项：可能是"今日已领"，也可能是"没资格"。
			// 本包无法区分（见 claimWelfareFor 的注释），如实记 skip。
			res.Status, res.Detail = checkinlog.StatusSkip, "无可领福利（或今日已领）"
		}
		out = append(out, res)
	}
	return out
}

// handleWelfareClaimAll POST /admin/welfare/claim/all —— 全量福利领取。
//
// 回执形状与**共享签到驱动**的 HandlerAll 一致（{ok, results:[…]}）：
// 前端已有一条分支认这个形状，沿用它能零改动接入既有渲染。
func (p *Provider) handleWelfareClaimAll(w http.ResponseWriter, r *http.Request) {
	list, err := p.localAccounts()
	if err != nil {
		writeError(w, http.StatusBadGateway, "列账号失败: "+err.Error())
		return
	}
	results := p.checkinAllOnce(r.Context(), list)
	okN := 0
	for _, x := range results {
		if x.Status == checkinlog.StatusOK {
			okN++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "claimed": okN, "results": results,
	})
}

// itoa 小整数的字符串化（避免为一行引入 strconv 依赖）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// 编译期断言：Provider 实现了跨上游全量签到的扩展点。
var _ gateway.DailyCheckinExt = (*Provider)(nil)
