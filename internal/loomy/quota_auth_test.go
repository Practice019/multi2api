package loomy

import (
	"errors"
	"testing"

	"workbuddy2api/internal/gateway"
)

// quota_auth_test.go —— 「这次额度查询失败是不是凭据失效」的判据。
//
// # 为什么这条判据的**宽窄**比它是否正确更重要
//
// 它驱动的是"摘号"：
//
//	判宽了（把网络超时也算） ⇒ 一次上游故障把整池号刷成禁用（不可接受）
//	判窄了（漏掉真失效）     ⇒ 这个号多留一轮，等下一次刷新再判（可接受）
//
// ⇒ 判据只能有一条：错误原文里带上游那句 `desc`。不许加启发式。
func TestQuotaErrIsSessionDeadOnlyMatchesUpstreamDesc(t *testing.T) {
	// 实测原文（2026-11-10，45 个真实账号逐个打；5 个回这一句）。
	// 它经 points.go 的 "积分接口 %s 返回 %s: %s" 拼进 error。
	dead := errors.New("loomy: 积分接口 /points/records 返回 100002: 登录已失效，请重新登录")
	if !quotaErrIsSessionDead(dead) {
		t.Error("实测的失效原文必须被判为凭据失效")
	}
	// 第二种已实测的账号级形态（点「刷新本上游额度」时真实出现）：100003 账号已被停用。
	// 它与 100002 同属「账号层、不自愈」，但成因不同（一个要重登、一个要等解封）。
	suspended := errors.New("loomy: 积分接口 /points/records 返回 100003: 账号已被停用")
	if !quotaErrIsSessionDead(suspended) {
		t.Error("「账号已被停用」也是账号级失效，必须被判为需要摘号")
	}

	// 网络类 / 5xx / 解析类**都不是**账号的错。
	for _, msg := range []string{
		"loomy: 调用积分接口失败（/points/records）: dial tcp 1.2.3.4:443: i/o timeout",
		"loomy: 积分接口 /points/records 返回 500: 服务开小差",
		"loomy: 积分账本回执里没有 balance/availableBalance 字段: {}",
		"loomy: 积分接口回执不是合法 JSON（HTTP 502 waf）: <html>",
	} {
		if quotaErrIsSessionDead(errors.New(msg)) {
			t.Errorf("这不是凭据失效（判宽了会摘掉整池号）: %s", msg)
		}
	}
	if quotaErrIsSessionDead(nil) {
		t.Error("nil error 不是失效")
	}
}

// TestQuotaAuthFailedIsTransient 标记必须是"**上一次**查询的结论"，成功即清。
//
// 不清的后果：用户重登之后这个号会被一直当成失效的 —— 那正是
// "禁用后刷新成功却不恢复"的一半原因。
func TestQuotaAuthFailedIsTransient(t *testing.T) {
	p := NewWithConfig(Config{})
	const uid = "260101010101010101"

	// 没查过 ⇒ 不算失效（默认不摘任何号）。
	if p.QuotaAuthFailed(uid) {
		t.Error("没查过就报失效 —— 会把好号摘掉")
	}
	p.quotaAuth.mark(uid, true)
	if !p.QuotaAuthFailed(uid) {
		t.Error("记下失效后应当报 true")
	}
	p.quotaAuth.mark(uid, false)
	if p.QuotaAuthFailed(uid) {
		t.Error("刷新成功后必须清掉标记（否则重登的号一直被误判）")
	}
}

// TestLoomyImplementsQuotaAuthFailureExt 扩展点必须真的被 ExtOf 认出来。
//
// ⚠ 这条不是形式主义：扩展点靠类型断言发现，**写成访问器方法就永远不被发现**
// （本仓有 4 个上游因此长期没有"＋ 添加账号"按钮的前例）。
func TestLoomyImplementsQuotaAuthFailureExt(t *testing.T) {
	p := NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.QuotaAuthFailureExt](p); !ok {
		t.Fatal("loomy 没有实现 gateway.QuotaAuthFailureExt —— " +
			"核心会拿不到「这次失败是不是凭据失效」的判据")
	}
}