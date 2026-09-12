package scheduler

import (
	"strings"
	"unicode/utf8"
)

// triggerSchedule 触发来源标识（写入历史，用于区分定时任务与人工点击）。
//
// 核心自己需要它：任何槽位的历史里都要写 trigger。具体取值由装配处与
// 上游复用 —— 它们不是上游业务，而是"这次是谁触发的"这一通用事实。
const (
	triggerSchedule = "schedule"
	triggerManual   = "manual"
)

// shortErr 把错误压成一行短文本，避免历史文件被长堆栈撑爆。
//
// 截断按**字符边界**退让，不直接切字节：上游的额度告警往往是中文（3 字节/字符），
// 按字节切 120 极易落在字符中间，产生非法 UTF-8 —— 实测 6 组样本里 3 组中招，
// 序列化进 JSON 后变成 \ufffd，用户看到的就是乱码尾巴。
//
// 核心用它压缩**任务执行错误**（job.go 的 lastErr）。上游有自己的同名实现
// （internal/workbuddy/history.go）—— 那是有意的重复：上游不得 import 核心包，
// 而两边都只有十几行、语义完全一致，重复的代价低于引入第三个公共依赖。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	// 换行与回车都压平：只替 \n 会留下裸 \r（"a\r\nb" → "a\r b"），
	// 而 \r 进落盘文件会让行式解析器/编辑器把一行当两行。
	s := strings.ReplaceAll(err.Error(), "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	const max = 120
	if len(s) <= max {
		return s
	}
	// 从 max 往前退，直到前缀是合法 UTF-8。最多退 3 字节（UTF-8 单字符上限）。
	n := max
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}
