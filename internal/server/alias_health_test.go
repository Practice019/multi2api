package server

import (
	"strings"
	"testing"
)

// 轮询必须只在「有健康账号」的候选之间进行。
//
// # 用户原话（2026-10-24）
//
//	「右边有三个我设置一样的 所以请求右边一样的模型需要有三个上游对应
//	  上游有可用健康模型才进去发请求」
//	「请求到网关直接不尝试请求这个上游 直接切换有健康账号的上游去发请求啊」
//
// # 实测背景
//
// workbuddy 6 个号全被 429 冷却时，网关仍要花 1.2-2.5 秒逐个号试；
// 而同一时刻 workbuddy-intl 的 17 个号全部健康、提供同一个模型。
func TestPickAliasTargetFiltersUnhealthy(t *testing.T) {
	src := sliceFuncBody(t, readHandlerSrc(t)+readFileSrc(t, "apikey_models.go"), "func (h *Handler) pickAliasTarget(")
	if src == "" {
		t.Fatal("找不到 pickAliasTarget")
	}
	if !strings.Contains(src, "healthyAliasCandidates") {
		t.Error("★ pickAliasTarget 没有做健康筛选 —— 会去试没有健康账号的上游")
	}
}

// 健康筛选必须用 AvailableUIDsFor（当下事实），不是熔断状态（滞后推断）。
func TestHealthyAliasCandidatesUsesLivePoolState(t *testing.T) {
	src := sliceFuncBody(t, readFileSrc(t, "apikey_models.go"), "func (h *Handler) healthyAliasCandidates(")
	if src == "" {
		t.Fatal("找不到 healthyAliasCandidates")
	}
	if !strings.Contains(src, "AvailableUIDsFor") {
		t.Error("★ 没用 AvailableUIDsFor —— 判据必须与 pickForTier 同口径")
	}
	// 渠道前缀不该被误杀（它们不在账号池里）。
	if !strings.Contains(src, "Provider.Has") {
		t.Error("★ 没区分渠道前缀 —— 渠道候选会被误判成\"无健康账号\"而全部剔除")
	}
}

// 跨上游切换：上游没号时必须切到下一个有号的，而不是直接失败。
func TestNextAliasUpstreamPicksHealthy(t *testing.T) {
	src := sliceFuncBody(t, readFileSrc(t, "apikey_models.go"), "func (h *Handler) nextAliasUpstream(")
	if src == "" {
		t.Fatal("找不到 nextAliasUpstream")
	}
	if !strings.Contains(src, "AvailableUIDsFor") {
		t.Error("★ 切上游时没检查目标上游有没有健康账号")
	}
	if !strings.Contains(src, "tried") {
		t.Error("★ 没记录已试过的上游 —— 两个上游都没号时会来回横跳（死循环）")
	}
}

// 切换只能用别名候选（用户显式配置），不能用 normModelKey 模糊猜。
//
// 实测 normModelKey 会把这四组判成同一个模型，而它们是不同的模型：
//
//	nvidia/nemotron-3.5-lightning-free vs nvidia/nemotron-3.5-lightning:free
//	poolside/laguna-s-2.1-free        vs poolside/laguna-s-2.1:free
//	qwen3.8-max                        vs qwen-3.8-max
//	qwen3.7-plus                       vs qwen-3.7-plus
//
// 换过去等于偷偷换了用户要的模型 —— 正是 2026-10-18 用户否决换上游的原因。
func TestNextAliasUpstreamDoesNotGuessByNormKey(t *testing.T) {
	src := sliceFuncBody(t, readFileSrc(t, "apikey_models.go"), "func (h *Handler) nextAliasUpstream(")
	if strings.Contains(src, "normModelKey") || strings.Contains(src, "alternateUpstream") {
		t.Error("★ nextAliasUpstream 用了模糊匹配 —— 会把不同模型判成同一个，偷偷换掉用户要的东西")
	}
}
