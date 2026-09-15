// healthprobe_ext.go —— 主动健康检查扩展点（移植自 AIClient2API 的 health-check 机制）。
//
// # 为什么要主动探测（移植动机）
//
// 此前冷却/熔断的账号**只能被动恢复**：要等冷却时刻自然到期，或等某次
// 请求恰好选中它。两个问题：
//
//	① 额度是权益包时（trae / codearts / loomy），上游经常"提前回血"——
//	   冷却还有几个小时才到期，账号其实已经能用了，但一直被晾着；
//	② 401 禁用的账号（会话被踢）只有重新登录才能恢复，而登录往往
//	   只需重跑一次 OAuth —— 探测成功说明会话其实已恢复。
//
// AIClient2API（A2）的做法：每 10 分钟对不健康/冷却中的节点发一次
// **健康检查请求**（用一个便宜的健康模型），成功就 `markProviderHealthy`
// 自动恢复。本扩展点把同一件事交给上游自报：
//
//	if hp, ok := gateway.ExtOf[gateway.HealthProbeExt](pv); ok {
//	    err := hp.ProbeHealth(ctx, cred)   // nil = 账号健康可用
//	}
//
// 没有实现该扩展点的上游 = 不参与主动探测（冷却到期自然恢复），不报错。
package gateway

import "context"

// HealthProbeExt 上游自报「怎么探测我这个账号还活着」。
//
// # 实现要求
//
//   - 必须用**便宜**的端点（模型目录 / 额度查询这类），不要用完整对话
//   - 必须真实校验凭证（鉴权头会随请求带出，401 即失败）
//   - 失败返回 error，调用方只记日志、不惩罚（健康检查不是业务请求）
//
// 由核心的健康检查任务（cmd/server 注册的 pool-health-check）调用：
// 对处于冷却/熔断/禁用中的账号逐个探测，成功则提前恢复。
type HealthProbeExt interface {
	// ProbeHealth 探测一份凭证是否健康可用。nil = 健康。
	ProbeHealth(ctx context.Context, cred Credential) error
}
