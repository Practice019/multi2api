// payload.go 出站请求体改写。
//
// Raccoon 是**标准 OpenAI 兼容**上游（无加密、无信封、无字段白名单），
// 所以本层只做两件与协议无关的规范化：
//
//  1. 强制 stream:true（适配器恒发流式）
//  2. max_tokens 归一（客户端可能带来自其它 provider 的值）
//
// **不做**指纹脱敏、不做提示词替换、不做 thinking 注入 —— 那些是
// workbuddy/codearts 系的问题（它们有内容审核与反探测）。
package raccoon

import (
	"encoding/json"
	"log"
)

// maxOutputTokensUpperBound max_tokens 上界。
//
// 取兜底表里最大值（100000）之上的一个安全值：上游对超大 max_tokens
// 会直接 4xx，而 DSH 可能注入一个来自其它 provider 的大值
// （例如从 workbuddy 切过来时带着 384000）。
const maxOutputTokensUpperBound = 1_000_000

// PrepareBody 改写发往上游的请求体。
//
// 无法解析时**原样返回**（不猜）。
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	clampMaxTokens(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// clampMaxTokens 把 max_tokens 收敛到上界，并删掉非法值。
func clampMaxTokens(obj map[string]any) {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		f, ok := toFloat(v)
		if !ok || f <= 0 || f != f {
			// 非法（非数字 / 0 / 负数 / NaN）：删掉。
			// 上游对类型错误的字段会 400，而删掉只是回到"用上游默认上限"。
			delete(obj, key)
			continue
		}
		if int64(f) > maxOutputTokensUpperBound {
			log.Printf("raccoon: max_tokens %d -> %d（上游上界）", int64(f), maxOutputTokensUpperBound)
			obj[key] = maxOutputTokensUpperBound
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
