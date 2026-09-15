// health.go 主动健康检查与刷新失败计数（移植自 AIClient2API 的稳定性机制）。
//
// # 从 A2 移植的三件事
//
//	① RecoverableCandidates + ClearCooldown —— 主动探测冷却中的账号：
//	   A2 每 10 分钟对不健康节点发一次健康检查（便宜端点），成功即恢复。
//	   此前冷却只能被动到期，额度"提前回血"的账号会被白白晾几小时。
//	② 熔断失败计数的时间窗衰减 —— A2 的错误计数在 10s 窗口外重置：
//	   只有**突发**失败才累计到熔断阈值；零散错误不该慢慢攒坏一个号。
//	③ NoteRefreshFailure —— A2 刷新最多试 5 次，连续失败直接判不健康：
//	   refresh token 已死，反复白试只会烧请求次数。
package pool

import (
	"sort"
	"time"
)

// breakerFailureWindow 熔断失败计数的衰减窗口。
//
// 窗口内（从最近一次错误起 60s）的失败才累计；窗口外的失败先把计数清零再记。
// 语义与 A2 的 10s 错误窗口一致，只是取更保守的 60s：
// "只有突发才算数"而"隔很久一次的零星错误不叠加"。
const breakerFailureWindow = 60 * time.Second

// maxRefreshFails 凭证续期连续失败上限（A2 的 refresh max attempts，5 → 我们取 3）。
// 达到上限说明 refresh token 已死，直接禁用（需重新登录），不再无限重试。
const maxRefreshFails = 3

// Recoverable 一个值得主动探测的账号（处于冷却/熔断中）。
type Recoverable struct {
	UID      string `json:"uid"`
	Provider string `json:"provider,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RecoverableCandidates 返回值得主动探测的账号。
//
// 判据（与 A2 的 health check 列表同构）：
//
//	① 处于冷却（until）或熔断（breakerUntil）中，且**未到恢复时刻**
//	  —— 已经到点的账号走自然恢复，探测它们没有收益；
//	② 最近一次错误不在 minAge 内（刚报错的账号不急着探，
//	   A2 也跳过 lastError 距现在 < healthCheckInterval 的节点）；
//	③ **不含**被禁用的账号（A2 的 isDisabled 在健康检查里直接 continue
//	   —— 禁用是用户/会话死亡的明确决定，探测成功也不该自动翻案）。
//
// 探测成功后调用方用既有的 Pool.ClearCooldown(uid) 恢复（它已清
// 冷却/熔断/软限流计数 —— 语义与"健康检查宣布这个号没问题"一致）。
func (p *Pool) RecoverableCandidates(minAge time.Duration) []Recoverable {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	out := make([]Recoverable, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if e == nil {
			continue
		}
		if e.disabled {
			continue
		}
		inCooldown := e.until.After(now) || e.breakerUntil.After(now)
		if !inCooldown {
			continue
		}
		if minAge > 0 && !e.lastErr.IsZero() && now.Sub(e.lastErr) < minAge {
			continue
		}
		out = append(out, Recoverable{UID: uid, Provider: e.provider, Reason: e.reason})
	}
	// 确定性顺序（健康检查逐号串行，稳定顺序便于日志对照）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// NoteRefreshFailure 记录一次凭证续期失败；连续失败达上限返回 true（该禁用）。
//
// # 为什么由 pool 管而不是各上游自己管
//
// 各上游的刷新实现（CredentialRefresher）各不相同，但"连续失败几次就该
// 放弃这个号"是**跨上游统一**的稳定性语义 —— 与熔断/冷却同一层。
// 清零点：NoteSuccess（一次成功说明 token 又活了）。
func (p *Pool) NoteRefreshFailure(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e == nil {
		return false
	}
	e.refreshFails++
	if e.refreshFails >= maxRefreshFails {
		e.disabled = true
		e.reason = "凭证续期连续失败 " + itoa(e.refreshFails) + " 次（refresh token 失效，需重新登录）"
		p.dirty.Store(true)
		return true
	}
	return false
}
