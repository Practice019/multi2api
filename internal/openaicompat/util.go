// util.go 本包自用的小助手。
//
// # 为什么这里又写了一份 truncate（而不是复用 internal/upstream 的那份）
//
// `internal/upstream/client.go` 里有一模一样的函数，但它是**未导出**的：
// 本包若要用它，就得把 `internal/upstream` 的 API 敞开（导出它）。
// 为了省 5 行代码去加宽一个共享库的公开面，代价比收益大 —— 所以这里各留一份。
//
// （本包**是**上游实现，可以 import `internal/upstream` 这个共享库 ——
//
//	`internal/workbuddy` 就是这么做的；但"能 import 包"不等于"能用它的私有函数"。）
package openaicompat

// truncate 把上游响应片段截断到 n 字节，超长时补省略号。
//
// 用途：错误信息里只带上游响应的一小段。整页 HTML 塞进错误里会让日志与前端提示都不可读，
// 而这一段通常正是"上游到底在抱怨什么"的关键。
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// 不切坏 UTF-8：向前回退到合法边界（多字节字符被砍成半个会变成乱码）。
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
