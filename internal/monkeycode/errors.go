// errors.go 本上游的错误分类。
//
// # 为什么必须自己分类
//
// 默认分类器只看状态码，而 MonkeyCode 的**同一个状态码有三种含义**：
//
//	403 Forbidden                     → 该模型不在你的套餐里（换号无用，别罚号）
//	403 {"error":"invalid ohmyagent request"}  → 签名错了（是我们的 bug，别罚号）
//	404 {"message":"路由不存在"}       → 协议选错了（也是我们的 bug，别罚号）
//
// 若交给默认分类器：403 会被归 ErrKindAuth（换号）→ 每个号都试一遍，白跑；
// 更糟的是某些实现会把 ErrKindAuth 累积成"号坏了" → **把好号禁掉**。
//
// AGENTS.md §5 的判例说的就是这个：workbuddy 的裸数字 12153 曾被拿去判 codearts 的
// 错误体，把健康账号永久禁用。上游的错误码只能由上游自己解释。
package monkeycode

import (
	"strings"

	"workbuddy2api/internal/gateway"
)

// Classify 实现 gateway.ErrorClassifier。
//
// 判据顺序（每条对应一次实测或一个明确的语义）：
//
//	1. 200-ish 不进这里（调用方只在 >=400 时问）
//	2. 签名/协议类文案 → FatalClient（**不换号**，直接结束：换号也还是错）
//	3. 403 且非签名类     → Client（只换号，不罚）
//	4. 404                → NotFound（短冷却，不累计错误）
//	5. 401                → Auth（session 过期，可换号）
//	6. 402 / 余额类       → HardCredit（长冷却到下次重置）
//	7. 429                → SoftRate（短冷却）
//	8. 5xx                → Server（喂熔断）
//	9. 其余 4xx           → Client
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	lower := strings.ToLower(body)

	// 2. 我们自己的 bug —— 换号无用，直接结束。
	if strings.Contains(lower, "invalid ohmyagent request") ||
		strings.Contains(body, "签名") ||
		strings.Contains(lower, "signature") {
		return gateway.ErrKindFatalClient
	}
	if strings.Contains(body, "路由不存在") {
		// 协议选错 = 本包 bug（interfaceOf 没覆盖到这个模型）。
		// 归 FatalClient 而不是 NotFound：NotFound 会重试别的号，而每个号都会同样 404。
		return gateway.ErrKindFatalClient
	}

	switch {
	case status == 401:
		// session 过期（30 天硬过期，无续期）。换号。
		return gateway.ErrKindAuth
	case status == 402:
		return gateway.ErrKindHardCredit
	case status == 403:
		// 套餐不含该模型。**不罚号**：号是好的，只是档位不够。
		return gateway.ErrKindClient
	case status == 404:
		return gateway.ErrKindNotFound
	case status == 429:
		return gateway.ErrKindSoftRate
	case status >= 500:
		return gateway.ErrKindServer
	case status >= 400:
		// 余额类文案（有些版本用 400/403 带文案表达）。
		if strings.Contains(body, "余额") || strings.Contains(lower, "insufficient") ||
			strings.Contains(lower, "quota") {
			return gateway.ErrKindHardCredit
		}
		if strings.Contains(body, "拦截") || strings.Contains(body, "风控") {
			return gateway.ErrKindContentBlocked
		}
		return gateway.ErrKindClient
	}
	return gateway.ErrKindNone
}
