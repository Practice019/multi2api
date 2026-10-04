// harden.go 出口层的**健壮性**中间件。
//
// # 这个文件解决什么问题
//
// 一次稳定性排查里确认了三件事，本文件处理前两件（第三件已在别处）：
//
//	① 任何 handler 里的 panic 会让**那个连接**被 net/http 掐断，
//	   客户端拿到一个"连接被重置"，没有任何错误信息，网关日志里也
//	   只有 net/http 打的一行 stack —— 用户看到的是"网关挂了"。
//	② http.Server 只设了 ReadHeaderTimeout，缺 IdleTimeout 与
//	   MaxHeaderBytes（WriteTimeout 刻意不设，见下）。
//	③ 单账号并发闸门（Pool.MaxInFlight）已存在且正确，不需要动。
//
// # 为什么**不**设 http.Server.WriteTimeout
//
// 这是本文件最容易做错的地方，必须写清楚。
//
// WriteTimeout 的语义是"从**读请求头结束**到**写完响应**的总时限"。
// 本项目的主力接口是 SSE（对话流式），而实测：
//
//	流式搜索      35.4s，569 帧
//	流式生图      25-30s（工具执行期间只有保活注释帧）
//	工具循环最长   3 轮 × 5 分钟 = 15 分钟（每轮一次生图）
//
// 设一个"看起来合理"的 WriteTimeout（比如 60s）会**直接掐断**上面
// 每一类正常请求 —— 而且是那种"小请求都好好的、长请求偶发中断"的
// 故障形态，极难归因到超时设置上。
//
// 所以本文件的策略是：
//
//	http.Server 只设那些**不会伤到长连接**的项（IdleTimeout / MaxHeaderBytes）
//	逐请求的超时由 handler 自己的 ctx 管（工具循环已接请求 ctx，见
//	chattool_loop.go）—— 那种超时是"取消上游请求"，不是"掐客户端连接"，
//	会返回一个正经的错误而不是静默断连。
package server

import (
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

// panicRecovery 兜住 handler 里的 panic，把它变成一个可诊断的 500。
//
// # 为什么要显式写这个（net/http 其实会 recover）
//
// net/http 的 Server 确实会 recover，但它做的事只有两件：
//
//	关掉那个连接（客户端看到"连接被重置"，不是 500）
//	往 stderr 打一段 stack
//
// 对本项目这不够：
//
//	客户端拿不到状态码也拿不到错误原因 —— 它会以为"网关崩了"或
//	  "网络断了"，进而重试；而重试会再触发一次同样的 panic。
//	panic 发生在**已经开始写响应之后**时，客户端拿到的是半截 SSE
//	  （没有任何终止帧），会一直挂着等到超时。
//
// 所以这里显式 recover：
//
//	未写过响应 → 回一个 500 + OpenAI 形状的错误（客户端立刻明白）
//	已写过响应 → 补一个 SSE 错误帧 + [DONE]（否则客户端挂到超时）
//
// 并且把 stack 打进日志 —— panic 是**编程错误**，必须能定位到行。
func (h *Handler) panicRecovery(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// wroteHeader 跟踪"有没有写过"。包一层 ResponseWriter 记这个事实
		// （标准库没有查询这个的方法）。
		pw := &panicSafeWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				if pw.wrote {
					// 已经写过内容：状态码与已发字节都收不回来，
					// 只能补错误帧 + [DONE]，让客户端能正常收尾。
					// 尽力而为：写失败也没别的办法了。
					_, _ = pw.Write([]byte("data: " +
						`{"error":{"message":"internal gateway panic","type":"gateway_error"}}` +
						"\n\ndata: [DONE]\n\n"))
					if fl, ok := w.(http.Flusher); ok {
						fl.Flush()
					}
					return
				}
				writeOpenAIError(w, http.StatusInternalServerError,
					"gateway_panic", "网关内部错误，请把日志里的 PANIC 行反馈给维护者")
			}
		}()
		next(pw, r)
	}
}

// panicSafeWriter 记录"是否已写过响应头/内容"。
//
// 只包一层 WriteHeader/Write 的记账，不改任何行为 —— 中间件不该
// 改变响应语义（那正是"加了中间件之后行为变了"这类 bug 的来源）。
type panicSafeWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *panicSafeWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *panicSafeWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Flush 透传 Flush（SSE 依赖它）。
//
// # 为什么必须显式实现
//
// 包一层 ResponseWriter 会**丢掉**原类型的可选接口（http.Flusher）。
// 所有 `w.(http.Flusher)` 断言都会失败 → SSE 不再逐帧 flush →
// 客户端看到的是"内容攒到最后一次性到达"，也就是**流式失效**。
//
// 这正是本项目刚修过的那类回归（"文字不是流式的了"），所以这里
// 显式把 Flush 接出来，并加了测试钉住。
func (w *panicSafeWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// ServerTimeouts 返回"不会伤到长连接"的 http.Server 超时设置。
//
// 刻意**不含 WriteTimeout** —— 理由见文件头（它会掐断正常的长 SSE）。
//
// 返回的项：
//
//	IdleTimeout      keep-alive 空闲连接回收。不设的话每个空闲连接
//	                 会一直占着 fd 与 goroutine，慢速客户端攒多了会耗尽。
//	MaxHeaderBytes   请求头上限（默认 1 MB 也够，但显式写出来让
//	                 "我们考虑过这件事"可见）。
//
// ReadHeaderTimeout 由调用方设（已有 30s，与本函数无关）。
func ServerTimeouts() (idle time.Duration, maxHeader int) {
	return 120 * time.Second, 1 << 20
}
