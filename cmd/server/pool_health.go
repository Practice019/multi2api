// pool_health.go 账号池主动健康检查任务（移植自 AIClient2API 的 health-check 机制）。
//
// # 干什么
//
// 冷却/熔断中的账号此前只能**被动**等恢复时刻到期；A2 的做法是每 10 分钟
// 主动探测一次（用便宜端点），成功就提前恢复。这里注册成核心调度任务
// `pool-health-check`：
//
//	取池里处于冷却/熔断中、且最近错误超过 minAge 的账号
//	→ 逐个问所属上游的 HealthProbeExt（模型目录/额度等便宜端点）
//	→ 探测成功 → Pool.ClearCooldown 提前恢复（记日志）
//	→ 探测失败 → 只记日志，不惩罚（健康检查不是业务请求）
//
// 被禁用的账号不探测（禁用是用户/会话死亡的明确决定，见 pool 的判据）。
package main

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// healthCheckMinAge 最近错误距今小于该时长的不急着探测（A2 也跳过
// lastError 距现在 < healthCheckInterval 的节点 —— 刚报错的号大概率还坏着）。
const healthCheckMinAge = 2 * time.Minute

// runPoolHealthCheck 一趟账号池健康检查。
func runPoolHealthCheck(ctx context.Context, p *pool.Pool, reg *gateway.Registry) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cands := p.RecoverableCandidates(healthCheckMinAge)
	if len(cands) == 0 {
		return nil
	}
	okN, failN := 0, 0
	for _, c := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pv, ok := reg.Get(c.Provider)
		if !ok {
			continue // 上游没接上（配置里关了）→ 账号本来就要被对账逐出
		}
		hp, ok := gateway.ExtOf[gateway.HealthProbeExt](pv)
		if !ok {
			continue // 该上游不参与主动探测（冷却到期自然恢复）
		}
		cred, ok := registryCredential(p, reg, c.Provider, c.UID)
		if !ok {
			continue
		}
		if err := hp.ProbeHealth(ctx, cred); err != nil {
			failN++
			continue
		}
		if p.ClearCooldown(c.UID) {
			log.Printf("pool: 健康检查通过，提前恢复冷却账号 uid=%s provider=%s（原原因：%s）",
				c.UID, c.Provider, c.Reason)
			okN++
		}
	}
	if okN > 0 || failN > 0 {
		log.Printf("pool: 健康检查完成 —— 提前恢复 %d，仍不可用 %d", okN, failN)
	}

	// ⚠ 2026-10-26：**自动禁用账号的恢复探测已搬走**，不再挂在本任务上。
	//   它现在是一条独立任务 pool-revive-probe，间隔默认 24 小时且可配。
	//   理由见 config 里 ReviveProbeIntervalSeconds 的注释：冷却是"歇一会儿"
	//   （30 分钟量级），被禁是"等天级恢复"，两者共用一个 10 分钟的节奏时
	//   后者会每 10 分钟白打一次上游。搬走之后**两边都不会被探两次**
	//   （本任务的 RecoverableCandidates 本来就不含自动禁用的号，见 health.go）。
	return nil
}

// reviveProbeMinAge 解封探测的最小"最近错误"年龄。
//
// 刻意取 0（不设门槛），与本任务上面那个 2 分钟的门槛不同：
// 那条门槛是为了"刚报错的号别急着探"，而本任务**本身就是 24 小时一轮**——
// 上一轮到现在至少过了 24 小时，"刚报错"这种情形在时间尺度上不存在。
// 再叠一层门槛只会让"禁用后再也没被碰过"的号永远等不到探测。
const reviveProbeMinAge = time.Duration(0)

// runPoolReviveProbe 一趟「解封探测」：给被自动禁用的账号发一个便宜请求探活，
// 通了就解除禁用。
//
// # 为什么必须有一条主动探测（否则"一次成功即恢复"是死条款）
//
// 自动禁用把号从候选集摘掉，而解除禁用依赖「这个号请求成功一次」——
// 它已经选不中了，所以**永远不会有那一次成功**。这是自动禁用机制的经典死锁
// （new-api 靠定期 channel test 打破它，见 controller/channel-test.go）。
// 池侧已经把两件事备好了：AutoDisabledRecoverables 给候选集、
// RecordAutoDisableSuccess 负责解除**本机制置的**禁用；本函数是那个**调用方**。
//
// # 为什么探的是"便宜端点"而不是真发一次对话
//
// 用的是各上游的 HealthProbeExt（模型目录/额度等）——
// 与 pool-health-check 同一条判据，不额外消耗对话额度。
// 上游没实现 HealthProbeExt 时**跳过**（关掉自动禁用会由对账逐出），
// 与 health-check 的行为一致。
//
// # 人工禁用不会被翻案
//
// AutoDisabledRecoverables 只返回 **owned=true**（即本机制置的）禁用；
// 操作员按下的禁用、凭证死亡导致的禁用都不在里面 —— 探测成功不该推翻人的决定。
// 判据见 pool/autodisable.go 的注释与 new-api 的 ShouldEnableChannel。
func runPoolReviveProbe(ctx context.Context, p *pool.Pool, reg *gateway.Registry) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cands := p.AutoDisabledRecoverables(reviveProbeMinAge)
	if len(cands) == 0 {
		return nil
	}
	okN, failN := 0, 0
	for _, c := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pv, ok := reg.Get(c.Provider)
		if !ok {
			continue // 上游没接上（配置里关了）→ 账号本来就要被对账逐出
		}
		hp, ok := gateway.ExtOf[gateway.HealthProbeExt](pv)
		if !ok {
			continue // 该上游不参与主动探测
		}
		cred, ok := registryCredential(p, reg, c.Provider, c.UID)
		if !ok {
			continue
		}
		if err := hp.ProbeHealth(ctx, cred); err != nil {
			failN++
			continue
		}
		if p.RecordAutoDisableSuccess(c.UID) {
			log.Printf("pool: 解封探测通过，解除自动禁用 uid=%s provider=%s（原原因：%s）",
				c.UID, c.Provider, c.Reason)
			okN++
		}
	}
	if okN > 0 || failN > 0 {
		log.Printf("pool: 解封探测完成 —— 解除 %d，仍不可用 %d", okN, failN)
	}
	return nil
}

// registryCredential 按 (provider, uid) 组装凭证（与 registryRouter.Credential 同构，
// 见 multiprovider.go 的注释：secret 为 nil 时回落到 *auth.Auth 本体）。
func registryCredential(p *pool.Pool, reg *gateway.Registry, id, uid string) (gateway.Credential, bool) {
	if _, ok := reg.Get(id); !ok {
		return gateway.Credential{}, false
	}
	if p == nil {
		return gateway.Credential{}, false
	}
	a := p.AuthByUID(uid)
	if a == nil {
		return gateway.Credential{}, false
	}
	if got, ok := p.ProviderOf(uid); !ok || got != id {
		return gateway.Credential{}, false
	}
	cred := gateway.Credential{Provider: id, UID: uid, Nickname: a.Nickname}
	if secret, ok := p.SecretOf(uid); ok && secret != nil {
		cred.Secret = secret
	} else {
		cred.Secret = a // 默认上游形态：Secret 就是 *auth.Auth 本体
	}
	return cred, true
}
