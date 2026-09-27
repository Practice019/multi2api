// payload.go 出站请求体改写。
//
// # 为什么本层比其它上游薄得多
//
// 参照项目的 Cline 适配器只做三件事（cline-adapter.ts:412-448）：
//
//  1. 强制 stream:true（上游仅流式）
//  2. max_tokens 上界收敛到 943718
//  3. 工具 schema 的 enum 清洗（Gemini 系对空串 enum 返回 400）
//
// 它**不做**指纹脱敏（那是 workbuddy/codearts 系的问题）、**不做**提示词替换、
// **不做** thinking 注入（Cline 的 reasoning_effort 原样透传，
// 「绝不能加白名单」——见下方注释）。
//
// # 为什么 reasoning_effort 必须原样透传
//
// 上游对不认识的档位**静默忽略而不报错**（实测 reasoning_effort: 'banana'
// 返回 HTTP 200 且思考量为 0）。所以最坏情况是"开关无效"，
// 不会是"请求失败"。加白名单反而会把模型专属的有效档位挡掉。
package cline

import (
	"encoding/json"
	"log"
)

// PrepareBody 改写发往上游的请求体。
//
// 无法解析时**原样返回**（不猜）：上游会给出它自己的错误，
// 而那比我们编造的请求更容易定位。
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}

	// 1. 强制 stream（上游仅流式）
	obj["stream"] = true

	// 2. max_tokens 上界收敛
	clampMaxTokens(obj)

	// 3. 工具 schema 的 enum 清洗
	sanitizeTools(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// clampMaxTokens 把 max_tokens 收敛到上游上界。
//
// 上界取自内嵌目录里最大的 maxTokens（muse-spark-1.3-contributor = 943718），
// **不自行编造更大的值**。
//
// ⚠ 为什么必须做：上游网关对超大 max_tokens 会直接 4xx，而 DSH 可能注入一个
// 来自其它 provider 的大值（例如从 workbuddy 切过来时带着 384000）。
// 两个字段名都认：max_tokens 与 max_completion_tokens。
func clampMaxTokens(obj map[string]any) {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		f, ok := toFloat(v)
		if !ok {
			// 非数字：删掉。上游对类型错误的字段会 400，
			// 而删掉只是回到"用上游默认上限"，代价更小。
			delete(obj, key)
			continue
		}
		n := int64(f)
		if n <= 0 {
			delete(obj, key)
			continue
		}
		if n > maxOutputTokensUpperBound {
			log.Printf("cline: max_tokens %d -> %d（上游上界）", n, maxOutputTokensUpperBound)
			obj[key] = maxOutputTokensUpperBound
		}
	}
}

// sanitizeTools 递归清洗工具 schema 里的空串 enum。
//
// # 为什么需要（参照 cline-adapter.ts 的实测）
//
// Gemini 系对 `"enum": [""]` 这类空串枚举返回 400。而工具 schema 是客户端
// 生成的，出现空串 enum 很常见（某些 SDK 用空串表示"任意值"）。
//
// 处理：**递归遍历 tools[].function.parameters**，把 enum 数组里的空串剔除；
// 剔完为空则删掉该 enum 键（留一个空数组同样是非法的）。
func sanitizeTools(obj map[string]any) {
	tools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"].(map[string]any); ok {
			cleanEmptyEnums(params)
		}
	}
}

// cleanEmptyEnums 递归剔除 schema 里的空串 enum。
func cleanEmptyEnums(schema map[string]any) {
	if raw, ok := schema["enum"].([]any); ok {
		kept := make([]any, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok && s == "" {
				continue // 空串枚举是 Gemini 系的 400 源
			}
			kept = append(kept, v)
		}
		if len(kept) == 0 {
			delete(schema, "enum")
		} else {
			schema["enum"] = kept
		}
	}
	// 递归 properties / items / anyOf / oneOf / allOf
	if props, ok := schema["properties"].(map[string]any); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]any); ok {
				cleanEmptyEnums(sub)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		cleanEmptyEnums(items)
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := schema[key].([]any); ok {
			for _, v := range arr {
				if sub, ok := v.(map[string]any); ok {
					cleanEmptyEnums(sub)
				}
			}
		}
	}
}

// toFloat 把 JSON 数字转成 float64。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
