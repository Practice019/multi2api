// errors.go Raccoon 的错误分类与续期终态判定。
package raccoon

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// ErrRefreshExpired 续期遇到**终态**失败：refresh_token 已失效，需重新登录。
//
// 判据（参照 raccoon-oauth.ts:299-301）：
//
//	HTTP 401        → 终态
//	code === 200003 → 终态（"登录态已过期"）
//
// ⚠ 与"可重试"的边界是本常量存在的全部意义：
// 网络抖动与 5xx **不得**归入这里，否则用户每次网络抖动都要重新登录。
var ErrRefreshExpired = errors.New("raccoon: 登录态已过期，请重新登录")

// Classify 按 HTTP 状态码 + 响应体判定错误类别（gateway.ErrorClassifier）。
//
// # 判据
//
//	401              → ErrKindAuth        凭证失效
//	429              → ErrKindSoftRate    限流
//	400/4xx          → ErrKindClient
//	>=500            → ErrKindServer
//	402 / 额度文案    → ErrKindHardCredit
//	其它              → ErrKindNone
//
// # ⚠ 业务失败可能是 HTTP 200 + 非 0 code
//
// 所以正文检查必须独立于状态码：一个 200 的响应体里带着 `"code":402030`
// （积分不足）同样要归到 ErrKindHardCredit，否则那个号的额度耗尽会被忽略，
// 下一轮又被选中。
func Classify(status int, body string) gateway.ErrorKind {
	lower := strings.ToLower(body)

	// 额度耗尽：状态码或正文命中。**放在状态码判据之前** ——
	// 额度耗尽的形态可能是 200 + 业务错误。
	if status == 402 || containsAny(lower, hardCreditMarkers) {
		return gateway.ErrKindHardCredit
	}

	switch {
	case status == 401:
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

// hardCreditMarkers 积分不足的文案特征。
//
// ⚠ 与其它上游的词表分开维护：那是别家的文案。
var hardCreditMarkers = []string{
	"insufficient points",
	"insufficient credit",
	"insufficient balance",
	"not enough points",
	"points exhausted",
	"out of points",
	"payment required",
	"quota exceeded",
	"积分不足",
	"积分已用完",
	"余额不足",
}

// containsAny 报告 s 是否含 needles 中任意一项。
func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
