// errors.go Qoder 的错误分类与续期终态判定。
package qoder

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// ErrRefreshExpired 续期终态：refresh_token 已失效，需重新登录。
var ErrRefreshExpired = errors.New("qoder: refresh_token 已失效，请重新登录")

// ErrLoginPending 用户尚未完成授权（**不是错误**）。
//
// ⚠ 设备码轮询的判据是 **HTTP 404**：该端点返回 404 而任意不存在的路径
// 返回 401，说明它被网关豁免认证、由业务层报「会话未就绪」。
//
// 这与 Cline 的 `authorization_pending` 是同一类语义，但**判据形态完全不同**
//（Cline 看响应体的 error 字段且状态码可能 400；Qoder 看状态码 404）。
var ErrLoginPending = errors.New("qoder: 授权尚未完成")

// CodeTokenNotReady / CodeAccountNotReady 轮询的业务码。
const (
	codeTokenNotReady   = 11217
	codeAccountNotReady = 12151
)

// Classify 按 HTTP 状态码 + 响应体判定错误类别（gateway.ErrorClassifier）。
//
// # 判据
//
//	401/403          → ErrKindAuth
//	429              → ErrKindSoftRate
//	400/4xx          → ErrKindClient
//	>=500            → ErrKindServer
//	额度耗尽文案/码   → ErrKindHardCredit
//	其它              → ErrKindNone
//
// # ⚠ 错误帧必须能抛错（真实缺陷）
//
// Qoder 的错误用**独立的 `event: error` 行 + 顶层 `{code,message,type}`**，
// **不是** OpenAI 的 `{error:{message}}`。早期解析器只认后者 →
// 错误被静默当成「正常结束、无内容」，UI 表现为
// **「干净地停止、无任何报错」**。
func Classify(status int, body string) gateway.ErrorKind {
	lower := strings.ToLower(body)

	if containsAny(lower, hardCreditMarkers) {
		return gateway.ErrKindHardCredit
	}

	switch {
	case status == 401 || status == 403:
		return gateway.ErrKindAuth
	case status == 429:
		return gateway.ErrKindSoftRate
	case status >= 500:
		return gateway.ErrKindServer
	case status >= 400:
		return gateway.ErrKindClient
	}
	return gateway.ErrKindNone
}

// hardCreditMarkers 额度耗尽的文案特征。
var hardCreditMarkers = []string{
	"insufficient quota",
	"insufficient credit",
	"insufficient balance",
	"quota exhausted",
	"usage limit",
	"credits exhausted",
	"额度不足",
	"余额不足",
	"配额不足",
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// IsInBandError 报告一个 SSE 帧是否是 Qoder 的错误帧。
//
// 用法：消费 SSE 时逐帧判 —— 命中即必须抛出，**不能**当成"正常结束"。
//
// 判据（两条都要认）：
//
//	1. `event: error` 行（Qoder 的独立错误事件）
//	2. 顶层 `{code, message, type}` 且 code 非 0（无 error 包裹）
func IsInBandError(frame string) (bool, string) {
	if strings.Contains(frame, "event: error") {
		return true, extractInBandMessage(frame)
	}
	// 顶层 {code,message,type} 形态
	trimmed := strings.TrimSpace(frame)
	if strings.HasPrefix(trimmed, "{") &&
		strings.Contains(trimmed, `"code"`) &&
		strings.Contains(trimmed, `"type"`) &&
		!strings.Contains(trimmed, `"choices"`) {
		if msg := extractInBandMessage(frame); msg != "" {
			return true, msg
		}
	}
	return false, ""
}

// extractInBandMessage 从错误帧里抽 message（尽力而为，抽不到给原文片段）。
func extractInBandMessage(frame string) string {
	const key = `"message"`
	i := strings.Index(frame, key)
	if i < 0 {
		if len(frame) > 200 {
			return frame[:200]
		}
		return frame
	}
	rest := frame[i+len(key):]
	rest = strings.TrimLeft(rest, " \t: ")
	if len(rest) == 0 || rest[0] != '"' {
		if len(frame) > 200 {
			return frame[:200]
		}
		return frame
	}
	end := strings.Index(rest[1:], `"`)
	if end < 0 {
		return rest
	}
	return rest[1 : 1+end]
}
