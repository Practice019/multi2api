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
