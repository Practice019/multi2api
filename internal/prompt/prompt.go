// Package prompt 提供网关自有系统提示词：内置默认 + 文件覆盖 + 降级中性提示词。
//
// # 为什么需要这一层（借鉴 workbuddy2api-panel）
//
// 客户端（Claude Code / Codex 等 CLI）会在 system prompt 里注入若干**固定模板句**。
// 上游内容审核不是语义审核，而是对这些模板句做**逐字精确匹配**，
// 命中就整单拦截（HTTP 400 + 审核文案，如 code 11128）。
//
// 后果是一批**完全合法**的流量被误杀：用户只是在用 Claude Code 而已。
//
// 内层（internal/upstream/sanitize.go）做的是"把已知指纹串改写掉"，
// 但它只能覆盖**已知**的几种形态；模板句一变（客户端升级、换 CLI），
// 就得重新补规则，是一场追不上的赛跑。
//
// 本层换一个思路：**从源头消灭 system / developer 来源的指纹**——
// 出站前用网关自有提示词整体替换客户端注入的 system/developer 消息，
// 于是那些模板句根本不会出现在请求体里。
//
// # 两层的关系（互不替代）
//
//	prompt 层（本包）→ 解决 **system / developer 消息**里的指纹
//	sanitize 层       → 兜底 **user / assistant / tool 消息**里的指纹串
//
// user 消息里用户自己粘贴的日志、assistant 历史、工具调用参数，
// 都不归提示词层管 —— 那些必须靠 sanitize 逐串擦。
// 所以关掉任一层都会留下一个明确的缺口，两层都必须开着。
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Default 返回内置默认系统提示词。
//
// # 为什么要暴露它，而不是只让 Load 用
//
// 出站客户端（internal/upstream）在 custom 模式下必须拿到**一段**提示词。
// 如果装配层忘了注入 PromptText，最"安静"的失败是 Rewrite 因为
// systemPrompt=="" 而原样返回 —— 表现为"功能配了但完全没生效"。
//
// 让客户端能拿到 Default() 之后，那条路径变成"回落内置默认"，
// 即"配置没接上，但功能仍然按默认语义工作"，是可观测且安全的方向。
func Default() string { return defaultPrompt }

// 提示词模式。
const (
	// ModeCustom 出站前用网关自有提示词**替换**客户端 system / developer 消息。
	//
	// 这是默认值：它从源头消灭 system 来源的指纹误报，
	// 代价是丢弃客户端自己的 system（例如 Claude Code 的工程约定）。
	ModeCustom = "custom"
	// ModePassthrough 透传客户端原始 system，不做改写。
	//
	// 保留客户端人格的完整语义，代价是仍可能撞上指纹误报 ——
	// 此时由降级机制（见 gate.go）兜底：撞一次之后自动换中性提示词重试。
	ModePassthrough = "passthrough"
)

// Degraded 降级提示词：误报处理用，刻意极简中性。
//
// # 触发场景
//
// passthrough 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案）时，
// 判定为 system 来源的指纹误报，**同请求内**换这段最小中性提示词重试一次。
//
// # 为什么单独写一段而不是复用 defaultPrompt
//
// defaultPrompt 是"一份完整的工程助手人格"，约 2KB。降级的目标是
// **尽可能不携带任何可能被匹配的文本**，所以它必须短、平、无特征：
// 越短越不容易撞上逐字匹配表。复用 defaultPrompt 等于把 2KB 的表面积
// 重新暴露一遍，那与"降级"的目的相反。
//
// # 定位说明
//
// 它不是对抗手段：只用于绕开 **system 来源的误报**，
// 不改变用户指令的语义，也不影响任何内容的合法性判断 ——
// 用户内容本身触发审核时，第二次仍会被拦，并如实返回给调用方。
const Degraded = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本。
//
//   - file 非空 → 读文件；不存在/读失败**返回 error**（调用方 fail fast）
//   - file 空   → 返回内置 defaultPrompt
//
// # 为什么 file 非空但不可读时必须报错而不是回落内置默认（fail fast）
//
// 这是一个**静默降级**极易发生的位置：用户配了 `prompt.file` 指向自己的
// 人格文件，路径打错一个字符，如果静默回落内置默认，表现是
// "网关能跑、请求正常、只是我的自定义人格没了" —— 没有任何信号。
//
// 用户会花很久去查"为什么我的人格不生效"，而真因只是一个路径。
// 启动时报错把这次失败暴露在唯一能立刻发现的位置。
//
// # mode 参数为什么不参与判断
//
// custom / passthrough 的路由由调用方决定（见 internal/upstream 的 applyPrompt）。
// Load 只负责一件事：**拿到一段提示词文本**，不关心它怎么被用。
// 把路由语义塞进来会让它既是加载器又是策略器。
func Load(mode, file string) (string, error) {
	_ = mode // 保留参数以便未来按 mode 校验（如 passthrough 下 file 无意义）
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt 文件 %s 读取失败: %w", file, err)
	}
	if len(raw) == 0 {
		// 空文件是一个**几乎必然是配置错误**的状态：用户显然想要一份提示词，
		// 但拿到了空串。当作错误报出来，而不是发一个空 system 给上游。
		return "", fmt.Errorf("prompt 文件 %s 为空", file)
	}
	return string(raw), nil
}

// Rewrite 解析 OpenAI 请求体并替换系统提示词：
//
//   - 删除 messages 中**所有** role 为 system / developer 的消息
//   - 在 messages 头部插入一条 {"role":"system","content":systemPrompt}
//   - 其余字段与 user / assistant / tool 消息逐字不动
//
// # 为什么解析失败时原样返回，而不是报错
//
// Rewrite 位于**出站改写的关键路径**上，它的输入是客户端刚发来的原始 body。
// 任何解析错误（非 JSON、超大、编码异常）都**不应该**让请求在这里失败：
//
//   - 本函数的职责是"优化"，不是"校验"；
//   - 真正的校验属于上游，让它按原始语义处理，客户端才能拿到上游的真实错误；
//   - 在这里造一个错误，等于把一个上游能诊断的问题变成一个我们自己编的问题。
//
// 因此所有失败路径都返回**入参本身**（同一个 []byte）。
//
// # 为什么 developer 也要删
//
// developer 是 OpenAI 新规范里 system 的别名，上游按 system 级指令处理它。
// 只删 system 会让 developer 携带的模板句继续漏网。
// （注：internal/upstream 的 normalizeRoles 还会把 developer 归一为 system，
// 但那是"上游不认识 developer 这个 role"的协议兼容，与本处的"内容替换"正交，
// 两者叠加不冲突 —— 先删掉就不再需要归一。）
//
// # 为什么 systemPrompt 为空时不动
//
// 空提示词插入一条 content 为 "" 的 system 消息，对上游是新形态，
// 可能触发参数校验。没有提示词就什么都不做，是最保守的选择。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// 无 messages 字段或类型不符 → 插入单条 system 后原样保留其余字段。
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// 过滤掉所有 system / developer 消息，保留 user / assistant / tool 及其他角色。
	//
	// 注意这里是**全删**而不是"只保留第一条"：客户端有时会塞多条 system
	// （例如 Claude Code 的 system + 一段 developer 补充）。只删第一条
	// 会让后面那些继续漏网 —— 而那正是本层要消灭的东西。
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	// 头部插入单条 system 消息。
	//
	// 用 prepend 而不是 append：system 必须在最前面才是 system 语义，
	// 追加到尾部会让它变成一段"最后的用户输入"，语义完全变了。
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
