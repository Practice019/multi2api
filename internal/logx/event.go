// event.go 结构化事件日志：**工程化的行格式**
// （用户 2026-11-16：「打印的文本要符合工程化的那种语言，现在特别乱，
//
//	不像成熟项目 / 企业级项目的语言」）。
//
// # 现有行格式的问题
//
// 时间/级别/作用域三段其实已经规范了（见 Line）。乱的是**消息体**：
//
//	travel u_123: depart ok location=4
//	travel u_123: claim ok record=5 reward=300
//	workbuddy: 额度刷新未成功 uid=u_9 status=unknown（显示为未知，不填 0）
//	统一签到: 成功 42 · 失败 3 · 跳过 5（已禁用×5）
//
// 四种毛病：
//
//  1. **事件名与参数糊在一起** —— `depart ok location=4` 里哪是事件哪是字段？
//  2. **字段命名不统一** —— 一处 `uid=`、一处 `record=`、一处 `status=`，
//     但同一个概念在不同文件里叫法不同（uid / account / user）
//  3. **中英混排 + 整句中文** —— `活动 `xx` 成功` 这种没法机器解析，
//     而"企业级"的标志就是**日志能被检索与聚合**（grep / jq / Loki）
//  4. **括号里塞说明** —— `（显示为未知，不填 0）` 是文档，不是日志
//
// # 本文件的格式（对标 Go 生态的 slog/zap 与 Caddy/Traefik 的行）
//
//	15:04:05 INFO  travel    travel.depart.ok uid=u_123 location=4
//	15:04:05 WARN  workbuddy  quota.refresh.fail uid=u_9 status=unknown
//
// 三段结构（时间/级别/作用域由 Line 负责，本文件负责后半段）：
//
//	<event>  <k=v> <k=v> ...
//
// 规则（每条都能被测试钉住）：
//
//   - **event** 点分小写：`<组件>.<动作>[.<结果>]`，如 `travel.claim.ok`
//   - **字段名** 蛇形小写：`uid` `record_id` `reward` `elapsed_ms`
//   - **字段值** 只在含空格/引号/等号时加双引号：`err="dial tcp: timeout"`
//   - **字段顺序由调用方给定** —— 调用方知道什么顺序读起来最顺，
//     统一排序反而把"主键在前"这个信息丢掉。同一事件必须**始终同一顺序**。
//   - **正文不用 CJK**：中文只出现在 `msg="..."` 这种明确的人话字段里，
//     且只在真的需要解释时才用。
package logx

import (
	"fmt"
	"strings"
	"time"
)

// Event 打一条结构化事件。
//
//	Event(LevelInfo, "travel", "travel.depart.ok", "uid", uid, "location", 4)
//	→ 15:04:05 INFO  travel   travel.depart.ok uid=u_123 location=4
//
// kv 是交替的 key/value 对，**key 必须是字符串**；奇数个参数会被忽略末尾那个
// （并在行末标 `malformed=true` —— 让写错的地方自己暴露，而不是静默吞掉）。
func Event(l Level, scope, event string, kv ...any) {
	if !Enabled(l) {
		return
	}
	write(Line(l, scope, eventLine(event, kv)))
}

// eventLine 把 event + kv 拼成 `<event> k=v ...`（不含前三个固定段）。
func eventLine(event string, kv []any) string {
	var b strings.Builder
	ev := strings.TrimSpace(event)
	if ev == "" {
		ev = "event"
	}
	b.WriteString(ev)
	malformed := len(kv)%2 != 0
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			// key 不是字符串：这是调用点的 bug，但要能看出来。
			k = fmt.Sprint(kv[i])
			malformed = true
		}
		k = strings.TrimSpace(k)
		if k == "" {
			malformed = true
			continue
		}
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(fmtValue(kv[i+1]))
	}
	if malformed {
		b.WriteString(" malformed=true")
	}
	return b.String()
}

// fmtValue 字段值的书写规则。
//
//	空值        → `-`（一个明确的"没有"，而不是 `key=` 看着像截断）
//	需要引号    → 双引号包裹，内部的双引号与反斜杠转义
//	其余        → 原样
//
// ⚠ 只对**需要**的值加引号：给所有值都加会让行变长且难读，
//
//	而"哪些值需要"是可判定的（含分隔符就必须加）。
func fmtValue(v any) string {
	if v == nil {
		return "-"
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case time.Duration:
		// 时长统一成带单位的毫秒：`38.2s` → `elapsed_ms=38200`
		// 不直接写 `38.2s`，因为字段名已经说明了单位，值里再带一次是重复。
		return fmt.Sprintf("%.0f", float64(x)/float64(time.Millisecond))
	case error:
		if x == nil {
			return "-"
		}
		s = x.Error()
	default:
		s = fmt.Sprint(x)
	}
	if s == "" {
		return "-"
	}
	if strings.ContainsAny(s, " \t\n\r\"=") {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		s = strings.ReplaceAll(s, "\n", `\n`)
		s = strings.ReplaceAll(s, "\r", `\r`)
		s = strings.ReplaceAll(s, "\t", `\t`)
		return `"` + s + `"`
	}
	return s
}

// Eventf 事件名 + 一个说明性的 msg（**唯一**允许 CJK 的位置）。
//
// 用途：错误原因本身就是中文时，把它放进 `msg="..."`，让行仍然可解析。
func Eventf(l Level, scope, event, msg string, kv ...any) {
	if !Enabled(l) {
		return
	}
	all := make([]any, 0, len(kv)+2)
	if strings.TrimSpace(msg) != "" {
		all = append(all, "msg", msg)
	}
	all = append(all, kv...)
	write(Line(l, scope, eventLine(event, all)))
}

// ShortUID 账号标识的缩写 —— **转发** logbuf.ShortUID，不在这里另写一份。
//
// # 为什么必须是转发（这条差点被我违反）
//
// 本仓的 `logbuf.ShortUID` 是 T-403 收敛出来的**全仓唯一实现**（改造前散着 8 份），
// 而且它比"取前 8 位"聪明得多：按实测的各上游 uid 形状剥前缀再取区分位 ——
// 因为 `loomy-260915174611208423` 取前 8 位会得到 `loomy-26`，44 个号全撞。
//
// 我第一版在这里手写了"取前 8 位"，被 `TestShortUIDHasExactlyOneImplementation`
// 当场抓住（那份门禁存在的理由就是不让复制品回流）。
//
// ⇒ 这里只做转发。要改缩写规则，改 logbuf 那一份。
// （本包不再提供 ShortUID：请直接用 logbuf.ShortUID —— 全仓唯一实现。）

// ShortErr 错误的**单行短形态**（去换行、限长）。
//
// 为什么限长：上游错误页有时是一整页 HTML，直接进日志就是一次刷屏。
// 截断必须**可见**（结尾加 … ），否则会让人以为错误原文就这样。
func ShortErr(err error) string {
	if err == nil {
		return ""
	}
	return ShortString(err.Error(), 80)
}

// ShortString 去换行 + 截断到 n 个字符（n<=0 用 80）。
func ShortString(s string, n int) string {
	if n <= 0 {
		n = 80
	}
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
