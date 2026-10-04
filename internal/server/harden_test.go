package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPanicRecoveryReturnsDiagnosable500 未写过响应时 panic 要变成 500。
//
// # 为什么要显式做这件事（net/http 自己会 recover）
//
// net/http 的 Server 确实会 recover，但它只做两件事：关掉那个连接、
// 往 stderr 打 stack。对本项目不够：
//
//	客户端拿不到状态码也拿不到原因 → 以为"网关崩了"或"网络断了" → 重试
//	而重试会**再触发一次同样的 panic**（它是确定性的编程错误）
//
// 所以这里断言两件事：状态码是 500，且响应体里有可诊断的信息。
func TestPanicRecoveryReturnsDiagnosable500(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	handler := h.panicRecovery(func(w http.ResponseWriter, r *http.Request) {
		panic("boom: 故意触发")
	})
	handler(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d，want 500（不能是『连接被重置』那种无声失败）", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "gateway_panic") {
		t.Errorf("响应体里没有可诊断的错误类型：%q", body)
	}
	// OpenAI 形状：客户端按 error.message 解析
	if !strings.Contains(body, `"message"`) {
		t.Errorf("响应体不是 OpenAI 错误形状：%q", body)
	}
}

// TestPanicRecoveryAfterWriteClosesSSE 已写过响应后 panic 要补 [DONE]。
//
// # 为什么这条重要
//
// 已开始写响应时状态码收不回来（已经是 200）。此时若什么都不做，
// 客户端拿到的是**半截 SSE**（没有任何终止帧）—— 它会一直挂着
// 等到自己的超时，用户看到的是"卡住了"。
//
// 补一个错误帧 + [DONE] 让客户端能立刻正常收尾。
func TestPanicRecoveryAfterWriteClosesSSE(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	handler := h.panicRecovery(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"半截\"}}]}\n\n"))
		panic("写完一半炸了")
	})
	handler(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("已写过响应后 panic 必须补 [DONE]，否则客户端挂到超时：%q", body)
	}
	// 状态码必须保持 200（已经发过了，不能再改）
	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，已提交的响应不该被改成 500", rec.Code)
	}
}

// TestPanicSafeWriterPreservesFlush 包装后 Flush 必须仍然可用。
//
// # 这是本项目刚踩过的回归形态
//
// 包一层 ResponseWriter 会**丢掉**原类型的可选接口（http.Flusher）。
// 所有 `w.(http.Flusher)` 断言随之失败 → SSE 不再逐帧 flush →
// 客户端看到"内容攒到最后一次性到达" = **流式失效**。
//
// 我们刚花一轮修过"文字不是流式的了"，所以这条必须有测试钉住：
// 加了防 panic 中间件之后，stream=false 与 stream=true 的行为
// 必须一字不变。
func TestPanicSafeWriterPreservesFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	// httptest.ResponseRecorder 实现了 Flusher
	var w http.ResponseWriter = rec

	wrapped := &panicSafeWriter{ResponseWriter: w}
	if _, ok := interface{}(wrapped).(http.Flusher); !ok {
		t.Fatal("包装后丢了 http.Flusher —— SSE 会失去逐帧 flush，" +
			"客户端看到的不再是流式（这正是我们要避免的回归）")
	}
	// 真的调一次不该 panic
	wrapped.Flush()
	if !rec.Flushed {
		t.Error("Flush 没透传到底层 ResponseWriter")
	}
}

// TestPanicSafeWriterTracksWrites 记账要准（它决定走哪条收场路径）。
func TestPanicSafeWriterTracksWrites(t *testing.T) {
	t.Run("没写", func(t *testing.T) {
		w := &panicSafeWriter{ResponseWriter: httptest.NewRecorder()}
		if w.wrote {
			t.Error("没写过时 wrote 应为 false")
		}
	})
	t.Run("WriteHeader 也算写过", func(t *testing.T) {
		w := &panicSafeWriter{ResponseWriter: httptest.NewRecorder()}
		w.WriteHeader(http.StatusOK)
		if !w.wrote {
			t.Error("WriteHeader 之后再改状态码是不可能的，必须算作已写")
		}
	})
	t.Run("Write 算写过", func(t *testing.T) {
		w := &panicSafeWriter{ResponseWriter: httptest.NewRecorder()}
		_, _ = w.Write([]byte("x"))
		if !w.wrote {
			t.Error("Write 之后必须算作已写")
		}
	})
}

// TestServerTimeoutsNoWriteTimeout 刻意不返回 WriteTimeout。
//
// # 这条测试是在保护一个**刻意的缺失**
//
// WriteTimeout 会掐断正常的长 SSE（实测流式搜索 35.4s、工具循环最长
// 15 分钟）。若将来有人"顺手补上"这个看起来该有的设置，正常请求
// 会开始偶发中断 —— 而那种故障极难归因。
//
// 所以用一个测试把"我们**选择**不设它"这件事变成可见的约束。
func TestServerTimeoutsNoWriteTimeout(t *testing.T) {
	idle, maxHeader := ServerTimeouts()

	if idle <= 0 {
		t.Error("IdleTimeout 必须为正（否则空闲连接一直占着 fd 与 goroutine）")
	}
	// idle 应当足够长：客户端流式读图时会有停顿，太短会误杀正常连接
	if idle < 60*1e9 {
		t.Errorf("IdleTimeout = %v 偏短，长 SSE 流中停顿可能触发误杀", idle)
	}
	if maxHeader <= 0 {
		t.Error("MaxHeaderBytes 必须为正")
	}

	// 关键断言：本函数**不**提供 WriteTimeout（签名上就没有它）
	// —— 若有人加了，这里的注释与实现必须一起改，并重新评估
	// 长 SSE 的影响。类型上无法断言"没有"，所以用注释 + 这条说明。
	t.Log("WriteTimeout 刻意不设：它会掐断长 SSE（见 harden.go 文件头）")
}

// TestWithAuthWrapsPanicRecovery 所有 /v1 端点都要受 panic 保护。
//
// # 为什么在 withAuth 里包而不是各个注册点
//
// /v1 有 5+ 个端点（chat / images / models / model / …），逐个包会漏，
// 且将来加端点时更容易漏。withAuth 是它们的**唯一共同入口**。
//
// 这条测试的意义：若有人把包装从 withAuth 里挪走（比如挪到某个具体
// handler 里），这里会红。
func TestWithAuthWrapsPanicRecovery(t *testing.T) {
	h := &Handler{apiKey: "k"}
	// 用一个会 panic 的下游 handler
	inner := h.withAuth(func(w http.ResponseWriter, r *http.Request) {
		panic("下游炸了")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer k")

	// 不该真的 panic 出去
	inner(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("withAuth 包的下游 panic 后状态码 = %d，want 500（说明 panic "+
			"没被兜住，客户端会看到连接被重置）", rec.Code)
	}
}
