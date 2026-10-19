// logging_mode.go 请求日志「模式」列的取值口径（2026-10-20 独立出来以便单测）。
//
// # 为什么单独一个文件、一个函数
//
// 这个值以前是**写在调用处的字面量** "sync"，于是渠道转发的流式请求在日志里
// 全部被误报成非流式 —— 用户截图问「应该不是非流式吧」，就是它。
//
// 字面量无法被测试（只能靠端到端跑真上游），抽成纯函数后一条单测就能钉住语义：
// mode 记的是**客户端意图**（请求体 stream 字段），不是网关内部实际转发方式。
package server

// channelLogMode 把客户端意图（请求体的 stream 字段）映射成日志的 mode 值。
//
// 取值只有两种，与账号池路径（logging.go 里 `mode := "sync"; if ... = "stream"`）
// 保持完全一致 —— 两条路径写进同一个 JSONL、被同一个聚合函数读，
// 语义若分叉，统计出来的"流式占比"就没有意义。
func channelLogMode(clientWantsStream bool) string {
	if clientWantsStream {
		return "stream"
	}
	return "sync"
}
