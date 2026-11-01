package server

import (
	"fmt"
	"strings"

	"workbuddy2api/internal/gateway"
)

// failBrief 把"这一次为什么失败"压成一句话，供请求日志的 detail/decision 列使用。
//
// # 为什么需要（用户实测痛点）
//
// 旧日志里失败请求只有状态码与一句通用文案：
//
//	status=503  provider=loomy  decision=""
//	all accounts unavailable (cooling/disabled) —— 104 个账号：0 禁用、0 冷却
//	  : upstream client (http 408) 响应超时，请重试
//
// 于是"是上游慢、还是池子空、还是账号被限"必须翻容器日志才能分辨。
// 这个函数把三者一次说清：上游名 + 错误类别 + 状态码。
//
// ⚠ 不进客户端文案（那里只放脱敏后的一句话），只进**日志**——日志是排障证据，
// 客户端看到的是契约层错误。
func failBrief(provider string, kind gateway.ErrorKind, err error) string {
	var b strings.Builder
	if p := strings.TrimSpace(provider); p != "" {
		b.WriteString(p)
		b.WriteString(" ")
	}
	// ErrorKind 自带 String()（"server" / "auth" / "client"…），与 upstream 侧拼写逐字一致。
	//
	// ⚠ 排除 "none" 与 "unknown"：ErrKindNone.String() 返回 "none"，
	//   而 None 的语义是"无法归类"—— 把它当类别写进日志等于伪造一个分类，
	//   排障时会让人以为上游报了个叫 none 的错。
	if k := kind.String(); k != "" && k != "unknown" && k != "none" {
		b.WriteString(k)
	}
	// 状态码：从错误里挖（*chatError / UpstreamHTTPError 都带 Status）。
	if st := statusOfErr(err); st > 0 {
		fmt.Fprintf(&b, " %d", st)
	}
	if b.Len() == 0 {
		// 什么都没挖到时给一个**明确**的占位，而不是空串：
		// 空串在日志里与"没记录"不可区分，而这两件事排障时意义完全不同。
		return "unknown_failure"
	}
	return strings.TrimSpace(b.String())
}

// statusOfErr 从错误里取 HTTP 状态码（取不到返回 0）。
//
// 与 classifyErr 的分工：那个回答"属于哪一类"，这个只取"上游报了几号"，
// 且**不**做任何分类决策 —— 纯粹给日志用。
func statusOfErr(err error) int {
	if err == nil {
		return 0
	}
	if ue, ok := gateway.AsUpstreamHTTPError(err); ok {
		return ue.Status
	}
	// 本包内部的 chatError 也带 Status。
	if ce, ok := err.(*chatError); ok {
		return ce.Status
	}
	return 0
}
