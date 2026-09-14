// inband_test.go 钉住"上游用 2xx + 流内错误信封拒绝请求"这一类形态。
//
// # 为什么需要这一组测试
//
// 实测（CodeArts / 华为 InferHub）：模型未注册、通道不对、额度耗尽这几类拒绝
// **都不改 HTTP 状态码**，而是塞进 200 的流里：
//
//	data:{"error":{"code":"InferHub.002002009","message":"The model is not registered"}}
//	data:{"error":"InferHub.4005.200 unsupported model"}
//
// 改造前这类帧被洗成一个**空的成功响应**（normalizeFrame 剥掉 error 字段，
// 而空流兜底只认 validFrames==0，错误信封是合法 JSON 因此计数为 1）。
// 客户端只看到 `200 + content:""`，错误原文连日志都没有 ——
// 这正是"内置对话测试正常、外部调用 api 只有 codearts 报错"的根因。
//
// 下面每条都是**变异验证**：把 inBandErrorOf 的判定去掉，用例立刻变红。
package upstream

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAggregateInBandErrorIsNotSwallowed 是本次修复在非流式路径上的主闸门。
//
// 用例的正文形态取自 **`go test -tags probe` 抓到的真实上游响应**，
// 不是猜的。第一版按 OpenAI 风格的顶层 `error` 写，线上仍然回 200 + 空响应 ——
// CodeArts 用的是华为系的 `error_code` / `error_msg`。
func TestAggregateInBandErrorIsNotSwallowed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // 必须出现在 Message 与 Body 里的关键子串
	}{
		{
			name: "实测：模型未注册（带 text:[DONE] 噪声字段）",
			raw: "data:{\"text\":\"[DONE]\",\"error_code\":\"InferHub.002002009.404\"," +
				"\"error_msg\":\"The model is not registered, please request other model\"}\n\n",
			want: "The model is not registered",
		},
		{
			name: "实测：benefit 通道不可用（带 details 数组）",
			raw: "data:{\"error_code\":\"InferHub.4004.200\",\"error_msg\":\"benefit not found\"," +
				"\"details\":[{\"error_code\":\"InferHub.4004.200\",\"error_msg\":\"requestId: abc\"}]}\n\n",
			want: "benefit not found",
		},
		{
			name: "实测：额度耗尽（200 + 流内业务错误）",
			raw: "data:{\"error_code\":\"InferHub.4291.200\",\"error_msg\":\"insufficient quota\"}\n\n",
			want: "insufficient quota",
		},
		{
			name: "只有 error_code 没有 error_msg",
			raw:  "data:{\"error_code\":\"InferHub.002002009.404\"}\n\ndata: [DONE]\n\n",
			want: "InferHub.002002009.404",
		},
		{
			name: "只有 error_msg 没有 error_code",
			raw:  "data: {\"error_msg\":\"The model is not registered\"}\n\ndata: [DONE]\n\n",
			want: "The model is not registered",
		},
		{
			name: "通用性：OpenAI 风格顶层 error 对象",
			raw: "data:{\"error\":{\"code\":\"upstream_err\"," +
				"\"message\":\"The model is not registered\"}}\n\ndata: [DONE]\n\n",
			want: "The model is not registered",
		},
		{
			name: "通用性：顶层 error 字符串",
			raw:  "data: {\"error\":\"InferHub.4005.200 unsupported model\"}\n\ndata: [DONE]\n\n",
			want: "unsupported model",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := Aggregate(strings.NewReader(c.raw))

			var ib *InBandError
			if !errors.As(err, &ib) {
				t.Fatalf("期望 *InBandError，实际 err=%v resp=%v\n"+
					"★ 错误信封被当成有效数据帧聚合成了成功响应 ——\n"+
					"  客户端会收到 200 + content:\"\"，且错误原文彻底消失。", err, resp)
			}
			if !strings.Contains(ib.Message, c.want) {
				t.Errorf("Message=%q，期望包含 %q", ib.Message, c.want)
			}
			// Body 必须是**上游原文**：分类器要靠它认出 InferHub.4291 这类
			// 只存在于原文里的判据（见 codearts.DetectQuotaExhausted）。
			if !strings.Contains(ib.Body, c.want) {
				t.Errorf("Body=%q 未保留上游原文", ib.Body)
			}
			// 非流式路径一个字节都还没写给客户端 → 恒为 false。
			if ib.Committed {
				t.Error("Aggregate 路径的 Committed 必须为 false（响应尚未提交）")
			}
		})
	}
}

// TestAggregateInBandErrorIsNotSwallowedRealFrames 用**逐字节的真实响应**再钉一遍。
//
// 上面那些是拆行后的等价形态；这一条直接用 probe 抓到的原文，
// 确保"字段名 + 无空格 data: 帧"这两个真实细节同时被覆盖。
func TestAggregateInBandErrorIsNotSwallowedRealFrames(t *testing.T) {
	// probe 实测原文（模型未注册）
	const realNotRegistered = `data:{"text":"[DONE]","error_code":"InferHub.002002009.404",` +
		`"error_msg":"The model is not registered, please request other model"}` + "\n\n"

	resp, err := Aggregate(strings.NewReader(realNotRegistered))
	var ib *InBandError
	if !errors.As(err, &ib) {
		t.Fatalf("真实帧未被识别成错误信封：err=%v resp=%v", err, resp)
	}
	if !strings.Contains(ib.Message, "InferHub.002002009.404") {
		t.Errorf("Message 应带上 error_code，实际 %q", ib.Message)
	}
	if !strings.Contains(ib.Body, "The model is not registered") {
		t.Errorf("Body 应保留 error_msg 原文，实际 %q", ib.Body)
	}
}

// TestAggregateErrorWithChoicesIsNotInBandError 确认判据**没有过宽**。
//
// 正常数据帧上多带一个诊断用字段是合法的，不能被当成失败 ——
// 否则一次误判就会把正常回复变成错误。
func TestAggregateErrorWithChoicesIsNotInBandError(t *testing.T) {
	for _, extra := range []string{
		`"error":"diagnostic only"`,
		`"error_code":"some.diagnostic.code"`,
		`"error_msg":"diagnostic note"`,
	} {
		raw := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\"," + extra +
			",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"

		resp, err := Aggregate(strings.NewReader(raw))
		if err != nil {
			t.Fatalf("带 choices 的帧不应被判成错误信封（%s）: %v", extra, err)
		}
		msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != "hi" {
			t.Errorf("content=%q，期望 hi（%s）", msg["content"], extra)
		}
	}
}

// TestAggregateNormalStreamUnaffected 是回归护栏：正常流一个字节都不该变。
func TestAggregateNormalStreamUnaffected(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0," +
		"\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
		"\"usage\":{\"total_tokens\":7}}\n\ndata: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("正常流不应报错: %v", err)
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v，期望 glm-5.2", resp["model"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q，期望 你好", msg["content"])
	}
}

// TestStreamInBandErrorFirstFrameDoesNotCommit 是流式路径上的主闸门。
//
// 首帧即错误信封时，Stream 必须**一个字节都不写**并把 *InBandError 交回来：
// 这样调用方还能改用 4xx/5xx 回一个真正的错误。
//
// 一旦这里写出任何东西（哪怕只是一个空壳帧），HTTP 200 就被提交，
// 客户端只能看到一个空回复 —— 那正是本 bug 的形态。
func TestStreamInBandErrorFirstFrameDoesNotCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(
		"data: {\"error\":{\"code\":\"InferHub.002002009\","+
			"\"message\":\"The model is not registered\"}}\n\ndata: [DONE]\n\n"))

	var ib *InBandError
	if !errors.As(err, &ib) {
		t.Fatalf("期望 *InBandError，实际 %v", err)
	}
	if ib.Committed {
		t.Error("首帧即错误时 Committed 必须为 false（状态码仍可改）")
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("首帧即错误时不应写出任何内容，实际写出 %q\n"+
			"★ 写出即提交 200，调用方再也回不了 4xx/5xx。", got)
	}
	// SSE 响应头必须**惰性设置**：否则调用方随后回的那个 JSON 错误
	// 会带着 Content-Type: text/event-stream 发出去，客户端按事件流解析 JSON。
	if ct := rec.Header().Get("Content-Type"); ct != "" {
		t.Errorf("未写出任何内容时不应设置 Content-Type，实际 %q", ct)
	}
}

// TestStreamInBandErrorAfterContentKeepsRawError 覆盖"错误出现在内容之后"。
//
// 此时状态码已提交、无法回滚，但错误细节**必须**到达客户端：
// normalizeFrame 会把 error 字段剥掉，所以 Stream 必须走原样下发。
func TestStreamInBandErrorAfterContentKeepsRawError(t *testing.T) {
	const contentFrame = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0," +
		"\"delta\":{\"content\":\"部分内容\"}}]}\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(contentFrame+
		"data: {\"error\":{\"code\":\"InferHub.4291.200\",\"message\":\"insufficient quota\"}}\n\n"))

	var ib *InBandError
	if !errors.As(err, &ib) {
		t.Fatalf("期望 *InBandError，实际 %v", err)
	}
	if !ib.Committed {
		t.Error("内容之后才错误时 Committed 必须为 true（响应已提交）")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "部分内容") {
		t.Errorf("已流出的内容不应被吞掉: %q", body)
	}
	if !strings.Contains(body, "insufficient quota") {
		t.Errorf("错误帧的 error 字段必须原样下发（不能被 normalizeFrame 剥掉）: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("必须补 [DONE] 让客户端收尾，实际尾部 %q", tail(body, 40))
	}
}

// TestStreamErrorEnvelopeNotNormalizedAway 是本 bug 的最小复现。
//
// 改造前这条会产出 `{"id":"chatcmpl-wb2api","object":"chat.completion.chunk",
// "usage":null}` —— 一个没有 choices、没有 error 的空壳，
// 正是实测中外部客户端收到的那一帧。
func TestStreamErrorEnvelopeNotNormalizedAway(t *testing.T) {
	rec := httptest.NewRecorder()
	_ = Stream(rec, strings.NewReader(
		"data:{\"error\":{\"code\":\"InferHub.002002009\",\"message\":\"not registered\"}}\n\n"))

	body := rec.Body.String()
	if strings.Contains(body, "chatcmpl-wb2api") {
		t.Errorf("错误信封被规范化成了一个空壳帧（本 bug 的原始形态）: %q", body)
	}
	if body != "" {
		t.Errorf("首帧即错误时不应写出任何帧，实际 %q", body)
	}
}

// tail 返回字符串末尾 n 个 rune（用于断言失败时的可读输出）。
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// TestRewriteModelField 覆盖上游模型名规范化的通用动作。
func TestRewriteModelField(t *testing.T) {
	orig := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)

	// 1) 确实不同 → 改写，其余字段不受影响。
	out := RewriteModelField(orig, "GLM-5.2")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("改写后不是合法 JSON: %v", err)
	}
	if obj["model"] != "GLM-5.2" {
		t.Errorf("model=%v，期望 GLM-5.2", obj["model"])
	}
	if !strings.Contains(string(out), "hi") {
		t.Errorf("改写不该动其它字段: %s", out)
	}

	// 2) 相同 → **逐字节**原样返回（同一底层数组），不重编码。
	//
	// 这条不变式有意义：重编码会重排键顺序并丢空白，
	// 而调用方（各上游）依赖"没改就不动"。
	same := RewriteModelField(orig, "glm-5.2")
	if len(same) != len(orig) || &same[0] != &orig[0] {
		t.Errorf("模型名未变化时不应重编码（原 %q，得 %q）", orig, same)
	}

	// 3) 空模型名 → 不改（否则会把"客户端漏字段"变成我们造的请求）。
	if got := RewriteModelField(orig, ""); &got[0] != &orig[0] {
		t.Error("目标模型名为空时不应改写")
	}

	// 4) 非 JSON / 无 model 字段 → 原样返回，不能造出一个假 body。
	if got := RewriteModelField([]byte("not json"), "GLM-5.2"); string(got) != "not json" {
		t.Errorf("非 JSON 输入应原样返回，实际 %q", got)
	}
	noModel := []byte(`{"messages":[]}`)
	if got := RewriteModelField(noModel, "GLM-5.2"); &got[0] != &noModel[0] {
		t.Error("没有 model 字段时不该凭空加上")
	}

	// 5) 空输入。
	if got := RewriteModelField(nil, "GLM-5.2"); got != nil {
		t.Errorf("nil 输入应返回 nil，实际 %q", got)
	}
}
