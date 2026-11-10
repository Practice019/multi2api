package loomy

import (
	"strings"
	"sync"
)

// quota_auth.go 记录「最近一次额度查询是不是**因为凭据失效**而失败」。
//
// # 为什么由本包来记（这是上游的事实）
//
// `QuotaExt.RefreshQuota` 的契约只回 (QuotaView, ok)，没有错误原文 —— 核心
// 因此分不清"这个号的 session 死了"与"我们只是查不到额度"。而本包**看得见**
// 那次失败的原因：积分网关对失效 session 回的是
//
//	HTTP 200 + {"code":"100002","desc":"登录已失效，请重新登录"}
//
//（实测 2026-11-10：45 个真实账号逐个打，5 个回这一句；复测仍是同一句。）
//
// `pointsCall` 把 `desc` 原样拼进 error（见 points.go 的 "积分接口 %s 返回 %s: %s"），
// 所以判据就是**错误原文里有没有那句 desc** —— 与 errorclassifier.go 的
// markerSessionDead 用同一个字符串，不新造第二份词汇表。
//
// # 为什么不是永久标记
//
// 它是"**上一次查询的结论**"：下一次成功查询必须清掉。否则用户重登之后，
// 这个号会被一直当成失效的（那正是"禁用后刷新成功却不恢复"的一半原因）。
type quotaAuthTracker struct {
	mu   sync.Mutex
	dead map[string]bool
}

// mark 记下某账号最近一次查询是否为凭据失效（成功则清掉）。
func (t *quotaAuthTracker) mark(uid string, dead bool) {
	if uid == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !dead {
		// 成功即清：不建空记录（与 RecordAutoDisableSuccess 同一条节约手法）。
		delete(t.dead, uid)
		return
	}
	if t.dead == nil {
		t.dead = map[string]bool{}
	}
	t.dead[uid] = true
}

// isDead 读某账号最近一次查询的结论。
func (t *quotaAuthTracker) isDead(uid string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dead[uid]
}

// QuotaAuthFailed 实现 gateway.QuotaAuthFailureExt。
func (p *Provider) QuotaAuthFailed(uid string) bool {
	return p.quotaAuth.isDead(uid)
}

// quotaErrIsSessionDead 本次额度查询的失败是不是「凭据失效」。
//
// ⚠ 判据只有一条：错误原文里带上游那句 desc（markerSessionDead）。
// **不许**在这里加宽判据（网络超时、5xx、限流都不是账号的错）——
// 宽判据的代价是"一次上游故障把整池号摘掉"，而漏判的代价只是"这个号
// 多留一轮，等下一次刷新再判"。方向不对称，所以宁可漏判。
func quotaErrIsSessionDead(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), markerSessionDead)
}