// autodisable_hardcredit_test.go 「额度耗尽」不得进自动禁用线（用户 2026-11-01 报障）。
//
// # 用户原话
//
//	「monkeycode 会自动封禁账号，并且第二天的免费额度恢复之后不会重新变成
//	  正常的账号。但是 loomy 根本不会封禁账号，账号的额度清空的时候也不会封禁，
//	  但是也不影响正常的使用。你想办法怎么统一起来。」
//
// # 统一后的口径（本文件钉这一半）
//
//	额度类不可用（HardCredit / SoftRate） → **永不摘号**：冷却 + 选号时跳过
//	                                        （loomy 那套语义：号还在这儿，只是这轮不用它）
//	凭据类不可用（SessionDead / NotFound / Server） → 可以摘号，且必须能被
//	                                        额度刷新成功或解封探测恢复
//
// 判据是「这个号会不会自己好」：额度耗尽次日（或充值后）必然恢复，
// 而凭证死亡、模型不存在不会。
package server

import (
	"strings"
	"testing"
)

// disabledLineSegment 取 applyErrorPolicy 里"自动禁用判定"那一段源码。
//
// ⚠ 判据落在**源码结构**上而不是"跑一次看效果"：这段逻辑的失效形态是
// "某个 kind 被悄悄加回 switch"，而那种改动在单元测试里要构造完整的
// Handler + Pool 才能暴露。本仓已有同类守卫（见 autodisable_softrate_test.go）。
func disabledLineSegment(t *testing.T) string {
	t.Helper()
	src := sliceFuncBody(t, readHandlerSrc(t), "func (h *Handler) applyErrorPolicy(")
	if src == "" {
		t.Fatal("找不到 applyErrorPolicy")
	}
	idx := strings.Index(src, "RecordAutoDisableFailure")
	if idx < 0 {
		t.Fatal("找不到 RecordAutoDisableFailure")
	}
	start := idx - 4000 // 覆盖整段注释 + switch 头（注释里会解释为什么不收某个 kind）
	if start < 0 {
		start = 0
	}
	return src[start:idx]
}

// stripComments 去掉整行注释（注释里**可以**提到被排除的 kind，那正是解释）。
func stripComments(seg string) string {
	var code []string
	for _, line := range strings.Split(seg, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	return strings.Join(code, "\n")
}

// HardCredit 不得出现在自动禁用的判定分支里。
func TestHardCreditDoesNotAutoDisable(t *testing.T) {
	code := stripComments(disabledLineSegment(t))
	if strings.Contains(code, "ErrKindHardCredit") {
		t.Error("★ 额度耗尽又被计进自动禁用线了 —— 一个「明天就自己好」的号会被今天摘掉，" +
			"而解除它要等 24 小时的解封探测（配置里常关着）。" +
			"正确动作：冷却到重置时刻 + 选号阶段跳过（见 applyErrorPolicy 的 HardCredit 分支）")
	}
}

// 不收进禁用线 ≠ 什么都不做：额度耗尽**仍必须**冷却到上游给出的重置时刻。
//
// 这条防的是"修一个 bug 顺手把正确行为也删了"：不冷却的话，那把号每来一个
// 请求就会被选中一次、白跑一次往返（每次都在客户端看来是"很慢"）。
func TestHardCreditStillCoolsDownUntilReset(t *testing.T) {
	src := sliceFuncBody(t, readHandlerSrc(t), "func (h *Handler) applyErrorPolicy(")
	idx := strings.Index(src, "case gateway.ErrKindHardCredit:")
	if idx < 0 {
		t.Fatal("找不到 HardCredit 的策略分支 —— 它不该被整段删掉，只该从禁用线里移出")
	}
	seg := src[idx:]
	if next := strings.Index(seg[1:], "case gateway.ErrKind"); next > 0 {
		seg = seg[:next+1]
	}
	if !strings.Contains(stripComments(seg), "CooldownUntilNextReset") {
		t.Error("★ 额度耗尽必须仍然冷却到「上游给出的下次重置时刻」" +
			"（否则每个请求都会白选它一次）")
	}
}
