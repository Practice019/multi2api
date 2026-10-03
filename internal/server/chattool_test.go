package server

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestToolCallsOfAcceptsBothShapes 必须认**两种**形状。
//
// # 这条是本次开发踩到的**静默失败**，必须钉死
//
// `wire.Aggregate` 把 tool_calls 放进 message 时用的是
// **`[]map[string]any`**（见 wire/sse.go 的 `make([]map[string]any, …)`），
// 而不是 JSON 反序列化后的 `[]any`。
//
// 我第一版只断言 `.([]any)`，后果（实测）：
//
//	上游确实回了 tool_calls（37 KB 流、finish_reason=tool_calls）
//	聚合也成功（message 里躺着 1 个调用）
//	**但断言失败 → 0 个调用 → 循环判成"模型没调工具" → 直接收尾**
//
// 客户端收到的是 finish_reason=tool_calls 却没有任何工具结果 ——
// 看起来像"上游不支持工具"，真相只是一个类型写错。
// 全程没有任何报错、没有日志线索。
//
// 所以两种形状都要有测试：少认一种 = 这条 bug 复活。
func TestToolCallsOfAcceptsBothShapes(t *testing.T) {
	one := map[string]any{
		"id":       "call_1",
		"type":     "function",
		"function": map[string]any{"name": "generate_image", "arguments": `{"prompt":"猫"}`},
	}

	t.Run("Aggregate 形态 []map[string]any", func(t *testing.T) {
		msg := map[string]any{"tool_calls": []map[string]any{one}}
		got := toolCallsOf(msg)
		if len(got) != 1 {
			t.Fatalf("认不出 []map[string]any（%d 个）—— 这正是那条静默失败：\n"+
				"wire.Aggregate 用的就是这个形态，认不出等于工具循环永远不执行", len(got))
		}
	})
	t.Run("JSON 反序列化形态 []any", func(t *testing.T) {
		msg := map[string]any{"tool_calls": []any{one}}
		got := toolCallsOf(msg)
		if len(got) != 1 {
			t.Fatalf("认不出 []any（%d 个）", len(got))
		}
	})
	t.Run("没有 tool_calls", func(t *testing.T) {
		if got := toolCallsOf(map[string]any{"content": "hi"}); len(got) != 0 {
			t.Errorf("没有 tool_calls 时应返回空，得到 %d", len(got))
		}
	})
	t.Run("nil message", func(t *testing.T) {
		if got := toolCallsOf(nil); len(got) != 0 {
			t.Errorf("nil message 应返回空，得到 %d", len(got))
		}
	})
}

// TestParseToolCallsExtractsFields 从两种形状里都要取出 id/name/arguments。
func TestParseToolCallsExtractsFields(t *testing.T) {
	one := map[string]any{
		"id":       "call_abc",
		"type":     "function",
		"function": map[string]any{"name": "web_search", "arguments": `{"query":"Go 1.26"}`},
	}
	for _, shape := range []struct {
		name string
		val  any
	}{
		{"[]map[string]any", []map[string]any{one}},
		{"[]any", []any{one}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			calls := parseToolCalls(map[string]any{"tool_calls": shape.val})
			if len(calls) != 1 {
				t.Fatalf("应当解析出 1 个调用，实际 %d", len(calls))
			}
			c := calls[0]
			if c.ID != "call_abc" {
				t.Errorf("id = %q", c.ID)
			}
			if c.Name != "web_search" {
				t.Errorf("name = %q", c.Name)
			}
			// arguments 是 JSON **字符串**（协议里套了一层）。
			// 原样带走由工具解析 —— 这里只确认没被截断/变形。
			var args struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(c.Args, &args); err != nil {
				t.Fatalf("arguments 不是合法 JSON: %v（原值 %s）", err, c.Args)
			}
			if args.Query != "Go 1.26" {
				t.Errorf("query = %q", args.Query)
			}
		})
	}
}

// TestExistingToolNames 摘客户端已有的工具名（合并时以它为准）。
//
// # 为什么这条重要
//
// agent 框架的工具名与我们的撞车概率很高（`web_search` 几乎是标配）。
// 上游遇到**重名**工具会直接 400（duplicate tool name），
// 而那个错误看起来与生图毫无关系，排查时会绕远路。
//
// 返回 nil 的情形也要对：没带工具、空数组、畸形 JSON。
func TestExistingToolNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"没带 tools", `{"model":"m","messages":[]}`, nil},
		{"空数组", `{"model":"m","tools":[]}`, nil},
		{"畸形 JSON", `{`, nil},
		{"嵌套形态（OpenAI 标准）", `{"tools":[{"type":"function","function":{"name":"bash"}}]}`, []string{"bash"}},
		{"扁平形态", `{"tools":[{"name":"read","type":"function"}]}`, []string{"read"}},
		{"多个", `{"tools":[{"function":{"name":"bash"}},{"function":{"name":"web_search"}}]}`, []string{"bash", "web_search"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := existingToolNames([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("得到 %d 个名字 %v，want %d 个 %v", len(got), got, len(tc.want), tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("缺名字 %q（得到 %v）", w, got)
				}
			}
		})
	}
}

// TestPartitionToolCalls 按归属把调用分成"我们的"与"客户端的"。
//
// # 这是"agent 框架零改动"的核心判据
//
//	我们的   → 网关自己执行，**不告诉客户端**
//	客户端的 → 原样透传，客户端执行
//
// 第一版没有这个分工：只要发现任何 tool_calls 就全部拿来执行，
// 于是 agent 框架的工具名被送到 ExecuteChatTool → "未知工具名"错误。
// 对 DSH 来说就是它的 bash 永远不执行（而现象看起来像"工具坏了"）。
func TestPartitionToolCalls(t *testing.T) {
	mine := map[string]bool{"generate_image": true, "web_search": true}

	t.Run("只有我们的", func(t *testing.T) {
		ours, theirs := partitionToolCalls([]toolCall{
			{ID: "1", Name: "generate_image"},
		}, mine)
		if len(ours) != 1 || len(theirs) != 0 {
			t.Errorf("ours=%d theirs=%d，want 1/0", len(ours), len(theirs))
		}
	})
	t.Run("只有客户端的", func(t *testing.T) {
		ours, theirs := partitionToolCalls([]toolCall{
			{ID: "1", Name: "bash"},
		}, mine)
		if len(ours) != 0 || len(theirs) != 1 {
			t.Errorf("ours=%d theirs=%d，want 0/1", len(ours), len(theirs))
		}
	})
	t.Run("混在一起（模型一轮里既想画图又想跑命令）", func(t *testing.T) {
		ours, theirs := partitionToolCalls([]toolCall{
			{ID: "1", Name: "bash"},
			{ID: "2", Name: "generate_image"},
			{ID: "3", Name: "read"},
		}, mine)
		if len(ours) != 1 {
			t.Errorf("ours=%d，want 1（只有 generate_image）", len(ours))
		}
		if len(theirs) != 2 {
			t.Errorf("theirs=%d，want 2（bash + read）", len(theirs))
		}
		if ours[0].Name != "generate_image" {
			t.Errorf("ours[0] = %q", ours[0].Name)
		}
	})
	t.Run("空输入", func(t *testing.T) {
		ours, theirs := partitionToolCalls(nil, mine)
		if len(ours) != 0 || len(theirs) != 0 {
			t.Errorf("空输入应当两组都空")
		}
	})
}

// TestInjectToolsMerges 注入必须是**合并**，不能替换客户端的工具。
//
// # 这条是本次最关键的一条（第一版写成替换，是真 bug）
//
// 第一版是 `obj["tools"] = wireTools` —— 那是**替换**。
// 当时它不出问题，只因为那条"客户端带 tools 就不接管"的判据
// 挡住了所有带工具的客户端。判据一删，替换就会**抹掉 agent 框架的
// 全部工具**（bash / read / write …）—— 模型再也执行不了任何命令，
// 而现象是"DSH 突然变笨了"，与本功能毫无表面关联。
func TestInjectToolsMerges(t *testing.T) {
	in := []byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"bash","description":"DSH 自己的"}}]}`)
	out, err := injectTools(in, []gateway.ChatTool{{
		Name: "generate_image", Description: "生图",
	}}, map[string]bool{"bash": true})
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("注入后不是合法 JSON: %v", err)
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("工具数 = %d，want 2（客户端的 bash + 我们的 generate_image）—— "+
			"少于 2 说明替换掉了客户端的工具，agent 框架会瘫痪", len(tools))
	}
	names := map[string]bool{}
	for _, it := range tools {
		m, _ := it.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if n, _ := fn["name"].(string); n != "" {
			names[n] = true
		}
	}
	if !names["bash"] {
		t.Error("客户端的 bash 被抹掉了 —— 这正是替换 vs 合并的 bug")
	}
	if !names["generate_image"] {
		t.Error("我们的 generate_image 没注入")
	}
}

// TestInjectToolsSkipsDuplicateNames 重名时以客户端为准（不注入我们的）。
//
// 上游遇到重名工具会直接 400（duplicate tool name），
// 而那个错误看起来与生图毫无关系。
func TestInjectToolsSkipsDuplicateNames(t *testing.T) {
	// 客户端已经有一个 web_search（agent 框架的标配）
	in := []byte(`{"model":"m","tools":[{"type":"function","function":{"name":"web_search","description":"客户端自己的搜索"}}]}`)
	out, err := injectTools(in, []gateway.ChatTool{
		{Name: "web_search", Description: "我们的搜索"},
		{Name: "generate_image", Description: "生图"},
	}, map[string]bool{"web_search": true})
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	tools, _ := obj["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("工具数 = %d，want 2（bash 的那个 web_search + generate_image）", len(tools))
	}
	// web_search 只出现一次，且**是客户端那个**（描述为"客户端自己的搜索"）
	count := 0
	for _, it := range tools {
		m, _ := it.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if fn["name"] == "web_search" {
			count++
			if fn["description"] != "客户端自己的搜索" {
				t.Errorf("web_search 的 description = %v，want 客户端那个（重名应以客户端为准）", fn["description"])
			}
		}
	}
	if count != 1 {
		t.Errorf("web_search 出现 %d 次，want 1（重名会让上游 400）", count)
	}
}

// TestInjectToolsShape 注入的工具定义必须是 OpenAI 的那个形状。
//
// 形状错了上游会 400（"tools[0].function.name is required" 之类），
// 而那看起来像"模型不支持工具"。
func TestInjectToolsShape(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out, err := injectTools(in, []gateway.ChatTool{{
		Name:        "generate_image",
		Description: "生成图片",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`),
	}}, nil)
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("注入后不是合法 JSON: %v", err)
	}
	// ① tools 是数组、每项 {type:"function", function:{name,description,parameters}}
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools 不是非空数组: %T", obj["tools"])
	}
	item, _ := tools[0].(map[string]any)
	if item["type"] != "function" {
		t.Errorf("type = %v，want function", item["type"])
	}
	fn, _ := item["function"].(map[string]any)
	if fn == nil {
		t.Fatal("缺 function 对象")
	}
	if fn["name"] == "" || fn["name"] == nil {
		t.Error("function.name 为空（上游会 400）")
	}
	if _, ok := fn["parameters"]; !ok {
		t.Error("缺 function.parameters（模型没法知道参数形状）")
	}
	// ② 原有字段没被动过
	if obj["model"] != "m" {
		t.Errorf("model 被改坏了: %v", obj["model"])
	}
	// ③ **不**动 stream：那个字段还要用来决定最后的发射形态。
	//    （第一版在这里强制 stream=false，是错的 —— 见 injectTools 注释。）
	if _, exists := obj["stream"]; exists {
		t.Errorf("不该动 stream 字段（客户端没发它，注入也不该造一个）：%v", obj["stream"])
	}
}

// TestAppendToolMessagesShape 追加的消息必须严格符合 OpenAI 协议。
//
// 少一条 tool 消息、或 tool_call_id 对不上，上游会 400
// （"tool_calls must be followed by tool messages"）—— 那是硬失败。
func TestAppendToolMessagesShape(t *testing.T) {
	var obj map[string]any
	_ = json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"画只猫"}]}`), &obj)

	msg := map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "generate_image", "arguments": `{}`},
		}},
	}
	results := []toolExecResult{
		{callID: "call_1", name: "generate_image", content: "图片已生成"},
	}
	if err := appendToolMessages(obj, msg, results); err != nil {
		t.Fatalf("追加失败: %v", err)
	}

	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("消息数 = %d，want 3（原始 1 + assistant 1 + tool 1）", len(msgs))
	}
	// 顺序：原始 → assistant(tool_calls) → tool
	asst, _ := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("第 2 条应是 assistant，实际 %v", asst["role"])
	}
	if _, ok := asst["tool_calls"]; !ok {
		t.Error("assistant 消息丢了 tool_calls —— tool 消息靠它回指")
	}
	if _, ok := asst["content"]; !ok {
		t.Error("assistant 消息缺 content 字段（协议要求存在，可为 null）")
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Errorf("第 3 条应是 tool，实际 %v", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %v，want call_1（对不上上游会 400）", toolMsg["tool_call_id"])
	}
}

// TestAppendArtifactsToResponse 产物要**同时**进正文 markdown 与结构化字段。
//
// # 用户的要求
//
// 「写进回复内容（markdown）+ 附结构化字段」
//
// 两个都要有：
//
//	markdown    给**人**看（任何 markdown 渲染器直接出图）
//	结构化字段  给**程序**读（调用的代码要拿 URL/扣分）
//
// 而且 markdown 必须从结构化字段里**剥掉**（否则调用方要纠结用哪个）。
func TestAppendArtifactsToResponse(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "画好了"},
		}},
	}
	arts := []gateway.Artifact{{
		"type":     "image",
		"url":      "https://example.invalid/a.png",
		"markdown": "![猫](https://example.invalid/a.png)",
	}}

	if err := appendArtifactsToResponse(resp, arts); err != nil {
		t.Fatalf("追加产物失败: %v", err)
	}

	// ① 正文里要有 markdown（用户要的"写进回复内容"）
	choices, _ := resp["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "![猫](") {
		t.Errorf("正文里没有图片 markdown —— 用户要求的是『写进回复内容』：%q", content)
	}
	if !strings.Contains(content, "画好了") {
		t.Errorf("模型原本的话被覆盖了：%q", content)
	}

	// ② 结构化字段要在**响应顶层**（不是 message 里）
	rawArts, ok := resp[gateway.ArtifactKey].([]gateway.Artifact)
	if !ok || len(rawArts) == 0 {
		t.Fatalf("缺 %s 顶层字段", gateway.ArtifactKey)
	}
	if rawArts[0]["url"] == nil {
		t.Error("结构化产物丢了 url")
	}
	// ③ markdown 要被剥掉（它已经在正文里了）
	if _, dup := rawArts[0]["markdown"]; dup {
		t.Error("结构化字段里还留着 markdown —— 与正文重复，调用方要纠结用哪个")
	}
}

// TestAppendArtifactsEmptyContentUsesMarkdown 模型没说话时正文应当是图片本身。
//
// 模型可能只调工具就收尾（content 为空）。那时若不补，
// 客户端会显示一个**空回复** —— 图片其实生成好了，但用户看不到。
func TestAppendArtifactsEmptyContentUsesMarkdown(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": ""},
		}},
	}
	arts := []gateway.Artifact{{
		"type":     "image",
		"markdown": "![猫](https://example.invalid/a.png)",
	}}
	if err := appendArtifactsToResponse(resp, arts); err != nil {
		t.Fatalf("失败: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "![猫](") {
		t.Errorf("模型没说话时正文应当是图片 markdown（否则用户看到空回复）：%q", content)
	}
}

// TestAppendArtifactsNoop 没有产物时响应不该被改动。
//
// 这是**向后兼容**的判据：普通聊天（不触发工具）必须与改造前逐字节一致，
// 不能凭空多出 loomy_artifacts 字段或改动 content。
func TestAppendArtifactsNoop(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "原始回复"},
		}},
	}
	if err := appendArtifactsToResponse(resp, nil); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if _, extra := resp[gateway.ArtifactKey]; extra {
		t.Error("没有产物时不该出现结构化字段（普通聊天要逐字节不变）")
	}
	choices, _ := resp["choices"].([]any)
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	if msg["content"] != "原始回复" {
		t.Errorf("正文被改动了：%v", msg["content"])
	}
}
