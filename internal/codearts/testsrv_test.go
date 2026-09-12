package codearts

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer 起一个假上游，把收到的请求路径与请求头回传给 onRequest。
//
// 回调带路径是必要的：ChatStream 一轮会发 3 个请求
// （heartbeat busy → chat → heartbeat idle），
// 只按"最后一次"取头会拿到 idle 心跳而不是 chat。
//
// 响应固定为一段最小的 OpenAI 形态 SSE，足以让调用方正常收尾。
func newTestServer(t *testing.T, onRequest func(path string, headers map[string]string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onRequest != nil {
			onRequest(r.URL.Path, collectHeaders(r))
		}
		// 心跳端点单独处理：它不发 chat 响应
		if r.URL.Path == HeartbeatPath {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w,
			`data:{"id":"t","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n"+
				`data:[DONE]`+"\n\n")
	}))
}

// collectHeaders 把请求头收集成 map。
//
// key 统一转小写：Go 的 http.Header 会把首字母大写（`Maas_type`），
// 而调用方习惯写小写（`maas_type`）—— 不统一会出现
// "头明明发了但断言取不到值" 的假失败。
func collectHeaders(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, vs := range r.Header {
		if len(vs) > 0 {
			out[strings.ToLower(k)] = vs[0]
		}
	}
	// httptest 会把 Host 单独放，补进来便于断言
	out["host"] = r.Host
	return out
}
