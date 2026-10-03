// chattool.go 出口层的**工具调用循环**（上游无关）。
//
// # 做什么（用户的要求）
//
// 用户原话：「我只要使用 Loomy 的任意一个对话模型，如果我要生图的话，
// 它自动调用生图模型」。
//
// 也就是：客户端**不需要**懂工具调用。它照常发一句"帮我画只猫"，
// 网关替它：
//
//	① 注入该上游声明的工具（gateway.ChatToolExt）
//	② 发请求；若模型要调工具（finish_reason=tool_calls），网关**替它执行**
//	③ 把执行结果作为 role:"tool" 消息回喂，让模型收尾
//	④ 循环直到模型不再调工具（或到轮次上限）
//	⑤ 把最终回复发给客户端，并把工具产物（图片 URL 等）附在末尾
//
// # 为什么工具是"上游私有"的
//
// 工具定义与执行都由**上游**提供（`ChatToolExt`），核心只驱动循环。
// 所以作用域天然是"请求路由到哪个上游，就用哪个上游的工具"——
// 不需要在核心写任何 `if provider == "loomy"`：
//
//	loomy     → 实现了 ChatToolExt → 注入生图 + 搜索
//	workbuddy → 没实现            → 一个工具都不注入（行为与改造前一致）
//
// # 为什么整个循环走**非流式**
//
// 工具调用要求"先知道模型要不要调工具，再决定给客户端什么"——
// 而流式是边读边发，第一帧出去后状态码与内容都收不回来了。
// 若边流边看，客户端会先收到 tool_calls 帧（那是**协议内部**的东西，
// 客户端不懂就会显示成乱码），然后才收到最终答案。
//
// 所以本实现在内部用非流式请求跑完整循环（实测上游支持 `stream:false`），
// 最后按**客户端要的形态**发射：要流式就编成 SSE 逐帧发，
// 不要就一个 JSON。代价是"带工具的这一轮没有逐字打字机效果"——
// 这是"客户端零改动"必须付的代价，且只在真的要生图/搜索时才发生。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"workbuddy2api/internal/gateway"
)

// maxToolRounds 工具循环的最大轮次。
//
// 每轮 = 一次模型请求 + 一次工具执行。3 轮够覆盖真实用法
// （"搜一下 → 基于结果再搜一次 → 回答" 已经是最长的合理链）。
//
// ⚠ 必须有上限：模型可能陷入"反复调同一个工具"的循环
// （实测存在这种倾向，尤其工具结果不合它意时）。
// 没有上限的循环会一直烧积分 —— 那是用户看不见的损失。
const maxToolRounds = 3

// toolLoopResult 一次工具循环的最终结果。
type toolLoopResult struct {
	// Body 最终的非流式响应体（OpenAI chat.completion 形状）。
	Body []byte
	// Artifacts 本轮所有工具产物（图片/搜索结果）。
	Artifacts []gateway.Artifact
	// Rounds 实际跑了几轮（诊断用）。
	Rounds int
	// Model 上游实际使用的模型名（回填进响应，保持与其它路径一致）。
	Model string
}

// toolCapable 出口层向装配层打听"这个上游有哪些工具、能不能执行"。
//
// # 为什么用接口断言而不是 `ExtOf[ChatToolExt](p)`
//
// 出口层**拿不到 `gateway.Provider`** —— 它只有上游 ID，而 ProviderRouter
// 接口刻意不暴露内部实例（见 handler.Config.Provider 的注释：
// 出口层不应认识任何具体上游）。所以能力发现走"问装配层"这条路，
// 与既有的 `providerIDs()` / `imageModelsOf()` 完全同形。
//
// 装配层实现它时才知道 Registry 在哪，也才有那个 Provider 实例。
type toolCapable interface {
	ChatTools(id string) ([]gateway.ChatTool, bool)
	ExecuteChatTool(ctx context.Context, id, uid, name string, args json.RawMessage) (gateway.ChatToolResult, bool, error)
}

// toolCapabilityOf 取出装配层的工具能力（没接线/没实现时 ok=false）。
func (h *Handler) toolCapabilityOf() (toolCapable, bool) {
	if h.cfg.Provider == nil {
		return nil, false
	}
	tc, ok := h.cfg.Provider.(toolCapable)
	return tc, ok
}

// chatToolsOf 问某上游有哪些可注入的工具。
//
// 返回空切片表示"该上游没有工具"（合法状态）—— 可能是它没实现扩展点，
// 也可能是它实现了但当前没有可用工具。
func (h *Handler) chatToolsOf(id string) []gateway.ChatTool {
	tc, ok := h.toolCapabilityOf()
	if !ok {
		return nil
	}
	list, ok := tc.ChatTools(id)
	if !ok {
		return nil
	}
	return list
}

// shouldRunToolLoop 报告本次请求是否该走工具循环。
//
// 判据（全部满足才走）：
//
//	① 装配层提供工具能力（本部署接了多上游）
//	② 被路由到的上游确实有工具
//	③ 请求里**没有** tools/tool_choice 字段
//	④ 未被配置显式关闭
//
// # 为什么 ③ 是"客户端自己带工具就不接管"
//
// 客户端自带 tools 说明它**懂**这个协议 —— 它期望拿到 tool_calls
// 自己去执行（这是标准用法：agent 框架都这么干）。
// 网关再插一脚替它执行，两边会打架：客户端收到的是"已经执行完的结果"，
// 而它按自己的期待仍在等 tool_calls。
//
// 所以：不带的（普通聊天客户端）→ 网关接管；带的（agent 客户端）→ 只转发。
// 这条让两类客户端都能正常工作，且各自的行为都可预期。
func (h *Handler) shouldRunToolLoop(providerID string, body []byte) (toolCapable, bool) {
	if h.cfg.DisableChatTools {
		return nil, false
	}
	tc, ok := h.toolCapabilityOf()
	if !ok {
		return nil, false
	}
	if len(h.chatToolsOf(providerID)) == 0 {
		return nil, false
	}
	if hasOwnTools(body) {
		return nil, false
	}
	return tc, true
}

// hasOwnTools 报告请求体里是否**真的**带了工具。
//
// # 为什么不能只看"字段存在"
//
// 有的客户端会显式发空容器：
//
//	{"tools":[]}            某些 SDK 初始化后就是空数组
//	{"tool_choice":{}}      同理
//
// 那与"没带工具"是同一个意思 —— 客户端没有要自己驱动工具循环。
// 若按"字段存在"判定，这些客户端就**永远拿不到自动生图**
// （而它们的 JSON 看起来"带了 tools"，排查时极难看出原因）。
//
// 所以判据是"**非空**容器"：空数组 / 空对象 / null 都算没带。
func hasOwnTools(body []byte) bool {
	var probe struct {
		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		// body 不是 JSON（畸形请求）→ 交给后续的解析路径去报错。
		// 这里返回 false 表示"不因这个判据而跳过工具循环"。
		return false
	}
	return nonEmptyJSON(probe.Tools) || nonEmptyJSON(probe.ToolChoice)
}

// nonEmptyJSON 报告一段原始 JSON 是否是"有内容的容器"。
//
// 空数组 `[]`、空对象 `{}`、`null`、以及空白都算**空**。
func nonEmptyJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null", "[]", "{}":
		return false
	}
	return true
}

// injectTools 把工具定义并进请求体。
//
// # 为什么是"并进"而不是"覆盖"
//
// 客户端没带 tools 才会走到这里（见 shouldRunToolLoop），
// 所以实际上不会覆盖任何东西。写成 append 而不是赋值，是为了
// 万一将来判据放宽，也不会静默丢掉客户端给的工具。
func injectTools(body []byte, tools []gateway.ChatTool) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	wireTools := make([]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{
			"name":        t.Name,
			"description": t.Description,
		}
		if len(t.Parameters) > 0 {
			// 参数 schema 原样塞进去：它是上游自己给的 JSON，
			// 再解析一遍只会引入"我以为它长这样"的假设。
			fn["parameters"] = json.RawMessage(t.Parameters)
		}
		wireTools = append(wireTools, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	obj["tools"] = wireTools
	// stream=false：循环内部必须拿完整响应（见文件头注释）。
	obj["stream"] = false
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// appendToolMessages 把"模型的 tool_calls + 工具结果"追加进消息历史。
//
// # 顺序与形状必须严格符合 OpenAI 协议
//
//	assistant 消息：带 tool_calls 数组（每个含 id/type/function）
//	tool 消息：每个 tool_call 一条，带 tool_call_id 指向它
//
// 少一条 tool 消息、或 id 对不上，上游会返回 400
// （"tool_calls must be followed by tool messages"）——
// 那是**硬失败**，不是"结果不理想"。
func appendToolMessages(obj map[string]any, msg map[string]any, results []toolExecResult) error {
	msgs, _ := obj["messages"].([]any)

	// ① assistant 消息原样入历史。
	//
	// ⚠ 必须保留上游给的 tool_calls 结构（含 id）—— tool 消息靠 id 回指它。
	// content 也要保留（有的模型边调工具边说一句"我来画"）。
	assistant := map[string]any{
		"role":       "assistant",
		"tool_calls": msg["tool_calls"],
	}
	if c, ok := msg["content"].(string); ok {
		assistant["content"] = c
	} else {
		// 协议要求 content 字段存在（可为 null）。缺了它有的上游会 400。
		assistant["content"] = nil
	}
	if rc, ok := msg["reasoning_content"]; ok {
		assistant["reasoning_content"] = rc
	}
	msgs = append(msgs, assistant)

	// ② 每个 tool_call 一条 tool 消息。
	for _, r := range results {
		msgs = append(msgs, map[string]any{
			"role":         "tool",
			"tool_call_id": r.callID,
			"content":      r.content,
		})
	}
	obj["messages"] = msgs
	return nil
}

// toolExecResult 一次工具执行的结果（含它对应的 call id）。
type toolExecResult struct {
	callID  string
	name    string
	content string
	// markdown/artifacts 只有最终轮才追加给客户端（见 collectArtifacts）。
	markdown  string
	artifacts []gateway.Artifact
}

// parseToolCalls 从非流式响应的 message 里取出 tool_calls。
//
// 返回空切片表示"模型没调工具"（正常收尾）。
func parseToolCalls(msg map[string]any) []struct {
	ID   string
	Name string
	Args json.RawMessage
} {
	// ⚠ 两种形状都认 —— 见下方 toolCallsOf 的注释（漏认一种就是静默失败）。
	rawList := toolCallsOf(msg)
	if len(rawList) == 0 {
		return nil
	}
	out := make([]struct {
		ID   string
		Name string
		Args json.RawMessage
	}, 0, len(rawList))
	for _, call := range rawList {
		id, _ := call["id"].(string)
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		argStr, _ := fn["arguments"].(string)
		out = append(out, struct {
			ID   string
			Name string
			Args json.RawMessage
		}{
			ID:   id,
			Name: name,
			// arguments 是 JSON **字符串**（这是 OpenAI 协议里最反直觉的一处：
			// 它在 JSON 里又编了一层）。原样带走，由工具自己解析 ——
			// 解析失败也是工具的事（它要把"参数不合法"回给模型去修）。
			Args: json.RawMessage(argStr),
		})
	}
	return out
}

// markdownForArtifacts 把产物编成追加到回复末尾的 markdown。
//
// # 为什么要追加（用户的要求）
//
// 用户要求"写进回复内容（markdown）+ 附结构化字段"。
//
// 回复正文是**模型写的**，网关改不了它的话 —— 所以做法是追加：
//
//	模型：图片已经生成好了：一只橘猫…
//	      ← 网关在这里追加 ↓
//	      ![一只橘猫](https://…png)
//
// 这样任何支持 markdown 的界面都能直接渲染出图；不支持 markdown 的
// 至少也能看到可点击的 URL（比只给一个结构化字段友好得多）。
func markdownForArtifacts(arts []gateway.Artifact) string {
	var parts []string
	for _, a := range arts {
		if s, ok := a["markdown"].(string); ok && strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// appendArtifactsToResponse 把产物与 markdown 挂进最终响应。
func appendArtifactsToResponse(resp map[string]any, artifacts []gateway.Artifact) error {
	if len(artifacts) == 0 {
		return nil
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	c0, _ := choices[0].(map[string]any)
	if c0 == nil {
		return nil
	}
	msg, _ := c0["message"].(map[string]any)
	if msg == nil {
		return nil
	}

	// ① markdown 追加到 content 末尾。
	md := markdownForArtifacts(artifacts)
	if md != "" {
		content, _ := msg["content"].(string)
		if strings.TrimSpace(content) == "" {
			// 模型没说话（只调了工具就收尾）—— 那正文就是图片本身，
			// 否则客户端会显示一个空回复。
			content = md
		} else {
			content = strings.TrimRight(content, "\n") + "\n\n" + md
		}
		msg["content"] = content
	}

	// ② 结构化产物挂在**响应顶层**（非标准字段）。
	//
	// 放顶层而不是 message 里：它是"这次网关做了什么"的记录，
	// 不是模型说的话。混进 message 会让"message 是模型输出"这个
	// 不变量被破坏（下游按协议解析的代码会看到异于上游的形状）。
	//
	// ⚠ 剥掉 markdown 字段：它已经在 content 里了，重复带一份
	// 只会让调用方纠结"该用哪个"。
	resp[gateway.ArtifactKey] = stripMarkdown(artifacts)
	return nil
}

// stripMarkdown 复制一份产物并去掉内部的 markdown 字段。
func stripMarkdown(arts []gateway.Artifact) []gateway.Artifact {
	out := make([]gateway.Artifact, 0, len(arts))
	for _, a := range arts {
		clean := make(gateway.Artifact, len(a))
		for k, v := range a {
			if k == "markdown" {
				continue
			}
			clean[k] = v
		}
		out = append(out, clean)
	}
	return out
}

// writeAsSSE 把一个**非流式**响应编成 SSE 逐帧发给客户端。
//
// # 为什么要这个（而不是要求客户端用非流式）
//
// 绝大多数客户端默认 `stream:true`。若工具循环只能返回 JSON，
// 那些客户端会因为"要的是事件流、拿到的是 JSON"而解析失败 ——
// 而它们**没有任何错**，是网关这边形态没对齐。
//
// 编出来的是标准 OpenAI 流式帧：一个首帧（role assistant + 内容）、
// 一个收尾帧（finish_reason + usage）、然后 [DONE]。
// 内容整块出现在一帧里（不是逐字），但协议上是合法的事件流。
func writeAsSSE(w http.ResponseWriter, resp map[string]any) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	id, _ := resp["id"].(string)
	if id == "" {
		id = "chatcmpl-wb2api"
	}
	model, _ := resp["model"].(string)
	created, _ := resp["created"]
	base := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
	}

	choices, _ := resp["choices"].([]any)
	var msg map[string]any
	var finish any = "stop"
	if len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			msg, _ = c0["message"].(map[string]any)
			if fr, ok := c0["finish_reason"]; ok && fr != nil {
				finish = fr
			}
		}
	}

	writeChunk := func(delta map[string]any, fin any) {
		frame := make(map[string]any, len(base)+1)
		for k, v := range base {
			frame[k] = v
		}
		frame["choices"] = []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": fin,
		}}
		b, err := json.Marshal(frame)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}

	// 首帧：role + 全部正文（含追加的图片 markdown）。
	delta := map[string]any{"role": "assistant"}
	if msg != nil {
		if c, ok := msg["content"].(string); ok && c != "" {
			delta["content"] = c
		}
		if rc, ok := msg["reasoning_content"]; ok {
			delta["reasoning_content"] = rc
		}
	}
	writeChunk(delta, nil)

	// 收尾帧：finish_reason。usage 也带上（有的客户端只在最后一帧读它）。
	frame := make(map[string]any, len(base)+2)
	for k, v := range base {
		frame[k] = v
	}
	frame["choices"] = []any{map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": finish,
	}}
	if u, ok := resp["usage"]; ok && u != nil {
		frame["usage"] = u
	}
	if b, err := json.Marshal(frame); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// toolCallsOf 把 message["tool_calls"] 归一成 []map[string]any。
//
// # ⚠ 认两种形状，少认一种就是一个**静默失败**（我踩过）
//
// `wire.Aggregate` 把 tool_calls 放进 message 时用的是
// **`[]map[string]any`**（见 wire/sse.go：`calls := make([]map[string]any, …)`），
// 而**不是** JSON 反序列化后的 `[]any`。
//
// 我第一版只断言 `.([]any)`，于是：
//
//	上游确实回了 tool_calls（37 KB 流、finish_reason=tool_calls）
//	聚合也成功（message["tool_calls"] 里躺着 1 个调用）
//	**但断言失败 → 0 个调用 → 循环判成"模型没调工具" → 直接收尾**
//
// 表现是"客户端收到 finish_reason=tool_calls 却没有工具结果"——
// 看起来像"上游不支持工具"，而真相只是一个类型写错了。
// 这类 bug 没有任何报错，只有实测能发现。
func toolCallsOf(msg map[string]any) []map[string]any {
	if msg == nil {
		return nil
	}
	switch v := msg["tool_calls"].(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
