// upstream_error.go 上游返回非 2xx 时的**结构化**错误（带状态码）。
//
// # 为什么需要它（2026-10-20 实测发现）
//
// 渠道转发原本把所有上游失败一律包成 502 channel_error。实测踩到后果：
// 上游 OpenCSG 对 `deepseek-v4.1-flash-1017` 返回
//
//	HTTP 429 {"error":{"code":"capacity_exceeded","message":"model capacity exceeded: tpm_exceeded"}}
//
// （该模型每分钟 token 额度用尽），经网关后变成 **502** —— 客户端看到"网关故障"于是**不会**
// 按限流重试，用户也被指错了排查方向（他会去查网关，而真相是上游配额）。
//
// 绕过网关直连上游验证过：同一把 key 打 /models ⇒ 200，打 /chat/completions ⇒ 429，
// 两种请求（带/不带 max_tokens）都是 429 ⇒ 与网关无关，是上游真实限流。
//
// **透传上游状态码是 new-api 的做法，也是任何中转站的基本要求：不要替上游改口径。**
//
// # 为什么定义在 gateway
//
// 核心层（internal/server）**不能** import 任何具体上游（架构守卫
// TestCoreDoesNotDependOnUpstreams），而它又必须能读出"这是上游的 429"。
// 把形状放进 gateway —— 核心与上游都依赖的那层契约 —— 两边都不用认识对方。
package gateway

import (
	"errors"
	"fmt"
)

// UpstreamHTTPError 上游返回的非 2xx。
type UpstreamHTTPError struct {
	// Status 上游返回的 HTTP 状态码（429/401/400/5xx…）。调用方据此决定透传什么给客户端。
	Status int
	// Body 上游响应体片段（供客户端与日志定位）。
	//
	// ⚠ 调用方负责保证其中不含密钥 —— 上游的错误体不该带 key，而我们的出站请求也从不回显它。
	Body string
	// Op 哪一步失败（chat / models …），便于日志定位。
	Op string
}

// Error 的文案与改造前**逐字一致**。
//
// ⚠ 刻意让 Op 不进文案：既有测试与日志都在匹配这个形状（"上游返回 HTTP %d（响应体片段：…）"），
// 这一改动是**加法**（多一种取状态码的能力），不该顺手改掉别人的口径 —— 那种"顺手"正是回归的来源。
func (e *UpstreamHTTPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("上游返回 HTTP %d（响应体片段：%s）", e.Status, e.Body)
}

// AsUpstreamHTTPError 从错误链里取出上游状态码。
//
// 用 errors.As 而不是类型断言：上游错误会被 wrap 若干层（渠道名 + 步骤名），
// 类型断言在包装后必然失败 —— 而那正是这条错误在真实链路里的形态。
func AsUpstreamHTTPError(err error) (*UpstreamHTTPError, bool) {
	var ue *UpstreamHTTPError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// RetryableStatus 该状态码是否值得"换 key / 换渠道"重试。
//
// 范围对齐 new-api 的默认可重试集合：408（超时）/ 429（限流）/ 5xx（上游故障）。
// ⚠ 400/401/403 这类**确定性**错误不该重试：重试只是把同一个错误多打几遍，
// 还会把"这个渠道的 key 不对"掩盖成"重试也没用"。
func RetryableStatus(code int) bool {
	return code == 408 || code == 429 || code >= 500
}
