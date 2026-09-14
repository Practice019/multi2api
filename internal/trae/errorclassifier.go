// errorclassifier.go TRAE 的 错误分类（gateway.ErrorClassifier 实现）。
//
// # 判据来自 traework2api 的实测（SPEC §4.3），映射到本网关的 ErrorKind
//
//	1005 + plan        → HardCredit  权益不足，硬冷却（账号整体没额度了）
//	401                → SessionDead token 失效（JWT 过期/被踢）→ 永久禁用
//	429                → SoftRate    限流 → 短冷却
//	404                → NotFound    模型不存在 → 只换号不累计
//	5xx                → Server      上游故障 → 冷却
//	其他 4xx           → Client      参数错 → 只换号
//	流内 event:error 1005 → HardCredit（SSE 已转成 {"error":{"code":"1005",...}}）
//
// ⚠ 只有**自己真正认识**的判据才返回硬分类；拿不准返回 ErrKindNone
// （只换号不惩罚）—— 与 gateway.ErrorClassifier 的契约一致。
package trae

import (
	"strings"

	"workbuddy2api/internal/gateway"
)

// sessionDeadMarkers 401 时区分"会话失效"与其它 401 的正文特征。
var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized"}

// Classify 按 HTTP 状态码 + body 判定错误类别（gateway.ErrorClassifier）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	return Classify(status, body)
}

// Classify 纯函数版（便于测试直接调用）。
func Classify(status int, body string) gateway.ErrorKind {
	lower := strings.ToLower(body)

	// 1005 + plan → 权益不足（硬冷却）。流内错误信封转换后是
	// {"error":{"code":"1005","message":"..."}}，也会命中下面的判断。
	if strings.Contains(body, `"code":1005`) ||
		(strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return gateway.ErrKindHardCredit
	}
	if status == 401 {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, m) {
				return gateway.ErrKindSessionDead
			}
		}
		// 401 一律按会话失效（JWT 过期最典型）
		return gateway.ErrKindSessionDead
	}
	if status == 429 {
		return gateway.ErrKindSoftRate
	}
	if status == 404 {
		return gateway.ErrKindNotFound
	}
	if status >= 500 {
		return gateway.ErrKindServer
	}
	if status >= 400 {
		return gateway.ErrKindClient
	}
	return gateway.ErrKindNone
}

// 编译期断言：Provider 实现错误分类扩展点。
var _ gateway.ErrorClassifier = (*Provider)(nil)
