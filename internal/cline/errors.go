// errors.go Cline 的错误分类与续期终态判定。
//
// # 为什么需要续期终态的**哨兵错误**
//
// 参照项目的调度器（refresh.ts）用 `error.name === 'RefreshTokenExpiredError'`
// 判定"这是一个终态错误，停止续期"，其余错误一律按可重试处理（网络抖动 1 分钟后
// 重试、5xx/429 10 分钟后重试）。
//
// 那个判据的由来是一条真实缺陷：CodeArts 与 Buddy **各自导出同名**的
// RefreshTokenExpiredError，跨模块 identity 不同，用 `instanceof` 判定会让
// 其中一个 provider 的失效信号穿透成"可重试"而无限重试
// （参照 refresh.ts:14-25）。所以参照项目改用 name 比较。
//
// Go 里没有这个问题：哨兵错误 + errors.Is 就是稳定的身份判定，
// 比字符串比较更不易漂移。本包据此定义 ErrRefreshExpired。
package cline

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// ErrRefreshExpired 续期遇到**终态**失败：refresh_token 已失效，需重新登录。
//
// 判定依据（参照 cline-auth.ts:290-297, 372-374, 389-394）：
//
//	HTTP 401 / 403          → 终态
//	2xx 但响应缺少 token    → 终态
//
// ⚠ 与"可重试"的边界是本常量存在的全部意义：
// 网络抖动、5xx、429 **不得**归入这里 —— 那会让调度器停止续期，
// 用户被迫在每次网络抖动后重新登录。
var ErrRefreshExpired = errors.New("cline: refresh_token 已失效，请重新登录")

// Classify 按 HTTP 状态码 + 响应体判定错误类别（gateway.ErrorClassifier）。
//
// # 判据（参照 openai-compat.ts:296-302 与 cline-adapter.ts 的三类特判）
//
//	401/403          → ErrKindAuth        凭证失效
//	429              → ErrKindSoftRate    限流
//	400              → ErrKindClient      客户端错误
//	>=500            → ErrKindServer      服务端故障
//	402 / 额度文案    → ErrKindHardCredit  额度耗尽
//	其它 4xx          → ErrKindClient
//	其它              → ErrKindNone
//
// # ⚠ 403 的语义分裂（参照 cline-adapter.ts:608-646）
//
// 同一个 403 既是"凭据失效"也是"地域限制"（`not available in your region`
// 等文案）。后者**不该**触发续期与换号 —— 换号、重登都改变不了地域，
// 而且若把它报成 AUTH，客户端会渲染成「API 密钥无效」，真实原因彻底丢失。
//
// 但 gateway.ErrorKind 里没有"地域限制"这一档，而下游（pool 的冷却策略）
// 只认这些类别。所以这里的处理是：**地域限制归到 ErrKindClient**
// （只换号不罚，不给账号冷却）—— 那是与"服务端策略拒绝"最接近的语义，
// 且不会把账号打上"需重新登录"的标记。续期路径另有 permissionDenied
// 判定（见 classifyRefresh），两者互不影响。
func Classify(status int, body string) gateway.ErrorKind {
	lower := strings.ToLower(body)

	// 额度耗尽：402 或文案命中
	// 放在状态码判据之前，因为额度耗尽的形态可能是 200 + 业务错误。
	if status == 402 || containsAny(lower, hardCreditMarkers) {
		return gateway.ErrKindHardCredit
	}

	switch {
	case status == 401:
		return gateway.ErrKindAuth
	case status == 403:
		// 地域限制 → 客户端类（只换号不罚）；其余 403 → 凭证失效
		if containsAny(lower, regionBlockedMarkers) {
			return gateway.ErrKindClient
		}
		return gateway.ErrKindAuth
	case status == 429:
		return gateway.ErrKindSoftRate
	case status == 400:
		return gateway.ErrKindClient
	case status >= 500:
		return gateway.ErrKindServer
	case status >= 400:
		return gateway.ErrKindClient
	}
	return gateway.ErrKindNone
}

// hardCreditMarkers 额度耗尽的文案特征。
//
// ⚠ 与 workbuddy 的词表**分开维护**：那是另一个上游的文案。
// 这里只收 Cline 实测/公开的形态。
var hardCreditMarkers = []string{
	"insufficient credits",
	"insufficient balance",
	"out of credits",
	"no credits",
	"credits exhausted",
	"payment required",
	"quota exceeded",
	"余额不足",
	"额度不足",
}

// regionBlockedMarkers 地域/访问限制的文案特征（403 的第二种语义）。
var regionBlockedMarkers = []string{
	"not available in your region",
	"not available in your country",
	"region not supported",
	"unsupported region",
	"geographic restriction",
	"country is not supported",
}

// permissionDeniedMarkers 地域限制在续期路径上的判据（参照 cline-adapter.ts:608-646）。
//
// 续期遇到这些文案时应**跳过重试**：地域限制不是凭据问题，
// 反复续期只会持续被拒。
var permissionDeniedMarkers = regionBlockedMarkers

// IsPermissionDenied 报告响应体是否表示地域/权限限制。
//
// 用途：续期路径据此**跳过重试**并如实上报（而不是无限重试或换号）。
func IsPermissionDenied(body string) bool {
	return containsAny(strings.ToLower(body), permissionDeniedMarkers)
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
