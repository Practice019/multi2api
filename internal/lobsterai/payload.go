package lobsterai

// payload.go —— LobsterAI 的出站请求体规范化（T-335 起向 wire 借实现）。
//
// 本上游只做两件与协议无关的规范化：
//  1. 强制 stream:true —— **本上游仅 SSE，`stream:false` 会让上游回 500**
//  2. max_tokens 归一（非数字/0/负数/NaN 删掉；超上界改成上界）
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
//
// ⚠ 上界是**本上游的**事实（200_000），shot 成别的上游的值会被上游 4xx ——
// 这也正是它必须是参数、不能写进共享实现的原因（T-335 实测：raccoon 用的是 1_000_000）。

import "workbuddy2api/internal/wire"

const maxOutputTokensUpperBound = 200_000

// PrepareBody 强制流式并收敛 max_tokens（实现见 wire.NormalizeBodyForStreaming）。
func PrepareBody(src []byte) []byte {
	return wire.NormalizeBodyForStreaming(src, maxOutputTokensUpperBound, "lobsterai")
}
