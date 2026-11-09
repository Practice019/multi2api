// shortuid.go 日志里怎么表示一个 uid —— **全仓唯一实现**（T-403）。
//
// # 为什么放在 logbuf（而不是 gateway）
//
// 最初放在 gateway（所有上游都 import 它，看起来最自然），但**被架构测试当场抓住**：
//
//  1. `internal/dailycheckin` 有一条显式规则 `TestNoGatewayImport` ——
//     "driver.go 引用了 internal/gateway —— 本包必须与契约解耦"。
//     那个包只需要"把 uid 缩短"，**不该为了一个日志格式化函数而依赖契约层**。
//  2. 更严重：架构测试用"**是否 import gateway**"来自动发现上游 ⇒
//     给 dailycheckin 加上 gateway 之后它被判定成上游，于是
//     `TestUpstreamsDoNotDependOnCore` 连报三条
//     "上游 lobsterai/monkeycode/qoder 依赖了另一个上游 dailycheckin"。
//
// logbuf 是**日志传输层**（缓冲/轮转/落盘），也是架构测试的 `discoveryExemptPackages`
// 之一 —— 上游 import 它**不会**被算成"依赖核心业务包"，也不会被误判成上游。
// "一个 uid 在日志里怎么写"本来就属于这一层。
//
// # 为什么需要共享实现（改造前有 8 份复制品）
//
// 改造前 cline / dailycheckin / lobsterai / loomy / qoder / raccoon / trae / cmd-server
// 各有**一份**几乎逐字相同的私有 shortUID，全部取**前 8 位**。实测各上游 uid 形状：
//
//	loomy-260915174611208423        → 前8 = loomy-26   ← 44 个号全撞
//	monkeycode-002d3463-53d0-…      → 前8 = monkeyco   ← 全撞
//	zcode-79e98e60-5be9-…           → 前8 = zcode-79   ← 会撞
//	user_15083583411_7985           → 前8 = user_150   ← 会撞
//	3929003642848586                → 前8 = 39290036   ← 会撞（时间戳前缀）
//	usr-01M3MJDCGSD05W4Y83JQC71XCT  → 前8 = usr-01M3   ← 会撞（ULID）
//	0234c548-963c-4d43-…            → 前8 = 0234c548   ← UUID，一般不撞
//	103668                          → 短 ⇒ 原样（lobsterai）
//
// 用户报的就是这个后果：日志里两个不同的号显示成同一个 `uid=26091517`，
// 而日志的**全部意义**就是"让我知道该处理哪个号"。
//
// # 规律与规则
//
// **前缀往往是品种名/类型名，不承载区分信息**；区分位在去掉前缀之后的部分（或尾部）。
//
//  1. uid ≤ 12 字符 ⇒ 原样返回（短到不需要截）
//  2. 否则：**前 head 位 + "…" + 尾部 8 位**
//     · 有分隔符（'-' 或 '_'）⇒ head = 6（含品种名与 1~2 位标识，够认出是哪个上游）
//     · 无分隔符（纯数字/十六进制）⇒ head = 4（只为"看着像哪类 id"）
//     · head 会被夹到 `len-8`，保证不与尾部 8 位重叠
//
// ⚠ 尾部 8 位是**唯一区分位**；前缀只为"人眼认出是哪个上游"。
// ⚠ 按**字符**截断（rune），不按字节：uid 形态由上游决定，按字节切会把多字节字符劈成两半，
// 日志里出现 `\ufffd`（本项目在 handler.go 的 contentBlockMsg 上踩过同一个坑）。
package logbuf

import "strings"

// ShortUID 把 uid 压成"日志里够用且能区分"的长度。
//
// 例（真实形状）：
//
//	loomy-260915174611208423        → loomy-…11208423
//	3929003642848586                → 3929…42848586
//	usr-01M3MJDCGSD05W4Y83JQC71XCT  → usr-01…C71XCT（尾 8 位）
//	103668                          → 103668（≤12 原样）
func ShortUID(uid string) string {
	rs := []rune(uid)
	if len(rs) <= 12 {
		return uid
	}
	head := 4 // 无分隔符：纯数字 / 十六进制
	if strings.IndexAny(uid, "-_") >= 0 {
		head = 6 // 有分隔符：品种名（+ 少量标识）
	}
	if head > len(rs)-8 {
		head = len(rs) - 8 // 不许与尾部 8 位重叠
	}
	if head < 1 {
		head = 1
	}
	return string(rs[:head]) + "…" + string(rs[len(rs)-8:])
}
