// payload.go 出站请求体改写。
//
// LobsterAI 只做两件与协议无关的规范化：
//
//  1. 强制 stream:true —— **本上游仅 SSE，`stream:false` 会让上游回 500**
//  2. max_tokens 归一
//
// **不做**指纹脱敏、不做提示词替换（那是 workbuddy/codearts 系的问题）。
//
// # ⚠ 一个必须保留的字段：reasoning_effort
//
// `thinking-level-control-v1` 能力声明之后，"关闭思考"用 `reasoning_effort: "off"`。
// 不带该能力时服务端对 `off` 直接回 HTTP 500 —— 这就是我们**必须**在
// X-LobsterAI-Client-Capabilities 里声明它的原因（见 lobsterai.go 的常量注释）。
//
// 本层**原样透传** reasoning_effort，不加白名单：上游对不认识的档位静默忽略，
// 加白名单反而会把模型专属的有效档位挡掉。
package lobsterai

import (
	"encoding/json"
	"log"
)

// maxOutputTokensUpperBound max_tokens 上界。
//
// 上游对超大 max_tokens 会直接 4xx，而 DSH 可能注入一个来自其它 provider
// 的大值（例如从 workbuddy 切过来时带着 384000）。
const maxOutputTokensUpperBound = 200_000

// PrepareBody 改写发往上游的请求体。
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	// 仅 SSE：上游对 stream:false 回 500
	obj["stream"] = true
	clampMaxTokens(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

func clampMaxTokens(obj map[string]any) {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		f, ok := toFloat(v)
		if !ok || f <= 0 || f != f {
			delete(obj, key)
			continue
		}
		if int64(f) > maxOutputTokensUpperBound {
			log.Printf("lobsterai: max_tokens %d -> %d（上游上界）", int64(f), maxOutputTokensUpperBound)
			obj[key] = maxOutputTokensUpperBound
		}
	}
}

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
