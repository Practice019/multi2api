// clientip_test.go 客户端 IP 的提取与跨层传递。
//
// # 为什么这两件事要一起测
//
// 它们的失败模式是同一个：**IP 丢失或串扰**，而两者都不报错。
//
//	提取错误 → 上游看到错乱的来源 IP（风控画像被污染）
//	串扰     → A 请求的 IP 被 B 请求覆盖（只在并发下出现，最难查）
//
// 串扰那条是本设计（ctx 传递而非共享字段）存在的唯一理由，
// 因此它有专门的并发用例。
package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func reqWith(h map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func TestExtractClientIP(t *testing.T) {
	cases := []struct {
		name string
		hdr  map[string]string
		want string
	}{
		{"无任何头", nil, ""},
		{"XFF 单段", map[string]string{"X-Forwarded-For": "203.0.113.7"}, "203.0.113.7"},
		{"XFF 多段只取首段", map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.1, 10.0.0.2"}, "203.0.113.7"},
		{"XFF 带空白", map[string]string{"X-Forwarded-For": "  203.0.113.7  , 10.0.0.1"}, "203.0.113.7"},
		{"XFF 优先于 X-Real-IP", map[string]string{"X-Forwarded-For": "1.1.1.1", "X-Real-IP": "2.2.2.2"}, "1.1.1.1"},
		{"回落 X-Real-IP", map[string]string{"X-Real-IP": "2.2.2.2"}, "2.2.2.2"},
		{"X-Real-IP 带空白", map[string]string{"X-Real-IP": "  2.2.2.2  "}, "2.2.2.2"},
		{"空 XFF 回落 X-Real-IP", map[string]string{"X-Forwarded-For": "", "X-Real-IP": "3.3.3.3"}, "3.3.3.3"},
		// IPv6 字面量带端口/方括号时不做特殊处理（上游中间件各自解析），
		// 这里只钉住"不被截断或改写"。
		{"IPv6", map[string]string{"X-Forwarded-For": "2001:db8::1"}, "2001:db8::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractClientIP(reqWith(c.hdr)); got != c.want {
				t.Errorf("ExtractClientIP=%q，期望 %q", got, c.want)
			}
		})
	}
}

// TestExtractClientIPNilSafe nil 请求不得 panic。
func TestExtractClientIPNilSafe(t *testing.T) {
	if got := ExtractClientIP(nil); got != "" {
		t.Errorf("nil 请求应返回空串，得到 %q", got)
	}
}

// TestClientIPContextRoundTrip 放进 ctx 能原样取回。
func TestClientIPContextRoundTrip(t *testing.T) {
	ctx := WithClientIP(context.Background(), "203.0.113.7")
	if got := ClientIPFrom(ctx); got != "203.0.113.7" {
		t.Errorf("往返后=%q", got)
	}
}

// TestWithClientIPEmptyIsNoop 空 IP 不塞键：下游能区分"没传"与"传了空"。
func TestWithClientIPEmptyIsNoop(t *testing.T) {
	base := context.Background()
	if got := WithClientIP(base, ""); got != base {
		t.Error("空 IP 应原样返回同一个 ctx（不塞空值键）")
	}
	if got := ClientIPFrom(base); got != "" {
		t.Errorf("未放入时应返回空串，得到 %q", got)
	}
}

// TestClientIPNilContextSafe nil ctx 不得 panic。
//
// Provider 实现可能收到 nil ctx（测试直接构造调用）。一次 panic
// 会带崩整条出站路径 —— 而本函数的全部职责只是读一个字符串。
func TestClientIPNilContextSafe(t *testing.T) {
	if got := ClientIPFrom(nil); got != "" {
		t.Errorf("nil ctx 应返回空串，得到 %q", got)
	}
	// WithClientIP 对 nil ctx 也应安全（返回 nil，不 panic）。
	var nilCtx context.Context
	if got := WithClientIP(nilCtx, "1.2.3.4"); got != nil {
		t.Error("nil ctx 应原样返回 nil")
	}
}

// TestWithClientIPNilValueNotNilContext 传入 nil ctx 且 IP 非空时不得 panic。
func TestWithClientIPNilValueNotNilContext(t *testing.T) {
	var nilCtx context.Context
	got := WithClientIP(nilCtx, "1.2.3.4") // context.WithValue(nil, ...) 会 panic
	if got != nil {
		t.Errorf("应返回 nil，得到 %v", got)
	}
}

// TestClientIPNoCrossRequestBleed 并发下不同请求的 IP 不串扰。
//
// # 这是本设计存在的唯一理由
//
// 改造前的候选做法是把 clientIP 存在 Client 的共享字段上
// （`c.clientIP = ip` 然后出站时读）。那样在并发下：
//
//	A 请求写 → B 请求写 → A 请求读，拿到 B 的 IP
//
// 表现是上游看到一批来源错乱的请求，且**只在并发下出现**。
// 本用例用 200 个并发 goroutine 反复放入/取出，断言每个都拿回自己的值。
// 反向判别力：把 ctx 换成共享字段后本用例必然红（且 race detector 也会报）。
func TestClientIPNoCrossRequestBleed(t *testing.T) {
	const n = 200
	var wg sync.WaitGroup
	errs := make(chan string, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := "10.0.0." + itoa(i)
			ctx := WithClientIP(context.Background(), ip)
			// 中间穿插别的 goroutine 的写（靠调度自然发生），
			// 再取回自己的值。
			for j := 0; j < 50; j++ {
				if got := ClientIPFrom(ctx); got != ip {
					errs <- got + " != " + ip
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("并发串扰：%s", e)
	}
}

// itoa 避免为一个小工具引入 strconv 依赖到测试文件顶部。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
