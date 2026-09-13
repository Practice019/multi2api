package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrepareBodyForcesStream(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"model":"glm-5.2","messages":[]}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["stream"] != true {
		t.Errorf("stream=%v", m["stream"])
	}
}

// codeartsSSE 是 CodeArts（华为 InferHub）实测的 SSE 形态：`data:{...}`，**冒号后没有空格**。
//
// 这段形态不是编的：由 raw socket 抓包证实（`64 61 74 61 3A 7B` = "data:{"），
// 且 internal/codearts 的 contract_hermetic_test.go 与 testsrv_test.go 用的也是它。
//
// ⚠ 它必须作为**共享夹具**存在：这个 bug 的成因正是
// "internal/upstream 的测试全用带空格形态、internal/codearts 的测试全用不带空格形态，
//
//	两个包对同一份帧形态各自为政，没有任何一个测试跨过那条边界"。
const codeartsSSE = "data:{\"id\":\"as-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data:{\"id\":\"as-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"}}]}\n\n" +
	"data:{\"id\":\"as-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
	"data:[DONE]\n\n"

// TestAggregateAcceptsCodeartsFramelessSpace 守住冒号后**无空格**的 data 帧。
//
// # 这个测试对应真 bug（不是防御性编程）
//
// 实测：`codearts/glm-5.3-flash` 非流式恒 502
//
//	{"error":{"code":"upstream_parse","message":"upstream stream contained no valid data events"}}
//
// 而**同一个上游、同一个模型**的流式请求是 200。根因是 Aggregate 只认 `"data: "`：
// CodeArts 的每一帧都不匹配 → validEvents 恒为 0 → 走到"空流"错误分支。
// 流式之所以"看着是好的"，是 Stream 的兜底分支把不认识的帧**原样透传**了 ——
// 一个解析假设，两种截然不同的失败表现（这正是不该只测一种形态的理由）。
func TestAggregateAcceptsCodeartsFramelessSpace(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(codeartsSSE))
	if err != nil {
		t.Fatalf("CodeArts 的无空格 data 帧必须能聚合，实际报错: %v", err)
	}
	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices 缺失: %v", resp["choices"])
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q，期望 \"你好，世界\"", msg["content"])
	}
	if resp["model"] != "glm-5.3-flash" {
		t.Errorf("model=%v", resp["model"])
	}
	if usage, ok := resp["usage"].(map[string]any); !ok || usage["total_tokens"].(float64) != 7 {
		t.Errorf("usage=%v", resp["usage"])
	}
}

// TestAggregateAcceptsBothFrameShapes 两种形态必须同时成立。
//
// 只测一种都会漏：上游是哪个（OpenAI 带空格 / CodeArts 不带空格）不是网关能选的，
// 而 Aggregate 是**两条路共用**的聚合器，它没有"我只服务某一个上游"的奢侈。
func TestAggregateAcceptsBothFrameShapes(t *testing.T) {
	// 带空格形态：把无空格夹具的每个帧头补上空格（sseFixture 即是该形态）。
	spaced := strings.ReplaceAll(codeartsSSE, "data:{", "data: {")
	spaced = strings.ReplaceAll(spaced, "data:[DONE]", "data: [DONE]")

	for _, tc := range []struct{ name, raw string }{
		{"CodeArts 无空格 data:{", codeartsSSE},
		{"OpenAI 带空格 data: {", spaced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := Aggregate(strings.NewReader(tc.raw))
			if err != nil {
				t.Fatalf("聚合失败: %v", err)
			}
			msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			if msg["content"] != "你好，世界" {
				t.Errorf("content=%q", msg["content"])
			}
		})
	}
}

// TestStreamCodeartsFramesAreRelayedAndCounted 守住流式路径对无空格帧的处理。
//
// 两个断言各自对应真 bug 的一半：
//
//  1. 帧必须被**转发**（早先落进"注释/其它行"分支也算转发，所以这条单看是绿的）；
//  2. 帧必须被**计数**为有效帧 —— 早先 validFrames 恒为 0，于是循环结束后
//     明明有数据却补发一帧 `{"error":{"message":"empty upstream stream"}}`，
//     客户端在一个成功的流里收到一个假的错误帧。
//
// 第 2 条是 1 的补充：只看"内容到了"会漏掉"同时报了一个假错误"。
func TestStreamCodeartsFramesAreRelayedAndCounted(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(codeartsSSE)); err != nil {
		t.Fatalf("Stream 不该报错: %v", err)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "你好") || !strings.Contains(body, "，世界") {
		t.Errorf("无空格帧的内容必须转发给客户端:\n%s", body)
	}
	if strings.Contains(body, "empty upstream stream") {
		t.Errorf("有有效帧时不得补发空流错误帧（validFrames 未被正确计数）:\n%s", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] 应恰好一个，实际 %d 个:\n%s", n, body)
	}
	// 每行仍是合法 SSE：以 "data: " 开头（规范化后）或是空行。
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if ln != "" && !strings.HasPrefix(ln, "data: ") {
			t.Errorf("非规范 SSE 行: %q", ln)
		}
	}
}

func TestPrepareBodyToolChoiceFunctionObject(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"tool_choice":{"type":"function","function":{"name":"get_weather"}},"tools":[{"type":"function"}]}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "get_weather" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
	if _, ok := m["tools"]; !ok {
		t.Error("tools should be kept for function choice")
	}
}

func TestPrepareBodyToolChoiceNone(t *testing.T) {
	for _, in := range []string{
		`{"tool_choice":"none","tools":[{}],"functions":[{}]}`,
		`{"tool_choice":{"type":"none"},"tools":[{}]}`,
	} {
		out := PrepareBodyOpt([]byte(in), true)
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, ok := m["tool_choice"]; ok {
			t.Errorf("%s: tool_choice should be deleted", in)
		}
		if _, ok := m["tools"]; ok {
			t.Errorf("%s: tools should be deleted", in)
		}
		if _, ok := m["functions"]; ok {
			t.Errorf("%s: functions should be deleted", in)
		}
	}
}

func TestPrepareBodyToolChoiceAuto(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"tool_choice":{"type":"auto"}}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
}

func TestPrepareBodyInvalidJSON(t *testing.T) {
	in := []byte(`{broken`)
	out := PrepareBodyOpt(in, true)
	if string(out) != string(in) {
		t.Error("invalid json should pass through unchanged")
	}
}

const sseFixture = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

func TestAggregate(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(sseFixture))
	if err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v", resp["model"])
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role=%v", msg["role"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", choices[0].(map[string]any)["finish_reason"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 7 {
		t.Errorf("usage=%v", usage)
	}
}

func TestAggregateSkipsNonDataLines(t *testing.T) {
	raw := ": comment\n\n" + sseFixture
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestAggregateToolCalls(t *testing.T) {
	// 流式 tool_calls：首片带 id/type/name + 空 arguments，后续只带 arguments 片段
	raw := `data: {"id":"x1","model":"deepseek-v4-pro","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""},"index":0}]}}],"usage":null}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"city\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"北京\"}"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"total_tokens":11}}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", msg["tool_calls"])
	}
	if calls[0]["id"] != "call_a" || calls[0]["type"] != "function" {
		t.Errorf("call meta=%v", calls[0])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("fn.name=%v", fn["name"])
	}
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("fn.arguments=%q", fn["arguments"])
	}
}

// streamFrames 把原始 SSE 输入经 Stream 处理后解析出所有 JSON 帧及 [DONE] 计数。
func streamFrames(t *testing.T, raw string) (frames []map[string]any, doneCount int) {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "data: [DONE]") {
			doneCount++
			continue
		}
		if strings.HasPrefix(ln, "data: ") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
				t.Fatalf("bad frame %q: %v", ln, err)
			}
			frames = append(frames, obj)
		}
	}
	return frames, doneCount
}

func TestNormalizeFrame(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string // 规范化后 marshal 的期望 JSON（Go map 键按字典序输出）
	}{
		{"empty content/refusal and finish_reason empty string",
			map[string]any{"id": "x", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "", "refusal": ""}, "finish_reason": ""},
			}},
			`{"choices":[{"delta":{},"finish_reason":null,"index":0}],"id":"x","object":"chat.completion.chunk","usage":null}`},
		{"non-empty tool_calls kept",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"id": "c1", "type": "function"}}}},
			}},
			`{"choices":[{"delta":{"tool_calls":[{"id":"c1","type":"function"}]},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`},
		{"empty tool_calls list dropped",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{}, "content": "hi"}},
			}},
			`{"choices":[{"delta":{"content":"hi"},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`},
		{"empty placeholder function_call dropped",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"function_call": map[string]any{"name": "", "arguments": ""}}},
			}},
			`{"choices":[{"delta":{},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`},
		{"top-level unknown fields dropped, usage null when absent",
			map[string]any{"id": "x", "object": "chat.completion.chunk", "created": 1, "junk": "noise", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			}},
			`{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":1,"id":"x","object":"chat.completion.chunk","usage":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(normalizeFrame(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != c.want {
				t.Errorf("got  %s\nwant %s", raw, c.want)
			}
		})
	}
}

func TestStreamNormalizesFrames(t *testing.T) {
	// 混合噪声帧：空 content/reasoning/refusal/function_call + 空 tool_calls + 顶层非标字段，
	// 随后非空 content + tool_calls 帧，最后 finish/usage 帧。
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"\",\"refusal\":\"\",\"tool_calls\":[],\"function_call\":{\"name\":\"\",\"arguments\":\"\"}},\"finish_reason\":\"\"}],\"extra_field\":\"junk\"}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\",\"tool_calls\":[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"},\"index\":0}]},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done frames=%d want 1", done)
	}
	if len(frames) != 3 {
		t.Fatalf("frames=%d want 3", len(frames))
	}

	// 帧 1：噪声全剔除，finish_reason ""→null，usage 缺失→null，顶层非标字段剥除
	f0 := frames[0]
	if _, ok := f0["extra_field"]; ok {
		t.Error("top-level extra_field should be dropped")
	}
	if f0["usage"] != nil {
		t.Errorf("usage should be null when absent, got %v", f0["usage"])
	}
	ch0 := f0["choices"].([]any)[0].(map[string]any)
	if ch0["finish_reason"] != nil {
		t.Errorf("frame1 finish_reason=%v want null", ch0["finish_reason"])
	}
	d := ch0["delta"].(map[string]any)
	// role 是合法白名单键保留；空 content/reasoning/refusal/tool_calls/function_call 噪声全剔除
	if len(d) != 1 || d["role"] != "assistant" {
		t.Errorf("frame1 delta should only keep role, got %#v", d)
	}
	for _, noise := range []string{"content", "reasoning_content", "refusal", "tool_calls", "function_call"} {
		if _, ok := d[noise]; ok {
			t.Errorf("frame1 delta should drop %q, got %#v", noise, d)
		}
	}

	// 帧 2：非空 content 与 tool_calls 保留，finish_reason ""→null
	f1 := frames[1]
	ch1 := f1["choices"].([]any)[0].(map[string]any)
	d1 := ch1["delta"].(map[string]any)
	if d1["content"] != "hello" {
		t.Errorf("frame2 content=%v", d1["content"])
	}
	tcs, ok := d1["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("frame2 tool_calls=%#v", d1["tool_calls"])
	}
	if ch1["finish_reason"] != nil {
		t.Errorf("frame2 finish_reason=%v want null (input empty string)", ch1["finish_reason"])
	}

	// 帧 3：finish_reason 非空保留，usage 保留
	f2 := frames[2]
	ch2 := f2["choices"].([]any)[0].(map[string]any)
	if ch2["finish_reason"] != "stop" {
		t.Errorf("frame3 finish_reason=%v want stop", ch2["finish_reason"])
	}
	if f2["usage"].(map[string]any)["total_tokens"].(float64) != 7 {
		t.Errorf("frame3 usage=%v", f2["usage"])
	}
}

func TestStreamDoneFallback(t *testing.T) {
	// 上游流在无 [DONE] 时 EOF，Stream 必须兜底写一个 [DONE]
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("missing [DONE] fallback: %q", body)
	}

	// 已有 [DONE] 时只写一次，不重复
	rec2 := httptest.NewRecorder()
	if err := Stream(rec2, strings.NewReader("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(rec2.Body.String(), "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, rec2.Body.String())
	}
}

func TestStreamPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(sseFixture))
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body missing chunks: %q", body)
	}
	// 逐行仍是合法 SSE（每行以 data: 开头或是空行）
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if ln != "" && !strings.HasPrefix(ln, "data: ") {
			t.Errorf("bad line: %q", ln)
		}
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
}

// TestAggregateEmptyStreamCases 覆盖空流检测：0 有效事件必须报错、[DONE] 即 break、
// [DONE] 后垃圾不进聚合、正常聚合回归。
func TestAggregateEmptyStreamCases(t *testing.T) {
	// 正常回归基流：content + finish_reason + usage，[DONE] 收尾。
	valid := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	cases := []struct {
		name    string
		raw     string
		wantErr bool
		// 回归断言（仅在 wantErr=false 时校验）
		wantContent string
		wantUsage   float64
	}{
		{
			name:    "空流（EOF 即止）",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "只有注释行和空行加 DONE",
			raw:     ": comment\n\n: another comment\n\ndata: [DONE]\n\n",
			wantErr: true,
		},
		{
			name:    "DONE 后跟垃圾帧不进聚合",
			raw:     valid[:len(valid)-len("data: [DONE]\n\n")] + "data: [DONE]\n\ndata: {\"junk\":\"should not aggregate\"}\n\n",
			wantErr: false,
			// 与 valid 基流一致的聚合期望
			wantContent: "hi",
			wantUsage:   7,
		},
		{
			name:        "正常流回归",
			raw:         valid,
			wantErr:     false,
			wantContent: "hi",
			wantUsage:   7,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := Aggregate(strings.NewReader(c.raw))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (resp=%v)", resp)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			if msg["content"] != c.wantContent {
				t.Errorf("content=%q want %q", msg["content"], c.wantContent)
			}
			if u, ok := resp["usage"].(map[string]any); ok {
				if u["total_tokens"].(float64) != c.wantUsage {
					t.Errorf("usage=%v want %v", u["total_tokens"], c.wantUsage)
				}
			} else {
				t.Errorf("usage missing")
			}
		})
	}
}

// TestAggregateEmptyStreamError 校验空流错误信息形如约定文案。
func TestAggregateEmptyStreamError(t *testing.T) {
	_, err := Aggregate(strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "no valid data events") {
		t.Fatalf("err=%v", err)
	}
}

// TestStreamEmptyFramesCase 覆盖流式空流检测：0 有效帧时写 error 帧（error 字段存活）,
// 恰好一个 [DONE]，并返回非 nil error。
func TestStreamEmptyFramesCase(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空流", ""},
		{"只有注释行", ": comment\n\n"},
		{"只有 DONE", "data: [DONE]\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := Stream(rec, strings.NewReader(c.raw))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			body := rec.Body.String()
			if n := strings.Count(body, "data: [DONE]"); n != 1 {
				t.Errorf("[DONE] count=%d want 1: %q", n, body)
			}
			// error 帧必须原样保留 error 字段（未被 normalizeFrame 白名单剥掉）
			var e map[string]any
			found := false
			for _, ln := range strings.Split(body, "\n") {
				ln = strings.TrimSpace(ln)
				if strings.HasPrefix(ln, "data: ") {
					payload := strings.TrimPrefix(ln, "data: ")
					if payload == "[DONE]" {
						continue
					}
					if json.Unmarshal([]byte(payload), &e) == nil {
						if em, ok := e["error"].(map[string]any); ok && em["message"] == "empty upstream stream" && em["type"] == "upstream_error" {
							found = true
						}
					}
				}
			}
			if !found {
				t.Errorf("error frame absent or error field stripped: %q", body)
			}
		})
	}
}

// TestStreamGarbageAfterDone 校验 DONE 之后的垃圾帧不出现在响应里。
func TestStreamGarbageAfterDone(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"should\":\"not appear\"}\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "should") {
		t.Errorf("garbage after DONE leaked into response: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	// 有效帧仍被透传
	if !strings.Contains(body, "hello") {
		t.Errorf("valid frame missing: %q", body)
	}
}

// TestStreamNormalPassthroughRegression 校验正常透传回归：帧被 normalize 后透传、
// 末尾恰好一个 [DONE]、无 error 帧；上游漏发 DONE 时自动补。
func TestStreamNormalPassthroughRegression(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"带 DONE 的正常流", sseFixture},
		{"漏发 DONE 自动补", "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := Stream(rec, strings.NewReader(c.raw)); err != nil {
				t.Fatal(err)
			}
			body := rec.Body.String()
			if strings.Contains(body, `"error"`) {
				t.Errorf("unexpected error frame: %q", body)
			}
			if n := strings.Count(body, "data: [DONE]"); n != 1 {
				t.Errorf("[DONE] count=%d want 1: %q", n, body)
			}
			// 帧被规范化：含 "id" 且有标准 object 字段
			if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
				t.Errorf("frame not normalized: %q", body)
			}
		})
	}
}
