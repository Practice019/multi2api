// errors.go PhanthyWork 的错误分类与本地哨兵。
//
// # 为什么分类必须由上游自己答（gateway.ErrorClassifier 的契约）
//
// 核心的出站循环拿到的只有 (HTTP 状态码, 上游响应体原文)，而**同一个状态码在
// 这个平台上混着两类完全不同的拒绝**。报告 §4.2 的原话：
//
//	⚠ 402 和 403 里混着两类完全不同的拒绝（"没积分" vs "套餐不含"），
//	  必须按**正文**分开，不能只看状态码。
//
// 因此本文件逐条对齐报告 §4.2 的「错误码分流表」，并用本仓的
// 跨上游中立词汇（gateway.ErrorKind）表达它。
//
// # 一条容易搞反的取舍
//
// 报告 §3 的 12 个坑里有一半的**症状**都是 403 request_not_authorized
// （时钟偏移、UA、签名段序、推理令牌过期……）。这里刻意**不**把
// request_not_authorized 归成"账号有问题"那一档：
//
//	它要么是客户端侧的拼装问题（本包的 bug），要么是推理令牌过期（可自愈）
//	把它判成 ErrKindSessionDead = **永久禁用这个账号** —— 一个账号会因为
//	一个 3 分钟的令牌过期被人工重登，这是本仓明确禁止过的形态
//	（见 gateway.ErrorClassifier 的文件头：workbuddy 的裸数字判据
//	  曾把 codearts 的健康账号永久禁用）。
//
// 所以 request_not_authorized → ErrKindClient（**只换号不罚**），
// 而"设备绑定冲突"这条真正的终态单独走 409 判据。
package phanthywork

import (
	"errors"
	"fmt"
	"strings"

	"workbuddy2api/internal/gateway"
)

// ── 本地哨兵 ─────────────────────────────────────────────────────────────

var (
	// ErrRegistrationConflict 该 installationId 已绑定另一把公钥（409）。
	//
	// 报告 §3⑥：**终态**，换公式 / 换公钥 / 重跑注册都是同一条 409。
	// 上游给它 24 小时熔断的理由也在同一处：期内重发等于持续触发
	// 服务端设备风控，还可能把该账号的其它调用一起拖下水。
	ErrRegistrationConflict = errors.New("phanthywork: 设备绑定冲突（409 终态，该 installationId 已绑定另一把公钥）")

	// ErrRefreshExpired refreshToken 已失效（invalid_grant / 无 refresh_token）→ 终态。
	//
	// 报告 §2.1 的判据原文：只有 invalid_grant / invalid_client /
	// unauthorized_client 才算"凭据废了"，其余（5xx / 429 / 网络）都是抖动，可重试。
	ErrRefreshExpired = errors.New("phanthywork: 登录凭据已失效（refreshToken 被吊销或已轮换，需重新登录）")

	// ErrNotConfigured 该部署没有可用的登录流程。
	ErrNotConfigured = errors.New("phanthywork: 本部署未配置登录流程")

	// ErrNoCredentials 凭证目录里没有可用凭证（正常状态，不是故障）。
	ErrNoCredentials = errors.New("phanthywork: 没有可用凭证")
)

// ── 错误分类 ─────────────────────────────────────────────────────────────

// 判据表（**只放本上游真实观测到的串**）。
//
// ⚠ 判据来源必须是**本上游自己的**响应。报告 §4.2 给出的是这份平台的分流表；
// 抄别家的错误码表正是 gateway.ErrorClassifier 文件头记的那两个危害的根因。
var (
	// hardCreditMarkers 额度真的用尽（吃长冷却，等重置）。
	hardCreditMarkers = []string{
		"insufficient_credits",
		"insufficient credits",
		"credit_balance_insufficient",
		"额度不足",
		"积分不足",
	}

	// planMarkers 套餐不含这个模型 —— **不是**额度问题，别吃长冷却。
	//
	// 报告 §4.2 明确把这一类从"积分"里分出来：积分再多也不行，
	// 处置是"换模型"，账号本身是健康的。
	planMarkers = []string{
		"model_not_allowed",
		"model_not_entitled",
		"not allowed for this plan",
		"plan_required",
		"missing_or_expired_plan",
		"no active plan",
		"模型不可用",
	}

	// modelMissingMarkers 上游不认识这个 id（换模型或重新拉目录）。
	modelMissingMarkers = []string{"model_not_found"}

	// bindingConflictMarkers 设备绑定冲突（409 终态）。
	bindingConflictMarkers = []string{
		"desktop_installation_conflict",
		"device_binding_conflict",
	}

	// bindingInvalidMarkers 绑定/签名算错（**本地 bug**，不是账号问题）。
	//
	// 报告 §2.4 的失败模式表：400/403 desktop_binding_invalid 值得**查代码**。
	// 所以它们只能落在"只换号不罚"那一档 —— 罚号会掩盖真正的根因。
	bindingInvalidMarkers = []string{
		"desktop_binding_invalid",
		"desktop_account_scope_invalid",
	}

	// authExpiredMarkers 登录态失效（可自动续期 / 需重登）。
	authExpiredMarkers = []string{
		"invalid_grant",
		"invalid_client",
		"unauthorized_client",
		"token_expired",
		"invalid_token",
		"desktop_installation_required",
		"登录已过期",
	}

	// softRateMarkers 限流（短冷却）。
	softRateMarkers = []string{
		"rate_limit",
		"ratelimit",
		"too many requests",
		"429",
		"限流",
	}
)

// Classify 按 (状态码, 响应体) 判定错误类别（gateway.ErrorClassifier）。
//
// **纯函数**：不发网络、不读写共享状态、不 panic（契约要求）。
// 它在出站循环的每次非 2xx 上被调用一次。
//
// 判据顺序（从"最确定"到"最保守"）：
//
//	① 正文里的具体业务码（这是本上游唯一可靠的判据来源）
//	② 状态码（RFC 语义，跨上游真的有共识的那一层）
//	③ 都不认识 → ErrKindNone（**不猜**）
//
// ⚠ 拿不准时回 ErrKindNone 比猜一个类别安全：猜错会让核心施加错误的冷却/禁用
// （gateway.ErrorClassifier 的两个危害就是猜错造成的）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	return Classify(status, body)
}

// Classify 是本包的分类实现（包级函数，便于离线测试）。
func Classify(status int, body string) gateway.ErrorKind {
	// ① 正文优先。**先看正文**的理由：同一状态码混着两类拒绝（§4.2）。
	switch {
	case gateway.ContainsAny(body, bindingConflictMarkers):
		// 设备绑定冲突 —— 唯一被本包判成"终态"的一类。
		// 报告 §2.4：409 之后该账号的**推理面彻底报废**，但数据面
		// （模型目录/额度/签到只读）不受影响 ⇒ 只有推理不可用 = 会话死。
		return gateway.ErrKindSessionDead
	case gateway.ContainsAny(body, hardCreditMarkers):
		return gateway.ErrKindHardCredit
	case gateway.ContainsAny(body, planMarkers):
		// 套餐不含：账号健康，换模型即可 ⇒ 只换号不罚。
		return gateway.ErrKindClient
	case gateway.ContainsAny(body, modelMissingMarkers):
		return gateway.ErrKindNotFound
	case gateway.ContainsAny(body, bindingInvalidMarkers):
		// 签名/绑定算错 = 本地拼装问题 ⇒ 只换号不罚（罚号会掩盖根因）。
		return gateway.ErrKindClient
	case gateway.ContainsAny(body, authExpiredMarkers):
		return gateway.ErrKindAuth
	case gateway.ContainsAny(body, softRateMarkers):
		return gateway.ErrKindSoftRate
	}

	// ② 状态码。
	switch {
	case status == 402:
		return gateway.ErrKindHardCredit
	case status == 429:
		return gateway.ErrKindSoftRate
	case status == 404:
		return gateway.ErrKindNotFound
	case status == 409:
		// 走到这里说明正文里没有我们认识的冲突码，但 409 在本上游
		// 只有一个来源（设备绑定），保守按终态处理。
		return gateway.ErrKindSessionDead
	case status == 401:
		return gateway.ErrKindAuth
	case status == 403:
		// ⚠ 刻意**不**升级成 SessionDead：
		// 403 里混着 request_not_authorized（时钟/UA/签名/令牌过期）
		// 与 model_not_allowed（套餐），两类都不是账号的错。
		return gateway.ErrKindClient
	case status == 408:
		// 上游读超时：临时性、上游侧（与 DefaultErrorKind 的判据一致）。
		return gateway.ErrKindServer
	case status >= 500:
		return gateway.ErrKindServer
	case status >= 400:
		return gateway.ErrKindClient
	}
	return gateway.ErrKindNone
}

// ── 上游错误的本地载体 ───────────────────────────────────────────────────

// UpstreamError 一次"上游给了非 2xx 响应"的错误载体。
//
// 它存在的理由是 gateway.Provider.Chat 的契约：**非 2xx 也要正常返回
// ChatStream**（把上游错误体带回来给核心分类），只有网络层失败才返回 error。
// 于是我们既需要把 (status, body) 带回来，也需要在"连 ChatStream 都还
// 构造不出来"的阶段（例如换推理令牌时上游就拒绝了）有一个可读的错误。
type UpstreamError struct {
	// Status HTTP 状态码（0 = 没拿到响应）。
	Status int
	// Body 上游响应体原文（**原样**，不做裁剪/美化 —— 分类器与排查都靠它）。
	Body string
	// Op 出错的阶段（"设备注册" / "换推理令牌" / "调模型"…），中文，直接进日志。
	Op string
}

// Error 实现 error。
func (e *UpstreamError) Error() string {
	if e == nil {
		return "phanthywork: 上游错误"
	}
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	if e.Status == 0 {
		return fmt.Sprintf("phanthywork: %s失败: %s", e.Op, body)
	}
	return fmt.Sprintf("phanthywork: %s失败（HTTP %d）: %s", e.Op, e.Status, body)
}

// Kind 返回这条错误的分类（便于调用方在没有 ChatStream 的路径上做判断）。
func (e *UpstreamError) Kind() gateway.ErrorKind {
	if e == nil {
		return gateway.ErrKindNone
	}
	return Classify(e.Status, e.Body)
}

// IsRegistrationConflict 报告一个错误是否是设备绑定终态。
func IsRegistrationConflict(err error) bool { return errors.Is(err, ErrRegistrationConflict) }

// IsRefreshExpired 报告一个错误是否是"凭据废了"的终态。
func IsRefreshExpired(err error) bool { return errors.Is(err, ErrRefreshExpired) }
