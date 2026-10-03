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
//	③ 未被配置显式关闭
//
// # ⚠ 这里**曾经**有一条"客户端自带 tools 就不接管"，已删除
//
// 那条判据写在"普通聊天客户端"的语境里，初衷是对的：客户端带 tools
// 说明它懂协议、要自己驱动工具循环，网关再插一脚会两边打架。
//
// 但它在 **agent 框架**（DSH / Claude Code / Cursor 这类）下**完全错误** ——
// 因为那些客户端**每轮都带自己的工具**（bash / read / write …）。
//
// 后果：它们永远命中那条判据 → 网关一个工具都不注入 →
// 模型没有 generate_image 可调 → 只能吐一段 SVG 源码或说"我画不了"。
//
// 用户观察到的"DSH 里生不了图"正是这条判据造成的 ——
// 看起来像"DSH 判定该模型不能生图"，其实是网关主动不给了。
//
// 用户的要求（原话）：
//
//	"我又不希望在 DSH 注册那些工具，因为我需要完成自包含、自依赖。"
//
// 所以正确的边界不是"客户端带工具就整个不接管"，而是**按工具名分工**：
//
//	模型调客户端自己的工具（bash/read/…）→ 原样透传，客户端执行
//	模型调网关注入的工具（generate_image/…）→ 网关自己执行，客户端无感
//
// 这样 agent 框架不必在配置里声明任何生图/搜索工具，
// 它以为模型就是"直接回复了一段带图的 markdown"。
//
// 分工的实现见 runToolLoop 的 partitionToolCalls；重名去重见 injectTools。
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
	return tc, true
}

// existingToolNames 摘出请求体里**客户端已声明**的工具名集合。
//
// # 为什么需要它（不去重会真的坏掉）
//
// agent 框架的工具名与我们的撞车概率很高 —— 尤其 `web_search`
// 几乎是标配。工具列表里出现**重名**时上游会直接拒绝
// （OpenAI 系返回 400：duplicate tool name），而那个错误看起来
// 与"生图"毫无关系，排查时会绕远路。
//
// 所以合并时以**客户端声明的为准**：它已经有 `web_search` 就不注入我们的
// （它那个归它执行，模型照常调）。我们的工具是"补充"，不是"覆盖"。
//
// 返回 nil 表示客户端没带工具（parse 失败也返回 nil —— 畸形请求
// 交给后续路径去报错，这里不因它而改变行为）。
func existingToolNames(body []byte) map[string]bool {
	var probe struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
			// 有的客户端用扁平形态（少见但合法）：{"name":"…","type":"…"}
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	if len(probe.Tools) == 0 {
		return nil
	}
	names := make(map[string]bool, len(probe.Tools))
	for _, t := range probe.Tools {
		if n := strings.TrimSpace(t.Function.Name); n != "" {
			names[n] = true
			continue
		}
		if n := strings.TrimSpace(t.Name); n != "" {
			names[n] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// injectTools 把我们声明的工具**并进**客户端已有的工具列表。
//
// # 为什么必须"合并"而不是"赋值"（第一版写成赋值，是真 bug）
//
// 第一版是 `obj["tools"] = wireTools` —— 那是**替换**。
// 当时它不会出问题，只因为上游那条"客户端带 tools 就不接管"的判据
// 挡住了所有带工具的客户端。判据一删，替换就会**抹掉 agent 框架的
// 全部工具**（bash / read / write …）—— 模型再也执行不了任何命令，
// 而现象是"DSH 突然变笨了"，与本功能毫无表面关联。
//
// # 为什么以客户端声明为准（重名不注入）
//
// agent 框架的工具名与我们的撞车概率很高（`web_search` 几乎是标配）。
// 上游遇到重名工具会直接 400（duplicate tool name）——
// 那个错误看起来与生图无关，排查时极难联想到。
//
// 所以：客户端已有的名字，我们不注入（它那个归它执行，模型照常调）。
// 我们的工具是**补充**，不是**替代**。
//
// # 为什么不去动 stream
//
// 第一版在这里强制 `obj["stream"] = false`（因为循环内部要完整响应）。
// 但"内部非流式"与"请求体写什么"是两件事：循环自己发的是非流式请求，
// 不该去改**客户端**请求体里的 stream 字段 —— 那个字段还要用来决定
// 最后以什么形态发射（见 runToolLoop 末尾）。写在这里会让
// 后续读 body 判断 stream 的代码永远看到 false。
func injectTools(body []byte, tools []gateway.ChatTool, existing map[string]bool) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}

	// 保留客户端已有的工具（原样，不解析不重构 —— 那是它的对象）。
	merged := make([]any, 0, len(tools)+4)
	if cur, ok := obj["tools"].([]any); ok {
		merged = append(merged, cur...)
	}

	for _, t := range tools {
		if existing[t.Name] {
			// 客户端已经声明了同名工具 → 让它的生效，我们不插。
			continue
		}
		fn := map[string]any{
			"name":        t.Name,
			"description": t.Description,
		}
		if len(t.Parameters) > 0 {
			// 参数 schema 原样塞进去：它是上游自己给的 JSON，
			// 再解析一遍只会引入"我以为它长这样"的假设。
			fn["parameters"] = json.RawMessage(t.Parameters)
		}
		merged = append(merged, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	obj["tools"] = merged
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// partitionToolCalls 按**归属**把模型发起的工具调用分成两组。
//
// # 这是"agent 框架零改动"的核心
//
//	ours   = 网关注入的工具 → 网关自己执行，**不告诉客户端**
//	theirs = 客户端声明的工具 → 原样透传，客户端执行
//
// 两类可以在**同一次**响应里同时出现（模型一轮里既想画图又想跑命令），
// 所以两件事都要做：
//
//	① 网关注入的工具自己执行完、结果回喂，客户端看不到这一步
//	② 客户端自己的工具必须**原样返回** tool_calls 给它 —— 否则它
//	   的执行循环卡住（它按自己的期待在等 tool_calls）
//
// 第一版没有这个分工：只要发现任何 tool_calls 就全部拿来执行，
// 于是 agent 框架的工具名会被送到 ExecuteChatTool →
// 返回"未知工具名"错误。对 DSH 来说就是它的 bash 永远不执行。
//
// 返回 (ours, theirs)。theirs 非空时调用方**必须**把这轮的 tool_calls
// 原样交给客户端（见 runToolLoop 的 early-return）。
func partitionToolCalls(calls []toolCall, mine map[string]bool) (ours, theirs []toolCall) {
	for _, c := range calls {
		if mine[c.Name] {
			ours = append(ours, c)
			continue
		}
		theirs = append(theirs, c)
	}
	return ours, theirs
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

// toolCall 模型发起的一次工具调用。
//
// 用具名类型而不是匿名结构体：它现在会跨函数传递
// （parseToolCalls → partitionToolCalls → execOneTool / 透传），
// 匿名结构体做不到（两处写法必须逐字相同且无法复用方法）。
type toolCall struct {
	// ID 工具调用 id。tool 消息靠它回指对应的调用 —— 对不上上游会 400。
	ID string
	// Name 工具名。**决定归属**（是我们的还是客户端的）。
	Name string
	// Args 模型给的参数（原始 JSON）。
	//
	// ⚠ 它是 JSON **字符串**（OpenAI 协议里最反直觉的一处：
	// arguments 字段的值本身是一段 JSON 文本）。原样带走由工具解析。
	Args json.RawMessage
}

// parseToolCalls 从非流式响应的 message 里取出 tool_calls。
//
// 返回空切片表示"模型没调工具"（正常收尾）。
//
// # ⚠ 这里有一个**静默失败**的陷阱（我踩过，务必保留这条注释）
//
// `wire.Aggregate` 把 tool_calls 放进 message 时用的是
// **`[]map[string]any`**（见 wire/sse.go 的 `make([]map[string]any, …)`），
// 而**不是** `[]any`。
//
// 我第一版断言 `.([]any)`，于是：
//
//	上游确实回了 tool_calls（37 KB 的流、finish_reason=tool_calls）
//	聚合也成功（message["tool_calls"] 里躺着 1 个调用）
//	**但断言失败 → 返回 0 个调用 → 循环判成"模型没调工具"→ 直接收尾**
//
// 表现是"客户端收到 finish_reason=tool_calls 但没有工具结果"——
// 看起来像"上游不支持工具"，而真相只是一个类型写错了。
//
// 两种形状都认（见 toolCallsOf）。少认一种就是这条 bug。
func parseToolCalls(msg map[string]any) []toolCall {
	rawList := toolCallsOf(msg)
	if len(rawList) == 0 {
		return nil
	}
	out := make([]toolCall, 0, len(rawList))
	for _, call := range rawList {
		id, _ := call["id"].(string)
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		argStr, _ := fn["arguments"].(string)
		out = append(out, toolCall{
			ID:   id,
			Name: name,
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
