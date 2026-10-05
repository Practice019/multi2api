// anthropic_test.go OpenAI ⇄ Anthropic 转换的守卫。
//
// # 为什么这个文件里的断言值得写得这么细
//
// 转换器是**唯一**"错了也不崩"的部件：它产出的 JSON 永远合法，
// 只是字段少了一个、角色错了一层、或者参数丢了一半。
// 那些缺陷表现为"模型答得不对"，而不是"报错" —— 于是会被归因到
// 上游/model/prompt，而不是我们的转换器。
//
// 所以这里的断言**全部逐字段比对语义**，不满足于"没报错"。
package zcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// 解 JSON 到 map 的测试助手（失败即 Fatal）。
func mustJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("产出不是合法 JSON: %v\n原文: %s", err, raw)
	}
	return m
}

// 取 messages 数组。
func messagesOf(t *testing.T, m map[string]any) []any {
	t.Helper()
	v, ok := m["messages"].([]any)
	if !ok {
		t.Fatalf("messages 缺失或不是数组: %#v", m["messages"])
	}
	return v
}

// ---- 请求转换 ----

// system 消息必须抽到顶层，且**多条要合并**。
//
// # 不抽的后果
//
// Anthropic 的 messages 里不接受 system 角色 → 上游 400。
// 而合并的细节（用 "\n\n" 连接）决定了提示词的段落边界是否保留 ——
// 用 "" 或 " " 连接会让两段指令粘成一段，模型行为会变。
func TestRequestSystemIsExtractedAndMerged(t *testing.T) {
	in := `{
	  "model":"GLM-5.3",
	  "messages":[
	    {"role":"system","content":"你是助手"},
	    {"role":"system","content":"回答要简短"},
	    {"role":"user","content":"hi"}
	  ]
	}`
	out := mustReq(t, in)

	sys, ok := out["system"].(string)
	if !ok {
		t.Fatalf("system 未抽到顶层: %#v", out["system"])
	}
	if !strings.Contains(sys, "你是助手") || !strings.Contains(sys, "回答要简短") {
		t.Errorf("多条 system 未合并: %q", sys)
	}
	if !strings.Contains(sys, "\n\n") {
		t.Errorf("合并应保留段落边界（用 \\n\\n 连接），实际 %q", sys)
	}

	msgs := messagesOf(t, out)
	if len(msgs) != 1 {
		t.Fatalf("system 抽走后应只剩 1 条消息，实际 %d 条: %#v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("剩余消息应为 user，实际 %v", first["role"])
	}
}

// developer 角色等价于 system（OpenAI 新规范）。
func TestRequestDeveloperRoleTreatedAsSystem(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"developer","content":"dev 指令"},{"role":"user","content":"hi"}]}`
	out := mustReq(t, in)
	if out["system"] != "dev 指令" {
		t.Errorf("developer 应被当作 system 抽出，实际 system=%#v", out["system"])
	}
	for _, m := range messagesOf(t, out) {
		if m.(map[string]any)["role"] == "developer" {
			t.Error("developer 角色不该出现在 Anthropic 的 messages 里（上游会 400）")
		}
	}
}

// tool 角色必须变成 user 消息里的 tool_result block。
//
// # 这是本文件最重要的一条
//
// Anthropic 的 messages 只认 user / assistant。OpenAI 的 tool 结果
// 是**独立消息**，直接传过去会 400。而变成 tool_result 之后还有一层：
// 它必须待在 **user** 消息里 —— 放 assistant 里上游同样拒绝。
func TestRequestToolRoleBecomesToolResultInUserMessage(t *testing.T) {
	in := `{
	  "model":"m",
	  "messages":[
	    {"role":"user","content":"查天气"},
	    {"role":"assistant","content":null,"tool_calls":[
	      {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}
	    ]},
	    {"role":"tool","tool_call_id":"call_1","content":"晴 25 度"}
	  ]
	}`
	out := mustReq(t, in)
	msgs := messagesOf(t, out)

	if len(msgs) != 3 {
		t.Fatalf("应得到 3 条消息（user / assistant / user-含结果），实际 %d 条: %#v", len(msgs), msgs)
	}

	// 第 2 条：assistant 的 tool_calls → tool_use blocks。
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("第 2 条应为 assistant，实际 %v", asst["role"])
	}
	blocks, ok := asst["content"].([]any)
	if !ok {
		t.Fatalf("assistant 的 content 应为 block 数组，实际 %#v", asst["content"])
	}
	var foundToolUse bool
	for _, b := range blocks {
		bm := b.(map[string]any)
		if bm["type"] == "tool_use" {
			foundToolUse = true
			if bm["id"] != "call_1" {
				t.Errorf("tool_use 的 id 不对: %v", bm["id"])
			}
			if bm["name"] != "get_weather" {
				t.Errorf("tool_use 的 name 不对: %v", bm["name"])
			}
			// ⚠ arguments 是 JSON 字符串，必须被解析成**对象**。
			// Anthropic 的 input 是对象；传字符串会被上游参数校验拒绝，
			// 而那个错误信息看不出是"类型不对"。
			input, ok := bm["input"].(map[string]any)
			if !ok {
				t.Fatalf("tool_use.input 必须是对象（不是字符串），实际 %#v", bm["input"])
			}
			if input["city"] != "北京" {
				t.Errorf("tool_use.input 内容不对: %#v", input)
			}
		}
	}
	if !foundToolUse {
		t.Error("assistant 消息里没有 tool_use block —— 工具调用丢了")
	}

	// 第 3 条：tool_result 必须在 **user** 消息里。
	third := msgs[2].(map[string]any)
	if third["role"] != "user" {
		t.Fatalf("tool_result 必须在 user 消息里，实际角色 %v", third["role"])
	}
	tBlocks, ok := third["content"].([]any)
	if !ok || len(tBlocks) == 0 {
		t.Fatalf("第 3 条 content 应为非空数组，实际 %#v", third["content"])
	}
	tr := tBlocks[0].(map[string]any)
	if tr["type"] != "tool_result" {
		t.Errorf("block 类型应为 tool_result，实际 %v", tr["type"])
	}
	if tr["tool_use_id"] != "call_1" {
		t.Errorf("tool_use_id 不对: %v", tr["tool_use_id"])
	}
	if tr["content"] != "晴 25 度" {
		t.Errorf("tool_result 内容不对: %#v", tr["content"])
	}
}

// 连续的多个 tool 结果必须**合并进同一条 user 消息**。
//
// # 为什么
//
// Anthropic 要求 user/assistant 交替。两条连续的 user 消息会被
// 上游以 400 拒绝（错误信息形如 "messages: roles must alternate"）。
// 而并行工具调用（模型一次调 2-3 个工具）**必然**产生连续的 tool 消息 ——
// 所以这不是边界情况，是常态。
func TestRequestConsecutiveToolResultsMergeIntoOneUserMessage(t *testing.T) {
	in := `{
	  "model":"m",
	  "messages":[
	    {"role":"user","content":"并行查两个"},
	    {"role":"assistant","content":null,"tool_calls":[
	      {"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}},
	      {"id":"c2","type":"function","function":{"name":"b","arguments":"{}"}}
	    ]},
	    {"role":"tool","tool_call_id":"c1","content":"结果A"},
	    {"role":"tool","tool_call_id":"c2","content":"结果B"}
	  ]
	}`
	out := mustReq(t, in)
	msgs := messagesOf(t, out)

	if len(msgs) != 3 {
		t.Fatalf("两个 tool 结果必须合并成 1 条 user 消息（共 3 条），实际 %d 条: %#v", len(msgs), msgs)
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("末条应为 user，实际 %v", last["role"])
	}
	blocks := last["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("应含 2 个 tool_result block，实际 %d 个", len(blocks))
	}
	gotIDs := map[string]bool{}
	for _, b := range blocks {
		bm := b.(map[string]any)
		if bm["type"] != "tool_result" {
			t.Errorf("block 类型应为 tool_result，实际 %v", bm["type"])
		}
		gotIDs[bm["tool_use_id"].(string)] = true
	}
	if !gotIDs["c1"] || !gotIDs["c2"] {
		t.Errorf("两个 tool_use_id 都该在，实际 %v", gotIDs)
	}

	// 顺带守：不能出现两条连续的 user。
	for i := 1; i < len(msgs); i++ {
		prev := msgs[i-1].(map[string]any)["role"]
		cur := msgs[i].(map[string]any)["role"]
		if prev == cur {
			t.Errorf("第 %d/%d 条角色连续相同（%v）—— Anthropic 要求交替，上游会 400", i-1, i, cur)
		}
	}
}

// tools 定义转换：function.parameters → input_schema。
func TestRequestToolsConversion(t *testing.T) {
	in := `{
	  "model":"m",
	  "messages":[{"role":"user","content":"hi"}],
	  "tools":[{"type":"function","function":{
	    "name":"get_weather",
	    "description":"查天气",
	    "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
	  }}],
	  "tool_choice":"auto"
	}`
	out := mustReq(t, in)

	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools 转换失败: %#v", out["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Errorf("name 不对: %v", tool["name"])
	}
	if tool["description"] != "查天气" {
		t.Errorf("description 不对: %v", tool["description"])
	}
	if _, has := tool["function"]; has {
		t.Error("Anthropic 的 tools 不该有 function 包装层")
	}
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema 缺失（Anthropic 要求这个字段名）: %#v", tool["input_schema"])
	}
	if schema["type"] != "object" {
		t.Errorf("input_schema 内容不对: %#v", schema)
	}
	// tool_choice: "auto" → {"type":"auto"}
	tc, ok := out["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "auto" {
		t.Errorf("tool_choice auto 转换不对: %#v", out["tool_choice"])
	}
}

// 缺 parameters 的 tool 也要给出合法 schema（Anthropic 要求该字段存在）。
func TestRequestToolWithoutParametersGetsValidSchema(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"noop"}}]}`
	out := mustReq(t, in)
	tools := out["tools"].([]any)
	tool := tools[0].(map[string]any)
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("缺 parameters 时应补一个合法 schema，实际 %#v", tool["input_schema"])
	}
	if schema["type"] != "object" {
		t.Errorf("兜底 schema 应为 object 类型: %#v", schema)
	}
}

// tool_choice 的三种映射。
func TestRequestToolChoiceMapping(t *testing.T) {
	build := func(tc string) map[string]any {
		in := `{"model":"m","messages":[{"role":"user","content":"hi"}],
		  "tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":` + tc + `}`
		return mustReq(t, in)
	}

	if tc, _ := build(`"auto"`)["tool_choice"].(map[string]any); tc["type"] != "auto" {
		t.Errorf(`"auto" 应映射成 {"type":"auto"}，实际 %#v`, tc)
	}
	if tc, _ := build(`"required"`)["tool_choice"].(map[string]any); tc["type"] != "any" {
		t.Errorf(`"required" 应映射成 {"type":"any"}，实际 %#v`, tc)
	}
	// "none" 没有 Anthropic 等价物 → 不设该字段（而不是编一个上游不认识的值）。
	if _, has := build(`"none"`)["tool_choice"]; has {
		t.Error(`"none" 应不设 tool_choice（Anthropic 没有等价语义）`)
	}
	// 指定具体函数。
	named := build(`{"type":"function","function":{"name":"get_x"}}`)["tool_choice"].(map[string]any)
	if named["type"] != "tool" || named["name"] != "get_x" {
		t.Errorf(`指定函数应映射成 {"type":"tool","name":...}，实际 %#v`, named)
	}
}

// max_tokens 必填：缺省时补默认值，且优先 max_completion_tokens。
func TestRequestMaxTokensIsAlwaysPresent(t *testing.T) {
	// 完全没给 → 补默认值（Anthropic 必填，缺了上游 400）。
	out := mustReq(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if n, ok := toFloat(out["max_tokens"]); !ok || n != defaultMaxTokens {
		t.Errorf("缺 max_tokens 时应补 %d，实际 %#v", defaultMaxTokens, out["max_tokens"])
	}

	// 给了 max_tokens → 用它。
	out = mustReq(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":123}`)
	if n, _ := toFloat(out["max_tokens"]); n != 123 {
		t.Errorf("应使用请求里的 max_tokens=123，实际 %v", out["max_tokens"])
	}

	// 两个都给 → max_completion_tokens 优先（OpenAI 的新字段语义更准）。
	out = mustReq(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":123,"max_completion_tokens":456}`)
	if n, _ := toFloat(out["max_tokens"]); n != 456 {
		t.Errorf("max_completion_tokens 应优先，实际 %v", out["max_tokens"])
	}
}

// stop → stop_sequences（字符串要变数组）。
func TestRequestStopBecomesStopSequences(t *testing.T) {
	out := mustReq(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":"END"}`)
	seq, ok := out["stop_sequences"].([]any)
	if !ok || len(seq) != 1 || seq[0] != "END" {
		t.Errorf(`stop:"END" 应变成 stop_sequences:["END"]，实际 %#v`, out["stop_sequences"])
	}

	// 空字符串等于没给（不该产出空数组，上游可能拒绝）。
	out = mustReq(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":""}`)
	if _, has := out["stop_sequences"]; has {
		t.Errorf(`stop:"" 应不产生 stop_sequences，实际 %#v`, out["stop_sequences"])
	}

	// 数组形式原样保留。
	out = mustReq(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":["A","B"]}`)
	seq, _ = out["stop_sequences"].([]any)
	if len(seq) != 2 {
		t.Errorf("数组形式应保留 2 项，实际 %#v", out["stop_sequences"])
	}
}

// model 必须原样透传（出口层已经按前缀改写过一次）。
func TestRequestModelPassedThrough(t *testing.T) {
	out := mustReq(t,
		`{"model":"GLM-5.3-Flash","messages":[{"role":"user","content":"hi"}]}`)
	if out["model"] != "GLM-5.3-Flash" {
		t.Errorf("model 被改动了: %#v", out["model"])
	}
}

// 采样参数与 stream 原样透传。
func TestRequestSamplingParamsPassThrough(t *testing.T) {
	out := mustReq(t,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.9,"stream":true}`)
	if n, _ := toFloat(out["temperature"]); n != 0.7 {
		t.Errorf("temperature 丢失: %#v", out["temperature"])
	}
	if n, _ := toFloat(out["top_p"]); n != 0.9 {
		t.Errorf("top_p 丢失: %#v", out["top_p"])
	}
	if out["stream"] != true {
		t.Errorf("stream 丢失: %#v", out["stream"])
	}
}

// 非法 JSON 要报错（而不是产出一个空请求打上游）。
func TestRequestRejectsMalformedBody(t *testing.T) {
	if _, err := openAIToAnthropic([]byte(`{not json`)); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// 空 messages 不 panic，且产出一个合法的空数组。
func TestRequestEmptyMessagesDoesNotPanic(t *testing.T) {
	out := mustReq(t, `{"model":"m","messages":[]}`)
	msgs, ok := out["messages"].([]any)
	if !ok {
		t.Fatalf("messages 应为数组（空也要是数组），实际 %#v", out["messages"])
	}
	if len(msgs) != 0 {
		t.Errorf("应为空数组，实际 %#v", msgs)
	}
}

// 只有 system 也要能转（没有 messages 段）。
func TestRequestSystemOnly(t *testing.T) {
	out := mustReq(t,
		`{"model":"m","messages":[{"role":"system","content":"只有系统提示"}]}`)
	if out["system"] != "只有系统提示" {
		t.Errorf("system 未抽出: %#v", out["system"])
	}
	if msgs, _ := out["messages"].([]any); len(msgs) != 0 {
		t.Errorf("system 抽走后 messages 应为空，实际 %#v", msgs)
	}
}

// 空的 assistant 消息（无文本无工具）应被跳过。
//
// Anthropic 不接受 content 为空的消息 —— 传过去会 400。
func TestRequestEmptyAssistantMessageSkipped(t *testing.T) {
	out := mustReq(t,
		`{"model":"m","messages":[{"role":"user","content":"a"},{"role":"assistant","content":null},{"role":"user","content":"b"}]}`)
	for _, m := range messagesOf(t, out) {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			t.Errorf("空的 assistant 消息应被跳过，实际仍在: %#v", mm)
		}
		if blocks, ok := mm["content"].([]any); ok && len(blocks) == 0 {
			t.Errorf("content 为空的 block 数组会上游 400: %#v", mm)
		}
	}
}

// arguments 是非法 JSON 时**不能丢信息**。
//
// 丢掉会让模型看到"调用了工具但没参数"，而错误排查会指向模型幻觉。
// 保留原始串至少能让上游报一个具体的错。
func TestRequestToolArgumentsInvalidJSONKeepsRaw(t *testing.T) {
	in := `{
	  "model":"m",
	  "messages":[
	    {"role":"user","content":"x"},
	    {"role":"assistant","content":null,"tool_calls":[
	      {"id":"c1","type":"function","function":{"name":"f","arguments":"{不是合法json"}}
	    ]}
	  ]
	}`
	out := mustReq(t, in)
	msgs := messagesOf(t, out)
	blocks := msgs[1].(map[string]any)["content"].([]any)
	var input map[string]any
	for _, b := range blocks {
		if bm := b.(map[string]any); bm["type"] == "tool_use" {
			input, _ = bm["input"].(map[string]any)
		}
	}
	if input == nil {
		t.Fatal("未找到 tool_use block")
	}
	if _, has := input["_raw"]; !has {
		t.Errorf("非法 arguments 应被保留在 _raw 里，实际 %#v", input)
	}
}

// ---- 非流式响应转换 ----

// 文本响应 + usage 字段名映射。
func TestResponseTextAndUsageMapping(t *testing.T) {
	in := `{
	  "id":"msg_01","type":"message","role":"assistant","model":"GLM-5.3",
	  "content":[{"type":"text","text":"你好"}],
	  "stop_reason":"end_turn",
	  "usage":{"input_tokens":10,"output_tokens":5}
	}`
	out := mustResp(t, in)

	if out["object"] != "chat.completion" {
		t.Errorf("object 应为 chat.completion，实际 %v", out["object"])
	}
	if out["model"] != "GLM-5.3" {
		t.Errorf("model 丢失: %v", out["model"])
	}
	choices := out["choices"].([]any)
	ch := choices[0].(map[string]any)
	if ch["finish_reason"] != "stop" {
		t.Errorf("end_turn 应映射成 stop，实际 %v", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("role 应为 assistant，实际 %v", msg["role"])
	}
	if msg["content"] != "你好" {
		t.Errorf("content 不对: %#v", msg["content"])
	}

	// ⚠ usage 字段名必须**改名**：Anthropic 用 input/output_tokens，
	// OpenAI 用 prompt/completion_tokens。不改名会让所有客户端的
	// 用量统计读成 0（而且不报错）。
	usage, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage 缺失: %#v", out["usage"])
	}
	if n, _ := toFloat(usage["prompt_tokens"]); n != 10 {
		t.Errorf("prompt_tokens 应为 10（来自 input_tokens），实际 %#v", usage["prompt_tokens"])
	}
	if n, _ := toFloat(usage["completion_tokens"]); n != 5 {
		t.Errorf("completion_tokens 应为 5（来自 output_tokens），实际 %#v", usage["completion_tokens"])
	}
	if n, _ := toFloat(usage["total_tokens"]); n != 15 {
		t.Errorf("total_tokens 应为 15，实际 %#v", usage["total_tokens"])
	}
}

// 工具调用的响应转换：input 对象 → arguments **字符串**。
func TestResponseToolUseBecomesToolCalls(t *testing.T) {
	in := `{
	  "id":"msg_02","type":"message","role":"assistant","model":"m",
	  "content":[
	    {"type":"text","text":"我来查"},
	    {"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京","days":3}}
	  ],
	  "stop_reason":"tool_use",
	  "usage":{"input_tokens":8,"output_tokens":20}
	}`
	out := mustResp(t, in)
	ch := out["choices"].([]any)[0].(map[string]any)

	if ch["finish_reason"] != "tool_calls" {
		t.Errorf("stop_reason tool_use 应映射成 tool_calls，实际 %v", ch["finish_reason"])
	}
	msg := ch["message"].(map[string]any)
	if msg["content"] != "我来查" {
		t.Errorf("文本部分丢失: %#v", msg["content"])
	}
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls 缺失: %#v", msg["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "toolu_1" {
		t.Errorf("tool call id 不对: %v", tc["id"])
	}
	if tc["type"] != "function" {
		t.Errorf("OpenAI 的 tool_call type 必须是 function，实际 %v", tc["type"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("function.name 不对: %v", fn["name"])
	}
	// ⚠ arguments 必须是 **JSON 字符串**（OpenAI 的约定），
	// 而且要能解回原对象（保证没有 double-encode）。
	argsStr, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments 必须是字符串（OpenAI 约定），实际 %#v", fn["arguments"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v（原文 %q）", err, argsStr)
	}
	if args["city"] != "北京" {
		t.Errorf("arguments 内容不对: %#v", args)
	}
	if n, _ := toFloat(args["days"]); n != 3 {
		t.Errorf("arguments 丢了数字字段: %#v", args)
	}
}

// 纯工具调用时 content 应为 null（不是空串）。
//
// OpenAI 客户端据此判断"这条消息没有文本"；空串会让一些客户端
// 渲染一个空的文本气泡。
func TestResponseToolOnlyHasNullContent(t *testing.T) {
	in := `{"id":"m","type":"message","role":"assistant","model":"m",
	  "content":[{"type":"tool_use","id":"t1","name":"f","input":{}}],
	  "stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	out := mustResp(t, in)
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != nil {
		t.Errorf("纯工具调用的 content 应为 null，实际 %#v", msg["content"])
	}
}

// thinking block → reasoning_content。
func TestResponseThinkingBecomesReasoningContent(t *testing.T) {
	in := `{"id":"m","type":"message","role":"assistant","model":"m",
	  "content":[{"type":"thinking","thinking":"让我想想…"},{"type":"text","text":"答案"}],
	  "stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	out := mustResp(t, in)
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "让我想想…" {
		t.Errorf("thinking 应映射成 reasoning_content，实际 %#v", msg["reasoning_content"])
	}
	if msg["content"] != "答案" {
		t.Errorf("正文应被保留，实际 %#v", msg["content"])
	}
}

// stop_reason 的完整映射。
func TestResponseStopReasonMapping(t *testing.T) {
	cases := []struct {
		stop string
		want string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"refusal", "content_filter"},
		{"未知原因", "stop"}, // 未知值按 stop（不能报一个 OpenAI 不认识的值）
		{"", "stop"},
	}
	for _, tc := range cases {
		in := `{"id":"m","type":"message","role":"assistant","model":"m",
		  "content":[{"type":"text","text":"x"}],"stop_reason":"` + tc.stop + `"}`
		out := mustResp(t, in)
		got := out["choices"].([]any)[0].(map[string]any)["finish_reason"]
		if got != tc.want {
			t.Errorf("stop_reason %q → %v，期望 %q", tc.stop, got, tc.want)
		}
	}
}

// 上游错误体必须**原样透传**（出口层要按业务码判）。
func TestResponseErrorPassthrough(t *testing.T) {
	in := `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`
	out, err := anthropicJSONToOpenAI([]byte(in))
	if err != nil {
		t.Fatalf("错误体不该导致转换失败: %v", err)
	}
	// 原样（不能被重包装成 chat.completion）。
	m := mustJSON(t, out)
	if m["type"] != "error" {
		t.Errorf("错误体被重包装了 —— 出口层会拿不到业务码: %#v", m)
	}
}

// 已经是 OpenAI 形态的响应体原样透传（兼容层会这样）。
func TestResponseAlreadyOpenAIPassthrough(t *testing.T) {
	in := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	out, err := anthropicJSONToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, out)
	if m["object"] != "chat.completion" {
		t.Errorf("已是 OpenAI 形态的体不该被改动: %#v", m)
	}
	if m["id"] != "x" {
		t.Errorf("id 被改动了: %v", m["id"])
	}
}

// 非法 JSON 要报错。
func TestResponseRejectsMalformedBody(t *testing.T) {
	if _, err := anthropicJSONToOpenAI([]byte(`<html>502</html>`)); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// 超长内容不丢字（考验 bufio 缓冲与字符串拼接）。
func TestResponseLongContentNotTruncated(t *testing.T) {
	long := strings.Repeat("很长的内容", 20000) // ~100KB 字符
	body, err := json.Marshal(map[string]any{
		"id": "m", "type": "message", "role": "assistant", "model": "m",
		"content":     []any{map[string]any{"type": "text", "text": long}},
		"stop_reason": "end_turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	out2, err := anthropicJSONToOpenAI(body)
	if err != nil {
		t.Fatalf("响应转换失败: %v", err)
	}
	out := mustJSON(t, out2)
	got := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if len(got) != len(long) {
		t.Errorf("内容长度变了：%d → %d", len(long), len(got))
	}
	if got != long {
		t.Error("内容被改动了")
	}
}

// ---- 流式转换 ----

// 完整的文本流：role 首帧 → 文本增量 → finish_reason → [DONE]。
func TestStreamTextConversion(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"GLM-5.3","usage":{"input_tokens":10,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	out := sb.String()

	frames := parseSSEFrames(t, out)

	// 必须有 [DONE] 收尾（缺了 OpenAI 客户端会一直等）。
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("缺少 data: [DONE] 收尾 —— 客户端会挂住")
	}

	// 第一帧必须带 role: assistant（客户端靠它初始化消息）。
	first := frames[0]
	delta := first["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["role"] != "assistant" {
		t.Errorf("首帧应带 role:assistant，实际 %#v", delta)
	}

	// 文本增量要拼成 "Hello world"。
	var text string
	for _, f := range frames {
		d := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := d["content"].(string); ok {
			text += s
		}
	}
	if text != "Hello world" {
		t.Errorf("文本增量拼出来是 %q，期望 %q", text, "Hello world")
	}

	// model 要透传。
	if frames[0]["model"] != "GLM-5.3" {
		t.Errorf("model 未透传: %v", frames[0]["model"])
	}
	// object 必须是 chunk。
	if frames[0]["object"] != "chat.completion.chunk" {
		t.Errorf("object 应为 chat.completion.chunk，实际 %v", frames[0]["object"])
	}

	// 末帧（[DONE] 之前）要有 finish_reason。
	last := frames[len(frames)-1]
	fr := last["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if fr != "stop" {
		t.Errorf("收尾帧的 finish_reason 应为 stop，实际 %v", fr)
	}
	// usage 应在收尾帧（out+in 都齐了）。
	if u, ok := last["usage"].(map[string]any); ok {
		if n, _ := toFloat(u["prompt_tokens"]); n != 10 {
			t.Errorf("usage.prompt_tokens 应为 10，实际 %v", u["prompt_tokens"])
		}
		if n, _ := toFloat(u["completion_tokens"]); n != 5 {
			t.Errorf("usage.completion_tokens 应为 5，实际 %v", u["completion_tokens"])
		}
	} else {
		t.Error("收尾帧应带 usage")
	}
}

// 工具调用的流式转换：分片参数必须**累加拼接**。
//
// # 这是流式里最容易错的一处
//
// Anthropic 把工具参数切成多个 `input_json_delta.partial_json` 片段
// （每片是**不完整的 JSON**，比如 `{"a":` / `1}`）。
// OpenAI 侧约定客户端自己累加 `arguments` —— 所以我们必须**原样发片段**，
// 不能合并也不能解析。一旦我们自己合并（或丢掉中间片），
// 客户端拼出来的 JSON 就坏了，表现为"工具调用参数解析失败"。
func TestStreamToolCallArgumentsAccumulate(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"m","usage":{"input_tokens":3,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	frames := parseSSEFrames(t, sb.String())

	// 首帧应带 id + name，arguments 为空。
	var (
		gotID, gotName string
		args           strings.Builder
		sawStart       bool
	)
	for _, f := range frames {
		d := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		tcs, ok := d["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tcv := range tcs {
			tc := tcv.(map[string]any)
			if id, ok := tc["id"].(string); ok && id != "" {
				gotID = id
				sawStart = true
			}
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if n, ok := fn["name"].(string); ok && n != "" {
				gotName = n
			}
			if a, ok := fn["arguments"].(string); ok {
				args.WriteString(a)
			}
		}
	}

	if !sawStart {
		t.Fatal("未发出带 id 的首帧 —— 客户端拿不到工具调用 id")
	}
	if gotID != "toolu_9" {
		t.Errorf("工具调用 id 不对: %q", gotID)
	}
	if gotName != "get_weather" {
		t.Errorf("工具名不对: %q", gotName)
	}

	// ⚠ 累加后的 arguments 必须是一个**合法 JSON**。
	// 这条断言同时守住了"片段没被丢"与"没被错误合并"。
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args.String()), &parsed); err != nil {
		t.Fatalf("累加后的 arguments 不是合法 JSON: %v（原文 %q）", err, args.String())
	}
	if parsed["city"] != "北京" {
		t.Errorf("arguments 内容不对: %#v", parsed)
	}

	// finish_reason 应为 tool_calls。
	last := frames[len(frames)-1]
	if fr := last["choices"].([]any)[0].(map[string]any)["finish_reason"]; fr != "tool_calls" {
		t.Errorf("stop_reason tool_use 应映射成 tool_calls，实际 %v", fr)
	}
}

// ping 事件必须被忽略（OpenAI 没有心跳），且**不能中断解析**。
func TestStreamPingIgnoredWithoutBreaking(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
		``,
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("ping 不该导致失败: %v", err)
	}
	if !strings.Contains(sb.String(), `"A"`) {
		t.Error("ping 之后的文本增量丢了 —— 心跳打断了流")
	}
	if !strings.Contains(sb.String(), "data: [DONE]") {
		t.Error("流未正常收尾")
	}
}

// 未知事件类型要**跳过并继续**（上游加新事件时不该整条流挂掉）。
func TestStreamUnknownEventsAreSkipped(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
		``,
		`event: some_future_event`,
		`data: {"type":"some_future_event","surprise":true}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"还在"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("未知事件不该导致失败: %v", err)
	}
	if !strings.Contains(sb.String(), "还在") {
		t.Error("未知事件之后的增量丢了")
	}
}

// 畸形帧（非 JSON）要跳过，不影响后续。
func TestStreamMalformedFrameIsSkipped(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
		``,
		`data: {这不是json`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"活着"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("畸形帧不该导致失败: %v", err)
	}
	if !strings.Contains(sb.String(), "活着") {
		t.Error("畸形帧之后的增量丢了")
	}
}

// `data:` 与 `data: ` 两种形态都要认（SSE 规范里空格可选）。
//
// # 这条是本仓实测踩过的坑
//
// 早先的规定只认 `data: `（带空格），于是某个上游的每一帧都匹配不上 ——
// 症状按"流式与否"劈成两半：stream:true 看着还好（帧被原样透传），
// stream:false 恒 502。
func TestStreamAcceptsDataWithoutSpace(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data:{"type":"message_start","message":{"id":"m","model":"m"}}`,
		``,
		`event: content_block_delta`,
		`data:{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"无空格"}}`,
		``,
		`event: message_stop`,
		`data:{"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("无空格的 data: 不该导致失败: %v", err)
	}
	if !strings.Contains(sb.String(), "无空格") {
		t.Error("`data:{...}`（无空格）形态未被识别 —— 这正是本仓实测过的 bug 形态")
	}
}

// 上游中途报错 → 转成 OpenAI 的 error 帧，且仍要收尾。
func TestStreamErrorFrameThenDone(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"忙"}}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("error 事件不该导致函数返回错误: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "overloaded_error") {
		t.Error("错误信息丢了 —— 客户端看不到失败原因")
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("出错后仍要发 [DONE] 收尾，否则客户端挂住")
	}
}

// 空流（上游直接断了）也要给出合法收尾，且**不伪造内容**。
func TestStreamEmptyGivesValidTerminator(t *testing.T) {
	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(""), &sb); err != nil {
		t.Fatalf("空流不该报错: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("空流也要发 [DONE]")
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Error("空流也要发 role 首帧（客户端靠它初始化）")
	}
	// 不能有正文内容 —— 我们是终端，不编数据。
	frames := parseSSEFrames(t, out)
	for _, f := range frames {
		d := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := d["content"].(string); ok && s != "" {
			t.Errorf("空流不该产出内容，实际 %q", s)
		}
	}
}

// thinking_delta → reasoning_content。
func TestStreamThinkingDelta(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"思考中"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("失败: %v", err)
	}
	frames := parseSSEFrames(t, sb.String())
	var reasoning string
	for _, f := range frames {
		d := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := d["reasoning_content"].(string); ok {
			reasoning += s
		}
	}
	if reasoning != "思考中" {
		t.Errorf("thinking_delta 应映射成 reasoning_content，实际 %q", reasoning)
	}
}

// 只有 data 行没有 event: 行时也要能工作（靠 data 里的 type）。
func TestStreamWorksWithoutEventLines(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"无事件行"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if !strings.Contains(sb.String(), "无事件行") {
		t.Error("缺 event: 行时增量丢了 —— 只能靠 data.type 判定")
	}
}

// 文本块与工具块混在同一响应里时，tool_calls 的 index 要**独立编号**。
//
// ⚠ Anthropic 的 index 是 content block 序号（文本块也占位），
// 而 OpenAI 的 tool_calls 数组只数工具。直接透传 index 会让
// 第一个工具调用拿到 index=1，某些客户端据此去数组里找 index=1 的元素
// 而数组只有 1 项 → 参数丢失。
func TestStreamToolCallIndexIsIndependentOfBlockIndex(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m","model":"m"}}`,
		``,
		// 文本块占 index 0
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"先说一句"}}`,
		``,
		// 工具块占 index 1，但应当是 tool_calls[0]
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_a","name":"f","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var sb strings.Builder
	if err := anthropicSSEToOpenAI(strings.NewReader(sse), &sb); err != nil {
		t.Fatalf("失败: %v", err)
	}
	frames := parseSSEFrames(t, sb.String())
	for _, f := range frames {
		d := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		tcs, ok := d["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tcv := range tcs {
			tc := tcv.(map[string]any)
			// 若透传了块序号，这里会是 1。
			if n, _ := toFloat(tc["index"]); n != 0 {
				t.Errorf("tool_calls[].index = %v，应为 0（不能透传 Anthropic 的块序号 1）—— "+
					"客户端会去数组里找不存在的 index=1", tc["index"])
			}
		}
	}
}

// parseSSEFrames 解析产出的 SSE，返回所有 data 帧（跳过 [DONE]）。
func parseSSEFrames(t *testing.T, sse string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("产出的帧不是合法 JSON: %v\n原文: %s", err, payload)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		t.Fatalf("没有产出任何 data 帧:\n%s", sse)
	}
	return out
}

// mustReq 把 OpenAI 请求体转成 Anthropic（失败即 Fatal）。
//
// 为什么不写成 mustConv(t, openAIToAnthropic(x))：Go 不允许把多值返回
// 展开成另一个函数的参数，所以助手要接**输入**而不是接结果。
func mustReq(t *testing.T, openAIBody string) map[string]any {
	t.Helper()
	out, err := openAIToAnthropic([]byte(openAIBody))
	if err != nil {
		t.Fatalf("请求转换失败: %v", err)
	}
	return mustJSON(t, out)
}

// mustResp 把 Anthropic 响应体转成 OpenAI（失败即 Fatal）。
func mustResp(t *testing.T, anthropicBody string) map[string]any {
	t.Helper()
	out, err := anthropicJSONToOpenAI([]byte(anthropicBody))
	if err != nil {
		t.Fatalf("响应转换失败: %v", err)
	}
	return mustJSON(t, out)
}
