package raccoon

// payload.go —— Raccoon 的出站请求体规范化（T-335 起向 wire 借实现）。
//
// Raccoon 是**标准 OpenAI 兼容**上游（无加密、无信封、无字段白名单），
// 所以本层只做两件与协议无关的规范化：
//  1. 强制 stream:true（适配器恒发流式）
//  2. max_tokens 归一（客户端可能带来自其它 provider 的值）
//
// **不做**指纹脱敏、不做提示词替换、不做 thinking 注入 —— 那些是
// workbuddy/codearts 系的问题（它们有内容审核与反探测）。
//
// ⚠ 上界是**本上游的**事实（1_000_000，取兜底表最大值 100000 之上的安全值）：
// 上游对超大 max_tokens 直接 4xx，而 DSH 可能注入一个从别的 provider 带过来的大值
//（例如从 workbuddy 切过来时带着 384000）。写错成别家的上界同样会 4xx（T-335 实测）。

import "workbuddy2api/internal/wire"

const maxOutputTokensUpperBound = 1_000_000

// PrepareBody 强制流式并收敛 max_tokens（实现见 wire.NormalizeBodyForStreaming）。
func PrepareBody(src []byte) []byte {
	return wire.NormalizeBodyForStreaming(src, maxOutputTokensUpperBound, "raccoon")
}
