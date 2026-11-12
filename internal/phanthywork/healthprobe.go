// healthprobe.go 账号池主动健康检查（gateway.HealthProbeExt）。
//
// # 为什么本上游需要它（以及为什么是"最小可用")
//
// 核心的池级任务（pool-health-check / pool-revive-probe）会对处于**冷却/熔断/禁用**
// 中的账号逐个探测，成功就提前恢复 —— 否则那些号只能等冷却自然到期。
// 本仓库对"探测"的硬要求是**只读、便宜、绝不触发写操作**（见 HealthProbeExt 的注释：
// "必须用便宜的端点（模型目录 / 额度查询这类），不要用完整对话"）。
//
// 本上游选 `GET /api/oauth/profile`：
//
//	它只需要 Bearer（报告实测五个只读端点全 200，这条在其中）
//	它**不写**任何东西（对比 desktop-installations/register 与 daily-login/claim 都是写操作）
//	它真的校验凭据（401/403 立刻暴露"这个号的 access token 已废"）
//
// ⚠ 不用 Chat 探活：那是完整对话，会消耗额度、也可能是限流来源。
// ⚠ 也不用 activities/summary：它虽然也是只读的，但**会换取一个新的推理令牌**
// （一个副作用），而健康检查会在冷却/禁用号上反复跑 —— 不该带副作用。
package phanthywork

import (
	"context"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言。
var _ gateway.HealthProbeExt = (*Provider)(nil)

// probeTimeout 探活超时（只读端点、冷却号可能很多，取短一点）。
const probeTimeout = 15 * time.Second

// ProbeHealth 探测一份凭证是否健康可用（nil = 健康）。
//
// 失败只返回 error，调用方**只记日志、不惩罚**（健康检查不是业务请求）——
// 这条是契约要求，本实现不额外做任何状态变更。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if strings.TrimSpace(a.OAuth.AccessToken) == "" {
		return errNoAccessToken
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// 只读：取一次账号档案。成功即证明这份 access token 现在可用。
	if _, _, err := p.client.FetchAccountScope(ctx, a); err != nil {
		return err
	}
	return nil
}
