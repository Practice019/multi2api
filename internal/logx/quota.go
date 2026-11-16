// quota.go 额度查询 / 刷新失败的**统一日志规格**。
//
// # 它解决什么（用户 2026-11-16 的实测日志）
//
// 「额度刷新失败」在 6 个包里各写各的，措辞几乎一样但都带一截说明性尾巴：
//
//	qoder:      额度刷新失败 uid=…: 上游未给出可用余额（显示为未知）
//	lobsterai:  额度刷新失败 uid=103668: lobsterai: 未登录（显示为未知）
//	raccoon:    额度刷新失败 uid=7555098: raccoon: authorization_verify_error: …（显示为未知）
//	codearts:   额度刷新失败 uid=…: …（显示为未知，不填 0）
//
// 三个毛病：
//
//  1. **`（显示为未知）` 是解释，不是数据** —— 它说明"界面上会显示未知"，
//     但界面上本来就看到了，日志里重复一遍是噪音
//  2. **错误串里又嵌了一遍上游名**（`lobsterai: lobsterai: …`）—— scope 已经写了
//  3. **中文整句** ⇒ 机器聚合不了
//
// # 本文件的规格
//
//	WARN qoder  quota.fail uid=01a045… reason=no_balance
//	WARN raccoon quota.fail uid=7555098… reason=unauthorized err="authorization verify failed"
//
// `reason=` 是**短码**（聚合键），`err=` 只在 guilty 的原因码下出现。
package logx

import (
	"strings"

	"workbuddy2api/internal/logbuf"
)

// 额度失败的原因短码。
const (
	// QuotaReasonUnauthorized 上游拒绝（401 / 403 / verify failed）。
	QuotaReasonUnauthorized = "unauthorized"
	// QuotaReasonNotLoggedIn 上游说"未登录 / 请先登录"（凭据失效的一种说法）。
	QuotaReasonNotLoggedIn = "not_logged_in"
	// QuotaReasonNoBalance 上游成功应答但**没有可用余额字段**（形状异常，不是失败）。
	QuotaReasonNoBalance = "no_balance"
	// QuotaReasonNetwork 网络 / 超时 / EOF。
	QuotaReasonNetwork = "network"
	// QuotaReasonOther 其余。
	QuotaReasonOther = "other"
)

// ClassifyQuotaReason 把额度失败原因归成短码。
func ClassifyQuotaReason(errText string) string {
	s := strings.ToLower(errText)
	switch {
	case s == "":
		return QuotaReasonOther
	case strings.Contains(s, "未登录") || strings.Contains(s, "请先登录") ||
		strings.Contains(s, "not logged in") || strings.Contains(s, "login"):
		return QuotaReasonNotLoggedIn
	case strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "unauthorized") || strings.Contains(s, "authorization") ||
		strings.Contains(s, "verify failed") || strings.Contains(s, "拒绝"):
		return QuotaReasonUnauthorized
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") ||
		strings.Contains(s, " eof") || strings.HasSuffix(s, "eof") ||
		strings.Contains(s, "connection reset") || strings.Contains(s, "no such host"):
		return QuotaReasonNetwork
	}
	return QuotaReasonOther
}

// QuotaFail 一次额度查询 / 刷新失败。
//
//	QuotaFail("qoder", uid, err)
//	→ WARN qoder quota.fail uid=01a045… reason=unauthorized err="401 Unauthorized"
//
// ⚠ 与 `RefreshFail` 同一条纪律：**自解释的原因不带 err**。
// `reason=unauthorized` 已经把话说完了；再附一长串上游原文只是噪音。
// 只有 `other` 才附（那时原文真的有用）。
func QuotaFail(scope, uid string, err error) {
	reason := QuotaReasonOther
	if err != nil {
		reason = ClassifyQuotaReason(err.Error())
	}
	kv := []any{"uid", uidShort(uid), "reason", reason}
	if reason == QuotaReasonOther {
		kv = append(kv, "err", ShortErr(err))
	}
	Event(LevelWarn, scope, "quota.fail", kv...)
}

// QuotaNoBalance 上游成功应答但**没有可用余额**（形状异常，不算上游故障）。
//
// 单独一个入口：它与"查询失败"不同 —— 上游没报错，只是我们没读到数。
// 混成一条会让运维去查一个不存在的故障。
func QuotaNoBalance(scope, uid string) {
	Event(LevelWarn, scope, "quota.fail", "uid", uidShort(uid), "reason", QuotaReasonNoBalance)
}

// uidShort 本包内转发 logbuf.ShortUID。
//
// ⚠ 不在这里重新实现：`logbuf.ShortUID` 是全仓唯一实现（T-403），
// 它按各上游 uid 的真实形状剥前缀 —— 手写"取前 8 位"会让 44 个 loomy 号全撞
// （`loomy-260915174611208423` 取前 8 位 = `loomy-26`，44 个号全一样）。
//
// 依赖方向 logx → logbuf 是单向的：logbuf 不 import logx，所以不成环。
func uidShort(uid string) string { return logbuf.ShortUID(uid) }
