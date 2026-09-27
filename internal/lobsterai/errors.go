// errors.go LobsterAI 的错误分类与续期终态判定。
package lobsterai

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// ErrRefreshExpired 续期遇到**终态**失败：refresh_token 已失效，需重新登录。
//
// 判据：HTTP 401/403，或信封失败（data 非对象通常意味着 accessToken/refreshToken
// 已失效）。
//
// ⚠ 网络抖动与 5xx **不得**归入这里，否则用户每次网络抖动都要重新登录。
var ErrRefreshExpired = errors.New("lobsterai: refresh_token 已失效，请重新登录")

// Classify 按 HTTP 状态码 + 响应体判定错误类别（gateway.ErrorClassifier）。
//
// # 判据
//
//	401/403          → ErrKindAuth
//	429              → ErrKindSoftRate
//	400/4xx          → ErrKindClient
//	>=500            → ErrKindServer
//	402 / 额度文案    → ErrKindHardCredit
//	其它              → ErrKindNone
//
// # ⚠ 业务失败可能是 HTTP 200 + 非 0 code
//
// LobsterAI 用统一信封 `{code,msg,data}`。所以正文检查必须独立于状态码 ——
// 一个 200 的响应体里带着积分不足的 msg 同样要归到 ErrKindHardCredit，
// 否则那个号的额度耗尽会被忽略，下一轮又被选中。
func Classify(status int, body string) gateway.ErrorKind {
	lower := strings.ToLower(body)

	// 额度耗尽：状态码或正文命中。**放在状态码判据之前**。
	if status == 402 || containsAny(lower, hardCreditMarkers) {
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

// hardCreditMarkers 积分不足的文案特征。
//
// ⚠ 与其它上游的词表分开维护：那是别家的文案。
var hardCreditMarkers = []string{
	"insufficient credits",
	"insufficient balance",
	"credits exhausted",
	"out of credits",
	"no credits remaining",
	"quota exceeded",
	"payment required",
	"积分不足",
	"余额不足",
	"积分已用完",
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
