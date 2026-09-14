// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
//
// # 本文件的四处盲区（借鉴 workbuddy2api-panel，均已实测补上）
//
// 改造前本文件只覆盖 "Claude Code 身份句 + Main branch + billing header + 尾随 kv"，
// 而实测存在四条**漏网**路径，每一条都会让请求原样带着指纹发上游并吃 400：
//
//	① tool_calls 盲区   —— content **字段缺失**时旧 sanitizeMessages 直接 continue，
//	                       整条消息（含 tool_calls）被跳过。工具调用轮的 assistant
//	                       消息经常只带 tool_calls 而不带 content 键，于是历史里任何
//	                       写进工具参数的被拦字符串（文件名、命令、写入内容）全部漏出。
//	                       （注意：`"content": null` 不会触发跳过 —— 键存在，
//	                        `ok` 就是 true；只有键彻底缺席才走 continue。
//	                        两种形态都必须被净化，见 sanitizeMessages 的注释。）
//	② 桌面版身份句      —— CLI 版以句号收尾、桌面版（Agent SDK）以逗号接续。
//	                       带句号的匹配串只命中前者，桌面形态原样漏网。
//	③ Codex CLI 身份句  —— 另一族 CLI 的模板句，此前完全没覆盖。
//	④ 裸数字 11128      —— 上游的**反探测**：请求体里只要出现这串数字就整单拦截
//	                       （与该数字的上下文无关）。而 11128 恰好是本类拦截自身的
//	                       错误码，于是"用户在对话里提到它"必然失败。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
//
// ⚠ 每新增一条改写规则，**必须**在这里补对应的预检串：
// 预检是净化层的前置闸门，漏补的后果是"规则写了但永远不触发"——
// 那是一种完全静默的失效（编译过、单测若只测 sanitizeText 也会过，
// 但线上走 hasFingerprint 短路时根本进不去）。
// 有测试钉住（见 sanitize_test.go 的 TestEveryRewriteReachable）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // header 键值段键名
	"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",        // 身份句（截断前缀即可命中）
	"Main branch (",              // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11128",                                           // 上游反探测：裸数字错误码
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// # 身份句为什么**不带结尾标点**
//
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
//
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
//
// ⚠ 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换 ——
// 否则会误伤 TestExactMatchOnlyVariantNotTouched 保护的零散文本
// （如用户自己写的 "...official CLI for Claude!"）。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截
		// （只留链接或只留半边均不拦，实测需整句同时出现）。
		// give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字的上下文无关——
		// "code=11128" / 裸 "11128" / "错误码 11128" / "Code=11128" 全部命中；
		// 相邻的 11148 / 11101 / 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求。
		//
		// 代价：用户对话中任何 11128 都会被改写——但这串数字出现在请求里本身就是
		// 拦截条件，不改写必然失败。插入连字符保留可读性与指代
		// （零宽空格无效，实测上游会归一化）。
		"11128",
		"11-128",
	},
}

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	return strings.TrimSpace(text)
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名有大小写变体（X-Anthropic-...），快速路径漏掉时再落正则（(?i)）兜底。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeHdrRe.MatchString(text)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// # 为什么这块长期是盲区（改造前真实存在的漏网）
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 但旧 sanitizeMessages 的形状是：
//
//	c, ok := m["content"]
//	if !ok { continue }        // ← 这里
//	...
//
// ⚠ 精确一点说：`"content": null` **不会**触发这次 continue ——
// JSON 里的 null 解码后键仍然存在，`ok` 是 true，于是流程会继续走到 tool_calls。
// 真正踩中的是**键彻底缺席**的形态（只带 tool_calls 的 assistant 消息）。
// 两种形态在真实客户端里都出现，所以两条都要有测试覆盖
// （见 sanitize_gaps_test.go 的 TestToolCallsSanitizedWhenContentNull 的两个子用例）。
//
// 无论哪种形态，后果都一样：**整条消息连 tool_calls 一起被跳过**——
// 历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
//
// 修法是把 content 与 tool_calls 拆成两个**各自独立**的判断（见 sanitizeMessages），
// 而不是在这里单独补一个 function 的遍历。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content 与 tool_calls；任一命中返回 true。
//
// ⚠ 两个字段必须**各自独立**判断，绝不能在 content 缺失时 continue：
// 只带 tool_calls 的 assistant 消息没有 content 键，一旦在 content 上短路，
// 那条消息的 tool_calls 就完全不会被净化（见 sanitizeToolCalls 的注释）。
//
// 注意 null 与缺失是两种形态，行为不同但都必须被净化：
//
//	"content": null  → 键存在，ok=true  → 旧代码不会跳过（本来就没问题）
//	content 键缺失    → ok=false        → 旧代码 continue，这正是漏网那条
//
// 有测试钉住（见 sanitize_test.go 的 TestToolCallsSanitizedWhenContentNull，
// 两个子用例分别覆盖 null 与缺失）。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
