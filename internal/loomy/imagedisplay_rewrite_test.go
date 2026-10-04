package loomy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newRewriteTestProvider 造一个 Provider，其探测客户端指向 stub（HEAD 行为可控）。
func newRewriteTestProvider(t *testing.T, headStatus int) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("探测应当用 HEAD（GET 会把整张图拉下来），实际 %s", r.Method)
		}
		// 带签名 → 模拟 COS：403（method 不在签名里）
		if strings.Contains(r.URL.RawQuery, "q-signature") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(headStatus)
	}))
	t.Cleanup(srv.Close)
	return &Provider{client: &Client{HTTP: &http.Client{Timeout: 3 * time.Second}, BaseURL: srv.URL}}
}

// urlWithSig 造一个"带 COS 签名"的 URL（host 用 stub 的）。
func urlWithSig(base string) string {
	return base + "/a.png?q-sign-algorithm=sha1&q-ak=x&q-sign-time=1;2" +
		"&q-key-time=1;2&q-header-list=host&q-url-param-list=&q-signature=deadbeef"
}

// TestRewriteImageURLsPreservesUnknownFields 改写**不得丢掉**其它字段。
//
// # 这是我第一版犯的错，而后果是计费数据丢失
//
// 第一版定义了一个只含 `Data []map[string]any` 的结构体，
// 反序列化 → 改 url → 重新 Marshal。结果：
//
//	上游原始:  {"created":…, "data":[…], "points_consumed":110}
//	我的输出:  {"data":[…]}
//
// `points_consumed` 是**计费依据** —— 丢掉它调用方再也算不出花了多少积分。
//
// 这是"局部反序列化再整体序列化"的经典数据损失：结构体只声明了你想动的
// 字段，Marshal 时就只剩它们。所以本测试用一个带**未知字段**的响应，
// 断言它们必须原样活下来。
func TestRewriteImageURLsPreservesUnknownFields(t *testing.T) {
	p := newRewriteTestProvider(t, http.StatusOK)
	// 注意 srv.URL 是 stub 地址，但 urlWithSig 会给它加签名 ——
	// displayImageURL 剥掉签名后 HEAD 会打到同一地址的裸路径 → 200。
	raw := []byte(`{
		"created": 1791141816,
		"points_consumed": 110,
		"some_future_field": {"nested": [1,2,3]},
		"data": [{"url": "` + urlWithSig(p.client.BaseURL) + `", "revised_prompt": "原始提示词"}]
	}`)

	out := p.rewriteImageURLs(context.Background(), raw)

	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}

	// ① 顶层未知字段必须还在
	if top["created"] == nil {
		t.Error("created 丢了 —— 局部反序列化再整体序列化的经典损失")
	}
	if top["points_consumed"] == nil {
		t.Error("points_consumed 丢了 —— 那是**计费依据**，不是可有可无的字段")
	}
	if top["some_future_field"] == nil {
		t.Error("未知字段丢了 —— 上游将来加字段也会被这样丢掉")
	}
	// ② data[].url 之外的字段也必须还在
	items, _ := top["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("data 条数 = %d", len(items))
	}
	item, _ := items[0].(map[string]any)
	if item["revised_prompt"] == nil {
		t.Error("data[] 里的其它字段丢了")
	}
	// ③ url 应当已被换成裸形式
	u, _ := item["url"].(string)
	if strings.Contains(u, "q-signature") {
		t.Errorf("url 里还有签名参数（HEAD 会 403，预览器取不到）：%s", u)
	}
	// ④ 原签名 URL 保留作兜底
	if item["url_signed"] == nil {
		t.Error("url_signed 丢了 —— bucket 若改私有读就没有兜底了")
	}
}

// TestRewriteImageURLsEndpointPathFixed 生图端点路径也必须拿到裸 URL。
//
// # 这条是用户报障的直接回归测试
//
// `GenerateImage` 是两条生图路径的**共同入口**：
//
//	对话工具路径  execGenerateImage → GenerateImage
//	生图端点路径  /v1/images/generations → ImageGen → GenerateImage
//
// 我第一版只在对话工具路径做了裸 URL 转换，**端点路径漏了** ——
// 实测 `/v1/images/generations` 返回的仍是签名 URL（HEAD 403）。
//
// 所以这条测试断言"改写发生在 GenerateImage 这一层"：
// 任何调用方都不需要自己处理 URL。
func TestRewriteImageURLsEndpointPathFixed(t *testing.T) {
	p := newRewriteTestProvider(t, http.StatusOK)
	raw := []byte(`{"created":1,"data":[{"url":"` + urlWithSig(p.client.BaseURL) + `"}]}`)

	out := p.rewriteImageURLs(context.Background(), raw)
	var top map[string]any
	_ = json.Unmarshal(out, &top)
	items, _ := top["data"].([]any)
	item, _ := items[0].(map[string]any)
	if u, _ := item["url"].(string); strings.Contains(u, "q-signature") {
		t.Error("GenerateImage 这一层没做 URL 改写 —— 端点路径会拿到 HEAD 403 的签名 URL")
	}
}

// TestRewriteImageURLsProbeFailureKeepsOriginal 探测失败时原样保留。
//
// 裸形式依赖"bucket 公开读"这个**部署事实**，不是我们能保证的。
// 探测失败（403/网络抖动）时必须退回签名 URL（GET 仍可用），
// 而不是给出一个取不到的裸链接。
func TestRewriteImageURLsProbeFailureKeepsOriginal(t *testing.T) {
	// stub 一律 403（模拟 bucket 改成私有读）
	p := newRewriteTestProvider(t, http.StatusForbidden)
	orig := urlWithSig(p.client.BaseURL)
	raw := []byte(`{"data":[{"url":"` + orig + `"}]}`)

	out := p.rewriteImageURLs(context.Background(), raw)

	var top map[string]any
	_ = json.Unmarshal(out, &top)
	items, _ := top["data"].([]any)
	item, _ := items[0].(map[string]any)
	if u, _ := item["url"].(string); u != orig {
		t.Errorf("探测失败时应当保留原签名 URL，实际换成了 %q", u)
	}
}

// TestRewriteImageURLsNotImageShapeIsNoop 非生图响应原样返回。
//
// 错误信封（4xx 的 {"error":…}）里没有 data —— 改写它没意义，
// 而且不该把格式弄坏（调用方要靠它读上游的错误说明）。
func TestRewriteImageURLsNotImageShapeIsNoop(t *testing.T) {
	p := newRewriteTestProvider(t, http.StatusOK)
	for _, raw := range []string{
		`{"error":{"message":"该模型暂未开放","type":"not_found_error"}}`,
		`{"data":[]}`,
		`not json at all`,
		``,
	} {
		got := p.rewriteImageURLs(context.Background(), []byte(raw))
		if string(got) != raw {
			t.Errorf("非生图形状应当原样返回：\n in=%q\nout=%q", raw, string(got))
		}
	}
}

// TestRewriteImageURLsNoHTTPClientIsSafe 没有 HTTP 客户端时不 panic。
func TestRewriteImageURLsNoHTTPClientIsSafe(t *testing.T) {
	p := &Provider{} // client 为 nil
	raw := []byte(`{"data":[{"url":"https://x/a.png?q-signature=deadbeef"}]}`)
	got := p.rewriteImageURLs(context.Background(), raw)
	if string(got) != string(raw) {
		t.Errorf("nil client 时应原样返回")
	}
}
