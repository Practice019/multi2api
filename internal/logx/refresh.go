// refresh.go 后台续期的**统一日志规格**。
//
// # 它解决什么（用户 2026-11-16 的实测日志）
//
// 同一轮续期在 6 个包里各写各的，措辞全不一样：
//
//	minimax:     后台续期 成功 0 / 失败 5（跳过 0）
//	phanthywork: 后台续期 成功 0 / 失败 5（跳过 0）
//	cline:       后台续期完成 —— 续期 0，跳过 0，失败 1
//	raccoon:     后台续期完成 —— 续期 0，跳过 0，失败 1
//	workbuddy:   后台续期完成 —— 续期 0，跳过 0（其中寿命未过半 0），失败 1
//
// 而且**每个账号失败都单独打一行**，每行还带着层层嵌套的完整错误串
// （`phanthywork: 上游拒绝了续期凭据（可重试…）：phanthywork: … gateway: … （上游 400：{...}）`）
// —— 5 个账号就是 5 条各 200 字符的同义行，把真正的信号淹掉。
//
// # 本文件的规格
//
// 逐账号：**一行一条，短、结构化、可聚合**
//
//	15:04:05 WARN  phanthywork  refresh.rejected uid=cmwfxhqz… reason=rotated_elsewhere
//
// 一轮结束：**一行汇总**
//
//	15:04:05 INFO  phanthywork  refresh.summary refreshed=0 skipped=0 failed=5 elapsed_ms=10339
//
// # 为什么逐账号行不写完整错误
//
// 完整错误在排障时要，但它有三个更好的去处：
//   - 签到/续期历史（持久化，可查）
//   - 账号详情页
//   - DEBUG 级别
//
// 而 INFO 行里塞 200 字符的嵌套错误，收益是"极少数情况少查一次"，
// 代价是**每轮 5 条同义噪音**。所以这里只留一个 `reason=` 短码 +
// 限长的 `err=`（80 字符）。
package logx

import (
	"strings"

	"workbuddy2api/internal/logbuf"
)

// 续期失败的原因短码（`reason=` 的取值）。
//
// ⚠ 这些码是**聚合键**：同一种原因的多次失败应当能被数出来。
// 所以宁可粗一点（几种归一类），也不要把自由文本塞进 reason。
const (
	// RefreshReasonRotated 被另一个端轮换掉了（最常见，且可重试）。
	RefreshReasonRotated = "rotated_elsewhere"
	// RefreshReasonExpired refresh_token 真的过期了（要重登）。
	RefreshReasonExpired = "expired"
	// RefreshReasonInvalid 上游明确判无效（invalid_grant 之类）。
	RefreshReasonInvalid = "invalid"
	// RefreshReasonNetwork 网络 / 超时。
	RefreshReasonNetwork = "network"
	// RefreshReasonUpstream 上游 5xx / 未知拒因。
	RefreshReasonUpstream = "upstream"
	// RefreshReasonOther 其余（调用方自己判不了的）。
	RefreshReasonOther = "other"
)

// ClassifyRefreshReason 把失败原因归成短码。
//
// 判据用**子串**而不是错误类型：跨 6 个包、每个包自己的错误包装都不一样，
// 而"上游说了什么"在错误文本里是一致的（invalid_grant / expired / timeout）。
func ClassifyRefreshReason(errText string) string {
	s := strings.ToLower(errText)
	switch {
	case s == "":
		return RefreshReasonOther
	case strings.Contains(s, "rotated") || strings.Contains(s, "轮换"):
		return RefreshReasonRotated
	case strings.Contains(s, "invalid_grant") || strings.Contains(s, "invalid or expired"):
		return RefreshReasonInvalid
	case strings.Contains(s, "expired") || strings.Contains(s, "过期"):
		return RefreshReasonExpired
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") ||
		strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") ||
		strings.Contains(s, "network") || strings.Contains(s, " eof") ||
		strings.HasSuffix(s, "eof") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") || strings.Contains(s, "tls handshake"):
		// ⚠ `EOF` 必须算网络类：实测 `Post "https://…": EOF` 被归成 other，
		//   于是日志写 reason=other，运维完全看不出是网络断了还是凭据坏了。
		return RefreshReasonNetwork
	case strings.Contains(s, "http 5") || strings.Contains(s, " 502") ||
		strings.Contains(s, " 503") || strings.Contains(s, " 504"):
		return RefreshReasonUpstream
	}
	return RefreshReasonOther
}

// RefreshFail 一轮续期里**单个账号**失败。
//
// 事件名由调用方给（`refresh.rejected` / `refresh.failed`），
// 因为它携带"可重试 vs 终态"的语义 —— 那正是运维要区分的东西。
//
// ⚠ err 会被限长到 80 字符：完整错误在续期历史里，不在 INFO 行里。
func RefreshFail(scope, event, uid string, err error) {
	reason := RefreshReasonOther
	if err != nil {
		reason = ClassifyRefreshReason(err.Error())
	}
	kv := []any{"uid", logbuf.ShortUID(uid), "reason", reason}
	// ⚠ 自解释的原因**不带 err**：`reason=rotated_elsewhere` 已经把话说完了，
	//   再附一长串中文嵌套错误只是噪音（用户实测报的就是这个）。
	//   只有归不进已知类的（other / upstream）才附原文 —— 那时它真的有用。
	if reason == RefreshReasonOther || reason == RefreshReasonUpstream {
		kv = append(kv, "err", ShortErr(err))
	}
	// ★ 用户要求「多账号 log 不要刷屏，一行总结即可」⇒ 按 (scope,event,reason) 折叠。
	//   实测 phanthywork 5 个号全 rotated_elsewhere：改造前 5 行，现在 1 行 `（×5）`。
	Coalesce(LevelWarn, scope, event+":"+scope+":"+reason, "", "%s", eventLine(event, kv))
}

// RefreshSummary 一轮续期的**汇总行**。
//
// 只在真有动作时打（refreshed>0 || failed>0）—— 空闲部署每 10 分钟一行
// "什么都没做"会淹没真正的信号。这条判据由调用方决定，不由本函数猜。
//
// 级别：有失败 WARN，否则 INFO。
func RefreshSummary(scope string, refreshed, skipped, failed int, extra ...any) {
	lvl := LevelInfo
	if failed > 0 {
		lvl = LevelWarn
	}
	// ⚠ 与 RefreshSummaryLine 共用同一份正文生成 —— 两条路径的形态不会漂移。
	write(Line(lvl, scope, RefreshSummaryLine(scope, refreshed, skipped, failed, extra...)))
}

// RefreshSummaryLine 与 RefreshSummary 内容相同，但**不写日志**，只返回正文。
//
// 用途：有些调用方持有一个 `summary() string` 的函数（如 workbuddy 的
// keepaliveSummary），由上层决定打不打（manual 触发时不打）。
// 那种地方需要"生成文本"而不是"直接打"，但又必须与其它包的形态完全一致。
//
// ⚠ 形态必须与 RefreshSummary 一模一样 —— 两者共用这个函数生成正文，
// 所以不可能漂移。
func RefreshSummaryLine(scope string, refreshed, skipped, failed int, extra ...any) string {
	kv := make([]any, 0, 8+len(extra))
	kv = append(kv, "refreshed", refreshed, "skipped", skipped, "failed", failed)
	kv = append(kv, extra...)
	return eventLine("refresh.summary", kv)
}

// RefreshExpired 终态：refresh_token 已失效，必须重登。
//
// ★ 走 Coalesce 而不是直接 Event：多账号同时失效时**折成一行**。
//
//	实测 raccoon 2 个号、qoder 1 个号都打到这一档 —— 每个号一行就是刷屏。
//
// ⚠ 与 `RefreshFail` 分开成两个入口：这一档是**终态**（要人动手重登），
//
//	`RefreshFail` 是可重试的。合成一条会让运维分不清"要不要管"。
func RefreshExpired(scope, uid string) {
	Coalesce(LevelWarn, scope, "refresh.expired:"+scope, "",
		"%s", eventLine("refresh.expired",
			[]any{"uid", logbuf.ShortUID(uid), "reason", RefreshReasonInvalid}))
}
