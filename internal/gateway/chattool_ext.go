// 扩展点：上游自报"用我的模型对话时，可以注入这些工具"。
//
// # 为什么需要它（用户的诉求）
//
// 用户原话：「我希望只能在使用 Loomy 上游任意一个模型时，启用该上游对应的工具。」
//
// 那条"只能在使用某上游时启用"的作用域**不需要**在核心写任何
// `if provider == "loomy"` —— 它由本扩展点天然表达：
//
//	声明了 ChatToolExt 的上游 → 它的模型对话时注入它的工具
//	没声明的上游             → 一个工具都不注入（行为与改造前逐字节一致）
//
// 核心**不认识任何具体上游**（架构硬约束，见 arch_test.go），
// 所以"哪些工具可用"只能由上游自己回答。这条与 QuotaExt / ImageGenExt
// 完全同构 —— 加一个新上游时核心零改动。
//
// # 为什么工具是"上游私有"的而不是核心内置的
//
// 工具背后是**上游的端点**。Loomy 的两个工具实打实打在它自己的服务上：
//
//	generate_image  → POST {base}/api/v1/images/generations
//	web_search      → POST {base}/api/v1/search/tencent
//
// 换个上游（workbuddy / codearts / cline…）这些路径根本不存在。
// 把它们做成"核心内置工具"就等于让核心假装知道所有上游的端点 ——
// 那正是本项目一直在拆的那种耦合。
package gateway

import (
	"context"
	"encoding/json"
)

// ChatToolExt 上游自报"我可注入对话的工具 + 怎么执行它们"。
//
// 可选实现：不实现的上游，其对话请求**不注入任何工具**。
type ChatToolExt interface {
	// ChatTools 返回可注入对话的工具定义。
	//
	// 只在**本次请求路由到本上游**时才会被调用与注入 ——
	// 调用方（出口层）拿到的是"被选中的那个上游"的 Provider。
	//
	// 返回空切片表示"我暂时没有可注入的工具"（合法状态，
	// 例如该上游的工具需要某个未配置的凭据）。返回副本，
	// 调用方不应假设可以修改。
	ChatTools() []ChatTool

	// ExecuteChatTool 执行一次工具调用，返回**喂回给模型**的结果。
	//
	//	name  工具名（`ChatTool.Name` 之一）
	//	args  模型给出的参数（原始 JSON，未解析 —— 由上游自己解释）
	//	uid   用哪个账号执行（与生图端点同一条：让上游按 uid 取自己的凭证）
	//
	// 返回的 ChatToolResult.Content 会被包成一条 `role:"tool"` 消息
	// 发回给上游，所以它应当是**模型能读懂的自然语言或 JSON 文本**。
	//
	// ⚠ 实现不得 panic（契约要求，与 Provider 一致）。
	// ⚠ 工具执行失败**不要**返回 error：那会让整个请求 502。
	//    用户看到的应当是"模型说这次没生成成功"，而不是"网关挂了"。
	//    把失败写进 Content（并置 Result.OK=false）即可。
	//    error 只用于**调用方用法错误**（如未知工具名），那种情况
	//    是核心的 bug，必须显式暴露。
	ExecuteChatTool(ctx context.Context, uid, name string, args json.RawMessage) (ChatToolResult, error)
}

// ChatTool 一个工具的定义（OpenAI tools 数组里的一项）。
type ChatTool struct {
	// Name 工具名。模型会用它发起调用，所以必须**跨请求稳定**
	//（改名 = 会话历史里的旧调用对不上）。
	Name string
	// Description 给模型看的说明。**这是唯一影响模型"要不要用"的东西** ——
	// 写得含糊，模型就会在你期待它生图时写了一篇散文。
	Description string
	// Parameters 参数的 JSON Schema（原样塞进 `function.parameters`）。
	Parameters json.RawMessage
}

// ChatToolResult 一次工具执行的结果。
type ChatToolResult struct {
	// Content 喂回给模型的文本（会成为 `role:"tool"` 消息的 content）。
	//
	// ⚠ 应当**包含足够模型收尾的信息**：生图工具要给图片 URL，
	// 搜索工具要给条目摘要 —— 否则模型只能回一句"我不知道结果"。
	Content string

	// OK 本次执行是否成功。false 时 Content 应当是**失败原因**，
	// 让模型能向用户解释（"图片服务暂时不可用"），
	// 而不是让用户看到一句无声的失败。
	OK bool

	// Markdown 可选的 markdown 片段，由出口层**追加到最终回复里**。
	//
	// # 为什么需要它（用户的要求）
	//
	// 用户要求："写进回复内容（markdown）+ 附结构化字段"。
	//
	// 但回复文本是**模型写的**，网关改不了模型的话。所以做法是：
	// 网关把图片 markdown 追加在模型回复之后 —— 即使用户的客户端
	// 不支持渲染 markdown 图片，至少 URL 是可见、可点、可复制的。
	//
	// ⚠ 只有**最终回复**才追加（见 Artifacts）。中途轮次的图片不追加，
	// 否则模型把同一张图说两遍时用户会看到两张一样的图。
	Markdown string

	// Artifacts 结构化产物（图片 URL、扣分等），由出口层附在响应的
	// 非标准字段上。供会解析 JSON 的调用方读取（markdown 是给人看的）。
	Artifacts []Artifact
}

// Artifact 一个工具产出的结构化结果。
//
// 用 map 而不是具体结构体：不同工具的产物字段完全不同
// （生图给 url/size/points，搜索给 hits/requestId），
// 定一个公共结构体会逼每个工具把字段塞进别人的形状里。
type Artifact map[string]any

// ArtifactKey 出口层用来在响应里挂产物列表的字段名。
//
// 非 OpenAI 标准字段（OpenAI 没有这个概念）。选一个不撞名的键：
// 直接叫 `artifacts` 会与 OpenAI 将来的字段混淆，
// 加前缀表明"这是本网关的扩展"。
const ArtifactKey = "loomy_artifacts"
