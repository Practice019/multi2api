// errorclassifier.go MiMo 的错误分类（gateway.ErrorClassifier）。
//
// # 为什么**必须**实现（不实现的后果）
//
// 出口层对没实现分类器的上游会回落 **workbuddy 的判据**（handler.go 分类分派），
// 拿别人的错误码表判 MiMo 的 body = 裸数字误伤（codearts 被 "12153" 永久禁用
// 的同型事故）。
//
// # 判据表（评审报告 §2.1 错误码全表，实测形态钉死）
//
//	401 body type=="invalid_key"     → SessionDead（key 被删/失效：人为死法，可见禁用）
//	402 / insufficient_quota         → HardCredit（欠费：冷却可恢复，≠死刑）
//	                                 → FreeUsageLimitError/SubscriptionUsageLimitError 同 HardCredit
//	429（含官方限流词表）             → SoftRate（精确恢复时刻走 SoftRateExt）
//	400 error.code=="421"（字符串）   → None（内容审查：不罚号，param 拼进消息由上层展示）
//	400 error.code=="441" 或 HTTP 441 → SoftRate（风控短冷却）
//	400 Param Incorrect(reasoning…)  → None（方言未满足：dialect 已重试过，仍败也不罚号）
//	404 → NotFound；≥500 → Server；其它 → None（只换号不罚）
//
// ⚠ MiMo 网关的 `error.code` 是**字符串**（"401"），部分形态 code 缺失只有
// message —— 先解析 body 再按状态码兜底（loomy 文体）。解析失败 = 中性分类，
// 绝不 panic 也不硬判（P9 实测同一域名两种错误体）。
package mimo

import (
	"encoding/json"
	"strconv"
	"strings"

	"workbuddy2api/internal/gateway"
)

// rateLimitMarkers 官方 retry.ts:192-202 的限流词表（照抄进分类器判据）。
var rateLimitMarkers = []string{
	"too many requests", "too_many_requests", "rate limit", "rate_limit",
	"rate limited", "rate increased too quickly",
}

// quotaMarkers 余额/credits 耗尽（不可重试的"钱尽"语义 → 冷却而非禁用）。
var quotaMarkers = []string{
	"insufficient_quota", "quota exceeded",
	"freeusagelimiterror", "subscriptionusagelimiterror",
}

// mimoErrBody MiMo/开放AI 兼容的错误信封（error.code 是字符串！）。
type mimoErrBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    any    `json:"code"` // 字符串或数字都可能出现 → any 后归一
	} `json:"error"`
}

func (e *mimoErrBody) codeString() string {
	switch v := e.Error.Code.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	default:
		return ""
	}
}

// Classify 按状态码 + body 判定错误类别（gateway.ErrorClassifier）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	return Classify(status, body)
}

// Classify 纯函数版（测试直接钉）。
func Classify(status int, body string) gateway.ErrorKind {
	var env mimoErrBody
	parsed := json.Unmarshal([]byte(body), &env) == nil
	code := ""
	typeStr := ""
	if parsed {
		code = normalizeCode(env.codeString())
		typeStr = strings.ToLower(env.Error.Type)
	}
	lower := strings.ToLower(body)

	// ---- body 优先（同一状态码承载多种语义：400 包 421/441/reasoning）----
	if typeStr == "invalid_key" {
		return gateway.ErrKindSessionDead // key 失效/被删：可见禁用，需重新导入
	}
	if code == "421" {
		return gateway.ErrKindNone // 内容审查：账号无过错，只换号
	}
	if code == "441" || status == 441 {
		return gateway.ErrKindSoftRate // 风控：短冷却
	}
	for _, m := range quotaMarkers {
		if strings.Contains(lower, m) {
			return gateway.ErrKindHardCredit // 欠费/额度尽：冷却等重置
		}
	}
	if code == "400" && strings.Contains(env.Error.Param, "reasoning_content") {
		return gateway.ErrKindNone // 方言未满足：dialect 层处理，这里不罚号
	}
	for _, m := range rateLimitMarkers {
		if strings.Contains(lower, m) || code == "429" {
			return gateway.ErrKindSoftRate
		}
	}

	// ---- 状态码兜底 ----
	switch {
	case status == 401 || status == 403:
		if status == 403 {
			// paid 面的 403 语义未知（可能区域风控）：按软处理，不判死账号。
			return gateway.ErrKindNone
		}
		return gateway.ErrKindSessionDead
	case status == 402:
		return gateway.ErrKindHardCredit
	case status == 429:
		return gateway.ErrKindSoftRate
	case status == 404:
		return gateway.ErrKindNotFound
	case status >= 500:
		return gateway.ErrKindServer
	case status >= 400:
		return gateway.ErrKindClient
	}
	return gateway.ErrKindNone
}

// normalizeCode "401" / "429 " / 带引号残留 → 纯数字串。
func normalizeCode(s string) string { return strings.TrimSpace(s) }

// 编译期断言：Provider 实现错误分类。
var _ gateway.ErrorClassifier = (*Provider)(nil)
