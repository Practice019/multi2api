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

// TestHasOwnTools 客户端自带 tools 时不该接管。
//
// # 为什么这条判据重要
//
// 客户端带 tools 说明它**懂协议**、要自己驱动工具循环（agent 框架都这么干）。
// 网关再插一脚替它执行，两边会打架：客户端按自己的期待仍在等 tool_calls，
// 却收到了"已经执行完的结果"。
//
// 所以：不带的（普通聊天客户端）→ 网关接管；带的 → 只转发。
func TestHasOwnTools(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"只有 messages", `{"model":"m","messages":[]}`, false},
		{"带 tools", `{"model":"m","tools":[{"type":"function"}]}`, true},
		{"tool_choice 是空对象（等于没带）", `{"model":"m","tool_choice":{}}`, false},
		{"tools 是 null（等于没带）", `{"model":"m","tools":null}`, false},
		{"带 tool_choice", `{"model":"m","tool_choice":"auto"}`, true},
		{"tools 是空数组（等于没带）", `{"model":"m","tools":[]}`, false},
		{"非法 JSON", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasOwnTools([]byte(tc.body)); got != tc.want {
				t.Errorf("hasOwnTools(%s) = %v，want %v", tc.body, got, tc.want)
			}
		})
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
	}})
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
	// ② stream 被强制 false（循环内部要完整响应）
	if v, ok := obj["stream"].(bool); !ok || v {
		t.Errorf("stream = %v，want false —— 循环内部必须拿完整响应", obj["stream"])
	}
	// ③ 原有字段没被动过
	if obj["model"] != "m" {
		t.Errorf("model 被改坏了: %v", obj["model"])
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
