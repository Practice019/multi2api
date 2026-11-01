package server

import (
	"fmt"
	"strings"
)

// appendAttemptTrace 把换号重试的观测拼进 decision 串。
//
// # 形状
//
//	<原 decision> attempts=3 spentMs=118000 last=loomy server 408
//
// 原 decision 为空时不留前导空格（日志列不能出现莫名的前导空白）。
//
// # 为什么不塞进 logbuf.Entry 的新字段
//
// Entry 是**跨包契约**（面板按列读、有 SQL 镜像），加字段要动 schema 与
// 存储侧；而这三个数字是排障用的，塞进既有的 decision 串即可被 grep 到、
// 又不会让历史日志与新日志列数不一致。
func appendAttemptTrace(decision string, attempts int, spentMs int64, lastFail string) string {
	if attempts <= 0 {
		return decision
	}
	var b strings.Builder
	if decision != "" {
		b.WriteString(decision)
		b.WriteString(" ")
	}
	fmt.Fprintf(&b, "attempts=%d spentMs=%d", attempts, spentMs)
	if lf := strings.TrimSpace(lastFail); lf != "" {
		fmt.Fprintf(&b, " last=%s", lf)
	}
	return b.String()
}
