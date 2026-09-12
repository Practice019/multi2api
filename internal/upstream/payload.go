// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEfforts(src, sanitize, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return prepareBodyOptWithLimits(src, sanitize, efforts, nil)
}

// prepareBodyOptWithLimits 是最完整的改写入口，额外接受每模型的 max_tokens 上限。
//
// limits 为 nil 表示不做上限裁剪（未知模型表时）。
//
// 本函数是 PrepareBodyOpt / PrepareBodyOptWithEfforts 共同的下游实现 ——
// 三层签名是历史顺序（先有 PrepareBodyOpt，再叠 efforts，最后叠 limits），
// 保持它们各自的签名不变是为了不让既有调用方跟着改。
func prepareBodyOptWithLimits(src []byte, sanitize bool, efforts map[string][]string, limits map[string]int) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	normalizeToolChoice(obj)
	normalizeRoles(obj)
	normalizeReasoningEffort(obj, efforts)
	// 字段归一必须在白名单**之前**：先把新名映射成上游认识的旧名，
	// 再剔除白名单外的字段。顺序反了会把刚映射出来的字段也删掉。
	normalizeMaxTokens(obj)
	// 裁剪在归一之后：先拿到统一的 max_tokens，再按模型上限收窄。
	clampMaxTokens(obj, limits)
	applyFieldWhitelist(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// PrepareBodyOptWithLimits 是最完整的**导出**改写入口，额外接受 limits 与 efforts。
//
// 各上游在"出站前统一改写请求体"这一步共用本函数：
//
//	workbuddy → 经 Client.prepareBody 间接调用（无 limits）
//	codearts  → 直接调用并传入自己的 MaxTokensTable()
//
// limits 为 nil 表示不做上限裁剪（未知模型表时）。
// 未知字段会被剔除（见 supportedFields），因为这层要保证"只发上游认识的字段"。
func PrepareBodyOptWithLimits(src []byte, sanitize bool, efforts map[string][]string, limits map[string]int) []byte {
	return prepareBodyOptWithLimits(src, sanitize, efforts, limits)
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// supportedFields 是**上游确实认识**的顶层字段白名单。
//
// 为什么要白名单而不是"原样透传+清洗"：
// 上游对未知字段**直接 400**（参数校验），而网关会把这种参数错误
// 误判成账号故障去冷却账号 —— 于是**一个客户端带的新字段就能毒化整个账号池**，
// 表现为所有客户端都 503。
//
// 实测踩过的坑：客户端发 `max_completion_tokens` + `store` + `stream_options`，
// 每次都 400 → 账号被冷却 → 只有那个客户端失败、其它客户端正常。
//
// 因此策略从"透传一切"改为"只发上游认识的"：未知字段静默丢弃（而非报错），
// 让客户端的新规范字段不至于打挂网关。
var supportedFields = map[string]bool{
	"model":               true,
	"messages":            true,
	"stream":              true,
	"tools":               true,
	"tool_choice":         true,
	"temperature":         true,
	"top_p":               true,
	"max_tokens":          true,
	"frequency_penalty":   true,
	"presence_penalty":    true,
	"parallel_tool_calls": true,
	"stop":                true,
	"seed":                true,
	"user":                true,
	"n":                   true,
	"logprobs":            true,
	"top_logprobs":        true,
	"response_format":     true,
	"reasoning_effort":    true,
	"reasoningEffort":     true,
	"functions":           true,
	"function_call":       true,
}

// normalizeMaxTokens 把 OpenAI 新字段 max_completion_tokens 归一为 max_tokens。
//
// 上游只认旧名 max_tokens；发新名会得到参数校验错误，
// 而该错误会被误判成账号故障（详见 supportedFields 的注释）。
//
// 两者同时出现时以 max_tokens 为准：它才是上游真正生效的那个字段，
// 客户端若显式给了它说明意图明确；max_completion_tokens 只是同义冗余。
func normalizeMaxTokens(obj map[string]any) {
	mct, hasMCT := obj["max_completion_tokens"]
	if !hasMCT {
		return
	}
	delete(obj, "max_completion_tokens")
	if _, hasMT := obj["max_tokens"]; hasMT {
		return // 显式的 max_tokens 优先
	}
	obj["max_tokens"] = mct
}

// clampMaxTokens 把 max_tokens 裁剪到模型上限。
//
// 为什么必须做：客户端（尤其 DSH）会带远超模型上限的 max_tokens
// （实测 384000，而某些模型上限只有 65536），超限即参数校验错误 ——
// 于是每次请求都被上游拒绝，网关再把参数错误误判成账号故障并冷却，
// 最终表现为"那个客户端全失败、其它客户端正常"。
//
// 选择裁剪而不是报错：客户端要的是"尽量长的输出"，
// 给它模型能给的极限即可；因为"要多了"而让整个请求失败没有道理。
//
// limits 为 nil / 模型未知时不裁剪 —— 猜一个上限可能把合理请求改小，
// 让上游自己校验更安全。
func clampMaxTokens(obj map[string]any, limits map[string]int) {
	if len(limits) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	limit, ok := limits[model]
	if !ok || limit <= 0 {
		return
	}
	mt, ok := obj["max_tokens"].(float64)
	if !ok || int(mt) <= limit {
		return
	}
	obj["max_tokens"] = limit
	log.Printf("max_tokens clamped model=%s %d -> %d (模型上限)", model, int(mt), limit)
}

// applyFieldWhitelist 剔除上游不认识的顶层字段（就地修改）。
func applyFieldWhitelist(obj map[string]any) {
	for k := range obj {
		if !supportedFields[k] {
			delete(obj, k)
		}
	}
}
